// Package client wraps messagix to provide a high-level Instagram DM client.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"go.mau.fi/mautrix-meta/pkg/messagix"
	"go.mau.fi/mautrix-meta/pkg/messagix/cookies"
	"go.mau.fi/mautrix-meta/pkg/messagix/methods"
	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"
	"go.mau.fi/mautrix-meta/pkg/messagix/types"

	"github.com/ocythoe/igdm-go/internal/config"
)

// IGClient is a high-level Instagram DM client wrapping messagix.
type IGClient struct {
	mu          sync.Mutex
	client      *messagix.Client
	cookies     *cookies.Cookies
	ownUserID   int64
	name        string
	connected   bool
	ready       chan struct{}
	userHandler func(ctx context.Context, evt any)
	savedState  json.RawMessage // persisted state to load after LoadMessagesPage
}

// ThreadInfo represents basic info about a DM thread.
type ThreadInfo struct {
	Key          int64
	Name         string
	Participants []int64
	LastMessage  string
	IsGroup      bool
}

// NewIGClient creates a client with existing session data (cookies + state).
func NewIGClient(name string, session *config.SessionData) (*IGClient, error) {
	cks := &cookies.Cookies{Platform: types.Instagram}
	if session != nil && session.Cookies != nil {
		if err := json.Unmarshal(session.Cookies, cks); err != nil {
			return nil, fmt.Errorf("unmarshal cookies: %w", err)
		}
		// Ensure platform is set (MarshalJSON only saves values map)
		cks.Platform = types.Instagram
	}

	logger := zerolog.New(zerolog.NewConsoleWriter()).
		With().Timestamp().
		Str("account", name).
		Logger()

	cli := messagix.NewClient(cks, logger, &messagix.Config{
		MayConnectToDGW: true,
	})

	// Store state for loading AFTER LoadMessagesPage (loading before causes nil panic)
	var savedState json.RawMessage
	if session != nil {
		savedState = session.State
	}

	return &IGClient{
		client:     cli,
		cookies:    cks,
		name:       name,
		ready:      make(chan struct{}),
		savedState: savedState,
	}, nil
}

// NewClientWithCookies creates a client from pre-obtained cookies.
func NewClientWithCookies(name string, cks *cookies.Cookies) *IGClient {
	logger := zerolog.New(zerolog.NewConsoleWriter()).
		With().Timestamp().
		Str("account", name).
		Logger()

	cli := messagix.NewClient(cks, logger, &messagix.Config{
		MayConnectToDGW: true,
	})

	return &IGClient{
		client:  cli,
		cookies: cks,
		name:    name,
		ready:   make(chan struct{}),
	}
}

// setupCompositeHandler installs the single event handler on the messagix client.
// This is called once during Connect and never replaced.
// It handles: ready tracking, cursor updates (PostHandlePublishResponse), and user handler forwarding.
func (c *IGClient) setupCompositeHandler() {
	c.client.SetEventHandler(func(ctx context.Context, evt any) {
		switch e := evt.(type) {
		case *messagix.Event_Ready:
			log.Info().Str("account", c.name).Msg("Event_Ready — initial sync complete")
			select {
			case <-c.ready:
			default:
				close(c.ready)
			}

		case *messagix.Event_Reconnected:
			log.Info().Str("account", c.name).Msg("Event_Reconnected — reconnect sync complete")
			// Also close ready channel on reconnect so callers don't time out
			select {
			case <-c.ready:
			default:
				close(c.ready)
			}

		case *messagix.Event_PublishResponse:
			if e.Table != nil {
				fields := e.Table.NonNilFields()
				if len(fields) > 0 {
					log.Debug().Strs("fields", fields).Str("topic", e.Topic).Msg("Event_PublishResponse")
				}
				// CRITICAL: Update sync group cursors after each publish response.
				// Without this, Instagram's server stops pushing new messages because
				// the client never acknowledges advancing its cursor position.
				// This is exactly what the Beeper bridge does in handleTableLoop.
				c.client.PostHandlePublishResponse(e.Table)
			} else {
				log.Debug().Str("topic", e.Topic).Msg("Event_PublishResponse (nil table)")
			}

		case *messagix.Event_SocketError:
			log.Warn().Err(e.Err).Int("attempts", e.ConnectionAttempts).Msg("Event_SocketError")

		case *messagix.Event_PermanentError:
			log.Error().Err(e.Err).Msg("Event_PermanentError")
		}

		// Forward ALL events to the user handler (listener, etc.)
		if c.userHandler != nil {
			c.userHandler(ctx, evt)
		}
	})
}

