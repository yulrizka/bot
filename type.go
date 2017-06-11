package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Client represent a chat client. Currently supports telegram
type Client interface {
	AddPlugins(...Plugin) error
	Start() error
	Stop()

	Username() string // bot username
}

// Message represents chat message
type Message struct {
	ctx          context.Context
	ID           string
	From         User
	Date         time.Time
	Chat         Chat
	Text         string
	Format       MessageFormat
	ReplyTo      *Message
	ReplyToID    string
	ReceivedAt   time.Time
	Raw          json.RawMessage `json:"-"`
	Retry        int             `json:"-"`
	DiscardAfter time.Time       `json:"-"`
}

func (m *Message) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

func (m *Message) WithContext(ctx context.Context) *Message {
	if ctx == nil {
		panic("nil context")
	}
	m2 := new(Message)
	m2 = m
	m2.ctx = ctx
	return m2
}

// JoinMessage represents information that a user join a chat
type JoinMessage struct {
	*Message
}

// LeftMessage represents information that a user left a chat
type LeftMessage struct {
	*Message
}

// ChannelMigratedMessage represents that a chat type has been upgraded. Currently works on telegram
type ChannelMigratedMessage struct {
	FromID     string
	ToID       string
	ReceivedAt time.Time
	Raw        json.RawMessage `json:"-"`
}

// MessageFormat represents formatting of the message
type MessageFormat string

// Available MessageFormat
const (
	Text     MessageFormat = ""
	Markdown MessageFormat = "markdown"
	HTML     MessageFormat = "html"
)

// User represents user information
type User struct {
	ID        string
	FirstName string
	LastName  string
	Username  string
}

// FullName returns first name + last name
func (u User) FullName() string {
	if u.LastName != "" {
		return fmt.Sprintf("%s %s", u.FirstName, u.LastName)
	}
	return u.FirstName
}

// ChatType is type of the message
type ChatType string

// Available ChatType
const (
	Channel    ChatType = "channel"
	Group      ChatType = "group"
	Private    ChatType = "private"
	SuperGroup ChatType = "supergroup"
	Thread     ChatType = "thread"
)

// Chat represents a chat session
type Chat struct {
	ID       string
	Type     ChatType
	Title    string
	Username string
}

// Name returns the title of the chat
func (t Chat) Name() string {
	var buf bytes.Buffer
	buf.WriteString(t.Title)
	if t.Username != "" {
		buf.WriteString(" (@")
		buf.WriteString(t.Username)
		buf.WriteString(")")
	}

	return buf.String()
}

// Plugin is pluggable module to process messages
type Plugin interface {
	Name() string
	Init(out chan Message) error
	Handle(in interface{}) (handled bool, msg interface{})
}
