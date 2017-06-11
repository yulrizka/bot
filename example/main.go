package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/yulrizka/bot"
)

// marcoPolo is an example plugin that will reply text marco with polo
type marcoPolo struct {
	out chan bot.Message
}

func (*marcoPolo) Name() string {
	return "MarcoPolo"
}

// Initialize should store the out channel to send message and return
// input channel which is an inbox for new channel.
// The reason 'in' is a return value so that plugin can specify the size
// of the channel
func (m *marcoPolo) Init(out chan bot.Message) error {
	m.out = out
	return nil
}

// Handle incoming message that could be in any type (*bot.Message, *bot.JoinMessage, etc).
// return handled false will pass modifiedMsg to other plugins down the chain
func (m *marcoPolo) Handle(inMsg interface{}) (handled bool, modifiedMsg interface{}) {
	if inMessage, ok := inMsg.(*bot.Message); ok {
		if strings.TrimSpace(strings.ToLower(inMessage.Text)) == "marco" {
			text := fmt.Sprintf("POLO! -> %s (<@%s>)\n", inMessage.From.FullName(), inMessage.From.Username)
			// send message
			msg := bot.Message{
				Chat: inMessage.Chat,
				Text: text,
			}
			m.out <- msg
		}
	}
	return true, inMsg
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)

	key := os.Getenv("SLACK_KEY")
	if key == "" {
		panic("TELEGRAM_KEY can not be empty")
	}
	slack, err := bot.NewSlack(context.Background(), key)
	if err != nil {
		log.Fatal(err)
	}
	plugin := marcoPolo{}
	if err := slack.AddPlugins(&plugin); err != nil {
		panic(err)
	}

	slack.Start()
}
