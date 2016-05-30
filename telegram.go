package bot

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

var poolDuration = 1 * time.Second

/**
 * Telegram API specific data structure
 */

// TResponse represents response from telegram
type TResponse struct {
	Ok     bool      `json:"ok"`
	Result []TUpdate `json:"result"`
}

// TUpdate represents an update event from telegram
type TUpdate struct {
	UpdateID int64    `json:"update_id"`
	Message  TMessage `json:"message"`
}

// TMessage is Telegram message
type TMessage struct {
	MessageID int64  `json:"message_id"`
	From      TUser  `json:"from"`
	Date      int64  `json:"date"`
	Chat      TChat  `json:"chat"`
	Text      string `json:"text"`
}

// TUser is Telegram User
type TUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

// TChat represents Telegram chat session
type TChat struct {
	Type  string `json:"type"`
	Title string `json:"title"`
	TUser
}

// TChatTypeMap maps betwwen string to bot.ChatType
var TChatTypeMap = map[string]ChatType{
	"private":    Private,
	"group":      Group,
	"SuperGroup": SuperGroup,
	"Channel":    Channel,
}

// Telegram API
type Telegram struct {
	url        string
	input      map[Plugin]chan *Message
	output     chan Message
	quit       chan struct{}
	lastUpdate int64
}

// NewTelegram creates telegram API Client
func NewTelegram(key string) *Telegram {
	if key == "" {
		log.Fatalf("telegram API key must not be empty")
	}
	return &Telegram{
		url:    fmt.Sprintf("https://api.telegram.org/bot%s", key),
		input:  make(map[Plugin]chan *Message),
		output: make(chan Message),
		quit:   make(chan struct{}),
	}
}

// Start consuming from telegram
func (t *Telegram) Start() {
	t.pool()
}

//AddPlugin add processing module to telegram
func (t *Telegram) AddPlugin(p Plugin) error {
	input, err := p.Init(t.output)
	if err != nil {
		return err
	}
	t.input[p] = input

	return nil
}

func (t *Telegram) pool() {
	timer := time.NewTicker(poolDuration)
	for {
		select {
		case <-timer.C:
			resp, err := http.Get(fmt.Sprintf("%s/getUpdates?offset=%d", t.url, t.lastUpdate+1))
			if err != nil {
				log.Printf("ERROR getUpdates, %s", err)
				continue
			}
			if err := t.parse(resp); err != nil {
				log.Printf("ERROR parsing response, %s", err)
			}
		case <-t.quit:
			return
		}
	}
}

func (t *Telegram) parse(resp *http.Response) error {
	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)
	var tresp TResponse
	if err := decoder.Decode(&tresp); err != nil {
		return err
	}

	if !tresp.Ok {
		return nil
	}

	for _, update := range tresp.Result {
		m := update.Message
		t.lastUpdate = update.UpdateID

		msg := Message{
			ID: strconv.FormatInt(m.MessageID, 10),
			From: User{
				ID:        strconv.FormatInt(m.From.ID, 10),
				FirstName: m.From.FirstName,
				LastName:  m.From.LastName,
				Username:  m.From.Username,
			},
			Date: time.Unix(m.Date, 0),
			Chat: Chat{
				ID:       strconv.FormatInt(m.Chat.ID, 10),
				Type:     TChatTypeMap[m.Chat.Type],
				Username: m.Chat.Username,
			},
			Text: m.Text,
		}
		for plugin, ch := range t.input {
			select {
			case ch <- &msg:
			default:
				log.Printf("WARN %s input channel is full, skiping message %s", plugin.Name(), msg.ID)
			}
		}
	}

	return nil
}
