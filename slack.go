package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"bytes"
	"github.com/gorilla/websocket"
	"github.com/uber-go/zap"
	"mime/multipart"
	"net/http/httputil"
)

const slackURL = "https://slack.com/api"

var (
	ignoreMessageType = map[string]bool{
		"channel_joined":  true,
		"channel_created": true,
		"channel_rename":  true,
		"im_created":      true,
		"team_join":       true,
		"user_change":     true,
	}
)

type Slack struct {
	ctx    context.Context
	quit   context.CancelFunc
	token  string
	input  map[Plugin]chan interface{}
	output chan Message

	handler func(interface{}) (handled bool, msg interface{})
	plugins []Plugin

	url             string
	teamID          string
	teamName        string
	domain          string
	enterprise_id   string
	enterprise_name string
	id              string
	name            string

	idToMember       map[string]slackUser
	userNameToMember map[string]slackUser
	ims              map[string]slackIm
	channels         map[string]slackChannel
	nameToChannels   map[string]slackChannel
}

type slackUser struct {
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

type slackIm struct {
	ID            string
	IsIM          bool
	User          string
	Created       int64
	IsUserDeleted bool
}

type slackChannel struct {
	Id         string
	Name       string
	Created    int64
	Creator    string
	IsArchived bool
	IsMember   bool
	NumMembers int64
	Topic      struct {
		Value   string
		Creator string
		LastSet int64
	}
	Purpose struct {
		Value   string
		Creator string
		LastSet int64
	}
}

type slackResponse struct {
	Ok      bool
	Error   string
	Warning string
}

type slackError struct {
	Message    string
	ErrorMsg   string
	WarningMsg string
}

func (ew slackError) Error() string {
	return fmt.Sprintf("%s error:%q warning:%q", ew.Message, ew.ErrorMsg, ew.WarningMsg)
}

func NewSlack(ctx context.Context, token string) (*Slack, error) {
	ctx, quit := context.WithCancel(ctx)
	return &Slack{
		ctx:    ctx,
		quit:   quit,
		token:  token,
		input:  make(map[Plugin]chan interface{}),
		output: make(chan Message, OutboxBufferSize),
		handler: func(inMsg interface{}) (handled bool, msg interface{}) {
			return true, inMsg
		},
	}, nil
}

func (s *Slack) AddPlugins(plugins ...Plugin) error {
	if len(plugins) == 0 {
		return nil
	}
	for i := len(plugins) - 1; i >= 0; i-- {
		p := plugins[i]
		s.plugins = append(s.plugins, p)

		// add middle ware
		next := s.handler
		s.handler = func(inMsg interface{}) (handled bool, msg interface{}) {
			ok, msg := p.Handle(inMsg)
			if ok {
				return true, msg
			}
			return next(msg)
		}
	}

	return nil
}

func (s *Slack) Start() error {
	if err := s.init(); err != nil {
		return fmt.Errorf("failed to initialize connection: %s", err)
	}
	for _, p := range s.plugins {
		if err := p.Init(s.output, s); err != nil {
			return fmt.Errorf("failed to initialize plugin %q: %s", p.Name(), err)
		}
	}
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
					log.Error("failed to parse message: ", zap.Error(err), zap.String("raw", string(raw)))
					continue
				}
				if msg == nil {
					continue
				}
				// ignore message from our self
				if msg.From.ID == s.id || msg.From.Username == s.name {
					continue
				}
				s.handler(msg)
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
					Mrkdwn   bool   `json:"mrkdwn"`
				}{counter, "message", msg.Chat.ID, msg.Text, "", msg.Format == Markdown}

				switch msg.Chat.Type {
				case Private:
					if strings.HasPrefix(msg.Chat.ID, "D") {
						// possibly already a valid chat id
						break
					}
					var idOrUsername = msg.Chat.ID
					if idOrUsername == "" {
						idOrUsername = msg.Chat.Username
					}
					channel, err := s.imID(idOrUsername)
					if err != nil {
						log.Error("failed to get IM: ", zap.Error(err), zap.Object("msg", outMsg))
						continue
					}
					msg.Chat.ID = channel
					outMsg.Channel = channel
				case Thread:
					outMsg.ThreadTs = msg.ReplyTo.ID
				}

				// if message has attachment, we must use the web API
				if len(msg.Attachments) > 0 {
					if err := s.chatPostMessage(msg); err != nil {
						log.Error("failed to send message: ", zap.Error(err), zap.Object("msg", outMsg))
					}
					continue
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

func (s *Slack) init() error {
	log.Info("Initializing Slack")
	data := url.Values{}
	data.Set("token", s.token)

	resp, err := http.Post(slackURL+"/rtm.connect", "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("faile to create connect request: %s", err)
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
		return fmt.Errorf("failed to parse response: %s", err)

	}
	if !sResp.Ok {
		return fmt.Errorf("failed to connect error:%s warning:%s", sResp.Error, sResp.Warning)
	}
	s.url = sResp.URL
	s.teamID = sResp.Team.ID
	s.teamName = sResp.Team.Name
	s.domain = sResp.Team.Domain
	s.enterprise_id = sResp.Team.EnterpriseID
	s.enterprise_name = sResp.Team.EnterpriseName
	s.id = sResp.Self.ID
	s.name = sResp.Self.Name

	errCh := make(chan error)
	go func() {
		var err error
		defer func() {
			errCh <- err
		}()

		var members map[string]slackUser
		members, err = userList(s.token)
		if err != nil {
			err = fmt.Errorf("failed to get list of user: %s", err)
			return
		}
		s.idToMember = members

		s.userNameToMember = make(map[string]slackUser)
		for _, user := range members {
			s.userNameToMember[user.Name] = user
		}
	}()

	go func() {
		var err error
		defer func() {
			errCh <- err
		}()

		var ims map[string]slackIm
		ims, err = imList(s.token)
		if err != nil {
			err = fmt.Errorf("failed to get list of user: %s", err)
			return
		}
		s.ims = ims
	}()

	errs := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		err := <-errCh
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	close(errCh)
	if len(errs) > 0 {
		return fmt.Errorf("init failuers: %s", strings.Join(errs, ";"))
	}

	log.Info("Initialize completed", zap.String("botname", s.name), zap.String("botID", s.id))

	// since it's not used and quite some big response, skip it for now
	enableFetchChannels := true
	go func() {
		if !enableFetchChannels {
			return
		}

		var channels map[string]slackChannel
		channels, err = channelsList(s.token)
		if err != nil {
			log.Error("failed to get list of channels", zap.Error(err))
			return
		}
		s.channels = channels
		s.nameToChannels = make(map[string]slackChannel)
		for _, ch := range channels {
			s.nameToChannels[ch.Name] = ch
		}
	}()

	return nil
}

func userList(token string) (map[string]slackUser, error) {
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
		Members []slackUser
	}
	if err := json.NewDecoder(resp.Body).Decode(&sResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %s", err)

	}
	if !sResp.Ok {
		return nil, fmt.Errorf("userList failed error:%s warning:%s", sResp.Error, sResp.Warning)
	}

	members := make(map[string]slackUser)
	for _, member := range sResp.Members {
		members[member.ID] = member
	}
	return members, nil
}

