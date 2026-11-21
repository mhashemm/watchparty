package mpv

import (
	"math"
	"math/rand"
	"slices"
	"strconv"
)

// overruled is shown when a set undid the person's key, pressed as the party changed.
const overruled = "your key came as the party changed: press it again"

// ourSeek is a seek this node sent, by the id of its set, which the script says with it
// (wentTo), on the entry it is for.
type ourSeek struct {
	id    int64
	at    float64
	entry int
}

// pendingSet is a set the script hasn't acked, and what the ack is weighed against (landed): the
// stamp and the second this node held as it sent it; seek: it seeks, so the ack's second is
// from before mpv lands; still: mpv was stopped, so the ack's second is where it was.
type pendingSet struct {
	id          int64
	at          float64
	seek, still bool
}

// scriptMsg is one message from the script (watchparty.lua), sent as
// watchparty <kind> <id> <pause> <time-pos> <n> <over|> <playlist-pos>.
type scriptMsg struct {
	// id: a set's id, a start's label, or hello's script name
	kind, id string
	// paused: mpv's pause as it says it, "yes" or "no"
	paused string
	// known: mpv had a time-pos, at
	at    float64
	known bool
	// read: the message's number, which the next set says (c.lastScriptMsg)
	read  string
	over  bool
	entry string
}

// readScript parses a client-message's args, if it is the script's.
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

// stoppedAt is whether mpv was stopped on a known second.
func (m scriptMsg) stoppedAt() bool {
	return m.known && m.paused == "yes"
}

// report is a property change made from what the script said, read as mpv's.
func report(name, data string) Event {
	return Event{EventType: propertyChange, Name: name, Data: data, fromScript: true}
}

// scriptSaid is a message from the script (watchparty.lua): its name, once, or what mpv holds when
// it was sent, as reports to read as mpv's: the pause it holds, and the person's seek. While a
// set is on its way, what the script says is from before it, which the set overwrites, or from
// the set itself: none is the person's, and the ack says where mpv ended (landed).
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
		// where mpv starts: every later entry it opens, the script says (start)
		if e, err := strconv.Atoi(m.entry); err == nil && e >= 0 {
			c.current = e
		}
		c.seen[playlistPos] = true
		c.opens()
		// what mpv holds: the first pause this node hears, which only marks where it starts
		return []Event{report(pause, m.paused)}
	}
	id, _ := strconv.ParseInt(m.id, 10, 64)
	switch m.kind {
	case "refused":
		c.answered(id, outcome{Error: "mpv refused it"})
		if c.ground(id) {
			c.paused = m.paused == "yes"
			// a key that moved mpv while the set was on its way, as at an ack
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
		// stopped, and still: a frame-step, which moves mpv without a seek (ORDER.md)
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

// started is the script saying whose file mpv opened (watchparty.lua): this node's move, by its
// set's id; mpv's own (auto), after a file's end or error; or the person's. While a set is on its
// way, a start that isn't its own is weighed at its ack, which says the entry mpv ended on.
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

// answered gives the script's answer to the setState waiting on it.
func (c *Client) answered(id int64, o outcome) {
	if c.waiting[id] {
		c.replies[id] = o
	}
}

// ground forgets the set on its way once the script answered it or it failed, and whether id
// was that set.
func (c *Client) ground(id int64) bool {
	if c.pending == nil || c.pending.id != id {
		return false
	}
	c.pending = nil
	return true
}

// landed is the script's ack of the set on its way: what mpv holds once it has all of it. What
// differs from what this node holds is the person's, made while the set was on its way and not
// overwritten by it, and goes out as their report: nothing else of this node's goes out while
// a set is on its way, so the ack is always weighed against the state it was sent from. over: the set undid a key of the
// person's this node never read; made as the party changed, it would lose to that change
// anyway (stopped over playing), so it stays undone, and the person is told.
func (c *Client) landed(id int64, m scriptMsg) []Event {
	f := c.pending
	if !c.ground(id) {
		return nil
	}
	// it goes out first, as a move does
	out := c.wentElsewhere(m)
	if m.paused != holds(c.paused) {
		out = append(out, report(pause, m.paused))
	}
	// mpv stood where this node held it, with no seek of its own: anywhere else, the person's
	// seek ran just before the set, or folded into a seek of this node's landing meanwhile
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

// wentElsewhere is the entry the script says mpv ended a set on, if it isn't the one this node
// heads to: the person's move, or mpv's own, while the set was on its way, from where mpv stopped.
func (c *Client) wentElsewhere(m scriptMsg) []Event {
	n, err := strconv.Atoi(m.entry)
	if err != nil || n < 0 || n == c.heading() {
		return nil
	}
	c.headingTo = -1
	move := report(playlistPos, m.entry)
	if m.stoppedAt() {
		// it may have played a moment there before the set stopped it
		at := m.at
		move.stoppedAt = &at
	}
	return []Event{move}
}

// holds is a pause as mpv and the script say it.
func holds(paused bool) string {
	if paused {
		return "yes"
	}
	return "no"
}

// pauseReq stops or plays mpv where it is.
func (c *Client) pauseReq(paused bool) error {
	return c.setState(paused, nil, nil, c.paused)
}

// setState has the script take mpv to a state in one message (watchparty.lua), and returns once mpv has
// all of it (landed): stopped or playing, on the entry if one is given, on the second if one
// is. still: mpv was stopped before. The person's key can only land before it, which it
// overwrites, or after it, which is theirs (ORDER.md).
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
		// the entry it is for: if a key took mpv to another before this node heard it, the
		// script only stops it there, and the ack's entry sends that move out, newer. A seek
		// there would go out as this node's second on it, with the move from 0.
		on = strconv.Itoa(c.heading())
	}
	c.pending = &pendingSet{id: id, at: c.target(), seek: second != nil, still: still}
	// one set at a time: a message, or a command a line starts (later), waits for its ack, so
	// the ack is always weighed against the state it was sent from. The ack can come while mpv
	// answers the command itself.
	c.applying++
	defer func() { c.applying-- }()
	c.waiting[id] = true
	defer delete(c.waiting, id)
	err := c.do("script-message-to", c.script, scriptName+"-set", holds(paused), at, to, strconv.FormatInt(id, 10), c.lastScriptMsg, on)
	if err == nil {
		// a move is acked once its file starts, which a slow stream can take far longer than
		// 5s to do; the script acks it then, or as mpv gives up on the file (its
		// --network-timeout, 60s), and mpv quitting ends the wait. moveLimit only backs that up,
		// for a --network-timeout=0
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

// wentTo is a seek mpv runs to pos, as the script says at mpv's seek event (watchparty.lua), which
// is where it lands (ORDER.md). This node's own if it carries this node's id and is on its
// second; mpv runs seeks in order, so those marked before it were folded into it. Any other
// is the person's: their seek alone, or folded into this node's (a key's step added to it, or
// their second replacing it). It goes out as mpv's report of where it is, as a seek always did
// (positionChanged), unless it is where this node already is or heads.
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
		// from before a set on its way, which overwrites it, or the ack says where it went
		return nil
	}
	if c.paused && math.Abs(pos-where) <= timePosTolerance {
		c.pos = pos
		return nil
	}
	c.seekWhilePlaying = !c.paused
	return []Event{report(timePos, seconds(pos))}
}
