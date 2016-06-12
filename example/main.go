package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/yulrizka/bot"
)

// marcoPolo is an example plugin that will reply text marco with polo
type marcoPolo struct {
	in  chan interface{}
	out chan bot.Message
}

func (*marcoPolo) Name() string {
	return "MarcoPolo"
}

// Initialize should store the out channel to send message and return
// input channel which is an inbox for new channel.
// The reason 'in' is a return value so that plugin can specify the size
// of the channel
func (m *marcoPolo) Init(out chan bot.Message) (in chan interface{}, err error) {
	m.in = make(chan interface{}, 100)
	m.out = out
	go m.process()
	return m.in, nil
}

func (m *marcoPolo) process() {
	for rawMsg := range m.in {
		if message, ok := rawMsg.(*bot.Message); ok {
			if strings.TrimSpace(strings.ToLower(message.Text)) == "marco" {
				text := fmt.Sprintf("POLO! -> %s (@%s)\n", message.From.FullName(), message.From.Username)
				m.out <- bot.Message{
					Chat: message.Chat,
					Text: text,
				}
			}
		}
	}
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)

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