func imList(token string) (map[string]slackIm, error) {
	data := url.Values{}
	data.Set("token", token)

	resp, err := http.Post(slackURL+"/im.list", "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("faile to create connect request: %s", err)
	}
	defer resp.Body.Close()

	var sResp struct {
		slackResponse
		Ims []slackIm
	}

	if err := json.NewDecoder(resp.Body).Decode(&sResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %s", err)

	}
	if !sResp.Ok {
		return nil, fmt.Errorf("failed to connect error:%s warning:%s", sResp.Error, sResp.Warning)
	}

	ims := make(map[string]slackIm)
	for _, im := range sResp.Ims {
		ims[im.User] = im
	}
	return ims, nil
}

func channelsList(token string) (map[string]slackChannel, error) {
	data := url.Values{}
	data.Set("token", token)

	resp, err := http.Post(slackURL+"/channels.list", "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("faile to create channels.list request: %s", err)
	}
	defer resp.Body.Close()

	var sResp struct {
		slackResponse
		Channels []slackChannel
	}

	if err := json.NewDecoder(resp.Body).Decode(&sResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %s", err)

	}
	if !sResp.Ok {
		return nil, fmt.Errorf("channels.list failed error:%s warning:%s", sResp.Error, sResp.Warning)
	}

	channels := make(map[string]slackChannel)
	for _, ch := range sResp.Channels {
		channels[ch.Id] = ch
	}
	return channels, nil
}

