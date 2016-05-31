package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/yulrizka/bot"
)

// marcoPolo is an example plugin that will reply text marco with polo
type marcoPolo struct {
	in  chan *bot.Message
	out chan bot.Message
}

func (*marcoPolo) Name() string {
	return "MarcoPolo"
}

// Initialize should store the out channel to send message and return
// input channel which is an inbox for new channel.
// The reason 'in' is a return value so that plugin can specify the size
// of the channel
func (m *marcoPolo) Init(out chan bot.Message) (in chan *bot.Message, err error) {
	m.in = make(chan *bot.Message, 100)
	m.out = out
	go m.process()
	return m.in, nil
}

func (m *marcoPolo) process() {
	for mIn := range m.in {
		if strings.TrimSpace(strings.ToLower(mIn.Text)) == "marco" {
			text := fmt.Sprintf("POLO! -> %s (%s)\n", mIn.From.Username, mIn.From.ID)

			m.out <- bot.Message{
				Chat: mIn.Chat,
				Text: text,
			}
		}
	}
}

func main() {
	key := os.Getenv("TELEGRAM_KEY")
	if key == "" {
		panic("TELEGRAM_KEY can not be empty")
	}
	telegram := bot.NewTelegram(key)
	plugin := marcoPolo{}
	if err := telegram.AddPlugin(&plugin); err != nil {
		panic(err)
	}

	telegram.Start()
}
