// Package listener processes incoming Instagram DM events from messagix.
package listener

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"go.mau.fi/mautrix-meta/pkg/messagix"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/ocythoe/igdm-go/internal/client"
)

// Message represents an incoming or outgoing Instagram DM.
type Message struct {
	ID            string    // Instagram message ID
	ThreadID      int64     // Conversation thread ID
	SenderID      int64     // Sender's user ID
	SenderName    string    // Sender's display name (from contacts)
	Text          string    // Message text
	Timestamp     time.Time // Message timestamp
	IsOwn         bool      // Whether this was sent by our account
	IsGroup       bool      // Whether this message is in a group chat
	MentionIDs    []int64   // User IDs mentioned in the message (parsed from comma-separated MentionIds)
	ReplyToUserID int64     // User ID of the message being replied to (0 if not a reply)
}

// TypingIndicator represents a typing event.
type TypingIndicator struct {
	ThreadID int64
	SenderID int64
	IsActive bool
}

// Handler is called for various Instagram DM events.
type Handler interface {
	OnMessage(msg *Message)
	OnTyping(typ *TypingIndicator)
	OnThreadUpdate(threadID int64, name string)
}

// HandlerFunc is a convenience adapter for handling messages.
type HandlerFunc func(msg *Message)

func (f HandlerFunc) OnMessage(msg *Message)          { f(msg) }
func (f HandlerFunc) OnTyping(_ *TypingIndicator)      {}
func (f HandlerFunc) OnThreadUpdate(_ int64, _ string) {}

// Listener processes messagix events and dispatches to registered handlers.
type Listener struct {
	accountName      string
	ownUserID        int64
	ownMQTTSenderID  int64                   // bot's own MQTT sender ID (different from FBID ownUserID)
	handlers         []Handler
	contacts         map[int64]string         // userID → name
	groupThreads     map[int64]bool           // threadKey → isGroup
	ignoredThreads   map[int64]bool           // threadKey → ignored
	botUserIDs       map[int64]bool           // known bot account sender IDs to ignore in groups
	followingCache   *client.FollowingCache   // receives IG contact sync data
}

// NewListener creates a new event listener for the given account.
func NewListener(accountName string, ownUserID int64) *Listener {
	return &Listener{
		accountName:    accountName,
		ownUserID:      ownUserID,
		contacts:       make(map[int64]string),
		groupThreads:   make(map[int64]bool),
		ignoredThreads: make(map[int64]bool),
		botUserIDs:     make(map[int64]bool),
	}
}

// IgnoreThread adds a thread to the ignore list.
func (l *Listener) IgnoreThread(threadKey int64) {
	l.ignoredThreads[threadKey] = true
}

// AddBotUserID registers a known bot account sender ID (skip in group chats).
func (l *Listener) AddBotUserID(id int64) {
	l.botUserIDs[id] = true
}

// SetOwnMQTTSenderID sets the bot's own MQTT sender ID, which differs from the
// FBID returned by OwnUserID(). Instagram echoes back the bot's own messages
// with this MQTT sender ID, so it must be recognised as "own" to avoid the
// bot replying to its own echoes.
func (l *Listener) SetOwnMQTTSenderID(id int64) {
	l.ownMQTTSenderID = id
}

// IsGroupThread checks if a thread is a group chat based on synced thread data.
func (l *Listener) IsGroupThread(threadKey int64) bool {
	isGroup, ok := l.groupThreads[threadKey]
	if !ok {
		return threadKey < 0 // fallback for unknown threads
	}
	return isGroup
}

// SetFollowingCache sets the FollowingCache that will receive IG contact sync data.
func (l *Listener) SetFollowingCache(fc *client.FollowingCache) {
	l.followingCache = fc
}

// AddHandler registers an event handler.
func (l *Listener) AddHandler(h Handler) {
	l.handlers = append(l.handlers, h)
}

// OnMessage adds a simple message handler function.
func (l *Listener) OnMessage(fn func(msg *Message)) {
	l.AddHandler(HandlerFunc(fn))
}