func (s *Slack) chatPostMessage(msg Message) error {
	data := url.Values{}
	data.Set("token", s.token)
	data.Set("channel", msg.Chat.ID)
	data.Set("text", msg.Text)
	data.Set("as_user", "true")
	if len(msg.Attachments) > 0 {
		attachments, err := json.Marshal(msg.Attachments)
		if err != nil {
			return fmt.Errorf("marshall attachments failed: %s", attachments)
		}
		data.Set("attachments", string(attachments))
	}
	if msg.Chat.Type == Thread {
		data.Set("thread_ts", msg.ReplyTo.ID)
	}

	resp, err := http.Post(slackURL+"/chat.postMessage", "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("chat.PostMessage request failed: %s", err)
	}
	defer resp.Body.Close()

	var sResp struct {
		slackResponse
	}
	if err := json.NewDecoder(resp.Body).Decode(&sResp); err != nil {
		return fmt.Errorf("failed to parse response: %s", err)
	}
	if !sResp.Ok {
		return fmt.Errorf("channels.list failed error:%s warning:%s", sResp.Error, sResp.Warning)
	}

	return nil
}

func (s *Slack) parseIncomingMessage(rawMsg []byte) (*Message, error) {
	log.Debug("incoming", zap.String("rawMsg", string(rawMsg)))

	var rawType struct {
		Type string
	}
	if err := json.Unmarshal(rawMsg, &rawType); err != nil {
		return nil, fmt.Errorf("failed parsing message type: %s", err)
	}

	if ignoreMessageType[rawType.Type] {
		// this type of message has specific structure, ignore for now
		return nil, nil
	}

	var raw struct {
		Type        string
		Channel     string
		User        string
		Username    string
		BotID       string
		Text        string
		Ts          string
		Attachments []Attachment
		SubType     string
		//SourceTeam string
		//Team string
	}
	if err := json.Unmarshal(rawMsg, &raw); err != nil {
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

	slackUser := s.idToMember[raw.User]
	user := User{
		ID:        raw.User,
		FirstName: slackUser.Profile.FirstName,
		LastName:  slackUser.Profile.LastName,
		Username:  slackUser.Name,
	}

	msg := Message{}
	chatType := Group
	if strings.HasPrefix(raw.Channel, "D") {
		chatType = Private
	}
	switch raw.Type {
	case "message":
		msg = Message{
			ID: raw.Ts,
			Chat: Chat{
				ID:   raw.Channel,
				Type: chatType,
			},
			From:        user,
			Date:        ts,
			Text:        raw.Text,
			Format:      Text,
			Attachments: raw.Attachments,
		}
		if raw.SubType == "bot_message" {
			msg.From.Username = raw.Username
			msg.From.ID = raw.BotID
		}
	}

	return &msg, nil
}

func (s *Slack) Stop() {
	s.quit()
}

func (s *Slack) UserName() string {
	return s.name
}

func (s *Slack) imID(userIDorName string) (string, error) {
	if userIDorName == "" {
		return "", errors.New("empty username")
	}
	var userID = userIDorName
	if strings.HasPrefix(userIDorName, "@") {
		member, ok := s.userNameToMember[userID[1:]]
		if !ok {
			return "", fmt.Errorf("failed to get user id for %q", userID)
		}
		userID = member.ID
	}

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
		dm = slackIm{
			ID:      sResp.Channel.ID,
			IsIM:    sResp.Channel.IsIM,
			User:    sResp.Channel.User,
			Created: sResp.Channel.Created,
		}
		s.ims[userID] = dm
	}
	return dm.ID, nil
}

