package bot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/rcrowley/go-metrics"
	"github.com/uber-go/zap"
)

var (
	OutboxBufferSize = 100
	poolDuration     = 1 * time.Second
	log              zap.Logger
	maxMsgPerUpdates = 100
	outboxWorker     = 2

	// stats
	updateCount         = metrics.NewRegisteredCounter("telegram.updates.count", metrics.DefaultRegistry)
	msgPerUpdateRate    = metrics.NewRegisteredCounter("telegram.messagePerUpdate", metrics.DefaultRegistry)
	updateDuration      = metrics.NewRegisteredTimer("telegram.updates.duration", metrics.DefaultRegistry)
	sendMessageDuration = metrics.NewRegisteredTimer("telegram.sendMessage.duration", metrics.DefaultRegistry)
)

func init() {
	log = zap.NewJSON(zap.AddCaller(), zap.AddStacks(zap.FatalLevel))
}

func SetLogger(l zap.Logger) {
	log = l.With(zap.String("module", "bot"))
}

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
	UpdateID int64           `json:"update_id"`
	Message  json.RawMessage `json:"message"`
}

// TMessage is Telegram incomming message
type TMessage struct {
	MessageID       int64     `json:"message_id"`
	From            TUser     `json:"from"`
	Date            int64     `json:"date"`
	Chat            TChat     `json:"chat"`
	Text            string    `json:"text"`
	ParseMode       string    `json:"parse_mode,omitempty"`
	MigrateToChatID *int64    `json:"migrate_to_chat_id,omitempty"`
	ReceivedAt      time.Time `json:"-"`
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
	"supergroup": SuperGroup,
	"channel":    Channel,
}

// Telegram API
type Telegram struct {
	url        string
	input      map[Plugin]chan interface{}
	output     chan Message
	quit       chan struct{}
	lastUpdate int64
}

