package mpv

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"slices"
	"strconv"
)

// state is what the party shares, and what every message says its sender holds: a stamp
// (a Lamport clock), the entry, stopped or playing, and where it is stopped or resumes from.
type state struct {
	stamp  int
	entry  int
	paused bool
	at     float64
	// leaderless: a stop with no leader (Event.Free); not weighed by over
	leaderless bool
}

// over orders two states the same way on every node: the higher stamp; with the same
// stamp the two were made at once and the safer wins: the higher entry, stopped over
// playing, then the earlier second. 0 is the same state.
func (s state) over(h state) int {
	switch {
	case s.stamp != h.stamp:
		return cmp.Compare(s.stamp, h.stamp)
	case s.entry != h.entry:
		return cmp.Compare(s.entry, h.entry)
	case s.paused != h.paused:
		if s.paused {
			return 1
		}
		return -1
	case !s.paused || math.Abs(s.at-h.at) <= timePosTolerance:
		return 0
	}
	return cmp.Compare(h.at, s.at)
}

// agrees is whether s is the same state as h, whatever their stamps.
func (s state) agrees(h state) bool {
	s.stamp = h.stamp
	return s.over(h) == 0
}

// held is the state this node holds: the entry it is heading to and its second there,
// which is the start of a file still loading unless a seek waits for it; one loaded before
// mpv reported it is not, as a seek let go there marked its second. Never the file
// mpv happens to be on: a state with the party's stamp on the entry it left is a tie it
// could win, and would drag the party back. Stopped once it is stopping the party
// (pauseParty): its own move says so, before mpv has stopped.
func (c *Client) held() state {
	at, entry := c.target(), c.heading()
	if c.onAStop() {
		// the party's entry and second, wherever the person has taken this node's own screen
		at, entry = c.resumedFrom, c.partyEntry
	}
	paused := c.paused || c.stopReason != ""
	return state{stamp: c.stamp, entry: entry, paused: paused, at: at, leaderless: paused && c.leaderless}
}

// onAStop is a node stopped on a member's state (by). Not a joiner, whose stamp is still
// unheard, nor a node taking a member's resume: a seek the person makes before mpv plays is
// theirs, newer, and stops it (moved).
func (c *Client) onAStop() bool {
	return c.paused && c.stopReason == "" && c.leaderID != "" && !c.tookResume && c.stamp >= 0
}

// led is a node following the leader of a stop: the member whose pause or seek it took. Only
// the leader seeks for the party; a seek the person makes here stays on their screen until
// the party resumes, from the resumer's screen. A stop nobody chose has no leader (free):
// anyone's seek goes out, and the first leads.
func (c *Client) led() bool {
	return c.onAStop() && !c.leaderless
}

// tag says what state a message was sent from, so whoever gets it knows whether it is
// newer than theirs. Only this node knows, at the moment it sends.
func (c *Client) tag(b []byte) []byte {
	s := c.held()
	head := fmt.Appendf(nil, `{"stamp":%d,"paused":%t,"at":%.6f,`, s.stamp, s.paused, s.at)
	if s.leaderless {
		head = append(head, `"free":true,`...)
	}
	if len(c.names) > 0 {
		head = fmt.Appendf(head, `"entry":%d,`, s.entry)
	}
	return append(head, b[1:]...)
}

// stateOf is the state a peer's pause, seek or move says its sender holds. Every node
// sends it whole (tag), so one that doesn't is refused, not filled in from this node's.
func (c *Client) stateOf(e Event) (state, error) {
	if e.Paused == nil || e.At == nil || len(c.names) > 0 && e.Entry == nil {
		return state{}, errors.New("no state")
	}
	s := state{stamp: e.Stamp, paused: *e.Paused, at: *e.At, leaderless: e.Free && *e.Paused}
	if len(c.names) > 0 {
		if s.entry = *e.Entry; s.entry < 0 || s.entry >= len(c.names) {
			return s, errors.New("no such entry")
		}
	}
	return s, nil
}