// Connect loads the messages page, optionally restores state, establishes the MQTT WebSocket,
// and waits for the initial LightSpeed sync to complete.
func (c *IGClient) Connect(ctx context.Context) error {
	c.mu.Lock()

	log.Info().Str("account", c.name).Msg("loading messages page")
	userInfo, _, err := c.client.LoadMessagesPage(ctx)
	if err != nil {
		c.mu.Unlock()
		return fmt.Errorf("load messages page: %w", err)
	}

	// Store our own user ID
	if userInfo != nil {
		c.ownUserID = userInfo.GetFBID()
		log.Info().
			Str("account", c.name).
			Int64("user_id", c.ownUserID).
			Str("username", userInfo.GetUsername()).
			Msg("authenticated")
	}

	// State loading is disabled for now. Loading state sets previouslyConnected=true
	// which makes the MQTT handshake take the "reconnect" path — this skips:
	//   - Subscribing to /ls_resp (the live message topic!)
	//   - Sending ReportAppState(FOREGROUND)
	//   - The full initial sync
	// Without these, Instagram won't push new messages.
	// The full handshake (fresh connect) works correctly — the library's handleReadyEvent
	// handles all subscriptions and FOREGROUND reporting.
	// TODO: Re-enable state loading only after implementing persistent subscription
	// restoration in the CONNECT packet's SubscribedTopics field.
	// if len(c.savedState) > 0 {
	// 	log.Info().Str("account", c.name).Msg("restoring sync state (cursors)")
	// 	if err := c.client.LoadState(c.savedState); err != nil {
	// 		log.Warn().Err(err).Msg("failed to load state, proceeding with fresh sync")
	// 	}
	// }

	// Set up the single composite handler (ready tracking + cursor updates + user forwarding)
	c.setupCompositeHandler()

	log.Info().Str("account", c.name).Msg("connecting to Instagram MQTT")
	if err := c.client.Connect(ctx); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("connect: %w", err)
	}

	c.connected = true
	c.mu.Unlock()

	// Wait for Event_Ready (initial sync complete) with a timeout.
	// Do NOT hold the mutex while waiting — the event handler runs in another goroutine.
	select {
	case <-c.ready:
		log.Info().Str("account", c.name).Msg("ready — connected and synced")
	case <-time.After(30 * time.Second):
		log.Warn().Str("account", c.name).Msg("timed out waiting for ready, proceeding anyway")
	}
	return nil
}

// Disconnect saves state and closes the connection.
func (c *IGClient) Disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connected {
		// Save state before disconnecting (like Beeper does)
		if state, err := c.client.DumpState(); err == nil && state != nil {
			c.savedState = state
		}
		c.client.Disconnect()
		c.connected = false
		log.Info().Str("account", c.name).Msg("disconnected")
	}
}

// IsConnected returns whether the client is currently connected.
func (c *IGClient) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// SetEventHandler registers a callback for messagix events.
// This just sets the userHandler field — the composite handler (set once in Connect)
// forwards all events to it. It does NOT replace the messagix-level handler,
// so PostHandlePublishResponse and ready tracking are never lost.
func (c *IGClient) SetEventHandler(handler func(ctx context.Context, evt any)) {
	c.mu.Lock()
	c.userHandler = handler
	c.mu.Unlock()
}

// SendMessage sends a text message to a thread.
func (c *IGClient) SendMessage(ctx context.Context, threadID int64, text string, replyToMessageID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return fmt.Errorf("not connected")
	}

	task := &socket.SendMessageTask{
		ThreadId:  threadID,
		Otid:      methods.GenerateEpochID(),
		Source:    table.MESSENGER_INBOX,
		SendType:  table.TEXT,
		Text:      text,
		SyncGroup: 1,
	}

	// If a reply-to message ID is provided, set reply metadata
	if replyToMessageID != "" {
		task.ReplyMetaData = &socket.ReplyMetaData{
			ReplyMessageId:  replyToMessageID,
			ReplySourceType: 1,
			ReplyType:       1,
		}
	}

	resp, err := c.client.ExecuteTasks(ctx, task)
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}

	log.Debug().
		Str("account", c.name).
		Int64("thread", threadID).
		Str("text", truncate(text, 50)).
		Any("response", resp).
		Msg("message sent")
	return nil
}

// SendTypingIndicator sends a typing indicator to the specified thread.
// Set isTyping=true to show "typing...", false to stop.
func (c *IGClient) SendTypingIndicator(ctx context.Context, threadID int64, isTyping bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return fmt.Errorf("not connected")
	}

	isTypingInt := int64(0)
	if isTyping {
		isTypingInt = 1
	}

	task := &socket.UpdatePresenceTask{
		ThreadKey:     threadID,
		IsGroupThread: 0, // 1:1 chat
		IsTyping:      isTypingInt,
		Attribution:   0,
		SyncGroup:     1,
		ThreadType:    1, // table.ONE_TO_ONE
	}

	// Typing indicators must use ExecuteStatelessTask (not ExecuteTasks).
	// ExecuteTasks is for stateful tasks like SendMessage; typing is stateless.
	if err := c.client.ExecuteStatelessTask(ctx, task); err != nil {
		return fmt.Errorf("send typing indicator: %w", err)
	}

	log.Debug().
		Str("account", c.name).
		Int64("thread", threadID).
		Bool("typing", isTyping).
		Msg("typing indicator sent")
	return nil
}

