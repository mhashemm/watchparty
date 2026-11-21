package mpv

import (
	"math"
	"math/rand"
	"slices"
	"strconv"
)

// overruled is shown when a set undid a key the person pressed while the party changed.
const overruled = "your key came as the party changed: press it again"

// ourSeek is a seek this node sent through a set (id), for the entry it was on.
type ourSeek struct {
	id    int64
	at    float64
	entry int
}

// pendingSet is the set the script hasn't acked yet. landed compares the ack with it:
// at is the second this node held when it sent the set; seek means the set seeks, so the
// ack's second may be from before mpv lands; still means mpv was stopped, so the ack's
// second is where it stood.
type pendingSet struct {
	id          int64
	at          float64
	seek, still bool
}

// scriptMsg is one message from watchparty.lua:
// watchparty <kind> <id> <pause> <time-pos> <n> <over|> <playlist-pos>.
type scriptMsg struct {
	// id: a set's id, a start's label, or the script's name in hello
	kind, id string
	// paused: "yes" or "no"
	paused string
	// known: mpv had a time-pos to send
	at    float64
	known bool
	// read: the message's number; the next set sends it back (lastScriptMsg)
	read  string
	over  bool
	entry string
}

// readScript parses a client-message's args, if the script sent it.
func readScript(args []string) (scriptMsg, bool) {
	if len(args) < 5 || args[0] != scriptName {
		return scriptMsg{}, false
	}
	arg := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	at, err := strconv.ParseFloat(args[4], 64)
	return scriptMsg{
		kind: args[1], id: args[2], paused: args[3], at: at, known: err == nil,
		read: arg(5), over: arg(6) == "over", entry: arg(7),
	}, true
}

// stoppedAt is whether mpv was paused on a known second.
func (m scriptMsg) stoppedAt() bool {
	return m.known && m.paused == "yes"
}

// report is a property change built from a script message, handled as if mpv sent it.
func report(name, data string) Event {
	return Event{EventType: propertyChange, Name: name, Data: data, fromScript: true}
}

// scriptSaid turns a script message into reports. While a set is pending, the script's
// pause and seek messages are ignored: they come from before the set, which overwrites
// them, or from the set itself. The ack says where mpv ended up (landed).
func (c *Client) scriptSaid(args []string) []Event {
	m, ok := readScript(args)
	if !ok {
		return nil
	}
	if m.read != "" {
		c.lastScriptMsg = m.read
	}
	if m.kind == "hello" {
		if c.script != "" {
			return nil
		}
		c.script = m.id
		// the entry mpv starts on; the script reports every later one (start)
		if e, err := strconv.Atoi(m.entry); err == nil && e >= 0 {
			c.current = e
		}
		c.seen[playlistPos] = true
		c.opens()
		// the first pause only records where mpv starts
		return []Event{report(pause, m.paused)}
	}
	id, _ := strconv.ParseInt(m.id, 10, 64)
	switch m.kind {
	case "refused":
		c.answered(id, outcome{Error: "mpv refused it"})
		if c.ground(id) {
			c.paused = m.paused == "yes"
			// the person may have moved to another file while the set was pending
			return c.wentElsewhere(m)
		}
	case "ack":
		c.answered(id, outcome{Error: "success"})
		return c.landed(id, m)
	case "start":
		return c.started(m.id, m.entry)
	case "pause":
		if c.pending != nil {
			return nil
		}
		var out []Event
		// a new second while paused, with no seek: a frame-step (ORDER.md)
		if m.stoppedAt() && c.paused && len(c.ourSeeks) == 0 && !c.on(m.at) {
			out = c.wentTo(0, m.at)
		}
		if m.stoppedAt() {
			c.pos = m.at
		}
		return append([]Event{report(pause, m.paused)}, out...)
	case seek:
		if m.known {
			return c.wentTo(id, m.at)
		}
	}
	return nil
}

// started handles the script saying mpv opened a file. label says whose start it was: a
// set's id (this node's move), "auto" (mpv moved on after a file ended or failed), or empty
// (the person). While a set is pending, a start that isn't its own is judged at the ack,
// which says which entry mpv ended on.
func (c *Client) started(label, entry string) []Event {
	n, err := strconv.Atoi(entry)
	if err != nil || n < 0 {
		return nil
	}
	c.lastStartAuto = label == "auto"
	if _, err := strconv.ParseInt(label, 10, 64); err == nil {
		// this node's own move
		if c.headingTo == n {
			c.headingTo = -1
		}
		if n != c.current {
			c.current = n
			c.clear(n)
		}
		return nil
	}
	if c.pending != nil {
		return nil
	}
	return []Event{report(playlistPos, entry)}
}

// answered hands the script's answer to the setState waiting on it.
func (c *Client) answered(id int64, o outcome) {
	if c.waiting[id] {
		c.replies[id] = o
	}
}

// ground clears the pending set if it is id, and says whether it was.
func (c *Client) ground(id int64) bool {
	if c.pending == nil || c.pending.id != id {
		return false
	}
	c.pending = nil
	return true
}