// on is whether pos is the second this node is on, or on its way to.
func (c *Client) on(pos float64) bool {
	return math.Abs(pos-c.target()) <= timePosTolerance
}

// heading is the entry this node is on, or on its way to.
func (c *Client) heading() int {
	if c.headingTo >= 0 {
		return c.headingTo
	}
	return c.current
}

// hear keeps what the transport delivers while the first file opens. Each pause and
// time-pos says the party's whole state, so only the newest by stamp is kept; the rest
// are notices, kept in order.
func (c *Client) hear(e IncomingMessage) {
	event := Event{}
	err := json.Unmarshal(e.Event, &event)
	if err != nil || !shared(event) {
		c.notices = append(c.notices, e)
		return
	}
	if s, err := c.stateOf(event); err == nil && (c.newestHeard == nil || s.over(c.newestState) > 0) {
		c.newestHeard, c.newestState = &e, s
	}
}

// join is the first file loaded: a continued party seeks only the file it continues on,
// before the party's position is applied and before joining, so whatever the party says
// lands after it. Not --start: mpv applies it to every later entry too. If it fails, the
// file is at 0. Then the newest state heard while it opened, the join, the notices; all as
// one transport message, so what comes meanwhile waits.
func (c *Client) join() {
	c.opening = false
	c.opens()
	c.applying++
	defer func() { c.applying-- }()
	if c.resumeAt > 0 {
		if err := c.setState(c.paused, &c.resumeAt, nil, c.paused); err != nil {
			log.Println("continuing where it stopped:", err)
		}
	}
	if c.newestHeard != nil {
		c.apply(*c.newestHeard)
	}
	c.announce()
	for _, e := range c.notices {
		c.apply(e)
	}
	c.newestHeard, c.notices = nil, nil
}

// announce joins the party: the members pause and answer with where they are, and this
// node takes the first answer.
func (c *Client) announce() {
	c.stamp = unheard
	hello, _ := json.Marshal(Event{Name: joined})
	c.say(hello)
}

// lost says the relay connection dropped. This node plays on: the party most likely does
// too, and back in the room it says where it is (back).
func (c *Client) lost() error {
	return c.showText(lostRelay)
}

// back tells the party, quietly, the state this node holds after a dropped relay
// connection, as it may have missed something or done something nobody heard: take stops
// the party only for a state that isn't its own. A joiner that was never answered joins
// again. Not while this node plays out the end: it would stop everyone just short of it.
func (c *Client) back() {
	joiner, finishing := c.stamp < 0, !c.paused && c.ending()
	var check []byte
	if !joiner && !finishing {
		check, _ = json.Marshal(Event{EventType: propertyChange, Name: pause, Data: holds(c.held().paused), Check: true})
		check = c.tag(check)
	}
	switch {
	case joiner:
		c.announce()
	case check != nil:
		c.say(check)
	}
}

func (c *Client) apply(e IncomingMessage) {
	if e.Opened {
		c.stamp = max(c.stamp, 1)
		return
	}
	if e.Lost {
		if err := c.lost(); err != nil {
			log.Println("ProcessIncomingEvents: lost the relay:", err)
		}
		return
	}
	if e.Back {
		c.back()
		return
	}
	if e.Missed || e.Resend {
		if err := c.chase(e); err != nil {
			log.Println("ProcessIncomingEvents: a member missed something:", err)
		}
		return
	}
	event := Event{}
	err := json.Unmarshal(e.Event, &event)
	if err == nil && shared(event) {
		err = c.take(event, e)
	} else if err == nil {
		switch event.Name {
		case showText:
			err = c.showText(event.Data)
			if line, ok := memberLine(event.Data, e); ok {
				log.Println(line)
			}
			if event.Data == e.HostName+HasLeft {
				err = errors.Join(err, c.left(e.HostName, e.From))
			}
		case pauseReason:
			err = c.showText(fmt.Sprintf("paused for %s: %s", e.HostName, event.Data))
		case buffered:
			// shown, never acted on: a peer's name can hold anything, so the log quotes it
			var secs int
			if secs, err = strconv.Atoi(event.Data); err == nil {
				log.Printf("buffered ahead: %d for %s\n", secs, memberOf(e))
				shown, ok := c.peerReadyOnScreen[e.From]
				if !ok {
					shown = -1
				}
				if louder(secs, shown) {
					c.peerReadyOnScreen[e.From] = secs
					err = c.showText(fmt.Sprintf("%s's network is slow: %ds ready", e.HostName, secs))
				}
			}
		case joined:
			delete(c.missed, e.From)
			err = c.answer(e.HostName, "joining")
		}
	}
	if err != nil {
		log.Printf("ProcessIncomingEvents: %s [event=%s] [host=%s]\n", err, e.Event, e.HostName)
	}
}

