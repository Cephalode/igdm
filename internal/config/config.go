// Package config manages account configurations and session persistence.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Account holds credentials and session info for one Instagram account.
type Account struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LLMConfig holds settings for the LLM backend used to generate replies.
type LLMConfig struct {
	BaseURL             string `json:"base_url"`               // default: https://open.bigmodel.cn/api/coding/paas/v4
	APIKeyEnv           string `json:"api_key_env"`            // env var name, default: GLM_API_KEY
	Model               string `json:"model"`                  // default: glm-5-turbo
	MaxHistory          int    `json:"max_history"`            // default: 50
	ResponseDelayMinSec int    `json:"response_delay_min_sec"` // default: 2
	ResponseDelayMaxSec int `json:"response_delay_max_sec"` // default: 8
}

// PersonalityData holds the structure of personality.json.
type PersonalityData struct {
	Personalities  map[string]string `json:"personalities"`   // account name → system prompt
	KnownContacts  map[string]string `json:"known_contacts"`  // MQTT sender ID → display name/username
	BotSenderIDs   map[string]int64  `json:"bot_sender_ids"`  // account name → MQTT sender ID
}

// Config is the top-level application configuration.
type Config struct {
	DataDir        string             `json:"data_dir"`
	Accounts       map[string]Account `json:"accounts"`
	LLM            LLMConfig          `json:"llm"`
	Personalities  map[string]string  `json:"personalities"`  // account name → system prompt
	KnownContacts  map[string]string  `json:"known_contacts"` // MQTT sender ID → display name/username
	BotSenderIDs   map[string]int64   `json:"bot_sender_ids"` // account name → MQTT sender ID
}

// SessionData stores persisted cookies + messagix state for reconnection.
type SessionData struct {
	Cookies json.RawMessage `json:"cookies"`
	State   json.RawMessage `json:"state,omitempty"`
}

// DefaultDataDir returns the default directory for session storage.
func DefaultDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".igdm")
}

// Load reads config from disk.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := &Config{
				DataDir:       DefaultDataDir(),
				Accounts:      make(map[string]Account),
				Personalities: make(map[string]string),
			}
			cfg.applyDefaults()
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	return &cfg, nil
}

// loadPersonality reads personality.json from the data directory.
// Returns nil if the file doesn't exist (caller should use defaults).
func loadPersonality(dataDir string) (*PersonalityData, error) {
	path := filepath.Join(dataDir, "personality.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read personality: %w", err)
	}
	var pd PersonalityData
	if err := json.Unmarshal(raw, &pd); err != nil {
		return nil, fmt.Errorf("parse personality: %w", err)
	}
	return &pd, nil
}

// applyDefaults sets zero-valued fields to sensible defaults.
func (c *Config) applyDefaults() {
	if c.DataDir == "" {
		c.DataDir = DefaultDataDir()
	}
	if c.Accounts == nil {
		c.Accounts = make(map[string]Account)
	}
	// LLM defaults
	if c.LLM.BaseURL == "" {
		c.LLM.BaseURL = "https://open.bigmodel.cn/api/coding/paas/v4"
	}
	if c.LLM.APIKeyEnv == "" {
		c.LLM.APIKeyEnv = "GLM_API_KEY"
	}
	if c.LLM.Model == "" {
		c.LLM.Model = "glm-5.1"
	}
	if c.LLM.MaxHistory == 0 {
		c.LLM.MaxHistory = 50
	}
	if c.LLM.ResponseDelayMinSec == 0 {
		c.LLM.ResponseDelayMinSec = 2
	}
	if c.LLM.ResponseDelayMaxSec == 0 {
		c.LLM.ResponseDelayMaxSec = 8
	}

	// Initialize maps
	if c.Personalities == nil {
		c.Personalities = make(map[string]string)
	}
	if c.KnownContacts == nil {
		c.KnownContacts = make(map[string]string)
	}
	if c.BotSenderIDs == nil {
		c.BotSenderIDs = make(map[string]int64)
	}

	// Load personality.json from data dir (overrides any inline values)
	pd, err := loadPersonality(c.DataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	if pd != nil {
		for k, v := range pd.Personalities {
			c.Personalities[k] = v
		}
		for k, v := range pd.KnownContacts {
			c.KnownContacts[k] = v
		}
		for k, v := range pd.BotSenderIDs {
			c.BotSenderIDs[k] = v
		}
	}
}

// GetLLMAPIKey reads the LLM API key from the environment variable specified in config.
func (c *Config) GetLLMAPIKey() string {
	return os.Getenv(c.LLM.APIKeyEnv)
}

// defaultPersonality is used when no personality is configured for an account.
const defaultPersonality = "You are a friendly Instagram user who has natural conversations. Keep responses casual and concise, like a real person texting."

// GetPersonality returns the system prompt / personality for the given account name.
// Falls back to the default personality if none is configured.
func (c *Config) GetPersonality(accountName string) string {
	base := defaultPersonality
	if p, ok := c.Personalities[accountName]; ok && p != "" {
		base = p
	}

	// Append known contacts so the LLM can identify who it's talking to
	if len(c.KnownContacts) > 0 {
		var contacts []string
		for id, name := range c.KnownContacts {
			contacts = append(contacts, fmt.Sprintf("- [%s]: %s", id, name))
		}
		sort.Strings(contacts)
		base += "\n\nKnown contacts (sender IDs map to usernames):\n" + strings.Join(contacts, "\n")
	}

	return base
}

// ResolveContactName returns the known name for a sender ID.
// Falls back to the raw ID as string.
func (c *Config) ResolveContactName(senderID int64) string {
	idStr := fmt.Sprintf("%d", senderID)
	if name, ok := c.KnownContacts[idStr]; ok {
		return name
	}
	return idStr
}

// GetBotSenderID returns the MQTT sender ID for a given account name.
// Returns 0 if not configured.
func (c *Config) GetBotSenderID(accountName string) int64 {
	if id, ok := c.BotSenderIDs[accountName]; ok {
		return id
	}
	return 0
}

// Save writes config to disk.
func (c *Config) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}

// SessionPath returns the session file path for an account.
func (c *Config) SessionPath(accountName string) string {
	return filepath.Join(c.DataDir, accountName+".json")
}

// SaveSession persists cookies + state for an account.
func (c *Config) SaveSession(accountName string, data *SessionData) error {
	if err := os.MkdirAll(c.DataDir, 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.SessionPath(accountName), raw, 0600)
}

// LoadSession reads persisted session data for an account.
func (c *Config) LoadSession(accountName string) (*SessionData, error) {
	raw, err := os.ReadFile(c.SessionPath(accountName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var data SessionData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

// DeleteSession removes a stored session.
func (c *Config) DeleteSession(accountName string) error {
	return os.Remove(c.SessionPath(accountName))
}
