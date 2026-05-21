package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/ocythoe/igdm-go/internal/agent"
	"github.com/ocythoe/igdm-go/internal/client"
	"github.com/ocythoe/igdm-go/internal/config"
	"github.com/ocythoe/igdm-go/internal/listener"
	"github.com/ocythoe/igdm-go/internal/login"
)

const configPath = "~/.igdm/config.json"

func main() {
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	log.Logger = log.Output(zerolog.NewConsoleWriter())

	// Parse --verbose flag before dispatching commands
	verbose := false
	filteredArgs := make([]string, 0, len(os.Args))
	for _, arg := range os.Args {
		if arg == "--verbose" || arg == "-v" {
			verbose = true
		} else {
			filteredArgs = append(filteredArgs, arg)
		}
	}
	os.Args = filteredArgs

	if verbose {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatal().Err(err).Msg("load config")
	}

	cmd := os.Args[1]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Enable debug logging for listen/agent commands (unless already set)
	if !verbose && (cmd == "listen" || cmd == "listen-all" || cmd == "agent" || cmd == "agent-all") {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}

	// Handle Ctrl+C
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Info().Msg("shutting down...")
		cancel()
	}()

	switch cmd {
	case "login":
		if len(os.Args) < 3 {
			fmt.Println("Usage: igdm login <account>")
			os.Exit(1)
		}
		runLogin(ctx, cfg, os.Args[2])

	case "send":
		if len(os.Args) < 5 {
			fmt.Println("Usage: igdm send <account> <recipient_username> <message>")
			os.Exit(1)
		}
		runSend(ctx, cfg, os.Args[2], os.Args[3], strings.Join(os.Args[4:], " "))

	case "listen":
		if len(os.Args) < 3 {
			fmt.Println("Usage: igdm listen <account>")
			os.Exit(1)
		}
		runListen(ctx, cfg, os.Args[2])

	case "listen-all":
		runListenAll(ctx, cfg)

	case "whoami":
		if len(os.Args) < 3 {
			fmt.Println("Usage: igdm whoami <account>")
			os.Exit(1)
		}
		runWhoami(ctx, cfg, os.Args[2])

	case "threads":
		if len(os.Args) < 3 {
			fmt.Println("Usage: igdm threads <account>")
			os.Exit(1)
		}
		runThreads(ctx, cfg, os.Args[2])

	case "debug":
		if len(os.Args) < 3 {
			fmt.Println("Usage: igdm debug <account> [other_account]")
			os.Exit(1)
		}
		otherAccount := ""
		if len(os.Args) >= 4 {
			otherAccount = os.Args[3]
		}
		runDebug(ctx, cfg, os.Args[2], otherAccount)

	case "history":
		if len(os.Args) < 4 {
			fmt.Println("Usage: igdm history <account> <thread_key> [limit]")
			os.Exit(1)
		}
		limit := 20
		if len(os.Args) >= 5 {
			if n, err := strconv.Atoi(os.Args[4]); err == nil && n > 0 {
				limit = n
			}
		}
		runHistory(ctx, cfg, os.Args[2], os.Args[3], limit)

	case "config":
		runConfig(cfg, os.Args[2:])

	case "agent":
		if len(os.Args) < 3 {
			fmt.Println("Usage: igdm agent <account>")
			os.Exit(1)
		}
		runAgent(ctx, cfg, os.Args[2])

	case "agent-all":
		runAgentAll(ctx, cfg)

	default:
		fmt.Printf("Unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`igdm - Instagram DM bot using mautrix-meta/messagix

Usage:
  igdm [flags] <command> [args...]

Flags:
  --verbose, -v    Enable debug logging for all commands

Commands:
  login <account>                      Login and save session
  send <account> <user> <message>      Send a DM
  listen <account>                     Listen for incoming messages
  listen-all                           Listen on all accounts
  agent <account>                      Auto-reply to DMs using LLM
  agent-all                            Auto-reply on all accounts
  whoami <account>                     Show logged in user info
  threads <account>                    List recent threads
  history <account> <thread_key> [N]   Show last N messages in a thread (default 20)
  config show                          Show current configuration
  config set <key> <value>             Set a configuration value
  config accounts                      List configured accounts
  debug <account> [other_account]      Debug/diagnostic output

Setup:
  1. Copy personality.json.example to ~/.igdm/personality.json
  2. Edit with your account names, personalities, and contact info
  3. Set the GLM_API_KEY environment variable (or configure api_key_env)
  4. Run: igdm login <account>

Architecture:
  Uses go.mau.fi/mautrix-meta/pkg/messagix — the same Go library
  that Beeper uses to bridge Instagram DMs. It implements Instagram's
  web messaging protocol: MQTT-over-WebSocket + Facebook's LightSpeed
  binary sync protocol for real-time message delivery.

Key fixes (Beeper pattern):
  - PostHandlePublishResponse called after every publish event to advance
    sync group cursors (without this, Instagram stops pushing new messages)
  - State persistence via DumpState/LoadState preserves sync cursors across
    restarts so the server knows where to resume pushing deltas
  - Composite event handler set once in Connect, never replaced

Examples:
  igdm login myaccount
  igdm send myaccount friend_username "Hello!"
  igdm listen myaccount
  igdm --verbose threads myaccount
  igdm history myaccount 123456789 50
  igdm config show
  igdm config set llm.model glm-5.1`)
}

func getAccount(cfg *config.Config, name string) (config.Account, bool) {
	a, ok := cfg.Accounts[name]
	return a, ok
}

func runLogin(ctx context.Context, cfg *config.Config, accountName string) {
	acct, ok := getAccount(cfg, accountName)
	if !ok {
		log.Fatal().Str("account", accountName).Msg("unknown account")
	}

	fmt.Printf("Logging in as @%s...\n", acct.Username)
	result, err := login.Login(ctx, acct.Username, acct.Password)
	if err != nil {
		log.Fatal().Err(err).Msg("login failed")
	}

	// Create client and connect to save full session
	cli := client.NewClientWithCookies(accountName, result.Cookies)
	if err := cli.Connect(ctx); err != nil {
		log.Fatal().Err(err).Msg("connect failed")
	}

	// Wait a moment for initial sync
	time.Sleep(3 * time.Second)

	// Save session (cookies + state with sync cursors)
	session, err := cli.SaveSession()
	if err != nil {
		log.Fatal().Err(err).Msg("save session failed")
	}

	if err := cfg.SaveSession(accountName, session); err != nil {
		log.Fatal().Err(err).Msg("persist session failed")
	}

	cli.Disconnect()
	fmt.Printf("✅ Logged in as @%s (user_id=%d). Session saved.\n", acct.Username, result.UserID)
}

func runSend(ctx context.Context, cfg *config.Config, accountName, recipient, message string) {
	cli := connectAccount(ctx, cfg, accountName)
	defer cli.Disconnect()

	fmt.Printf("Sending to @%s via %s...\n", recipient, accountName)
	if err := cli.SendToUser(ctx, recipient, message); err != nil {
		log.Fatal().Err(err).Msg("send failed")
	}
	fmt.Printf("✅ Message sent to @%s\n", recipient)

	// Save session state after send
	saveSessionState(cfg, accountName, cli)
}

func runListen(ctx context.Context, cfg *config.Config, accountName string) {
	cli := connectAccount(ctx, cfg, accountName)
	defer func() {
		saveSessionState(cfg, accountName, cli)
		cli.Disconnect()
	}()

	// Set up listener — this now just sets the userHandler field,
	// the composite handler is already active from Connect()
	lst := listener.NewListener(accountName, cli.OwnUserID())
	lst.AddHandler(listener.HandlerFunc(func(msg *listener.Message) {
		fmt.Println(listener.FormatMessage(accountName, msg, nil))
	}))
	cli.SetEventHandler(lst.HandleEvent)

	fmt.Printf("🎧 Listening for messages on @%s (user_id=%d). Press Ctrl+C to stop.\n",
		accountName, cli.OwnUserID())

	<-ctx.Done()
}

func runListenAll(ctx context.Context, cfg *config.Config) {
	var clients []*client.IGClient

	for name := range cfg.Accounts {
		cli, err := tryConnectAccount(ctx, cfg, name)
		if err != nil {
			log.Error().Err(err).Str("account", name).Msg("connect failed, skipping")
			continue
		}

		lst := listener.NewListener(name, cli.OwnUserID())
		lst.AddHandler(listener.HandlerFunc(func(msg *listener.Message) {
			fmt.Println(listener.FormatMessage(name, msg, nil))
		}))
		cli.SetEventHandler(lst.HandleEvent)
		clients = append(clients, cli)

		fmt.Printf("🎧 Listening on @%s (user_id=%d)\n", name, cli.OwnUserID())
	}

	if len(clients) == 0 {
		log.Fatal().Msg("no accounts connected")
	}

	fmt.Println("Listening on all accounts. Press Ctrl+C to stop.")
	<-ctx.Done()

	for _, c := range clients {
		saveSessionState(cfg, c.Name(), c)
		c.Disconnect()
	}
}

func runWhoami(ctx context.Context, cfg *config.Config, accountName string) {
	cli := connectAccount(ctx, cfg, accountName)
	defer cli.Disconnect()

	userInfo, err := cli.GetCurrentAccount(ctx)
	if err != nil {
		log.Fatal().Err(err).Msg("get current account")
	}

	fmt.Printf("Account: %s\n", accountName)
	fmt.Printf("  Username: %s\n", userInfo.GetUsername())
	fmt.Printf("  Name: %s\n", userInfo.GetName())
	fmt.Printf("  User ID: %d\n", userInfo.GetFBID())
	fmt.Printf("  Avatar: %s\n", userInfo.GetAvatarURL())
}

func runThreads(ctx context.Context, cfg *config.Config, accountName string) {
	cli := connectAccount(ctx, cfg, accountName)
	defer cli.Disconnect()

	threads, err := cli.GetThreads(ctx)
	if err != nil {
		log.Fatal().Err(err).Msg("fetch threads")
	}

	if len(threads) == 0 {
		fmt.Println("No threads found.")
		return
	}

	fmt.Printf("Threads for @%s:\n", accountName)
	for i, t := range threads {
		groupLabel := ""
		if t.IsGroup {
			groupLabel = " [GROUP]"
		}
		name := t.Name
		if name == "" {
			name = fmt.Sprintf("thread_%d", t.Key)
		}
		fmt.Printf("  %d. %s (key=%d)%s\n", i+1, name, t.Key, groupLabel)
	}
}

func runDebug(ctx context.Context, cfg *config.Config, accountName, otherAccount string) {
	fmt.Printf("=== DEBUG: @%s ===\n\n", accountName)

	acct, ok := getAccount(cfg, accountName)
	if !ok {
		log.Fatal().Str("account", accountName).Msg("unknown account")
	}

	// Step 1: Login to get the login user_id
	fmt.Println("--- STEP 1: Login user_id ---")
	result, err := login.Login(ctx, acct.Username, acct.Password)
	if err != nil {
		log.Fatal().Err(err).Msg("login failed")
	}
	fmt.Printf("Login returned user_id: %d\n\n", result.UserID)

	// Step 2: LoadMessagesPage to get the LSTable and user info from the HTML page
	fmt.Println("--- STEP 2: LoadMessagesPage initial sync data ---")
	cli := client.NewClientWithCookies(accountName, result.Cookies)
	userInfo, lsTable, pageUserID, err := cli.DebugDumpInitialSync(ctx)
	if err != nil {
		log.Fatal().Err(err).Msg("debug dump initial sync failed")
	}

	if userInfo != nil {
		fmt.Printf("UserInfo from page:\n")
		fmt.Printf("  Username: %s\n", userInfo.GetUsername())
		fmt.Printf("  Name:     %s\n", userInfo.GetName())
		fmt.Printf("  FBID:     %d\n", userInfo.GetFBID())
		fmt.Printf("  Avatar:   %s\n", userInfo.GetAvatarURL())
	}
	fmt.Printf("Page ownUserID (FBID): %d\n", pageUserID)
	fmt.Println()

	if lsTable != nil {
		fields := lsTable.NonNilFields()
		fmt.Printf("Non-nil fields (%d): %v\n\n", len(fields), fields)

		fmt.Println("LSDeleteThenInsertThread entries:")
		for i, t := range lsTable.LSDeleteThenInsertThread {
			if t != nil {
				fmt.Printf("  [%d] ThreadKey=%d ThreadType=%d ThreadName=%q FolderName=%q MailboxType=%d\n",
					i, t.ThreadKey, t.ThreadType, t.ThreadName, t.FolderName, t.MailboxType)
			}
		}
		fmt.Printf("  (count: %d)\n\n", len(lsTable.LSDeleteThenInsertThread))

		fmt.Println("LSUpdateOrInsertThread entries:")
		for i, t := range lsTable.LSUpdateOrInsertThread {
			if t != nil {
				fmt.Printf("  [%d] ThreadKey=%d ThreadType=%d ThreadName=%q FolderName=%q MailboxType=%d\n",
					i, t.ThreadKey, t.ThreadType, t.ThreadName, t.FolderName, t.MailboxType)
			}
		}
		fmt.Printf("  (count: %d)\n\n", len(lsTable.LSUpdateOrInsertThread))

		fmt.Println("LSVerifyContactRowExists entries:")
		for i, c := range lsTable.LSVerifyContactRowExists {
			if c != nil {
				fmt.Printf("  [%d] ContactId=%d Name=%q SecondaryName=%q\n",
					i, c.ContactId, c.Name, c.SecondaryName)
			}
		}
		fmt.Printf("  (count: %d)\n\n", len(lsTable.LSVerifyContactRowExists))

		fmt.Println("LSUpsertMessage entries:")
		for i, m := range lsTable.LSUpsertMessage {
			if m != nil {
				fmt.Printf("  [%d] ThreadKey=%d SenderId=%d Text=%q\n", i, m.ThreadKey, m.SenderId, m.Text)
			}
		}
		fmt.Printf("  (count: %d)\n\n", len(lsTable.LSUpsertMessage))

		// Dump full table
		jsonData := MarshalJSON(lsTable)
		if len(jsonData) > 50000 {
			jsonData = jsonData[:50000] + "\n... (truncated)"
		}
		fmt.Println("=== FULL LSTable JSON (truncated to 50KB) ===")
		fmt.Println(jsonData)
		fmt.Println("=== END LSTable JSON ===")
	} else {
		fmt.Println("  LSTable is NIL!")
	}

	if otherAccount == "" {
		fmt.Println("No other account specified, skipping search/send test.")
		return
	}

	otherAcct, ok := getAccount(cfg, otherAccount)
	if !ok {
		log.Fatal().Str("account", otherAccount).Msg("unknown other account")
	}

	connectedCli := client.NewClientWithCookies(accountName, result.Cookies)
	if err := connectedCli.Connect(ctx); err != nil {
		log.Fatal().Err(err).Msg("connect failed")
	}
	defer connectedCli.Disconnect()

	fmt.Printf("Connected as @%s (ownUserID=%d)\n\n", accountName, connectedCli.OwnUserID())

	fmt.Printf("--- Search for @%s ---\n", otherAcct.Username)
	searchResults, err := connectedCli.DebugSearchUserFull(ctx, otherAcct.Username)
	if err != nil {
		log.Error().Err(err).Msg("search failed")
	} else {
		fmt.Printf("Search results (%d):\n", len(searchResults))
		for i, r := range searchResults {
			if r != nil {
				fmt.Printf("  [%d] ResultId=%q DisplayName=%q FBID=%d\n",
					i, r.ResultId, r.DisplayName, r.GetFBID())
			}
		}
	}
	fmt.Println()

	// Send test
	fmt.Printf("--- Send test message to @%s ---\n", otherAcct.Username)
	testMsg := fmt.Sprintf("debug test %d", time.Now().Unix())
	if err := connectedCli.SendToUser(ctx, otherAcct.Username, testMsg); err != nil {
		log.Error().Err(err).Msg("send failed")
	} else {
		fmt.Printf("✅ Message sent: %q\n", testMsg)
	}

	saveSessionState(cfg, accountName, connectedCli)
	fmt.Println("=== DEBUG COMPLETE ===")
}

func runHistory(ctx context.Context, cfg *config.Config, accountName, threadKeyStr string, limit int) {
	threadKey, err := strconv.ParseInt(threadKeyStr, 10, 64)
	if err != nil {
		log.Fatal().Err(err).Str("thread_key", threadKeyStr).Msg("invalid thread key")
	}

	cli := connectAccount(ctx, cfg, accountName)
	defer func() {
		saveSessionState(cfg, accountName, cli)
		cli.Disconnect()
	}()

	messages, err := cli.GetThreadHistory(ctx, threadKey, limit)
	if err != nil {
		log.Fatal().Err(err).Msg("fetch thread history")
	}

	if len(messages) == 0 {
		fmt.Printf("No messages found in thread %d.\n", threadKey)
		return
	}

	fmt.Printf("Message history for thread %d (last %d messages):\n", threadKey, len(messages))
	fmt.Println(strings.Repeat("─", 60))
	for _, m := range messages {
		sender := fmt.Sprintf("user_%d", m.SenderID)
		if m.SenderID == cli.OwnUserID() {
			sender = "you"
		}
		fmt.Printf("  %s  %-12s  %s\n",
			m.Timestamp.Format("2006-01-02 15:04:05"),
			sender,
			m.Text,
		)
	}
	fmt.Println(strings.Repeat("─", 60))
	fmt.Printf("(%d messages)\n", len(messages))
}

func runConfig(cfg *config.Config, args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: igdm config <show|set|accounts> [key] [value]")
		os.Exit(1)
	}

	subCmd := args[0]
	switch subCmd {
	case "show":
		// Show config, masking passwords
		masked := &ConfigDisplay{
			DataDir:  cfg.DataDir,
			Accounts: make(map[string]AccountDisplay),
			LLM: LLMConfigDisplay{
				BaseURL:             cfg.LLM.BaseURL,
				APIKeyEnv:           cfg.LLM.APIKeyEnv,
				Model:               cfg.LLM.Model,
				MaxHistory:          cfg.LLM.MaxHistory,
				ResponseDelayMinSec: cfg.LLM.ResponseDelayMinSec,
				ResponseDelayMaxSec: cfg.LLM.ResponseDelayMaxSec,
			},
		}
		for name, acct := range cfg.Accounts {
			masked.Accounts[name] = AccountDisplay{
				Username: acct.Username,
				Password: "********",
			}
		}
		out, _ := json.MarshalIndent(masked, "", "  ")
		fmt.Println(string(out))

	case "set":
		if len(args) < 3 {
			fmt.Println("Usage: igdm config set <key> <value>")
			fmt.Println("\nSupported keys:")
			fmt.Println("  llm.model          - LLM model name")
			fmt.Println("  llm.base_url       - LLM API base URL")
			fmt.Println("  llm.api_key_env    - Environment variable for API key")
			fmt.Println("  llm.max_history    - Max conversation history per thread")
			fmt.Println("  llm.response_delay_min_sec - Min delay before reply (seconds)")
			fmt.Println("  llm.response_delay_max_sec - Max delay before reply (seconds)")
			os.Exit(1)
		}
		key, value := args[1], args[2]
		if err := setConfigValue(cfg, key, value); err != nil {
			log.Fatal().Err(err).Str("key", key).Msg("failed to set config value")
		}
		if err := cfg.Save(configPath); err != nil {
			log.Fatal().Err(err).Msg("failed to save config")
		}
		fmt.Printf("✅ Set %s = %s\n", key, value)

	case "accounts":
		if len(cfg.Accounts) == 0 {
			fmt.Println("No accounts configured.")
			return
		}
		fmt.Println("Configured accounts:")
		for name, acct := range cfg.Accounts {
			// Check if session exists
			sess, _ := cfg.LoadSession(name)
			sessionStatus := "no session"
			if sess != nil && sess.Cookies != nil {
				sessionStatus = "session saved"
			}
			fmt.Printf("  %-20s  @%-20s  [%s]\n", name, acct.Username, sessionStatus)
		}

	default:
		fmt.Printf("Unknown config subcommand: %s\n", subCmd)
		fmt.Println("Use: config <show|set|accounts>")
		os.Exit(1)
	}
}

// ConfigDisplay types for masked output
type AccountDisplay struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type LLMConfigDisplay struct {
	BaseURL             string `json:"base_url"`
	APIKeyEnv           string `json:"api_key_env"`
	Model               string `json:"model"`
	MaxHistory          int    `json:"max_history"`
	ResponseDelayMinSec int    `json:"response_delay_min_sec"`
	ResponseDelayMaxSec int    `json:"response_delay_max_sec"`
}

type ConfigDisplay struct {
	DataDir  string                      `json:"data_dir"`
	Accounts map[string]AccountDisplay   `json:"accounts"`
	LLM      LLMConfigDisplay            `json:"llm"`
}

func setConfigValue(cfg *config.Config, key, value string) error {
	switch key {
	case "llm.model":
		cfg.LLM.Model = value
	case "llm.base_url":
		cfg.LLM.BaseURL = value
	case "llm.api_key_env":
		cfg.LLM.APIKeyEnv = value
	case "llm.max_history":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer: %w", err)
		}
		cfg.LLM.MaxHistory = n
	case "llm.response_delay_min_sec":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer: %w", err)
		}
		cfg.LLM.ResponseDelayMinSec = n
	case "llm.response_delay_max_sec":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid integer: %w", err)
		}
		cfg.LLM.ResponseDelayMaxSec = n
	default:
		return fmt.Errorf("unknown config key: %s", key)
	}
	return nil
}

func runAgent(ctx context.Context, cfg *config.Config, accountName string) {
	apiKey := cfg.GetLLMAPIKey()
	if apiKey == "" {
		log.Fatal().Str("env", cfg.LLM.APIKeyEnv).Msg("LLM API key not set")
	}

	cli := connectAccount(ctx, cfg, accountName)
	defer func() {
		saveSessionState(cfg, accountName, cli)
		cli.Disconnect()
	}()

	// Set up memory manager
	memoryDir := filepath.Join(cfg.DataDir, "memory")
	mem, err := agent.NewMemoryManager(memoryDir)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init memory manager")
	}

	// Set up LLM agent
	llmAgent := agent.NewLLMAgent(agent.Config{
		BaseURL:           cfg.LLM.BaseURL,
		APIKey:            apiKey,
		Model:             cfg.LLM.Model,
		SystemPrompt:      cfg.GetPersonality(accountName),
		MaxHistoryMessages: cfg.LLM.MaxHistory,
		ResponseDelayMin:  time.Duration(cfg.LLM.ResponseDelayMinSec) * time.Second,
		ResponseDelayMax:  time.Duration(cfg.LLM.ResponseDelayMaxSec) * time.Second,
	}, mem)

	// Set up following cache — populated from MQTT sync data
	followingCache := client.NewFollowingCache()

	// Track which threads we've already replied to
	var repliedMu sync.Mutex
	replied := make(map[string]bool)

	// Track our own sent messages to skip their echo
	var sentMu sync.Mutex
	sentMessages := make(map[string]bool)

	addSentMessage := func(threadID int64, text string) {
		key := fmt.Sprintf("%d:%s", threadID, text)
		sentMu.Lock()
		sentMessages[key] = true
		sentMu.Unlock()
	}

	isSentByUs := func(threadID int64, text string) bool {
		key := fmt.Sprintf("%d:%s", threadID, text)
		sentMu.Lock()
		found := sentMessages[key]
		if found {
			delete(sentMessages, key)
		}
		sentMu.Unlock()
		return found
	}

	// Set up listener
	lst := listener.NewListener(accountName, cli.OwnUserID())
	lst.SetFollowingCache(followingCache)

	// Register bot sender IDs from config to prevent bot-to-bot loops
	for _, senderID := range cfg.BotSenderIDs {
		if senderID != 0 {
			lst.AddBotUserID(senderID)
		}
	}

	ownSenderID := cfg.GetBotSenderID(accountName)
	if ownSenderID != 0 {
		lst.SetOwnMQTTSenderID(ownSenderID)
	}

	ownUserID := cli.OwnUserID()
	selfIDs := map[int64]bool{
		ownUserID: true,
	}
	if ownSenderID != 0 {
		selfIDs[ownSenderID] = true
	}

	lst.AddHandler(listener.HandlerFunc(func(msg *listener.Message) {
		if msg.IsOwn {
			return
		}

		if isSentByUs(msg.ThreadID, msg.Text) {
			log.Debug().Str("msg_id", msg.ID).Msg("skipping echo of our own sent message")
			return
		}

		repliedMu.Lock()
		if replied[msg.ID] {
			repliedMu.Unlock()
			return
		}
		replied[msg.ID] = true
		repliedMu.Unlock()

		// Group chat filtering
		if msg.IsGroup {
			mentioned := false
			for _, mid := range msg.MentionIDs {
				if selfIDs[mid] {
					mentioned = true
					break
				}
			}
			repliedToUs := selfIDs[msg.ReplyToUserID]

			if !mentioned && !repliedToUs {
				log.Debug().
					Int64("thread", msg.ThreadID).
					Str("text", msg.Text).
					Msg("skipping group message — bot not mentioned or replied to")
				return
			}
		} else {
			// 1:1 DM — check if sender is followed
			if followingCache.Count() > 0 && !followingCache.IsFollowing(msg.SenderID) {
				log.Info().
					Int64("sender", msg.SenderID).
					Str("text", msg.Text).
					Msg("skipping DM from non-followed account")
				return
			}
		}

		incomingText := msg.Text

		log.Info().
			Str("account", accountName).
			Int64("thread", msg.ThreadID).
			Int64("sender", msg.SenderID).
			Bool("group", msg.IsGroup).
			Str("text", incomingText).
			Msg("agent received message — generating reply")

		if err := cli.SendTypingIndicator(ctx, msg.ThreadID, true); err != nil {
			log.Debug().Err(err).Msg("failed to send typing indicator")
		}

		resolvedSender := cfg.ResolveContactName(msg.SenderID)
		reply, err := llmAgent.GenerateResponse(ctx, msg.ThreadID, resolvedSender, incomingText)
		if err != nil {
			log.Error().Err(err).Int64("thread", msg.ThreadID).Msg("LLM response failed")
			_ = cli.SendTypingIndicator(ctx, msg.ThreadID, false)
			return
		}

		if err := cli.SendMessage(ctx, msg.ThreadID, reply, msg.ID); err != nil {
			log.Error().Err(err).Int64("thread", msg.ThreadID).Msg("failed to send reply")
			_ = cli.SendTypingIndicator(ctx, msg.ThreadID, false)
			return
		}
		addSentMessage(msg.ThreadID, reply)

		_ = cli.SendTypingIndicator(ctx, msg.ThreadID, false)

		log.Info().
			Int64("thread", msg.ThreadID).
			Str("reply", truncateStr(reply, 80)).
			Msg("agent replied")
	}))
	cli.SetEventHandler(lst.HandleEvent)

	fmt.Printf("🤖 Agent mode on @%s (model=%s). Auto-replying to DMs. Press Ctrl+C to stop.\n",
		accountName, cfg.LLM.Model)

	<-ctx.Done()
}

func runAgentAll(ctx context.Context, cfg *config.Config) {
	apiKey := cfg.GetLLMAPIKey()
	if apiKey == "" {
		log.Fatal().Str("env", cfg.LLM.APIKeyEnv).Msg("LLM API key not set")
	}

	// Shared memory manager
	memoryDir := filepath.Join(cfg.DataDir, "memory")
	mem, err := agent.NewMemoryManager(memoryDir)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init memory manager")
	}

	type agentClient struct {
		cli            *client.IGClient
		llmAgent       *agent.LLMAgent
		followingCache *client.FollowingCache
	}

	var clients []agentClient

	for name := range cfg.Accounts {
		cli, err := tryConnectAccount(ctx, cfg, name)
		if err != nil {
			log.Error().Err(err).Str("account", name).Msg("connect failed, skipping")
			continue
		}

		llmAgent := agent.NewLLMAgent(agent.Config{
			BaseURL:           cfg.LLM.BaseURL,
			APIKey:            apiKey,
			Model:             cfg.LLM.Model,
			SystemPrompt:      cfg.GetPersonality(name),
			MaxHistoryMessages: cfg.LLM.MaxHistory,
			ResponseDelayMin:  time.Duration(cfg.LLM.ResponseDelayMinSec) * time.Second,
			ResponseDelayMax:  time.Duration(cfg.LLM.ResponseDelayMaxSec) * time.Second,
		}, mem)

		followingCache := client.NewFollowingCache()

		var repliedMu sync.Mutex
		replied := make(map[string]bool)

		fc := followingCache
		ownUID := cli.OwnUserID()
		allSelfIDs := map[int64]bool{
			ownUID: true,
		}

		ownSenderID := cfg.GetBotSenderID(name)
		if ownSenderID != 0 {
			allSelfIDs[ownSenderID] = true
		}

		lst := listener.NewListener(name, cli.OwnUserID())
		lst.SetFollowingCache(followingCache)

		// Register all bot sender IDs
		for _, senderID := range cfg.BotSenderIDs {
			if senderID != 0 {
				lst.AddBotUserID(senderID)
			}
		}
		if ownSenderID != 0 {
			lst.SetOwnMQTTSenderID(ownSenderID)
		}

		lst.AddHandler(listener.HandlerFunc(func(msg *listener.Message) {
			if msg.IsOwn {
				return
			}

			repliedMu.Lock()
			if replied[msg.ID] {
				repliedMu.Unlock()
				return
			}
			replied[msg.ID] = true
			repliedMu.Unlock()

			// Group chat filtering
			if msg.IsGroup {
				mentioned := false
				for _, mid := range msg.MentionIDs {
					if allSelfIDs[mid] {
						mentioned = true
						break
					}
				}
				repliedToUs := allSelfIDs[msg.ReplyToUserID]

				if !mentioned && !repliedToUs {
					log.Debug().
						Int64("thread", msg.ThreadID).
						Str("text", msg.Text).
						Msg("skipping group message — bot not mentioned or replied to")
					return
				}
			} else {
				if fc.Count() > 0 && !fc.IsFollowing(msg.SenderID) {
					log.Info().
						Str("account", name).
						Int64("sender", msg.SenderID).
						Str("text", msg.Text).
						Msg("skipping DM from non-followed account")
					return
				}
			}

			incomingText := msg.Text

			log.Info().
				Str("account", name).
				Int64("thread", msg.ThreadID).
				Int64("sender", msg.SenderID).
				Bool("group", msg.IsGroup).
				Str("text", incomingText).
				Msg("agent received message — generating reply")

			resolvedSender := cfg.ResolveContactName(msg.SenderID)
			reply, err := llmAgent.GenerateResponse(ctx, msg.ThreadID, resolvedSender, incomingText)
			if err != nil {
				log.Error().Err(err).Int64("thread", msg.ThreadID).Msg("LLM response failed")
				return
			}

			if err := cli.SendMessage(ctx, msg.ThreadID, reply, msg.ID); err != nil {
				log.Error().Err(err).Int64("thread", msg.ThreadID).Msg("failed to send reply")
				return
			}

			log.Info().
				Str("account", name).
				Int64("thread", msg.ThreadID).
				Str("reply", truncateStr(reply, 80)).
				Msg("agent replied")
		}))
		cli.SetEventHandler(lst.HandleEvent)
		clients = append(clients, agentClient{cli: cli, llmAgent: llmAgent, followingCache: followingCache})

		fmt.Printf("🤖 Agent on @%s (model=%s)\n", name, cfg.LLM.Model)
	}

	if len(clients) == 0 {
		log.Fatal().Msg("no accounts connected")
	}

	fmt.Println("All agents running. Press Ctrl+C to stop.")
	<-ctx.Done()

	for _, c := range clients {
		saveSessionState(cfg, c.cli.Name(), c.cli)
		c.cli.Disconnect()
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

func connectAccount(ctx context.Context, cfg *config.Config, accountName string) *client.IGClient {
	cli, err := tryConnectAccount(ctx, cfg, accountName)
	if err != nil {
		log.Fatal().Err(err).Str("account", accountName).Msg("connect failed")
	}
	return cli
}

func tryConnectAccount(ctx context.Context, cfg *config.Config, accountName string) (*client.IGClient, error) {
	acct, ok := getAccount(cfg, accountName)
	if !ok {
		return nil, fmt.Errorf("unknown account: %s", accountName)
	}

	// Try loading existing session first (cookies + state with sync cursors)
	session, _ := cfg.LoadSession(accountName)
	var cli *client.IGClient
	var err error

	if session != nil && session.Cookies != nil {
		log.Info().Str("account", accountName).Msg("loading saved session")
		cli, err = client.NewIGClient(accountName, session)
		if err != nil {
			log.Warn().Err(err).Msg("session load failed, re-authenticating")
			session = nil
		}
	}

	if session == nil {
		log.Info().Str("account", accountName).Str("username", acct.Username).Msg("logging in fresh")
		result, err := login.Login(ctx, acct.Username, acct.Password)
		if err != nil {
			return nil, fmt.Errorf("login @%s: %w", acct.Username, err)
		}
		cli = client.NewClientWithCookies(accountName, result.Cookies)
	}

	if err := cli.Connect(ctx); err != nil {
		// If connection fails with saved session, try fresh login
		if session != nil {
			log.Warn().Err(err).Msg("saved session expired, re-authenticating")
			result, loginErr := login.Login(ctx, acct.Username, acct.Password)
			if loginErr != nil {
				return nil, fmt.Errorf("re-login @%s: %w", acct.Username, loginErr)
			}
			cli = client.NewClientWithCookies(accountName, result.Cookies)
			if err := cli.Connect(ctx); err != nil {
				return nil, fmt.Errorf("connect after re-login: %w", err)
			}
		} else {
			return nil, err
		}
	}

	// Save session for next time (includes state with sync cursors)
	saveSessionState(cfg, accountName, cli)

	return cli, nil
}

// saveSessionState persists the current session (cookies + sync state) to disk.
func saveSessionState(cfg *config.Config, accountName string, cli *client.IGClient) {
	if sess, err := cli.SaveSession(); err == nil {
		_ = cfg.SaveSession(accountName, sess)
	}
}

// MarshalJSON is a helper for debug output.
func MarshalJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