// shared is a message that says what state its sender holds.
func shared(e Event) bool {
	return e.Name == pause || e.Name == timePos || e.Name == playlistPos
}

// take weighs a peer's state against what this node holds: newer is adopted; older means
// its sender missed something, and the party stops for it to catch up; one made at the
// same moment as this node's, which wins, is told quietly, as it may never hear this one.
// A joiner's is neither: its join is being answered.
func (c *Client) take(e Event, m IncomingMessage) error {
	s, err := c.stateOf(e)
	if err != nil {
		return err
	}
	hostname := m.HostName
	h := c.held()
	over, told := s.over(h), c.leaderID == m.From
	// a resume reaching a node that plays: newer, this node missed the pause in between; a
	// tie from a member something this node sent never reached, that member resumed from
	// what it last heard; the state this node took from a member, sent again by it, to this
	// node alone, as what it sent looked lost: it arrived late; or a newer resume from a
	// member back from losing the relay, made where nobody heard it; or a tie from another
	// stop, more than the two seconds each could stop at from one. Not a joiner: it syncs
	// when its join is answered.
	elsewhere := !e.Again && !e.Check && math.Abs(s.at-c.resumedFrom) > 2*timePosTolerance
	apart := !s.paused && h.stamp >= 0 && s.entry == h.entry &&
		(!h.paused && (over > 0 || over == 0 && (c.missed[m.From] || e.Again && told || elsewhere)) || e.Check && over > 0)
	if over > 0 {
		// it heard something since, or made it: whatever it missed, this node now holds its
		// state, or stops the party for it. Not when a send reaches it: a tie it sent
		// before can still be on its way.
		delete(c.missed, m.From)
	}
	var a adoption
	switch {
	case apart:
		// the pause that answers it must be newer than the resume
		c.stamp = max(c.stamp, s.stamp)
	case over > 0:
		a = c.commit(s, m.From)
		c.leaderName, c.toldWhoLeads = hostname, false
	}
	switch {
	case apart:
		// each stops where it is, and the party settles on the earliest
		return c.answer(hostname, outOfSync)
	case over > 0:
		return c.adopt(a, e.Name, hostname)
	case over == 0:
		// where the pause it came with already took this node
		if e.Name == timePos && told {
			c.showText(fmt.Sprintf("synced by %s", hostname))
		}
		return nil
	case s.stamp < h.stamp && (s.stamp < 0 || s.agrees(h) && !(e.Check && !s.paused) || e.Again):
		// a joiner is being answered; one saying what this node holds was only sent at the
		// same moment as something newer, and missed nothing; one sent again crossed a newer
		// change, which its sender will hear. Not an older resume a member back from losing
		// the relay plays from: between two resumes the party stopped, and it played on.
		return nil
	case s.stamp < h.stamp:
		return c.answer(hostname, catchingUp)
	}
	return c.answer(hostname, "")
}

// adoption is a newer state this node has taken, and what mpv must do to get there.
type adoption struct {
	state
	from string
	// stop: mpv plays and must stop; seek: it goes to the second, as it isn't on it, or stops
	// from playing, a moment past it; still: it stays stopped, as the
	// state plays on another entry, or mpv waits on its cache; tell: mpv is told any of it;
	// was: mpv was stopped before; there: it was on the second already
	stop, moving, seek, still, tell, was, there bool
}

