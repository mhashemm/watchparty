package mpv

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode"
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
		c.heard(Said{Who: who, Text: text, Ours: ours})
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
// space, without the invisible format characters (unicode.Cf) a member could hide text with,
// turn it around (U+202E) or send an empty-looking message with, and cut to ChatLimit
// characters. The zero-width joiner stays: it makes one emoji of several, 👨‍👩‍👧.
func chatText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r != zwj && unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	words := strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
	words = slices.DeleteFunc(words, func(w string) bool { return strings.Trim(w, string(zwj)) == "" })
	s = strings.Join(words, " ")
	if r := []rune(s); len(r) > ChatLimit {
		s = strings.TrimSpace(string(r[:ChatLimit]))
	}
	return s
}

const zwj = '\u200d'

// wrapped is a message's words in lines chatWrap columns wide at most, a wider word cut, each
// escaped for ASS.
func wrapped(text string) []string {
	var lines []string
	var line []rune
	used := 0
	flush := func() {
		if len(line) > 0 {
			lines, line, used = append(lines, string(line)), nil, 0
		}
	}
	for _, word := range strings.Fields(text) {
		w := []rune(word)
		for columns(w) > chatWrap {
			flush()
			cut, n := 0, 0
			// a zero-width character stays with the one it marks
			for cut < len(w) && n+cells(w[cut]) <= chatWrap {
				n += cells(w[cut])
				cut++
			}
			lines, w = append(lines, string(w[:cut])), w[cut:]
		}
		if used > 0 && used+1+columns(w) > chatWrap {
			flush()
		}
		if used > 0 {
			line, used = append(line, ' '), used+1
		}
		line, used = append(line, w...), used+columns(w)
	}
	flush()
	for i := range lines {
		lines[i] = assText(lines[i])
	}
	return lines
}

func columns(w []rune) int {
	n := 0
	for _, r := range w {
		n += cells(r)
	}
	return n
}

// cells is about how many columns r takes on screen: CJK and emoji take two, so 48 of them
// would run twice as far as 48 letters, toward the notices. A mark, joiner or skin tone takes
// none, being part of the character before it. Erring wide only breaks a line early.
func cells(r rune) int {
	switch {
	case r == zwj, unicode.In(r, unicode.Mn, unicode.Me), r >= 0x1f3fb && r <= 0x1f3ff:
		return 0
	case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf && r != 0x303f, r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f, r >= 0xff00 && r <= 0xff60, r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f300 && r <= 0x1f64f, r >= 0x1f680 && r <= 0x1f6ff, r >= 0x1f900 && r <= 0x1faff, r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}

// assText is text as ASS shows it, not as tags: a backslash can't start one (\N, \h), nor a
// brace an override. mpv's own console escapes so too.
func assText(s string) string {
	return strings.NewReplacer(`\`, "\\\ufeff", "{", `\{`, "}", `\}`).Replace(s)
}
