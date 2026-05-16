// Package agent provides an LLM agent that generates responses to Instagram DMs
// using the ZhipuAI OpenAI-compatible chat completions API.
//
// The agent maintains per-conversation history via a MemoryManager (defined in
// memory.go), builds context-rich prompts, and adds a random human-like delay
// before returning responses. All HTTP communication uses only the Go standard library.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// Config holds the settings for an LLMAgent. Apply defaults with ApplyDefaults().
type Config struct {
	// BaseURL is the ZhipuAI API base URL (e.g. "https://open.bigmodel.cn/api/coding/paas/v4").
	BaseURL string

	// APIKey is the Bearer token used for Authorization. If empty, it is read
	// from the GLM_API_KEY environment variable at agent creation time.
	APIKey string

	// Model is the chat model identifier (e.g. "glm-5-turbo").
	Model string

	// SystemPrompt is injected as the first system message in every request.
	SystemPrompt string

	// MaxHistoryMessages caps the number of recent conversation messages sent
	// to the API (excluding the system prompt). Zero means unlimited.
	MaxHistoryMessages int

	// ResponseDelayMin is the minimum random delay before returning a response,
	// simulating human typing behaviour.
	ResponseDelayMin time.Duration

	// ResponseDelayMax is the maximum random delay before returning a response.
	ResponseDelayMax time.Duration
}

// ApplyDefaults fills in zero-valued fields with sensible defaults.
func (c *Config) ApplyDefaults() {
	if c.BaseURL == "" {
		c.BaseURL = "https://open.bigmodel.cn/api/coding/paas/v4"
	}
	if c.Model == "" {
		c.Model = "glm-5.1"
	}
	if c.MaxHistoryMessages == 0 {
		c.MaxHistoryMessages = 50
	}
	if c.ResponseDelayMin == 0 {
		c.ResponseDelayMin = 2 * time.Second
	}
	if c.ResponseDelayMax == 0 {
		c.ResponseDelayMax = 8 * time.Second
	}
}

// ---------------------------------------------------------------------------
// Conversation primitives
// ---------------------------------------------------------------------------

// ConversationMessage is a single message within a conversation history.
// The concrete memory implementation (memory.go) stores and retrieves these.
type ConversationMessage struct {
	Role       string    `json:"role"`        // "user" or "assistant"
	Content    string    `json:"content"`
	Timestamp  time.Time `json:"timestamp"`
	SenderName string    `json:"sender_name"` // populated for user messages
}

// ---------------------------------------------------------------------------
// API types (OpenAI-compatible)
// ---------------------------------------------------------------------------

// ChatMessage is the message format used in the chat completions request/response.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is the JSON body sent to /chat/completions.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
}

// chatResponse is the JSON body returned from /chat/completions.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// ---------------------------------------------------------------------------
// Response helper
// ---------------------------------------------------------------------------

// Response wraps the result of a GenerateResponse call.
type Response struct {
	Text  string
	Error error
}

// ---------------------------------------------------------------------------
// LLMAgent
// ---------------------------------------------------------------------------

// LLMAgent calls the ZhipuAI chat completions API to generate DM responses.
// It is safe for concurrent use — a mutex serialises API calls while the
// MemoryManager handles per-thread locking internally.
type LLMAgent struct {
	config     Config
	httpClient *http.Client
	memory     *MemoryManager
	mu         sync.Mutex // serialises API calls
}

// NewLLMAgent creates a new agent. The caller must supply a Config and a
// non-nil MemoryManager. If Config.APIKey is empty the GLM_API_KEY
// environment variable is used.
func NewLLMAgent(config Config, memory *MemoryManager) *LLMAgent {
	config.ApplyDefaults()

	if config.APIKey == "" {
		config.APIKey = os.Getenv("GLM_API_KEY")
	}

	return &LLMAgent{
		config: config,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		memory: memory,
	}
}