// SearchUser searches for an Instagram user by username and returns their user ID.
func (c *IGClient) SearchUser(ctx context.Context, username string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return 0, fmt.Errorf("not connected")
	}

	task := &socket.SearchUserTask{
		Query: username,
		SupportedTypes: []table.SearchType{
			table.SearchTypeContact, table.SearchTypeNonContact,
			table.SearchTypeIGContactFollowing, table.SearchTypeIGContactNonFollowing,
			table.SearchTypeIGNonContactFollowing, table.SearchTypeIGNonContactNonFollowing,
		},
		SurfaceType: 15,
	}

	resp, err := c.client.ExecuteTasks(ctx, task)
	if err != nil {
		return 0, fmt.Errorf("search user: %w", err)
	}

	for _, result := range resp.LSInsertSearchResult {
		if result.ThreadType == table.ONE_TO_ONE && result.GetFBID() != 0 {
			// Check if the display name matches
			if result.DisplayName == username {
				return result.GetFBID(), nil
			}
		}
	}

	// Fallback: return first match if exact match not found
	for _, result := range resp.LSInsertSearchResult {
		if result.ThreadType == table.ONE_TO_ONE && result.GetFBID() != 0 {
			return result.GetFBID(), nil
		}
	}

	return 0, fmt.Errorf("user @%s not found", username)
}

// GetThreadIDForUser computes the 1:1 thread key for a user.
// For Instagram DMs, the thread key is simply the OTHER user's FBID/ContactId.
// This was confirmed by examining initial sync data: every 1:1 thread's key
// matches the non-self participant's ContactId exactly.
// The previous XOR computation was incorrect and produced wrong thread keys.
func (c *IGClient) GetThreadIDForUser(userID int64) int64 {
	return userID
}

// SendToUser sends a DM to a user by their Instagram username.
// It searches for the user, computes the thread ID, and sends the message.
func (c *IGClient) SendToUser(ctx context.Context, username, text string) error {
	userID, err := c.SearchUser(ctx, username)
	if err != nil {
		return err
	}

	threadID := c.GetThreadIDForUser(userID)
	log.Info().
		Str("account", c.name).
		Str("username", username).
		Int64("user_id", userID).
		Int64("thread_id", threadID).
		Msg("resolved user to thread")

	return c.SendMessage(ctx, threadID, text, "")
}

// DebugDumpInitialSync returns the raw LSTable from LoadMessagesPage plus all user ID info.
func (c *IGClient) DebugDumpInitialSync(ctx context.Context) (userInfo types.UserInfo, lsTable *table.LSTable, ownUserID int64, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	userInfo, lsTable, err = c.client.LoadMessagesPage(ctx)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("load messages page: %w", err)
	}

	if userInfo != nil {
		ownUserID = userInfo.GetFBID()
	}

	return userInfo, lsTable, ownUserID, nil
}

// DebugSearchUserFull returns all search results for debugging.
func (c *IGClient) DebugSearchUserFull(ctx context.Context, username string) ([]*table.LSInsertSearchResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return nil, fmt.Errorf("not connected")
	}

	task := &socket.SearchUserTask{
		Query: username,
		SupportedTypes: []table.SearchType{
			table.SearchTypeContact, table.SearchTypeNonContact,
			table.SearchTypeIGContactFollowing, table.SearchTypeIGContactNonFollowing,
			table.SearchTypeIGNonContactFollowing, table.SearchTypeIGNonContactNonFollowing,
		},
		SurfaceType: 15,
	}

	resp, err := c.client.ExecuteTasks(ctx, task)
	if err != nil {
		return nil, fmt.Errorf("search user: %w", err)
	}

	return resp.LSInsertSearchResult, nil
}

// DebugFetchProfile returns the Instagram profile ID.
func (c *IGClient) DebugFetchProfile(ctx context.Context, username string) (int64, string, error) {
	return c.FetchProfile(ctx, username)
}

// GetUnderlyingClient exposes the internal messagix client for debug purposes.
func (c *IGClient) GetUnderlyingClient() *messagix.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client
}

