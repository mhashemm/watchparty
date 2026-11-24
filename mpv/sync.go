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

// state is what the party shares, and what every message says its sender held when it sent
// it: a stamp (a Lamport clock), the entry, paused or playing, and the second it is paused
// at or resumed from.
type state struct {
	stamp  int
	entry  int
	paused bool
	at     float64
	// leaderless: a stop nobody leads (Event.Free); over ignores it
	leaderless bool
}

// over orders two states the same way on every node: the higher stamp wins. The same stamp
// means both were made at once, and the safer wins: the higher entry, then paused over
// playing, then the earlier second. 0 means the same state.
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

// agrees is whether s is the same state as h, ignoring their stamps.
func (s state) agrees(h state) bool {
	s.stamp = h.stamp
	return s.over(h) == 0
}

// held is the state this node holds and tags its messages with. Its entry is the one this
// node is heading to, never the file mpv happens to be on: the party's stamp on the entry it
// is leaving would be a tie it could win, dragging the party back. Its second there is 0 for
// a file still loading, unless a seek is queued for it. It is paused as soon as this node
// starts stopping the party (pauseParty), before mpv has stopped.
func (c *Client) held() state {
	at, entry := c.target(), c.heading()
	if c.onAStop() {
		// the party's entry and second, wherever the person has taken this node's screen
		at, entry = c.resumedFrom, c.partyEntry
	}
	paused := c.paused || c.stopReason != ""
	return state{stamp: c.stamp, entry: entry, paused: paused, at: at, leaderless: paused && c.leaderless}
}

// onAStop is whether this node is paused on a stop it took from a member (leaderID). Not a
// joiner, whose stamp is still unheard, nor a node taking a member's resume: a seek the
// person makes before mpv plays is theirs, newer, and stops it (moved).
func (c *Client) onAStop() bool {
	return c.paused && c.stopReason == "" && c.leaderID != "" && !c.tookResume && c.stamp >= 0
}

// led is whether this node follows the leader of a stop: the member whose pause or seek it
// took. Only the leader seeks for the party; a follower's seek stays on its own screen until
// the party resumes, from the resumer's screen. A leaderless stop has no leader: anyone's
// seek goes out, and the first one leads.
func (c *Client) led() bool {
	return c.onAStop() && !c.leaderless
}

// tag adds the state this node holds to a message, so the receiver can tell whether it is
// newer than its own. It must be done at the moment of sending: only then is it true.
func (c *Client) tag(b []byte) []byte {
	s := c.held()
	head := fmt.Appendf(nil, `{"stamp":%d,"paused":%t,"at":%.6f,"speed":%s,"speedStamp":%d,`, s.stamp, s.paused, s.at, seconds(c.speed), c.speedStamp)
	if s.leaderless {
		head = append(head, `"free":true,`...)
	}
	if len(c.names) > 0 {
		head = fmt.Appendf(head, `"entry":%d,`, s.entry)
	}
	return append(head, b[1:]...)
}