// GenerateResponse is the main entry point. Given an incoming DM it:
//  1. Loads conversation history from the memory manager.
//  2. Stores the incoming user message to disk.
//  3. Builds the full messages array (system prompt + optional summary + history).
//  4. Calls the OpenAI-compatible chat completions API.
//  5. Stores the assistant response to disk.
//  6. Waits for a random human-like delay, then returns the response text.
func (a *LLMAgent) GenerateResponse(ctx context.Context, threadID int64, senderName string, incomingText string) (string, error) {
	// ---- 2. Record the incoming message ----
	userMsg := ConversationMessage{
		Role:       "user",
		Content:    incomingText,
		Timestamp:  time.Now(),
		SenderName: senderName,
	}
	if err := a.memory.StoreMessage(threadID, userMsg); err != nil {
		log.Warn().Err(err).Int64("thread", threadID).Msg("failed to store user message")
	}

	// ---- 1 & 3. Load history and build messages array ----
	history, err := a.memory.GetRecentHistory(threadID, a.config.MaxHistoryMessages)
	if err != nil {
		log.Warn().Err(err).Int64("thread", threadID).Msg("failed to load history")
	}

	messages := a.buildMessages(history)

	log.Debug().
		Int64("thread", threadID).
		Str("sender", senderName).
		Int("history_len", len(history)).
		Int("messages_len", len(messages)).
		Msg("calling LLM API")

	// ---- 4. Call the API (serialised to avoid concurrent requests) ----
	a.mu.Lock()
	reply, err := a.callAPI(ctx, messages)
	a.mu.Unlock()

	if err != nil {
		log.Error().
			Err(err).
			Int64("thread", threadID).
			Msg("LLM API call failed")
		return "", fmt.Errorf("LLM API call: %w", err)
	}

	// ---- 5. Save the assistant response ----
	assistantMsg := ConversationMessage{
		Role:      "assistant",
		Content:   reply,
		Timestamp: time.Now(),
	}
	if err := a.memory.StoreMessage(threadID, assistantMsg); err != nil {
		log.Warn().Err(err).Int64("thread", threadID).Msg("failed to store assistant message")
	}

	log.Info().
		Int64("thread", threadID).
		Str("sender", senderName).
		Str("reply", truncate(reply, 80)).
		Msg("LLM response generated")

	// ---- 6. Human-like delay ----
	delay := a.randomDelay()
	log.Debug().Dur("delay", delay).Msg("applying response delay")

	select {
	case <-time.After(delay):
		// delay elapsed normally
	case <-ctx.Done():
		// context cancelled — return the response anyway
	}

	return reply, nil
}

// buildMessages assembles the messages payload for the API.
// The structure is: [system prompt] + [optional summary] + [recent history (already includes new message)].
func (a *LLMAgent) buildMessages(history []ConversationMessage) []ChatMessage {
	var messages []ChatMessage

	// System prompt
	if a.config.SystemPrompt != "" {
		messages = append(messages, ChatMessage{
			Role:    "system",
			Content: a.config.SystemPrompt,
		})
	}

	// Optional conversation summary injected as a system message.
	// This provides long-term context beyond what fits in MaxHistoryMessages.
	// (Disabled by default — enable by calling memory.UpdateSummary externally.)

	// Recent history (already capped by GetRecentHistory limit)
	for _, m := range history {
		content := m.Content
		// Prefix user messages with sender name so the LLM knows who's talking
		if m.Role == "user" && m.SenderName != "" {
			content = "[" + m.SenderName + "]: " + content
		}
		messages = append(messages, ChatMessage{
			Role:    m.Role,
			Content: content,
		})
	}

	return messages
}

// callAPI makes a POST request to the /chat/completions endpoint and returns
// the assistant's reply text.
func (a *LLMAgent) callAPI(ctx context.Context, messages []ChatMessage) (string, error) {
	reqBody := chatRequest{
		Model:    a.config.Model,
		Messages: messages,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	url := a.config.BaseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.config.APIKey)

	log.Debug().
		Str("url", url).
		Str("model", a.config.Model).
		Int("messages", len(messages)).
		Msg("sending chat completions request")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(respBytes))
	}

	var chatResp chatResponse
	if err := json.Unmarshal(respBytes, &chatResp); err != nil {
		return "", fmt.Errorf("unmarshal response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("API returned no choices")
	}

	content := chatResp.Choices[0].Message.Content
	if content == "" {
		return "", fmt.Errorf("API returned empty content")
	}

	return content, nil
}

// randomDelay returns a uniform random duration in [min, max).
func (a *LLMAgent) randomDelay() time.Duration {
	min := a.config.ResponseDelayMin
	max := a.config.ResponseDelayMax
	if min >= max {
		return min
	}
	// math/rand/v2: Int64N returns a uniform value in [0, n).
	jitter := rand.Int64N(int64(max - min))
	return min + time.Duration(jitter)
}

// truncate shortens a string for logging purposes.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