// commit takes a newer state in the step that weighed it: the stamp with the entry, the
// stop and the second it stands for, all marked before mpv is told. Taken apart, this node
// would hold the party's stamp on its own entry or second, a tie it could win; and whatever
// it says meanwhile already names where it is going.
func (c *Client) commit(s state, from string) adoption {
	a := adoption{state: s, moving: s.entry != c.heading(), from: c.entryName(c.heading()), was: c.paused}
	a.stop = (a.moving || s.paused) && !c.paused
	if a.moving {
		// another file opens at its start: the second and the seeks of this one are gone
		c.pos, c.ourSeeks, c.seekPending = 0, nil, false
	}
	a.there = c.on(s.at)
	a.seek = a.stop && !a.moving || !a.there && (a.moving || c.paused)
	a.still = !s.paused && (a.moving || c.waitingOnCache)
	paused := s.paused || a.still
	a.tell = a.seek || a.moving || paused != c.paused
	c.stamp, c.leaderID, c.resumedFrom, c.tookResume, c.leaderless, c.partyEntry = s.stamp, from, s.at, !s.paused, s.leaderless, s.entry
	if a.moving {
		c.headingTo = s.entry
	}
	if a.tell {
		c.paused = paused
	}
	return a
}

// adopt tells mpv to go where commit took this node, in one set: stopped, on the entry, on
// the second, then playing if the state is. One that plays on another entry stops the party
// instead: loading it, this node can't be in step. If this node moved while mpv took it, the
// person's change is newer and stands (moved), and the rest is left.
func (c *Client) adopt(a adoption, name, hostname string) (err error) {
	defer func() {
		if err == nil {
			return
		}
		c.release(a)
		// it holds the party's stamp where mpv isn't: asked again, the party says where.
		// Not once mpv is gone: this node is leaving.
		if !errors.Is(err, ErrClosed) {
			c.announce()
		}
	}()
	from, plays := c.stamp, !a.paused && !a.still
	show := func(text string) bool {
		c.showText(text)
		return c.moved(from)
	}
	if a.tell {
		var second *float64
		var entry *int
		if a.seek {
			second = &a.at
		}
		if a.moving {
			entry = &a.entry
		}
		if err = c.setState(c.paused, second, entry, a.was); err != nil {
			return err
		}
		if c.moved(from) {
			return nil
		}
	}
	if a.stop && show(fmt.Sprintf("paused by %s", hostname)) {
		return nil
	}
	if a.moving && show(fmt.Sprintf("from %s to %s, by %s", a.from, c.names[a.entry], hostname)) {
		return nil
	}
	if a.seek && !a.there && name != pause && show(fmt.Sprintf("synced by %s", hostname)) {
		return nil
	}
	switch {
	case a.paused:
		return nil
	case a.moving:
		return c.answer(hostname, "")
	case a.still:
		return c.refuseResume()
	case plays && a.tell:
		return c.showText(fmt.Sprintf("resumed by %s", hostname))
	}
	return nil
}

// release lets go of what commit marked once mpv refused the set, as it does while it quits or
// between files: headingTo would head for a move mpv never makes, and a seek mark would put
// this node on a second it never reached (target).
func (c *Client) release(a adoption) {
	if a.moving && c.headingTo == a.entry {
		c.headingTo = -1
	}
	if a.seek {
		c.ourSeeks = slices.DeleteFunc(c.ourSeeks, func(s ourSeek) bool { return s.at == a.at })
		c.seekPending = c.seekPending && len(c.ourSeeks) > 0
	}
}