// HandleEvent is the main event dispatcher — pass this to client.SetEventHandler.
func (l *Listener) HandleEvent(ctx context.Context, evt any) {
	switch e := evt.(type) {
	case *messagix.Event_Ready:
		log.Info().
			Str("account", l.accountName).
			Bool("new_session", e.IsNewSession).
			Msg("connection ready")

	case *messagix.Event_PublishResponse:
		log.Debug().Str("topic", e.Topic).Msg("publish response")
		if e.Table == nil {
			return
		}
		l.processTable(e.Table)

	case *messagix.Event_SocketError:
		log.Warn().
			Str("account", l.accountName).
			Err(e.Err).
			Int("attempts", e.ConnectionAttempts).
			Msg("socket error")

	case *messagix.Event_PermanentError:
		log.Error().
			Str("account", l.accountName).
			Err(e.Err).
			Msg("permanent error")

	case *messagix.Event_Reconnected:
		log.Info().Str("account", l.accountName).Msg("reconnected")

	default:
		log.Debug().Type("event", evt).Msg("unhandled event")
	}
}

func (l *Listener) processTable(tbl *table.LSTable) {
	// Log what fields are present
	fields := tbl.NonNilFields()
	if len(fields) > 0 {
		log.Debug().Strs("fields", fields).Msg("processing LSTable")
	}

	// IMPORTANT: Process thread types BEFORE messages so group detection works.
	// Thread verification comes with message events and carries ThreadType.
	for _, thread := range tbl.LSVerifyThreadExists {
		if thread != nil {
			l.groupThreads[thread.ThreadKey] = thread.ThreadType == table.GROUP_THREAD
		}
	}
	for _, thread := range tbl.LSDeleteThenInsertThread {
		if thread != nil {
			l.groupThreads[thread.ThreadKey] = thread.ThreadType == table.GROUP_THREAD
		}
	}
	for _, thread := range tbl.LSUpdateOrInsertThread {
		if thread != nil {
			l.groupThreads[thread.ThreadKey] = thread.ThreadType == table.GROUP_THREAD
		}
	}

	// Process IG contact info — this contains following status data
	if len(tbl.LSDeleteThenInsertIGContactInfo) > 0 && l.followingCache != nil {
		l.followingCache.ProcessContactInfo(tbl.LSDeleteThenInsertIGContactInfo)
	}

	// Process contacts first so we have names
	for _, contact := range tbl.LSVerifyContactRowExists {
		if contact != nil {
			name := contact.Name
			if name == "" {
				name = contact.SecondaryName
			}
			l.contacts[contact.ContactId] = name
		}
	}

	// Upserted messages (new or updated)
	for _, msg := range tbl.LSUpsertMessage {
		if msg == nil || msg.Text == "" {
			continue
		}
		l.dispatchMessage(msg.Text, msg.ThreadKey, msg.SenderId, msg.TimestampMs, msg.MessageId,
			msg.MentionOffsets, msg.MentionLengths, msg.MentionIds, msg.ReplyToUserId)
	}

	// Inserted messages
	for _, msg := range tbl.LSInsertMessage {
		if msg == nil || msg.Text == "" {
			continue
		}
		l.dispatchMessage(msg.Text, msg.ThreadKey, msg.SenderId, msg.TimestampMs, msg.MessageId,
			msg.MentionOffsets, msg.MentionLengths, msg.MentionIds, msg.ReplyToUserId)
	}

	// New message range notifications — these indicate new messages arrived
	// in a thread. Log them even if individual message data isn't included.
	for _, rng := range tbl.LSInsertNewMessageRange {
		if rng != nil {
			log.Debug().
				Int64("thread", rng.ThreadKey).
				Int64("min", rng.MinTimestampMs).
				Int64("max", rng.MaxTimestampMs).
				Msg("new message range")
		}
	}

	// Typing indicators
	for _, typ := range tbl.LSUpdateTypingIndicator {
		if typ != nil {
			for _, h := range l.handlers {
				h.OnTyping(&TypingIndicator{
					ThreadID: typ.ThreadKey,
					SenderID: typ.SenderId,
					IsActive: typ.IsTyping,
				})
			}
		}
	}

	// Thread updates — notify handlers
	for _, thread := range tbl.LSDeleteThenInsertThread {
		if thread != nil {
			for _, h := range l.handlers {
				h.OnThreadUpdate(thread.ThreadKey, thread.ThreadName)
			}
		}
	}
	for _, thread := range tbl.LSUpdateOrInsertThread {
		if thread != nil {
			for _, h := range l.handlers {
				h.OnThreadUpdate(thread.ThreadKey, thread.ThreadName)
			}
		}
	}
}

