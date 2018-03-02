package bot

import (
	"context"
	"reflect"
	"testing"
	"time"
)

type mockPlugin struct {
	NameFn   func() string
	InitFn   func(out chan Message) error
	HandleFn func(interface{}) (handled bool, msg interface{})
}

func (m mockPlugin) Name() string {
	if m.NameFn == nil {
		panic("not implemented")
	}
	return m.NameFn()
}
func (m mockPlugin) Init(out chan Message, cli Client) error {
	if m.InitFn == nil {
		panic("not implemented")
	}
	return m.InitFn(out)
}
func (m mockPlugin) Handle(inMsg interface{}) (handled bool, msg interface{}) {
	if m.HandleFn == nil {
		panic("not implemented")
	}
	return m.HandleFn(inMsg)
}

func TestSlack_AddPlugin(t *testing.T) {
	t.Run("full chain", func(t *testing.T) {

		ctx := context.Background()
		s, err := NewSlack(ctx, "")
		if err != nil {
			t.Fatal(err)
		}

		p1 := mockPlugin{
			NameFn: func() string { return "p1" },
			InitFn: func(out chan Message) error { return nil },
			HandleFn: func(m interface{}) (handled bool, msg interface{}) {
				inMsg := m.(*Message)
				inMsg.Text += "1"
				return false, inMsg.WithContext(context.WithValue(ctx, "val", "1"))
			},
		}

		p2 := mockPlugin{
			NameFn: func() string { return "p2" },
			InitFn: func(out chan Message) error { return nil },
			HandleFn: func(m interface{}) (handled bool, msg interface{}) {
				inMsg := m.(*Message)
				inMsg.Text += "2"
				val := inMsg.Context().Value("val")
				return false, inMsg.WithContext(context.WithValue(ctx, "val", val.(string)+"2"))
			},
		}

		s.AddPlugins(p1, p2)
		s.initPlugin()
		var m Message
		handled, msgRaw := s.handler(&m)
		msg := msgRaw.(*Message)
		if got, want := handled, true; got != want {
			t.Errorf("got handled %t want %t", got, want)
		}
		if msg == nil {
			t.Fatal("nil message")
		}
		if got, want := msg.Text, "12"; got != want {
			t.Errorf("got handled %s want %s", got, want)
		}
		got, want := msg.ctx.Value("val"), "12"
		if got != want {
			t.Errorf("got context value %s want %s", got, want)
		}

	})

	t.Run("short circuit", func(t *testing.T) {
		s, err := NewSlack(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}

		p1 := mockPlugin{
			NameFn: func() string { return "p1" },
			InitFn: func(out chan Message) error { return nil },
			HandleFn: func(in interface{}) (handled bool, msg interface{}) {
				m := in.(*Message)
				m.Text += "1"
				return true, m
			},
		}
		p2 := mockPlugin{
			NameFn: func() string { return "p1" },
			InitFn: func(out chan Message) error { return nil },
			HandleFn: func(in interface{}) (handled bool, msg interface{}) {
				m := in.(*Message)
				m.Text += "2"
				return true, m
			},
		}

		s.AddPlugins(p1, p2)
		s.initPlugin()
		var m Message
		handled, rawMsg := s.handler(&m)
		if got, want := handled, true; got != want {
			t.Errorf("got handled %t want %t", got, want)
		}
		msg := rawMsg.(*Message)
		if msg == nil {
			t.Fatal("nil message")
		}
		if got, want := msg.Text, "1"; got != want {
			t.Errorf("got handled %q want %q", got, want)
		}
	})

}

func TestParseIncomingMessage(t *testing.T) {
	golden := []struct {
		name    string
		raw     string
		want    *Message
		wantErr bool
	}{
		{
			name:    "with attachement",
			raw:     `{"text":"","username":"gerrit","bot_id":"B06P5RMNH","attachments":[{"fallback":"Patch Set 1:\n\n(1 comment)","text":"Patch Set 1:\n\n(1 comment)","pretext":"[icas] New comment on change <https://gerrit.ecg.so/58629|58629>","title":"Comment by Daniela Remenska (<mailto:dremenska@ebay.com|dremenska@ebay.com>)","id":1,"mrkdwn_in":["text","pretext"]}],"type":"message","subtype":"bot_message","team":"T03L14Q4E","channel":"G04EC7B6V","event_ts":"1497280551.238075","ts":"1497280551.238075"}`,
			wantErr: false,
			want: &Message{
				ID: "1497280551.238075",
				From: User{
					Username: "gerrit",
				},
				Date: time.Unix(1497280551, 0),
				Chat: Chat{
					ID:   "G04EC7B6V",
					Type: Group,
				},
				Attachments: []Attachment{
					{
						Fallback: "Patch Set 1:\n\n(1 comment)",
						Text:     "Patch Set 1:\n\n(1 comment)",
						Pretext:  "[icas] New comment on change <https://gerrit.ecg.so/58629|58629>",
						Title:    "Comment by Daniela Remenska (<mailto:dremenska@ebay.com|dremenska@ebay.com>)",
						ID:       1,
					},
				},
			},
		},
	}

	for _, tt := range golden {
		t.Run(tt.name, func(t *testing.T) {
			s := Slack{}
			got, err := s.parseIncomingMessage([]byte(tt.raw))
			if (err != nil && !tt.wantErr) || (err == nil && tt.wantErr) {
				t.Errorf("got error %s, want error %t ", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v want %+v", got, tt.want)
			}
		})
	}

}
