package bot

import (
	"context"
	"testing"
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
func (m mockPlugin) Init(out chan Message) error {
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
			InitFn: func(out chan Message) error {
				return nil
			},
			HandleFn: func(m interface{}) (handled bool, msg interface{}) {
				inMsg := m.(*Message)
				inMsg.Text += "1"
				return false, inMsg.WithContext(context.WithValue(ctx, "val", "1"))
			},
		}

		p2 := mockPlugin{
			InitFn: func(out chan Message) error {
				return nil
			},
			HandleFn: func(m interface{}) (handled bool, msg interface{}) {
				inMsg := m.(*Message)
				inMsg.Text += "2"
				val := inMsg.Context().Value("val")
				return false, inMsg.WithContext(context.WithValue(ctx, "val", val.(string)+"2"))
			},
		}

		s.AddPlugins(p1, p2)
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
			InitFn: func(out chan Message) error {
				return nil
			},
			HandleFn: func(in interface{}) (handled bool, msg interface{}) {
				m := in.(*Message)
				m.Text += "1"
				return true, m
			},
		}
		p2 := mockPlugin{
			InitFn: func(out chan Message) error {
				return nil
			},
			HandleFn: func(in interface{}) (handled bool, msg interface{}) {
				m := in.(*Message)
				m.Text += "2"
				return true, m
			},
		}

		s.AddPlugins(p1, p2)
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
			t.Errorf("got handled %s want %s", got, want)
		}
	})

}
