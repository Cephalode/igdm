// Package agent implements the Instagram DM bot's AI agent, including
// per-thread conversation memory persisted to disk.
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// DailyMemory holds all messages for a single day, serialized as one JSON file.
type DailyMemory struct {
	Date     string               `json:"date"` // YYYY-MM-DD
	Messages []ConversationMessage `json:"messages"`
}

// MemoryManager manages per-thread conversation history stored on disk.
//
// Each thread (1:1 or group) gets its own directory under baseDir/<thread_id>/.
// Each day of conversation is stored as a separate JSON file (YYYY-MM-DD.json),
// and a summary.md file holds a running summary of important information.
type MemoryManager struct {
	baseDir  string
	mu       sync.RWMutex
	threadMu map[int64]*sync.RWMutex
}

// NewMemoryManager creates a new MemoryManager rooted at baseDir.
// It creates the directory if it does not already exist.
func NewMemoryManager(baseDir string) (*MemoryManager, error) {
	abs, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("resolve base dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, fmt.Errorf("create base dir: %w", err)
	}
	log.Debug().Str("path", abs).Msg("memory manager initialized")
	return &MemoryManager{
		baseDir:  abs,
		threadMu: make(map[int64]*sync.RWMutex),
	}, nil
}

// getThreadMu returns a per-thread mutex, creating one lazily if needed.
func (m *MemoryManager) getThreadMu(threadID int64) *sync.RWMutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mu, ok := m.threadMu[threadID]; ok {
		return mu
	}
	mu := &sync.RWMutex{}
	m.threadMu[threadID] = mu
	return mu
}

// GetThreadDir returns the filesystem path to the thread's memory directory.
func (m *MemoryManager) GetThreadDir(threadID int64) string {
	return filepath.Join(m.baseDir, fmt.Sprintf("%d", threadID))
}

// StoreMessage appends a message to today's daily JSON file for the given thread.
// It creates the thread directory and/or daily file as needed.
func (m *MemoryManager) StoreMessage(threadID int64, msg ConversationMessage) error {

	mu := m.getThreadMu(threadID)
	mu.Lock()
	defer mu.Unlock()

	threadDir := m.GetThreadDir(threadID)
	if err := os.MkdirAll(threadDir, 0700); err != nil {
		return fmt.Errorf("create thread dir: %w", err)
	}

	today := time.Now().UTC().Format("2006-01-02")
	filePath := filepath.Join(threadDir, today+".json")

	var daily DailyMemory

	raw, err := os.ReadFile(filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read daily file: %w", err)
		}
		// New file
		daily = DailyMemory{Date: today}
	} else {
		if err := json.Unmarshal(raw, &daily); err != nil {
			return fmt.Errorf("parse daily file: %w", err)
		}
	}

	daily.Messages = append(daily.Messages, msg)

	out, err := json.MarshalIndent(daily, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal daily memory: %w", err)
	}

	if err := os.WriteFile(filePath, out, 0600); err != nil {
		return fmt.Errorf("write daily file: %w", err)
	}

	log.Debug().
		Int64("thread", threadID).
		Str("date", today).
		Str("role", msg.Role).
		Int("total_messages", len(daily.Messages)).
		Msg("message stored")

	return nil
}

// GetRecentHistory loads up to limit messages from today and previous days,
// returned in chronological order (oldest first). If limit is 0, a default
// of 50 is used.
func (m *MemoryManager) GetRecentHistory(threadID int64, limit int) ([]ConversationMessage, error) {
	if limit == 0 {
		limit = 50
	}

	mu := m.getThreadMu(threadID)
	mu.Lock()
	defer mu.Unlock()

	threadDir := m.GetThreadDir(threadID)

	// List all .json files in the thread directory.
	entries, err := os.ReadDir(threadDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read thread dir: %w", err)
	}

	var dates []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		dateStr := strings.TrimSuffix(e.Name(), ".json")
		// Validate YYYY-MM-DD format loosely.
		if len(dateStr) == 10 {
			dates = append(dates, dateStr)
		}
	}
	if len(dates) == 0 {
		return nil, nil
	}

	// Sort newest first so we can fill from the most recent day backwards.
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))

	var allMsgs []ConversationMessage
	remaining := limit

	for _, date := range dates {
		if remaining <= 0 {
			break
		}
		filePath := filepath.Join(threadDir, date+".json")
		raw, err := os.ReadFile(filePath)
		if err != nil {
			log.Warn().Err(err).Str("file", filePath).Msg("skip unreadable daily file")
			continue
		}
		var daily DailyMemory
		if err := json.Unmarshal(raw, &daily); err != nil {
			log.Warn().Err(err).Str("file", filePath).Msg("skip unparseable daily file")
			continue
		}

		msgs := daily.Messages
		if len(msgs) > remaining {
			// Take only the last 'remaining' messages from this day.
			msgs = msgs[len(msgs)-remaining:]
		}
		// Prepend to maintain chronological order (we iterate newest first).
		allMsgs = append(msgs, allMsgs...)
		remaining = limit - len(allMsgs)
	}

	return allMsgs, nil
}

// GetSummary reads the summary.md file for the given thread.
// Returns an empty string if no summary exists.
func (m *MemoryManager) GetSummary(threadID int64) (string, error) {
	mu := m.getThreadMu(threadID)
	mu.RLock()
	defer mu.RUnlock()

	summaryPath := filepath.Join(m.GetThreadDir(threadID), "summary.md")
	raw, err := os.ReadFile(summaryPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read summary: %w", err)
	}
	return string(raw), nil
}

// UpdateSummary writes or overwrites the summary.md file for the given thread.
func (m *MemoryManager) UpdateSummary(threadID int64, summary string) error {
	mu := m.getThreadMu(threadID)
	mu.Lock()
	defer mu.Unlock()

	threadDir := m.GetThreadDir(threadID)
	if err := os.MkdirAll(threadDir, 0700); err != nil {
		return fmt.Errorf("ensure thread dir: %w", err)
	}

	summaryPath := filepath.Join(threadDir, "summary.md")
	if err := os.WriteFile(summaryPath, []byte(summary), 0600); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}

	log.Debug().
		Int64("thread", threadID).
		Int("summary_len", len(summary)).
		Msg("summary updated")

	return nil
}
