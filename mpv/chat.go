package mpv

import (
	"encoding/json"
	"strings"
)

// ChatLimit is the most characters a chat message carries: one or two lines on mpv's screen.
const ChatLimit = 200

// chat is a message people type to each other: only shown, on every mpv screen.
const chat = "chat"

// chatOut sends what this node's person typed to the party, and shows it on its own screen.
func (c *Client) chatOut(text string) error {
	text = chatText(text)
	if text == "" {
		return nil
	}
	b, _ := json.Marshal(Event{Name: chat, Data: text})
	c.say(b)
	return c.showText("you: " + text)
}

// chatIn shows a member's message. Its sender may not have cut it, so it is cut here.
func (c *Client) chatIn(e Event, m IncomingMessage) error {
	text := chatText(e.Data)
	if text == "" {
		return nil
	}
	return c.showText(m.HostName + ": " + text)
}

// chatText is a message trimmed, and cut to ChatLimit characters.
func chatText(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > ChatLimit {
		s = strings.TrimSpace(string(r[:ChatLimit]))
	}
	return s
}