func (s *Slack) Mentioned(field string) bool {
	if !strings.HasPrefix(field, "<@") || !strings.HasSuffix(field, ">") {
		return false
	}

	return field[2:len(field)-1] == s.id
}

func (s *Slack) Mention(u User) string {
	return "<@" + u.ID + ">"
}

func (s *Slack) FindUser(username string) (User, bool) {
	if strings.HasPrefix(username, "@") {
		username = username[1:]
	}
	var u User
	slackUser, ok := s.userNameToMember[username]
	if !ok {
		return u, false
	}

	return User{
		ID:        slackUser.ID,
		FirstName: slackUser.Profile.FirstName,
		LastName:  slackUser.Profile.LastName,
		Username:  slackUser.Name,
	}, true
}

func (s *Slack) EmulateReceiveMessage(raw []byte) error {
	msg, err := s.parseIncomingMessage(raw)
	if err != nil {
		return fmt.Errorf("failed to parse message: %s", err)
	}
	if msg == nil {
		return nil
	}
	s.handler(msg)
	return nil
}

func (s *Slack) SetTopic(chatID string, topic string) error {
	err := s.channelSetTopic(chatID, topic)
	if err != nil {
		if slackErr, ok := err.(slackError); ok {
			// it might be private group
			if slackErr.ErrorMsg == "channel_not_found" {
				err = s.groupSetTopic(chatID, topic)
			}
		}
	}
	return err
}

func (s *Slack) channelSetTopic(chatID string, topic string) error {
	data := url.Values{}
	data.Set("token", s.token)
	data.Set("channel", chatID)
	data.Set("topic", topic)
	resp, err := http.Post(slackURL+"/channels.setTopic", "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("chat.setTopic request failed: %s", err)
	}
	defer resp.Body.Close()

	var sResp struct {
		slackResponse
	}
	if err := json.NewDecoder(resp.Body).Decode(&sResp); err != nil {
		return fmt.Errorf("failed to parse response: %s", err)
	}
	if !sResp.Ok {
		return slackError{
			Message:    "channels.setTopic failed",
			ErrorMsg:   sResp.Error,
			WarningMsg: sResp.Warning,
		}
	}

	return nil
}

func (s *Slack) groupSetTopic(chatID string, topic string) error {
	data := url.Values{}
	data.Set("token", s.token)
	data.Set("channel", chatID)
	data.Set("topic", topic)
	resp, err := http.Post(slackURL+"/groups.setTopic", "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("groups.setTopic request failed: %s", err)
	}
	defer resp.Body.Close()

	var sResp struct {
		slackResponse
	}
	if err := json.NewDecoder(resp.Body).Decode(&sResp); err != nil {
		return fmt.Errorf("failed to parse response: %s", err)
	}
	if !sResp.Ok {
		return slackError{
			Message:    "groups.setTopic failed",
			ErrorMsg:   sResp.Error,
			WarningMsg: sResp.Warning,
		}

	}

	return nil
}

func (s *Slack) ChatInfo(chatID string) (ChatInfo, error) {
	ci, err := s.channelInfo(chatID)
	if err != nil {
		if slackErr, ok := err.(slackError); ok {
			// it might be private group
			if slackErr.ErrorMsg == "channel_not_found" {
				ci, err = s.groupInfo(chatID)
			}
		}
	}
	return ci, err
}

