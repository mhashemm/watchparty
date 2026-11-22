package mpv

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// react is an emoji reaction: a burst of it on every mpv screen, drawn by reactions.lua, and a
// line to heard. Like chat it is only shown, and never logged (chat.go).
const react = "react"

// reactEvery is how often one person's reaction bursts at most: each covers the screen for
// about two seconds. Each node holds every sender to it, its own person too.
const reactEvery = 2 * time.Second

const (
	// ReactionsFile is the reactions script's file; mpv names the script after it
	ReactionsFile   = reactionsScript + ".lua"
	reactionsScript = "watchparty_reactions"
	// reactBurst asks the script to burst an emoji; reactPicked is the script's picker
	// telling the node its person picked one
	reactBurst  = "watchparty-react"
	reactPicked = "watchparty-reacted"
)

// reactNotSent is what this node's screen says of a reaction picked while it has lost the relay.
const reactNotSent = "your reaction wasn't sent: lost the relay, reconnecting"

//go:embed reactions.lua
var reactionsLua []byte

//go:embed emoji.json
var emojiJSON []byte

// emoji is emoji.json: the reactions in the picker's order, and each one's shapes, back to
// front, as an ASS colour and drawing. It is embedded, so TestEveryReactionHasItsArt is what
// finds it broken.
var emoji = sync.OnceValue(func() (e struct {
	Order []string
	Art   map[string][][2]string
}) {
	json.Unmarshal(emojiJSON, &e)
	return e
})

// Reactions are the emoji a person can react with, in the picker's order, as code points in hex.
func Reactions() []string {
	return slices.Clone(emoji().Order)
}

// ReactionsScript is reactions.lua with its art: mpv loads it from ReactionsFile.
func ReactionsScript() []byte {
	var b bytes.Buffer
	b.Write(reactionsLua)
	b.WriteString("\n-- emoji.json's art (mpv/react.go)\n")
	for _, code := range emoji().Order {
		fmt.Fprintf(&b, "art_order[#art_order + 1] = %q\n", code)
		fmt.Fprintf(&b, "art[%q] = {", code)
		for _, s := range emoji().Art[code] {
			fmt.Fprintf(&b, "{%q, %q}, ", s[0], s[1])
		}
		b.WriteString("}\n")
	}
	return b.Bytes()
}

// EmojiSVG is a reaction drawn as the GUI shows it: the same shapes mpv draws, as SVG.
func EmojiSVG(code string) string {
	shapes, ok := emoji().Art[code]
	if !ok {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 288 288">`)
	for _, s := range shapes {
		// ASS's &HBBGGRR& is SVG's #RRGGBB
		c := strings.Trim(s[0], "&H")
		fmt.Fprintf(&b, `<path fill="#%s%s%s" d="%s"/>`, c[4:6], c[2:4], c[0:2], svgPath(s[1]))
	}
	b.WriteString("</svg>")
	return b.String()
}

// svgPath is an ASS drawing (m, l, b) as SVG path data.
func svgPath(drawing string) string {
	var b strings.Builder
	for _, f := range strings.Fields(drawing) {
		switch f {
		case "m":
			b.WriteString("M")
		case "l":
			b.WriteString("L")
		case "b":
			b.WriteString("C")
		default:
			b.WriteString(f + " ")
		}
	}
	return strings.TrimSpace(b.String())
}

// reactOut sends this node's person's reaction to the party, and bursts it here.
func (c *Client) reactOut(code string) error {
	if _, ok := emoji().Art[code]; !ok || !c.mayReact("") {
		return nil
	}
	if c.offline {
		return c.showText(reactNotSent)
	}
	b, _ := json.Marshal(Event{Name: react, Data: code})
	c.say(b)
	return c.burst("you", code, true)
}

// reactIn bursts a member's reaction: one this node knows, and not too soon after their last.
func (c *Client) reactIn(e Event, m IncomingMessage) error {
	if _, ok := emoji().Art[e.Data]; !ok || !c.mayReact(m.From) {
		return nil
	}
	return c.burst(m.HostName, e.Data, false)
}

// mayReact is whether from (this node's person: "") may burst now, and if so, notes it.
func (c *Client) mayReact(from string) bool {
	now := time.Now()
	if last, ok := c.reacted[from]; ok && now.Sub(last) < reactEvery {
		return false
	}
	c.reacted[from] = now
	return true
}

func (c *Client) burst(who, code string, ours bool) error {
	if c.heard != nil {
		c.heard(Said{Who: who, React: code, Ours: ours})
	}
	_, err := c.send(true, []any{"script-message-to", reactionsScript, reactBurst, code})
	return err
}
