package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/uber-go/zap"
)

const slackURL = "https://slack.com/api"

type Slack struct {
	ctx    context.Context
	token  string
	input  map[Plugin]chan interface{}
	output chan Message

	url             string
	teamID          string
	teamName        string
	domain          string
	enterprise_id   string
	enterprise_name string
	id              string
	name            string

	members map[string]SlackUser
	ims     map[string]im
}

type SlackUser struct {
	ID                 string
	TeamID             string
	Name               string
	Deleted            bool
	Color              string
	RealName           string
	RealNameNormalized string
	TZ                 string
	TZLabel            string
	TZOffset           int64
	Profile            struct {
		FirstName      string
		LastName       string
		AvatarHash     string
		Image24        string
		Image32        string
		Image48        string
		Image72        string
		Image192       string
		Image512       string
		Image1024      string
		ImageOriginal  string
		Title          string
		Phone          string
		GuestChannels  string
		GuestInvitedBy string
		Email          string
	}
	IsAdmin           bool
	IsOwner           bool
	IsPrimaryOwner    bool
	IsRestricted      bool
	IsUltraRestricted bool
	IsBot             bool
	Updated           int64
	EnterpriseUser    struct {
		ID             string
		EnterpriseID   string
		EnterpriseName string
		IsAdmin        bool
		IsOwner        bool
		Teams          []string
	}
}

type im struct {
	ID            string
	IsIM          bool
	User          string
	Created       int64
	IsUserDeleted bool
}

type slackResponse struct {
	Ok      bool
	Error   string
	Warning string
}

func NewSlack(ctx context.Context, token string) (*Slack, error) {
	data := url.Values{}
	data.Set("token", token)

	resp, err := http.Post(slackURL+"/rtm.connect", "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("faile to create connect request: %s", err)
	}
	defer resp.Body.Close()

	var sResp struct {
		slackResponse
		URL  string
		Team struct {
			ID             string
			Name           string
			Domain         string
			EnterpriseID   string
			EnterpriseName string
		}
		Self struct {
			ID   string
			Name string
		}
	}

	if err := json.NewDecoder(resp.Body).Decode(&sResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %s", err)

	}
	if !sResp.Ok {
		return nil, fmt.Errorf("failed to connect error:%s warning:%s", sResp.Error, sResp.Warning)
	}

	members, err := userList(token)
	if err != nil {
		return nil, fmt.Errorf("failed to get list of user: %s", err)
	}

	ims, err := imList(token)
	if err != nil {
		return nil, fmt.Errorf("failed to get list of user: %s", err)
	}
	return &Slack{
		ctx:    ctx,
		token:  token,
		input:  make(map[Plugin]chan interface{}),
		output: make(chan Message, OutboxBufferSize),

		url:             sResp.URL,
		teamID:          sResp.Team.ID,
		teamName:        sResp.Team.Name,
		domain:          sResp.Team.Domain,
		enterprise_id:   sResp.Team.EnterpriseID,
		enterprise_name: sResp.Team.EnterpriseName,
		id:              sResp.Self.ID,
		name:            sResp.Self.Name,

		members: members,
		ims:     ims,
	}, nil
}

func (s *Slack) AddPlugin(p Plugin) error {
	input, err := p.Init(s.output)
	if err != nil {
		return err
	}
	s.input[p] = input

	return nil
}

func (s *Slack) Start() error {
	conn, _, err := websocket.DefaultDialer.Dial(s.url, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to websocket: %s", err)
	}

	// handle incoming message
	go func() {
		for {
			select {
			case <-s.ctx.Done():
				return
			default:
				_, raw, err := conn.ReadMessage()
				if err != nil {
					log.Error("failed to receive data: ", zap.Error(err))
					continue
				}
				msg, err := s.parseIncomingMessage(raw)
				if err != nil {
					log.Error("failed to parse message: ", zap.Error(err))
					continue
				}
				if msg == nil {
					continue
				}
				for plugin, ch := range s.input {
					select {
					case ch <- msg:
					default:
						log.Warn("input channel full, skipping message", zap.String("plugin", plugin.Name()))
					}
				}
			}
		}
	}()

	// handle outgoing message
	go func() {
		var counter int64
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case msg := <-s.output:
				counter++
				outMsg := struct {
					ID       int64  `json:"id"`
					Type     string `json:"type"`
					Channel  string `json:"channel"`
					Text     string `json:"text"`
					ThreadTs string `json:"thread_ts,omitempty"`
				}{counter, "message", msg.Chat.ID, msg.Text, ""}

				switch msg.Chat.Type {
				case Private:
					outMsg.Channel, err = s.imID(msg.Chat.ID)
					if err != nil {
						log.Error("failed to get IM: ", zap.Error(err), zap.Object("msg", outMsg))
						continue
					}
				case Thread:
					outMsg.ThreadTs = msg.ReplyTo.ID
				}

				if err := conn.WriteJSON(&outMsg); err != nil {
					log.Error("failed to send message: ", zap.Error(err), zap.Object("msg", outMsg))
					continue
				}
			case <-t.C:
				counter++
				ping := struct {
					ID   int64  `json:"id"`
					Type string `json:"type"`
				}{counter, "ping"}
				if err := conn.WriteJSON(ping); err != nil {
					log.Warn("failed to send ping: ", zap.Error(err))
				}
			}
		}

	}()
	<-s.ctx.Done()
	return nil
}