// NewTelegram creates telegram API Client
func NewTelegram(key string) *Telegram {
	if key == "" {
		log.Fatal("telegram API key must not be empty")
	}
	return &Telegram{
		url:    fmt.Sprintf("https://api.telegram.org/bot%s", key),
		input:  make(map[Plugin]chan interface{}),
		output: make(chan Message, OutboxBufferSize),
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
	t.poolOutbox()
	t.poolInbox()
}

func (t *Telegram) poolOutbox() {
	for i := 0; i < outboxWorker; i++ {
		go func() {

		NEXTMESSAGE:
			for {
				select {
				case m := <-t.output:
					if !m.DiscardAfter.IsZero() && time.Now().After(m.DiscardAfter) {
						log.Warn("discarded message", zap.Object("msg", m))
						continue
					}

					outMsg := TOutMessage{
						ChatID:    m.Chat.ID,
						Text:      m.Text,
						ParseMode: string(m.Format),
					}

					var b bytes.Buffer
					if err := json.NewEncoder(&b).Encode(outMsg); err != nil {
						log.Error("encoding message", zap.Error(err))
						continue
					}
					started := time.Now()
					jsonMsg := b.String()

					var resp *http.Response
					var err error
					for m.Retry >= 0 {
						if !m.DiscardAfter.IsZero() && time.Now().After(m.DiscardAfter) {
							log.Warn("discarded message", zap.Object("msg", m))
							continue NEXTMESSAGE
						}
						resp, err = http.Post(fmt.Sprintf("%s/sendMessage", t.url), "application/json; charset=utf-10", &b)
						if err != nil {

							// check for timeout
							if netError, ok := err.(net.Error); ok && netError.Timeout() {
								m.Retry--
								log.Error("sendMessage timeout", zap.String("ChatID", outMsg.ChatID), zap.Error(err), zap.Int("retry", m.Retry))
								continue
							}

							log.Error("sendMessage failed", zap.String("ChatID", outMsg.ChatID), zap.Error(err))
							continue NEXTMESSAGE
						}
						metrics.GetOrRegisterCounter(fmt.Sprintf("telegram.sendMessage.http.%d", resp.StatusCode), metrics.DefaultRegistry).Inc(1)

						if resp.StatusCode == 429 { // rate limited by telegram
							m.Retry--
							if r, err := parseResponse(resp); err != nil {
								log.Error("sendMessage 429", zap.Error(err))
								var delay int
								if n, err := fmt.Sscanf(r.Description, "Too Many Requests: retry after %d", &delay); err != nil && n == 1 {
									if delay > 0 {
										d := time.Duration(delay) * time.Second
										log.Info("sendMessage delayed", zap.String("delay", d.String()))
										time.Sleep(d)
									}
								}
							}

							continue
						}

						break
					}

					sendMessageDuration.UpdateSince(started)
					if _, err := parseResponse(resp); err != nil {
						log.Error("parsing sendMessage response failed", zap.String("ChatID", outMsg.ChatID), zap.Error(err), zap.Object("msg", jsonMsg))
					}
				case <-t.quit:
					return
				}
			}
		}()
	}
}

func (t *Telegram) poolInbox() {
	for {
		select {
		case <-t.quit:
			return
		default:
			started := time.Now()
			resp, err := http.Get(fmt.Sprintf("%s/getUpdates?offset=%d", t.url, t.lastUpdate+1))
			if err != nil {
				log.Error("getUpdates failed", zap.Error(err))
				updateDuration.UpdateSince(started)
				continue
			}
			updateDuration.UpdateSince(started)
			updateCount.Inc(1)
			metrics.GetOrRegisterCounter(fmt.Sprintf("telegram.getUpdates.http.%d", resp.StatusCode), metrics.DefaultRegistry).Inc(1)

			nMsg, err := t.parseInbox(resp)
			if err != nil {
				log.Error("parsing updates response failed", zap.Error(err))
			}
			msgPerUpdateRate.Inc(int64(nMsg))
			if nMsg != maxMsgPerUpdates {
				time.Sleep(poolDuration)
			}
		}
	}
}

func (t *Telegram) parseInbox(resp *http.Response) (int, error) {
	defer resp.Body.Close()

	receivedAt := time.Now()
	decoder := json.NewDecoder(resp.Body)
	var tresp TResponse
	if err := decoder.Decode(&tresp); err != nil {
		return 0, err
	}

	if !tresp.Ok {
		log.Error("parsing response failed", zap.Int64("errorCode", tresp.ErrorCode), zap.String("description", tresp.Description))
		return 0, nil
	}

	var results []TUpdate
	json.Unmarshal(tresp.Result, &results)
	for _, update := range results {
		var m TMessage
		json.Unmarshal(update.Message, &m)
		t.lastUpdate = update.UpdateID

		var msg interface{}
		message := Message{
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
				Title:    m.Chat.Title,
				Username: m.Chat.Username,
			},
			Text:       m.Text,
			ReceivedAt: receivedAt,
			Raw:        update.Message,
		}
		if m.MigrateToChatID != nil {
			newChanID := strconv.FormatInt(*(m.MigrateToChatID), 10)
			chanMigratedMsg := ChannelMigratedMessage{
				Message:    message,
				FromID:     message.Chat.ID,
				ToID:       newChanID,
				ReceivedAt: receivedAt,
			}
			msg = &chanMigratedMsg
		}
		msg = &message
		log.Debug("update", zap.Object("msg", msg))
		for plugin, ch := range t.input {
			select {
			case ch <- msg:
			default:
				log.Warn("input channel full, skipping message", zap.String("plugin", plugin.Name()), zap.String("msgID", message.ID))
			}
		}
	}

	return len(results), nil
}

func (t *Telegram) Leave(chanID string) error {
	url := fmt.Sprintf("%s/leaveChat?chat_id=%s", t.url, url.QueryEscape(chanID))
	fmt.Printf("url = %+v\n", url)
	resp, err := http.Get(url)
	if err != nil {
		log.Error("leave failed", zap.Error(err))
		return err
	}
	defer resp.Body.Close()

	if _, err := parseResponse(resp); err != nil {
		log.Error("leave invalid response", zap.Error(err))
	}

	return nil
}

func parseResponse(resp *http.Response) (TResponse, error) {
	defer resp.Body.Close()

	var tresp TResponse
	if err := json.NewDecoder(resp.Body).Decode(&tresp); err != nil {
		return tresp, fmt.Errorf("decoding response failed %s", err)
	}
	if !tresp.Ok {
		return tresp, fmt.Errorf("code:%d description:%s", tresp.ErrorCode, tresp.Description)
	}

	return tresp, nil
}