// stateOf reads the state a peer's pause, seek or move was sent from. Every node sends it
// whole (tag), so a message without it is refused rather than filled in from ours.
func (c *Client) stateOf(e Event) (state, error) {
	if e.Paused == nil || e.At == nil || e.Speed == nil || len(c.names) > 0 && e.Entry == nil {
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

// on is whether pos is the second this node is on, or heading to.
func (c *Client) on(pos float64) bool {
	return math.Abs(pos-c.target()) <= timePosTolerance
}

// heading is the entry this node is on, or heading to.
func (c *Client) heading() int {
	if c.headingTo >= 0 {
		return c.headingTo
	}
	return c.current
}

// hear keeps what the transport delivers while the first file opens. Each pause, seek or
// move carries the party's whole state, so only the newest by stamp is kept; other messages
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

// join runs once the first file loaded. A continued party first seeks where it stopped
// (resumeAt), so whatever the party says lands after it; not with --start, which mpv applies
// to every later entry too. If the seek fails, the file stays at 0. Then come the newest
// state heard while opening, the join itself, and the notices, all applied as one transport
// message, so anything arriving meanwhile waits.
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

// lost shows that the relay connection dropped. This node plays on, as the party most
// likely does too, and says where it is once back in the room (back).
func (c *Client) lost() error {
	c.offline = true
	return c.showText(lostRelay)
}

// back quietly tells the party this node's state after the relay connection came back: it
// may have missed something, or done something nobody heard. take stops the party only for
// a state other than its own. A joiner that was never answered joins again. Nothing is sent
// while this node plays out the end, or it would stop everyone just short of it.
func (c *Client) back() {
	c.offline = false
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

// apply handles one transport message. Only the messages below are acted on, never a peer's
// raw mpv command (SECURITY.md).
func (c *Client) apply(e IncomingMessage) {
	if e.Opened {
		c.stamp = max(c.stamp, 1)
		return
	}
	if e.Lost {
		if err := c.lost(); err != nil {
			log.Println("apply: lost the relay:", err)
		}
		return
	}
	if e.Back {
		c.back()
		return
	}
	if e.typed {
		var err error
		if e.reaction != "" {
			err = c.reactOut(e.reaction)
		} else {
			err = c.chatOut(e.say)
		}
		if err != nil {
			log.Println("apply: chat:", err)
		}
		return
	}
	if e.Missed || e.Resend {
		if err := c.chase(e); err != nil {
			log.Println("apply: a member missed something:", err)
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
			// only shown, never acted on; a peer's name can hold anything, so the log quotes it
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
		case chat:
			// on its own: the error line below would quote the message
			if err := c.chatIn(event, e); err != nil {
				log.Println("apply: chat:", err)
			}
			return
		case react:
			if err := c.reactIn(event, e); err != nil {
				log.Println("apply: react:", err)
			}
			return
		case playSpeed:
			playing := !c.paused
			c.pace(event, e.HostName)
			err = c.keepPace(e.HostName, playing)
		case joined:
			delete(c.missed, e.From)
			err = c.answer(e.HostName, "joining")
		}
	}
	if err != nil {
		log.Printf("apply: %s [event=%s] [host=%s]\n", err, e.Event, e.HostName)
	}
}

// shared is whether a message carries its sender's state: a pause, seek or move.
func shared(e Event) bool {
	return e.Name == pause || e.Name == timePos || e.Name == playlistPos
}

// take compares a peer's state with this node's. A newer one is adopted. An older one means
// its sender missed something, and the party stops for it to catch up. One made at the same
// moment as ours, which wins, is answered quietly, as its sender may never hear ours. A
// joiner's is none of these: its join is being answered.
func (c *Client) take(e Event, m IncomingMessage) error {
	s, err := c.stateOf(e)
	if err != nil {
		return err
	}
	playing := !c.paused
	c.pace(e, m.HostName)
	// only a speed message names whoever changed it (pace)
	return errors.Join(c.takeState(e, m, s), c.keepPace("", playing))
}

// pace takes the party's speed a message says, if it is newer than this node's: the higher
// speedStamp, of the same the slower. The speed has a clock of its own, apart from the stamp,
// so anyone changes it, and a change leads no stop. Only a speed message comes from whoever
// changed it; any other passes on what its sender heard, so it names nobody.
func (c *Client) pace(e Event, hostname string) {
	if e.Speed == nil || !(*e.Speed >= minSpeed && *e.Speed <= maxSpeed) {
		return
	}
	v := *e.Speed
	if e.SpeedStamp < c.speedStamp || e.SpeedStamp == c.speedStamp && v >= c.speed {
		return
	}
	c.speed, c.speedStamp = v, e.SpeedStamp
	text := fmt.Sprintf("speed %s×", strconv.FormatFloat(v, 'f', -1, 64))
	if e.Name == playSpeed {
		text += " by " + hostname
	}
	c.showText(text)
}

// keepPace sets mpv to the party's speed, unless a set has already. If mpv played before the
// message (playing) and plays on, the party stops: each node takes the speed at another
// moment, so playing on would leave them apart. hostname: who changed it, if the message says.
func (c *Client) keepPace(hostname string, playing bool) error {
	if c.speed == c.mpvSpeed {
		return nil
	}
	if playing && !c.paused {
		// worded as for out of sync: the GUI's feed shows the pause that follows as this one
		text := "paused: speed changed"
		if hostname != "" {
			text += " by " + hostname
		}
		c.showText(text)
		c.pausedByNode = true
		return c.pauseReq(true)
	}
	return c.pauseReq(c.paused)
}

// takeState is take for the state, the speed aside.
func (c *Client) takeState(e Event, m IncomingMessage, s state) error {
	hostname := m.HostName
	h := c.held()
	over, told := s.over(h), c.leaderID == m.From
	// apart: a resume that reaches this node while it plays means the two are out of step.
	// Newer: this node missed the pause in between. A tie from a member that missed
	// something we sent: it resumed from what it last heard. The state we took from that
	// member, sent again to us alone because it looked lost: it arrived late. A newer resume
	// from a member back on the relay: nobody heard where it was made. A tie from another
	// stop, more than twice the tolerance away. Never for a joiner: it syncs when its join
	// is answered.
	elsewhere := !e.Again && !e.Check && math.Abs(s.at-c.resumedFrom) > 2*timePosTolerance
	apart := !s.paused && h.stamp >= 0 && s.entry == h.entry &&
		(!h.paused && (over > 0 || over == 0 && (c.missed[m.From] || e.Again && told || elsewhere)) || e.Check && over > 0)
	if over > 0 {
		// it has heard or made something newer, so whatever it missed, this node now takes
		// its state or stops the party for it. Not on a tie: one it sent before can still be
		// on its way.
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
		// the pause this seek came with already took this node there
		if e.Name == timePos && told {
			c.showText(fmt.Sprintf("synced by %s", hostname))
		}
		return nil
	case s.stamp < h.stamp && (s.stamp < 0 || s.agrees(h) && !(e.Check && !s.paused) || e.Again):
		// older but harmless: a joiner being answered; a state equal to ours, only sent at
		// the same moment as something newer; or a resend that crossed a newer change, which
		// its sender will hear. Not an older resume from a member back on the relay: the
		// party stopped between the two resumes, and that member played on.
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
	// stop: mpv is playing and must stop. moving: the state is on another entry. seek: mpv
	// must go to the second, as it isn't there, or stops from playing a moment past it.
	// still: mpv stays paused, as the state plays on another entry or mpv waits on its
	// cache. tell: mpv must be told any of it. was: mpv was paused before. there: mpv was
	// on the second already.
	stop, moving, seek, still, tell, was, there bool
}

// commit takes a newer state in the same step that compared it: the stamp together with
// its entry, stop and second, all recorded before mpv is told. Taken in parts, this node
// would hold the party's stamp on its own entry or second, a tie it could win. Recorded
// first, anything it says meanwhile already names where it is going.
func (c *Client) commit(s state, from string) adoption {
	a := adoption{state: s, moving: s.entry != c.heading(), from: c.entryName(c.heading()), was: c.paused}
	a.stop = (a.moving || s.paused) && !c.paused
	if a.moving {
		// the other file opens at its start, so this file's second and seeks are gone
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

// adopt sends mpv where commit took this node, in one set: paused, on the entry, on the
// second, then playing if the state plays. A state playing on another entry stops the party
// instead: while loading, this node can't be in step. If the person changed something while
// mpv applied it, their change is newer and stands (moved), and the rest is skipped.
func (c *Client) adopt(a adoption, name, hostname string) (err error) {
	defer func() {
		if err == nil {
			return
		}
		c.release(a)
		// this node holds the party's stamp but mpv isn't there: joining again makes the
		// party say where it is. Not once mpv is gone: this node is leaving.
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

// release undoes what commit recorded when mpv refused the set, as it does while quitting
// or between files: headingTo would wait for a move mpv never makes, and a seek mark would
// put this node on a second it never reached (target).
func (c *Client) release(a adoption) {
	if a.moving && c.headingTo == a.entry {
		c.headingTo = -1
	}
	if a.seek {
		c.ourSeeks = slices.DeleteFunc(c.ourSeeks, func(s ourSeek) bool { return s.at == a.at })
		c.seekPending = c.seekPending && len(c.ourSeeks) > 0
	}
}

// answer tells a member where the party is: a joiner or a member that fell behind (with a
// reason; the party stops for them), or one whose change lost a tie (no reason; the winner
// already stopped the party). A playing node answers by pausing: its own pause says where,
// with a stamp newer than anything it could send before mpv reports it. A paused node sends
// its second. A joiner only sends its second: it stops nobody, and only another joiner can
// take it.
func (c *Client) answer(hostname, reason string) error {
	from, joiner, playing := c.stamp, c.stamp < 0, !c.paused
	if joiner {
		c.say(c.tag(position(timePos, c.held().at)))
		return nil
	}
	// the reason goes first, as for a leave, so the GUI's feed doesn't show the pause that
	// follows as the person's own
	if reason == outOfSync {
		// the member only revealed it: this node may be the one out of step
		c.showText(fmt.Sprintf("paused: %s with %s", outOfSync, hostname))
	} else if reason != "" {
		c.showText(fmt.Sprintf("paused for %s: %s", hostname, reason))
	}
	if c.moved(from) {
		return nil
	}
	// nobody chose this stop: it is leaderless, and the first seek leads
	if playing {
		c.pausedByNode = true
		return c.pauseReq(true)
	}
	c.leaderless = true
	// tagged now, after the notice, so it says where this node is when it goes out
	c.say(c.tag(position(timePos, c.held().at)))
	return nil
}

// moved is whether this node's stamp changed since from. An action that waits on mpv more
// than once (adopt, answer) checks it between steps: meanwhile the person may have paused,
// resumed or seeked, which went out as a newer change. The remaining steps would undo it on
// this node alone, so the action stops.
func (c *Client) moved(from int) bool {
	return c.stamp != from
}

// chase handles the transport saying a member missed something this node sent. Missed: a
// tie from that member comes from what it last heard (take), until it sends something
// newer. Resend: this node's state goes to that member alone, as a pause marked Again. With
// the same stamp it shows nothing; an older one is ignored, as it may cross a newer change;
// and a resume the member already took from us tells it the first copy came late. A joiner
// has no state to send yet, and a member we took our state from already has it.
func (c *Client) chase(e IncomingMessage) error {
	switch {
	case e.Missed:
		c.missed[e.From] = true
		return nil
	case c.stamp < 0 || c.leaderID == e.From:
		// a joiner has no state yet; a state taken from that member, it already has
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

// left pauses the party, leaderless, for a member that left (from). If it led the stop, the
// stop becomes leaderless. It goes by the member's id, not its name, which others may share.
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

// refuseResume handles a peer's resume while this node still waits on its cache: mpv stays
// paused, and the node stops the party again, saying where and why.
func (c *Client) refuseResume() error {
	c.stopReason = "still buffering"
	again, _ := json.Marshal(Event{EventType: propertyChange, Name: pause, Data: "yes"})
	out := append([][]byte{again}, c.pauseExtras()...)
	// our own pause, newer than the resume it refuses, and leaderless
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

// target is this node's second, or where its last seek will land: answering a joiner with
// the old second while a seek is on its way would drag the party back to it.
func (c *Client) target() float64 {
	if c.seekPending && len(c.ourSeeks) > 0 {
		return c.ourSeeks[len(c.ourSeeks)-1].at
	}
	return c.pos
}
