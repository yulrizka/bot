package bot

import "time"

// Message represents chat message
type Message struct {
	ID   string
	From User
	Date time.Time
	Chat Chat
	Text string
}

// User represents user information
type User struct {
	ID        string
	FirstName string
	LastName  string
	Username  string
}

// ChatType is type of the message
type ChatType string

// Available ChatType
const (
	Private    = "private"
	Group      = "group"
	SuperGroup = "supergroup"
	Channel    = "channel"
)

// Chat represents a chat session
type Chat struct {
	ID       string
	Type     ChatType
	Username string
}

// Plugin is pluggable module to process messages
type Plugin interface {
	Name() string
	Init(out chan Message) (chan *Message, error)
}