func (s *Slack) channelInfo(chatID string) (c ChatInfo, err error) {
	data := url.Values{}
	data.Set("token", s.token)
	data.Set("channel", chatID)
	resp, err := http.Post(slackURL+"/channels.info", "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
	if err != nil {
		return c, fmt.Errorf("channel.info request failed: %s", err)
	}
	defer func() {
		io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}()

	var sResp struct {
		slackResponse
		slackChannel `json:"channel"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sResp); err != nil {
		return c, fmt.Errorf("failed to parse response: %s", err)
	}
	if !sResp.Ok {
		return c, slackError{
			Message:    "channels.info failed",
			ErrorMsg:   sResp.Error,
			WarningMsg: sResp.Warning,
		}
	}

	c.ID = sResp.slackChannel.Id
	c.Title = sResp.slackChannel.Name
	c.Topic = sResp.slackChannel.Topic.Value
	c.Description = sResp.slackChannel.Purpose.Value

	return c, nil

}

func (s *Slack) groupInfo(chatID string) (c ChatInfo, err error) {
	data := url.Values{}
	data.Set("token", s.token)
	data.Set("channel", chatID)
	resp, err := http.Post(slackURL+"/groups.info", "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
	if err != nil {
		return c, fmt.Errorf("chat.info request failed: %s", err)
	}

	defer func() {
		io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}()

	var sResp struct {
		slackResponse
		slackChannel `json:"group"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sResp); err != nil {
		return c, fmt.Errorf("failed to parse response: %s", err)
	}
	if !sResp.Ok {
		return c, slackError{
			Message:    "groups.info failed",
			ErrorMsg:   sResp.Error,
			WarningMsg: sResp.Warning,
		}
	}

	c.ID = sResp.slackChannel.Id
	c.Title = sResp.slackChannel.Name
	c.Topic = sResp.slackChannel.Topic.Value
	c.Description = sResp.slackChannel.Purpose.Value

	return c, nil
}

func (s *Slack) UploadFile(chatID string, filename string, r io.Reader) error {

	// write multipart filed
	var b bytes.Buffer
	var fw io.Writer
	var err error
	w := multipart.NewWriter(&b)


	fw, err = w.CreateFormFile("file", filename)
	if err != nil {
		return fmt.Errorf("Failed to create multipart filed: %v", err)
	}
	if _, err := io.Copy(fw, r); err != nil {
		return fmt.Errorf("Failed to read source file: %v", err)
	}

	// token
	if fw, err = w.CreateFormField("token"); err != nil {
		return fmt.Errorf("failed to add form field token", err)
	}
	if _, err = fw.Write([]byte(s.token)); err != nil {
		return fmt.Errorf("failed to add form field token value", err)
	}

	// channels
	if fw, err = w.CreateFormField("channels"); err != nil {
		return fmt.Errorf("failed to add form field channels", err)
	}
	if _, err = fw.Write([]byte(chatID)); err != nil {
		return fmt.Errorf("failed to add form field channels value", err)
	}

	// content
	//if fw, err = w.CreateFormField("content"); err != nil {
	//	return fmt.Errorf("failed to add form field channels", err)
	//}
	//if _, err = fw.Write([]byte("HELLO ahmy")); err != nil {
	//	return fmt.Errorf("failed to add form field channels value", err)
	//}
	w.Close()

	url := slackURL + "/files.upload"
	//url := "https://hookb.in/ZB7g11VG"
	req, err := http.NewRequest("POST", url, &b)
	req.Header.Set("Content-Type", w.FormDataContentType())


	body, _ := httputil.DumpRequest(req, true)
	fmt.Printf("body = %+v\n", string(body)) // for debugging

	// curl -F file=@dramacat.gif -F channels=C024BE91L,#general -F token=xxxx-xxxxxxxxx-xxxx https://slack.com/api/files.upload
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("chat.info request failed: %s", err)
	}

	body, _ = httputil.DumpResponse(resp, true)
	fmt.Printf("body = %+v\n", string(body)) // for debugging
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("Got response status: %d", resp.StatusCode)
	}

	return nil
}
