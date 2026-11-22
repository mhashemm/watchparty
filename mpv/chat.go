package mpv

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// ChatLimit is the most characters a chat message carries.
const ChatLimit = 200

// chat is a message people type to each other: only shown, on every mpv screen and to heard.
// It is never stored, not even in the log: the log goes to stderr, which a desktop can keep
// on disk (the systemd journal), so what shows it is unlogged and no line quotes it.
const chat = "chat"

// chatNotSent is what this node's screen says of a message typed while it has lost the relay.
const chatNotSent = "your message wasn't sent: lost the relay, reconnecting"

// Chat has a layer of its own on mpv's screen (osd-overlay), top-right, apart from show-text's
// notices top-left: one replaces the last, so a message would hide "paused for X: seek", and
// the next message the one before. It holds the last chatLines messages, each for chatOnScreen.
const (
	// chatOverlay is the layer's id, which mpv keeps per IPC connection
	chatOverlay  = 1
	chatLines    = 3
	chatOnScreen = 8 * time.Second
	// chatWrap is the characters on one line: unwrapped, ASS would run a long message across
	// the whole screen, over the notices
	chatWrap = 48
	// the colours are ASS's &HBBGGRR&: this node's own messages light moss, everyone else's
	// white. A name can be anything, "you" too; the colour is what says whose it is.
	chatOurs   = "&HA6E3B8&"
	chatTheirs = "&HFFFFFF&"
	chatStyle  = `{\an9\fs26\bord2\shad1\3c&H000000&\q2}`
)

// onScreen is a chat message on mpv's screen until it is due to go.
type onScreen struct {
	who, text string
	ours      bool
	until     time.Time
}

// chatOut sends what this node's person typed to the party, and shows it on its own screen.
func (c *Client) chatOut(text string) error {
	text = chatText(text)
	if text == "" {
		return nil
	}
	if c.offline {
		// the relay would drop it, and the screen would say it went
		return c.showText(chatNotSent)
	}
	b, _ := json.Marshal(Event{Name: chat, Data: text})
	c.say(b)
	return c.showChat("you", text, true)
}

// chatIn shows a member's message. Its sender may not have cut it, so it is cut here.
func (c *Client) chatIn(e Event, m IncomingMessage) error {
	text := chatText(e.Data)
	if text == "" {
		return nil
	}
	return c.showChat(m.HostName, text, false)
}

// showChat hands a message to heard and puts it on mpv's screen, under the ones before it.
func (c *Client) showChat(who, text string, ours bool) error {
	if c.heard != nil {
		c.heard(who, text, ours)
	}
	c.chatShown = append(c.chatShown, onScreen{who, text, ours, time.Now().Add(chatOnScreen)})
	c.chatShown = c.chatShown[max(0, len(c.chatShown)-chatLines):]
	time.AfterFunc(chatOnScreen, func() { c.post(input{chatDue: true}) })
	return c.drawChat()
}

// chatGoes takes the messages whose time is up off mpv's screen. One a newer message pushed
// off already changes nothing.
func (c *Client) chatGoes() error {
	before, now := len(c.chatShown), time.Now()
	c.chatShown = slices.DeleteFunc(c.chatShown, func(s onScreen) bool { return !now.Before(s.until) })
	if len(c.chatShown) == before {
		return nil
	}
	return c.drawChat()
}

// drawChat puts chatShown on mpv's screen, or takes the layer away.
func (c *Client) drawChat() error {
	if len(c.chatShown) == 0 {
		_, err := c.send(true, []any{"osd-overlay", chatOverlay, "none", ""})
		return err
	}
	lines := make([]string, 0, len(c.chatShown))
	for _, s := range c.chatShown {
		colour := chatTheirs
		if s.ours {
			colour = chatOurs
		}
		lines = append(lines, `{\1c`+colour+`}{\b1}`+assText(s.who)+`{\b0}: `+strings.Join(wrapped(s.text), `\N`))
	}
	_, err := c.send(true, []any{"osd-overlay", chatOverlay, "ass-events", chatStyle + strings.Join(lines, `\N`), 0, 720, 0})
	return err
}

// chatText is a message on one line, its runs of spaces and control characters made one
// space, and cut to ChatLimit characters.
func chatText(s string) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r < 0x20 || r == 0x7f }), " ")
	if r := []rune(s); len(r) > ChatLimit {
		s = strings.TrimSpace(string(r[:ChatLimit]))
	}
	return s
}

// wrapped is a message's words in lines of at most chatWrap characters, a longer word cut,
// each escaped for ASS.
func wrapped(text string) []string {
	var lines []string
	line := []rune{}
	for _, word := range strings.Fields(text) {
		w := []rune(word)
		for len(w) > chatWrap {
			if len(line) > 0 {
				lines, line = append(lines, string(line)), nil
			}
			lines, w = append(lines, string(w[:chatWrap])), w[chatWrap:]
		}
		if len(line) > 0 && len(line)+1+len(w) > chatWrap {
			lines, line = append(lines, string(line)), nil
		}
		if len(line) > 0 {
			line = append(line, ' ')
		}
		line = append(line, w...)
	}
	if len(line) > 0 {
		lines = append(lines, string(line))
	}
	for i := range lines {
		lines[i] = assText(lines[i])
	}
	return lines
}

// assText is text as ASS shows it, not as tags: a backslash can't start one (\N, \h), nor a
// brace an override. mpv's own console escapes so too.
func assText(s string) string {
	return strings.NewReplacer(`\`, "\\\ufeff", "{", `\{`, "}", `\}`).Replace(s)
}