// landed handles the ack of the pending set: what mpv holds now that it has applied it.
// Nothing of this node's own goes out while a set is pending, so the ack can be compared
// with the state the set was sent from. Anything that differs is the person's, done while
// the set was pending, and goes out as theirs.
//
// over: the set undid a key the person pressed before this node read it. That key came
// while the party changed and would lose to the change anyway, so it stays undone and the
// person is told.
func (c *Client) landed(id int64, m scriptMsg) []Event {
	f := c.pending
	if !c.ground(id) {
		return nil
	}
	// a move goes out first
	out := c.wentElsewhere(m)
	if m.paused != holds(c.paused) {
		out = append(out, report(pause, m.paused))
	}
	// mpv was stopped and the set didn't seek, so it should still be on the same second.
	// Anywhere else, the person seeked just before the set (or into one of our seeks)
	if m.known && f.still && !f.seek && len(c.ourSeeks) == 0 && math.Abs(m.at-f.at) > timePosTolerance {
		c.seekWhilePlaying = m.paused == "no"
		out = append(out, report(timePos, seconds(m.at)))
	} else if m.stoppedAt() && !f.seek {
		c.pos = m.at
	}
	if m.over && len(out) == 0 {
		c.later = append(c.later, func() { c.showText(overruled) })
	}
	return out
}

// wentElsewhere is a move by the person (or mpv) while a set was pending: the entry mpv
// ended on, if it isn't the one this node is heading to.
func (c *Client) wentElsewhere(m scriptMsg) []Event {
	n, err := strconv.Atoi(m.entry)
	if err != nil || n < 0 || n == c.heading() {
		return nil
	}
	c.headingTo = -1
	move := report(playlistPos, m.entry)
	if m.stoppedAt() {
		// mpv may have played a moment there before the set stopped it
		at := m.at
		move.stoppedAt = &at
	}
	return []Event{move}
}

// holds is a pause as mpv and the script write it.
func holds(paused bool) string {
	if paused {
		return "yes"
	}
	return "no"
}

// pauseReq pauses or plays mpv where it is.
func (c *Client) pauseReq(paused bool) error {
	return c.setState(paused, nil, nil, c.paused)
}

// setState sends the script a whole state in one message (watchparty.lua) and waits for
// its ack: paused or playing, plus the entry and the second when given. still: mpv was
// stopped before. Sending it whole means a key the person presses lands either before the
// set, which overwrites it, or after, where it is clearly theirs (ORDER.md).
func (c *Client) setState(paused bool, second *float64, entry *int, still bool) error {
	id, at, to := rand.Int63(), "-", "-"
	if second != nil {
		at = seconds(*second)
		c.ourSeeks = append(c.ourSeeks, ourSeek{id: id, at: *second, entry: c.heading()})
		c.ourSeeks = c.ourSeeks[max(0, len(c.ourSeeks)-maxSeeks):]
		c.seekPending = true
	}
	on := "-"
	if entry != nil {
		to = strconv.Itoa(*entry)
	} else if len(c.names) > 0 {
		// the entry the set is for. If the person moved mpv to another one we haven't heard
		// about, the script only pauses there and the ack reports the move. Seeking there
		// would send our second as if it were on that file.
		on = strconv.Itoa(c.heading())
	}
	c.pending = &pendingSet{id: id, at: c.target(), seek: second != nil, still: still}
	// one set at a time: peer messages and queued commands (later) wait for the ack, so the
	// ack is always compared with the state it was sent from. It can arrive before mpv's
	// reply to the command itself.
	c.applying++
	defer func() { c.applying-- }()
	c.waiting[id] = true
	defer delete(c.waiting, id)
	err := c.do("script-message-to", c.script, scriptName+"-set", holds(paused), at, to, strconv.FormatInt(id, 10), c.lastScriptMsg, on)
	if err == nil {
		// a move is acked once its file starts, which can take far longer than 5s on a slow
		// stream. The script acks when the file starts or mpv gives up on it
		// (--network-timeout, 60s), and mpv quitting ends the wait; moveLimit only covers
		// --network-timeout=0.
		limit := replyLimit
		if entry != nil {
			limit = moveLimit
		}
		_, err = c.await(id, limit)
	}
	if err != nil {
		c.ground(id)
	}
	return err
}

// wentTo handles a seek the script reported at mpv's seek event, which is where mpv lands
// (ORDER.md). It is ours if it carries one of our ids and lands on that seek's second; mpv
// runs seeks in order, so our earlier ones were folded into it. Anything else is the
// person's: their own seek, or one folded into ours (a key's step added to it, or their
// second replacing it). That goes out, unless mpv is already where this node is or heads.
func (c *Client) wentTo(id int64, pos float64) []Event {
	where := c.target()
	if i := slices.IndexFunc(c.ourSeeks, func(s ourSeek) bool { return s.id == id }); i >= 0 {
		at := c.ourSeeks[i].at
		c.ourSeeks = c.ourSeeks[i+1:]
		if math.Abs(pos-at) <= timePosTolerance {
			c.pos = pos
			c.seekPending = c.seekPending && len(c.ourSeeks) > 0
			return nil
		}
	}
	if c.pending != nil {
		// from before the pending set, which overwrites it; the ack says where mpv went
		return nil
	}
	if c.paused && math.Abs(pos-where) <= timePosTolerance {
		c.pos = pos
		return nil
	}
	c.seekWhilePlaying = !c.paused
	return []Event{report(timePos, seconds(pos))}
}