func (l *Listener) dispatchMessage(text string, threadKey, senderID, timestampMs int64, messageID string,
	mentionOffsets, mentionLengths, mentionIDsStr string, replyToUserID int64) {
	// Skip ignored threads
	if l.ignoredThreads[threadKey] {
		log.Debug().
			Int64("thread", threadKey).
			Str("msg_id", messageID).
			Msg("skipping message in ignored thread")
		return
	}

	isOwn := senderID == l.ownUserID || senderID == l.ownMQTTSenderID
	senderName := l.contactName(senderID)
	isGroup := l.IsGroupThread(threadKey)

	// Skip messages from known bot accounts in group chats (prevent bot-to-bot loops)
	if isGroup && l.botUserIDs[senderID] {
		log.Debug().
			Int64("thread", threadKey).
			Int64("sender", senderID).
			Str("msg_id", messageID).
			Msg("skipping message from known bot in group chat")
		return
	}
	parsedMentionIDs := parseMentionIDs(mentionIDsStr)
	if mentionIDsStr != "" {
		log.Debug().
			Str("raw_mention_ids", mentionIDsStr).
			Int64("ownUserID", l.ownUserID).
			Msg("mention data")
	}

	ts := time.UnixMilli(timestampMs)

	receivedLog := log.Info().
		Str("account", l.accountName).
		Str("msg_id", messageID).
		Int64("thread", threadKey).
		Int64("sender", senderID).
		Str("sender_name", senderName).
		Bool("own", isOwn).
		Bool("group", isGroup).
		Int("mentions", len(parsedMentionIDs)).
		Int64("reply_to_user", replyToUserID).
		Str("text", truncate(text, 80))
	if len(parsedMentionIDs) > 0 {
		ids := make([]string, len(parsedMentionIDs))
		for i, id := range parsedMentionIDs {
			ids[i] = fmt.Sprintf("%d", id)
		}
		receivedLog.Strs("mention_ids", ids)
	}
	receivedLog.Msg("message received")

	msg := &Message{
		ID:            messageID,
		ThreadID:      threadKey,
		SenderID:      senderID,
		SenderName:    senderName,
		Text:          text,
		Timestamp:     ts,
		IsOwn:         isOwn,
		IsGroup:       isGroup,
		MentionIDs:    parsedMentionIDs,
		ReplyToUserID: replyToUserID,
	}

	for _, h := range l.handlers {
		h.OnMessage(msg)
	}
}

// parseMentionIDs parses a comma-separated string of user IDs into a []int64 slice.
// Returns nil if the string is empty.
func parseMentionIDs(s string) []int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			log.Debug().Str("part", p).Msg("skipping invalid mention ID")
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

func (l *Listener) contactName(userID int64) string {
	if name, ok := l.contacts[userID]; ok {
		return name
	}
	return strconv.FormatInt(userID, 10)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// FormatMessage returns a human-readable string for a message.
func FormatMessage(accountName string, msg *Message, contacts map[int64]string) string {
	sender := "unknown"
	if msg.IsOwn {
		sender = "you"
	} else if name, ok := contacts[msg.SenderID]; ok && name != "" {
		sender = name
	}

	return fmt.Sprintf("[%s] [%s] thread=%d %s: %s",
		accountName,
		msg.Timestamp.Format("15:04:05"),
		msg.ThreadID,
		sender,
		strings.TrimSpace(msg.Text),
	)
}