// FetchProfile fetches an Instagram user's profile info (ID, name, etc).
func (c *IGClient) FetchProfile(ctx context.Context, username string) (int64, string, error) {
	c.mu.Lock()
	ig := c.client.Instagram
	c.mu.Unlock()

	if ig == nil {
		return 0, "", fmt.Errorf("not an Instagram client")
	}

	profile, err := ig.FetchProfile(ctx, username)
	if err != nil {
		return 0, "", fmt.Errorf("fetch profile: %w", err)
	}

	if profile == nil || profile.Data.User.ID == "" {
		return 0, "", fmt.Errorf("user @%s not found", username)
	}

	userID, err := strconv.ParseInt(profile.Data.User.ID, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("parse user ID: %w", err)
	}

	return userID, profile.Data.User.FullName, nil
}

// GetThreads returns a list of recent threads from the initial sync data.
func (c *IGClient) GetThreads(ctx context.Context) ([]ThreadInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Fetch threads via MQTT task
	_, tbl, err := c.client.FetchMoreThreads(ctx, 1)
	if err != nil {
		return nil, fmt.Errorf("fetch threads: %w", err)
	}

	var threads []ThreadInfo
	if tbl != nil {
		for _, t := range tbl.LSDeleteThenInsertThread {
			if t != nil {
				threads = append(threads, ThreadInfo{
					Key:     t.ThreadKey,
					Name:    t.ThreadName,
					IsGroup: t.ThreadType == table.GROUP_THREAD,
				})
			}
		}
		for _, t := range tbl.LSUpdateOrInsertThread {
			if t != nil {
				threads = append(threads, ThreadInfo{
					Key:     t.ThreadKey,
					Name:    t.ThreadName,
					IsGroup: t.ThreadType == table.GROUP_THREAD,
				})
			}
		}
	}

	return threads, nil
}

// GetCurrentAccount returns the current logged-in user info.
func (c *IGClient) GetCurrentAccount(ctx context.Context) (types.UserInfo, error) {
	return c.client.GetCurrentAccount()
}

// OwnUserID returns the logged-in user's numeric ID.
func (c *IGClient) OwnUserID() int64 {
	return c.ownUserID
}

// Name returns the account name/label.
func (c *IGClient) Name() string {
	return c.name
}

// SaveSession dumps the current cookies and state for persistence.
func (c *IGClient) SaveSession() (*config.SessionData, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cookieBytes, err := json.Marshal(c.client.GetCookies())
	if err != nil {
		return nil, fmt.Errorf("marshal cookies: %w", err)
	}

	// Try to dump state — this only works if connected and previously synced
	state, err := c.client.DumpState()
	if err != nil {
		// State dump can fail if not connected yet — use saved state if available
		log.Warn().Err(err).Msg("failed to dump state from client")
		state = c.savedState
	}

	return &config.SessionData{
		Cookies: cookieBytes,
		State:   state,
	}, nil
}

// WaitForReady blocks until the connection is ready or timeout.
func (c *IGClient) WaitForReady(ctx context.Context, timeout time.Duration) error {
	return c.client.WaitUntilCanSendMessages(ctx, timeout)
}

// ThreadMessage represents a single message in a thread's history.
type ThreadMessage struct {
	MessageID string
	ThreadKey int64
	SenderID  int64
	Text      string
	Timestamp time.Time
}

// GetThreadHistory fetches recent messages from a thread using FetchMessagesTask.
// limit controls the max number of messages returned (0 = use library default).
func (c *IGClient) GetThreadHistory(ctx context.Context, threadKey int64, limit int) ([]ThreadMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return nil, fmt.Errorf("not connected")
	}

	task := &socket.FetchMessagesTask{
		ThreadKey:            threadKey,
		Direction:            0, // fetch latest
		ReferenceTimestampMs: time.Now().UnixMilli(),
		SyncGroup:            1,
	}

	resp, err := c.client.ExecuteTasks(ctx, task)
	if err != nil {
		return nil, fmt.Errorf("fetch messages: %w", err)
	}

	var messages []ThreadMessage

	// Collect from both upsert and insert message arrays
	for _, m := range resp.LSUpsertMessage {
		if m != nil && m.Text != "" {
			messages = append(messages, ThreadMessage{
				MessageID: m.MessageId,
				ThreadKey: m.ThreadKey,
				SenderID:  m.SenderId,
				Text:      m.Text,
				Timestamp: time.UnixMilli(m.TimestampMs),
			})
		}
	}
	for _, m := range resp.LSInsertMessage {
		if m != nil && m.Text != "" {
			messages = append(messages, ThreadMessage{
				MessageID: m.MessageId,
				ThreadKey: m.ThreadKey,
				SenderID:  m.SenderId,
				Text:      m.Text,
				Timestamp: time.UnixMilli(m.TimestampMs),
			})
		}
	}

	// Apply limit
	if limit > 0 && len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}

	return messages, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
