package mpv

import (
	"encoding/json"
	"strings"
)

// ChatLimit is the most characters a chat message carries: one or two lines on mpv's screen.
const ChatLimit = 200

// chat is a message people type to each other: only shown, on every mpv screen and to heard.
// It is never stored, not even in the log: the log goes to stderr, which a desktop can keep
// on disk (the systemd journal), so its show-text is unlogged and no line quotes it.
const chat = "chat"

// chatOut sends what this node's person typed to the party, and shows it on its own screen.
func (c *Client) chatOut(text string) error {
	text = chatText(text)
	if text == "" {
		return nil
	}
	b, _ := json.Marshal(Event{Name: chat, Data: text})
	c.say(b)
	return c.showChat("you", text)
}

// chatIn shows a member's message. Its sender may not have cut it, so it is cut here.
func (c *Client) chatIn(e Event, m IncomingMessage) error {
	text := chatText(e.Data)
	if text == "" {
		return nil
	}
	return c.showChat(m.HostName, text)
}

// showChat puts a message on mpv's screen, as "who: text", and hands it to heard.
func (c *Client) showChat(who, text string) error {
	if c.heard != nil {
		c.heard(who, text)
	}
	_, err := c.send(true, []any{showText, who + ": " + text, showTextDurationSeconds * 1000})
	return err
}

// chatText is a message trimmed, and cut to ChatLimit characters.
func chatText(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > ChatLimit {
		s = strings.TrimSpace(string(r[:ChatLimit]))
	}
	return s
}