// answer tells a node that isn't where the party is where that is: a joiner, a node that
// fell behind (both with a reason, and the party stops for them), or one whose change was
// made at the same moment as one that won (with none: that one stopped the party already).
// A node that was playing answers by stopping: its own pause says where, with a stamp
// newer than anything it could say before mpv has reported it. One already stopped says
// where it is. A joiner only says where it is: it stops nobody, and only another joiner
// can take it.
func (c *Client) answer(hostname, reason string) error {
	from, joiner, playing := c.stamp, c.stamp < 0, !c.paused
	if joiner {
		c.say(c.tag(position(timePos, c.held().at)))
		return nil
	}
	// why first, like a leave: the GUI's feed takes the pause that follows for this one, not
	// for a pause of the person's own
	if reason == outOfSync {
		// the member only showed it: it may be this node that is out of step
		c.showText(fmt.Sprintf("paused: %s with %s", outOfSync, hostname))
	} else if reason != "" {
		c.showText(fmt.Sprintf("paused for %s: %s", hostname, reason))
	}
	if c.moved(from) {
		return nil
	}
	// a stop nobody chose: no leader, the first seek leads
	if playing {
		c.pausedByNode = true
		return c.pauseReq(true)
	}
	c.leaderless = true
	// tagged now, not before the notice: what it says is where this node is when it goes out
	c.say(c.tag(position(timePos, c.held().at)))
	return nil
}

// moved is whether this node has a newer stamp than from: the rule for an action that
// waits on mpv more than once (adopt, answer, resume). While mpv answers one step, the
// person can pause, resume or seek, and that goes out as this node's own change, newer than
// what the action was doing; the steps left would undo it here alone, so the action stops.
func (c *Client) moved(from int) bool {
	return c.stamp != from
}

// chase follows a member that missed something this node sent, as its transport tells: a
// tie from it comes from what it last heard (take), until it sends something newer; and
// the transport sends it this node's state again, to it alone, as a pause marked again:
// one of the same stamp shows nothing, an older one is let go, as it may cross a newer
// change, and the resume that member already took from this node says it came late. A
// joiner has no state to send yet, and a state taken from that member, it has.
func (c *Client) chase(e IncomingMessage) error {
	switch {
	case e.Missed:
		c.missed[e.From] = true
		return nil
	case c.stamp < 0 || c.leaderID == e.From:
		// a joiner has no state yet; one taken from that member, it has
		c.reply(e.Reply, nil)
		return nil
	}
	again, _ := json.Marshal(Event{EventType: propertyChange, Name: pause, Data: holds(c.held().paused), Again: true})
	again = c.tag(again)
	if c.reply(e.Reply, again) {
		log.Printf("sent again, as %s missed something: %s\n", e.HostName, again)
	}
	return nil
}

// left pauses the party for a member that left (from), a stop with no leader; a leader that
// left leaves its stop with none. By its id, not its name, which another member may share.
func (c *Client) left(hostname, from string) error {
	playing, ending := !c.paused, c.ending()
	if c.led() && from != "" && c.leaderID == from {
		c.leaderless = true
	}
	if !playing || ending {
		return nil
	}
	c.showText(fmt.Sprintf("paused for %s: left", hostname))
	c.pausedByNode = true
	return c.pauseReq(true)
}

func (c *Client) entryName(i int) string {
	if i < 0 || i >= len(c.names) {
		return "another video"
	}
	return c.names[i]
}

// refuseResume is a peer's resume reaching a node still waiting on its cache, which never
// reaches mpv: the node stops the party again, with where and why.
func (c *Client) refuseResume() error {
	c.stopReason = "still buffering"
	again, _ := json.Marshal(Event{EventType: propertyChange, Name: pause, Data: "yes"})
	out := append([][]byte{again}, c.pauseExtras()...)
	// this node's own pause, newer than the resume it refuses, a stop nobody chose
	c.stamp, c.leaderID, c.leaderless = c.stamp+1, "", true
	for i, b := range out {
		out[i] = c.tag(b)
	}
	for _, b := range out {
		select {
		case c.outgoing <- b:
		case <-c.ctx.Done():
			return nil
		}
	}
	return c.showText("paused for everyone: still buffering")
}

// target is where this node is, or where a seek it sent will land: a member that just arrived
// is still on its way, and answering another joiner with the old spot would drag the party
// back to it.
func (c *Client) target() float64 {
	if c.seekPending && len(c.ourSeeks) > 0 {
		return c.ourSeeks[len(c.ourSeeks)-1].at
	}
	return c.pos
}
