package bot

import (
	"bytes"
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
	Ok          bool            `json:"ok"`
	Result      json.RawMessage `json:"result,omitempty"`
	ErrorCode   int64           `json:"error_code,omitempty"`
	Description string          `json:"description"`
}

// TUpdate represents an update event from telegram
type TUpdate struct {
	UpdateID int64    `json:"update_id"`
	Message  TMessage `json:"message"`
}

// TMessage is Telegram incomming message
type TMessage struct {
	MessageID int64  `json:"message_id"`
	From      TUser  `json:"from"`
	Date      int64  `json:"date"`
	Chat      TChat  `json:"chat"`
	Text      string `json:"text"`
}

// TOutMessage is Telegram outgoing message
type TOutMessage struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty"`
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

//AddPlugin add processing module to telegram
func (t *Telegram) AddPlugin(p Plugin) error {
	input, err := p.Init(t.output)
	if err != nil {
		return err
	}
	t.input[p] = input

	return nil
}

// Start consuming from telegram
func (t *Telegram) Start() {
	go t.poolOutbox()
	t.poolInbox()
}

func (t *Telegram) poolOutbox() {
	for {
		select {
		case m := <-t.output:
			outMsg := TOutMessage{
				ChatID: m.Chat.ID,
				Text:   m.Text,
			}

			var b bytes.Buffer
			if err := json.NewEncoder(&b).Encode(outMsg); err != nil {
				log.Printf("ERROR, sendMessages encoding, %s", err)
				continue
			}
			resp, err := http.Post(fmt.Sprintf("%s/sendMessage", t.url), "application/json; charset=utf-9", &b)
			if err != nil {
				log.Printf("ERROR, sendMessages id:%s, %s", outMsg.ChatID, err)
				continue
			}
			if err := t.parseOutbox(resp, outMsg.ChatID); err != nil {
				log.Printf("ERROR parsing sendMessage response, %s", err)
			}
		case <-t.quit:
			return
		}
	}
}

func (t *Telegram) poolInbox() {
	timer := time.NewTicker(poolDuration)
	for {
		select {
		case <-timer.C:
			resp, err := http.Get(fmt.Sprintf("%s/getUpdates?offset=%d", t.url, t.lastUpdate+1))
			if err != nil {
				log.Printf("ERROR getUpdates, %s", err)
				continue
			}
			if err := t.parseInbox(resp); err != nil {
				log.Printf("ERROR parsing updates response, %s", err)
			}
		case <-t.quit:
			return
		}
	}
}

func (t *Telegram) parseInbox(resp *http.Response) error {
	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)
	var tresp TResponse
	if err := decoder.Decode(&tresp); err != nil {
		return err
	}

	if !tresp.Ok {
		log.Printf("ERROR parseInbox code:%d, %s", tresp.ErrorCode, tresp.Description)
		return nil
	}

	var results []TUpdate
	json.Unmarshal(tresp.Result, &results)
	for _, update := range results {
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

func (t *Telegram) parseOutbox(resp *http.Response, chatID string) error {
	defer resp.Body.Close()

	var tresp TResponse
	if err := json.NewDecoder(resp.Body).Decode(&tresp); err != nil {
		return fmt.Errorf("decoding response failed id:%s, %s", chatID, err)
	}
	if !tresp.Ok {
		return fmt.Errorf("code:%d description:%s", tresp.ErrorCode, tresp.Description)
	}

	return nil
}