func (s *Slack) parseIncomingMessage(msg []byte) (*Message, error) {
	var raw struct {
		Type    string
		Channel string
		User    string
		Text    string
		Ts      string
		//SourceTeam string
		//Team string
	}
	if err := json.Unmarshal(msg, &raw); err != nil {
		return nil, fmt.Errorf("failed parsing message type: %s", err)
	}

	var ts time.Time
	if raw.Ts != "" {
		timestamp, err := strconv.ParseFloat(raw.Ts, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse timestamp: %s", err)
		}
		ts = time.Unix(int64(timestamp), 0)
	}

	slackUser := s.members[raw.User]
	user := User{
		ID:        raw.User,
		FirstName: slackUser.Profile.FirstName,
		LastName:  slackUser.Profile.LastName,
		Username:  slackUser.Name,
	}

	switch raw.Type {
	case "message":
		msg := Message{
			ID: raw.Ts,
			Chat: Chat{
				ID:   raw.Channel,
				Type: Group,
			},
			From:   user,
			Date:   ts,
			Text:   raw.Text,
			Format: Text,
		}
		return &msg, nil
	}
	return nil, nil
}

func (s *Slack) UserName() string {
	return s.name
}

func (s *Slack) imID(userID string) (string, error) {
	dm, ok := s.ims[userID]
	if !ok {
		data := url.Values{}
		data.Set("token", s.token)
		data.Set("user", userID)
		resp, err := http.Post(slackURL+"/im.open", "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
		if err != nil {
			return "", fmt.Errorf("failed to create connect request: %s", err)
		}
		defer resp.Body.Close()

		var sResp struct {
			slackResponse
			Channel struct {
				ID      string
				IsIM    bool
				User    string
				Created int64
			}
		}
		if err := json.NewDecoder(resp.Body).Decode(&sResp); err != nil {
			return "", fmt.Errorf("failed to parse response: %s", err)

		}
		if !sResp.Ok {
			return "", fmt.Errorf("failed to open IM for user %s error:%s warning:%s", userID, sResp.Error, sResp.Warning)
		}
		dm = im{
			ID:      sResp.Channel.ID,
			IsIM:    sResp.Channel.IsIM,
			User:    sResp.Channel.User,
			Created: sResp.Channel.Created,
		}
		s.ims[userID] = dm
	}
	return dm.ID, nil
}

func userList(token string) (map[string]SlackUser, error) {
	data := url.Values{}
	data.Set("token", token)
	data.Set("presence", "true")

	resp, err := http.Post(slackURL+"/users.list", "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("faile to create connect request: %s", err)
	}
	defer resp.Body.Close()

	var sResp struct {
		slackResponse
		Members []SlackUser
	}

	if err := json.NewDecoder(resp.Body).Decode(&sResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %s", err)

	}
	if !sResp.Ok {
		return nil, fmt.Errorf("userList failed error:%s warning:%s", sResp.Error, sResp.Warning)
	}

	members := make(map[string]SlackUser)
	for _, member := range sResp.Members {
		members[member.ID] = member
	}
	return members, nil
}

func imList(token string) (map[string]im, error) {
	data := url.Values{}
	data.Set("token", token)

	resp, err := http.Post(slackURL+"/im.list", "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("faile to create connect request: %s", err)
	}
	defer resp.Body.Close()

	var sResp struct {
		slackResponse
		Ims []im
	}

	if err := json.NewDecoder(resp.Body).Decode(&sResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %s", err)

	}
	if !sResp.Ok {
		return nil, fmt.Errorf("failed to connect error:%s warning:%s", sResp.Error, sResp.Warning)
	}

	ims := make(map[string]im)
	for _, im := range sResp.Ims {
		ims[im.User] = im
	}
	return ims, nil
}
