package mpv

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	pause          = "pause"
	timePos        = "time-pos"
	pausedForCache = "paused-for-cache"
	playlistPos    = "playlist-pos"
	filename       = "filename"
	duration       = "duration"
	propertyChange = "property-change"
	seek           = "seek"
	setProperty    = "set_property"
	showText       = "show-text"
	pauseReason    = "pause-reason"
	joined         = "joined"
	clientMessage  = "client-message"

	// scriptName is what mpv names the script, from ScriptFile; another script with that
	// file name takes it, and this one gets a number after it (ORDER.md)
	scriptName = "watchparty"

	lostRelay = "lost the relay, reconnecting"
	// unheard is the stamp of a node that hasn't heard the party yet: so far below any the
	// party can hold that nothing a joiner does before it is answered wins over the party,
	// while joiners still order what they do among themselves.
	unheard = -1 << 30
	// catchingUp is why the party stops for a node whose pause or seek was older than
	// what the party holds: it missed something, and is told where the party is.
	catchingUp = "catching up"
	// outOfSync is why the party stops for a resume reaching a node that plays: one of the
	// two missed a pause or a resume, and plays from another second.
	outOfSync = "out of sync"

	showTextDurationSeconds = 5
	timePosTolerance        = 0.1
	endGraceSeconds         = 5
	maxSeeks                = 16
)

// How long the node waits on mpv before it gives up (errTimeout): replyLimit for a reply or a
// go's ack, moveLimit for a move's go, acked once its file starts, which mpv gives up on after
// its --network-timeout (60s; 81s seen, ORDER.md).
const (
	replyLimit = 5 * time.Second
	moveLimit  = 3 * time.Minute
)

// overruled is shown when a go undid the person's key, pressed as the party changed.
const overruled = "your key came as the party changed: press it again"

// The notices both transports show, around a member's name. apply reads HasLeft to pause
// the party, and gui/store.go reads the other two; e2e spells them again.
const (
	HasJoined   = " has joined"
	HasLeft     = " has left"
	ConnectedTo = "connected to "
)

var (
	errNoChange  = errors.New("no change")
	errDoNotSend = errors.New("do not send")
	errTimeout   = errors.New("mpv did not answer in time")

	ErrClosed = errors.New("mpv closed the IPC connection")
	// ErrNoScript: mpv didn't load Script, as it was built without Lua
	ErrNoScript = errors.New("this mpv can't run the sync script: install an mpv built with Lua")
)

// Script is the node's way into mpv, which party.Run hands mpv as ScriptFile with --script.
//
//go:embed watchparty.lua
var Script []byte

// ScriptFile is the file name Script is written as: mpv names a script after it.
const ScriptFile = scriptName + ".lua"

type IncomingMessage struct {
	// HostName is who sent it, as people know them; From tells members apart, which their
	// names may not: the mesh address, or the relay client's id. Notices have no From.
	HostName string
	From     string
	Event    []byte
	// Opened, Lost and Back are set only by this node's own transport, never from the wire.
	// Opened: this node opened the room with the key, or started the mesh, so its position
	// is the party's.
	Opened bool
	// Lost: its connection to the relay dropped. Back: it is in the room again, and
	// didn't open it; whatever was sent meanwhile is gone.
	Lost, Back bool
	// Missed: something this node sent failed to reach From, maybe the latest state it
	// heard. Resend: hand Reply this node's state, which the transport sends to From alone,
	// or nil for none.
	Missed, Resend bool
	Reply          chan<- []byte
}

type connection struct {
	conn    net.Conn
	scanner *bufio.Scanner
	closed  chan struct{}
}

// do sends mpv one command and waits for its answer on the loop, the node's one goroutine:
// meanwhile it takes the inputs that come, as loop does, so mpv's lines are handled and a
// transport message waits its turn (handle). Any other request's answer is kept for the do()
// waiting on it. Never from another goroutine.
func (c *Client) do(args ...any) error {
	_, err := c.ask(args...)
	return err
}

// ask is do, and gives the answer's data: get_property_string's value.
func (c *Client) ask(args ...any) (string, error) {
	if c.gone {
		return "", ErrClosed
	}
	req := Request{
		Command:   args,
		RequestId: rand.Int63(),
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	id := req.RequestId
	if c.out == nil {
		c.out = newMailbox[request]()
		go c.writer(c.out)
	}
	c.out.put(request{id: id, body: body})
	return c.await(id, replyLimit)
}

// await waits on the loop for id's answer, taking the inputs that come meanwhile: mpv's reply
// to a request, or the script's ack of a go (goTo), for at most limit.
func (c *Client) await(id int64, limit time.Duration) (string, error) {
	c.waiting[id] = true
	defer delete(c.waiting, id)
	expire := time.AfterFunc(limit, func() { c.post(input{expired: id}) })
	defer expire.Stop()
	for {
		if r, ok := c.replies[id]; ok {
			delete(c.replies, id)
			if r.unsent != nil {
				return "", fmt.Errorf("%w: %v", ErrClosed, r.unsent)
			}
			if r.expired {
				log.Printf("do: response timeout: [requestId=%d]\n", id)
				return "", errTimeout
			}
			log.Printf("do: received response: [requestId=%d] [error=%s]\n", id, r.Error)
			if r.Error != "success" {
				return "", errors.New(r.Error)
			}
			return r.data, nil
		}
		if c.gone {
			return "", ErrClosed
		}
		in, ok := c.next()
		if !ok {
			return "", ErrClosed
		}
		c.handle(in)
	}
}

// mailbox is the loop's inbox: what it is given, in the order given. Giving never waits,
// so mpv's lines are read while the loop writes to mpv: a reader waiting on the loop while
// the loop waits on mpv, and mpv on the reader, would wait forever.
type mailbox[T any] struct {
	mu    sync.Mutex
	items []T
	ready chan struct{}
}

func newMailbox[T any]() *mailbox[T] {
	return &mailbox[T]{ready: make(chan struct{}, 1)}
}

func (m *mailbox[T]) put(v T) {
	m.mu.Lock()
	m.items = append(m.items, v)
	m.mu.Unlock()
	select {
	case m.ready <- struct{}{}:
	default:
	}
}

func (m *mailbox[T]) poll() (T, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var v T
	if len(m.items) == 0 {
		return v, false
	}
	v = m.items[0]
	m.items = m.items[1:]
	return v, true
}

// request is one command on its way to mpv's socket.
type request struct {
	id   int64
	body []byte
}

// writer writes requests to mpv's socket in order, so the loop never waits on mpv reading
// it: a stalled mpv would make every peer wait. A write that fails comes back as an input.
func (c *Client) writer(out *mailbox[request]) {
	for {
		r, ok := out.poll()
		if !ok {
			select {
			case <-out.ready:
				continue
			case <-c.ctx.Done():
				return
			}
		}
		if _, err := c.conn.conn.Write(append(r.body, '\n')); err != nil {
			c.post(input{unsent: r.id, err: err})
			continue
		}
		log.Printf("do: sent request: %s\n", r.body)
	}
}

// outcome is mpv's answer to one request, or none in 5s.
type outcome struct {
	Response
	data    string
	expired bool
	unsent  error
}

// input is one thing that happened to this node: a line mpv sent, mpv's socket closing, a
// request's 5s running out or its write failing, or a transport message.
type input struct {
	line    []byte
	closed  bool
	expired int64
	unsent  int64
	err     error
	msg     *IncomingMessage
}

type Event struct {
	EventType string `json:"event,omitempty"`
	Name      string `json:"name"`
	Data      string `json:"data"`
	// Stamp, Paused, At and Entry are the state its sender was in when it sent this (tag)
	Stamp  int      `json:"stamp,omitempty"`
	Paused *bool    `json:"paused,omitempty"`
	At     *float64 `json:"at,omitempty"`
	Entry  *int     `json:"entry,omitempty"`
	// Again: the state its sender holds, sent again to a member the mesh failed to reach.
	// Check: the state its sender holds, back in the room after losing the relay.
	Again bool `json:"again,omitempty"`
	Check bool `json:"check,omitempty"`
	// Free: a stop nobody chose (answering a join, catching up, a leave): it has no leader,
	// and the first seek leads
	Free bool `json:"free,omitempty"`
	// Args: what a script said, in a client-message
	Args []string `json:"args,omitempty"`
	// fromScript: a report made from what the script said (scriptSaid), never sent as is.
	// stoppedAt: for a move the ack found, where mpv stopped on the new file
	fromScript bool
	stoppedAt  *float64
	Response
}

func (e Event) paused() bool {
	return e.Data == "yes"
}

type Request struct {
	Command   []any `json:"command"`
	RequestId int64 `json:"request_id"`
}
type Response struct {
	RequestId int64  `json:"request_id"`
	Error     string `json:"error"`
}

type Client struct {
	ctx      context.Context
	ready    chan struct{}
	outgoing chan<- []byte
	conn     *connection

	// the loop: the only goroutine that reads or writes what follows. It takes the
	// transport's messages from incoming, everything else from inbox, in the order it came;
	// a transport message waits in queue while another's change is still being applied
	// (busy); later is what an input started that sends mpv a command, run once it is
	// handled; replies are answers kept for the do() waiting on them
	incoming <-chan IncomingMessage
	inbox    *mailbox[input]
	out      *mailbox[request]
	queue    []IncomingMessage
	busy     int
	later    []func()
	replies  map[int64]outcome
	waiting  map[int64]bool
	// gone: mpv's socket closed
	gone bool
	// opening: the first file hasn't loaded yet. early is the newest state heard meanwhile,
	// by stamp, and notices the rest, in order
	opening bool
	early   *IncomingMessage
	best    state
	notices []IncomingMessage
	// snap is what Position and Finished read from another goroutine. reported: the last
	// second mpv reported, where it is as it closes, as the script's last messages are lost
	// then (ORDER.md)
	snap     atomic.Pointer[snapshot]
	reported float64
	// opened is New's one answer: nil once the first file loaded, mpv took every observe
	// (observed) and the script said its name (script), or why it never will
	opened   chan error
	observed bool
	script   string

	// what mpv last reported
	paused   bool
	pos      float64
	duration float64
	stalled  bool
	seen     map[string]bool

	// marked: the seeks this node sent that mpv hasn't landed, oldest first (wentTo).
	// flying: the go the script hasn't acked (goTo), one at a time. read: the number of the
	// last script message taken, which a go says, so the script can tell what it undoes
	marked []ourSeek
	flying *flight
	read   string
	// landing: mpv hasn't landed the last seek this node marked, so it is not yet on the
	// second it is heading to (target). seeking: the person's seek went while playing
	landing bool
	seeking bool
	reason  string

	// stamp orders what the party shares (a Lamport clock): the highest this node has
	// made or followed. A joiner starts at unheard.
	stamp int
	// since: the second the resume this node plays went from, its own or one it took
	since float64
	// missed: members (From) that something this node sent failed to reach
	missed map[string]bool

	resumeAt float64
	// by: the member (From) whose state this node last adopted; leader: its name. told: the
	// person here has been told who holds the seek, for that state
	by, leader string
	told       bool
	// toPlay: the state this node last took from a member plays: it is no follower of a pause.
	// free: the stop this node holds has no leader, as nobody chose it; asked: this node
	// paused mpv itself, for a reason other than the person's seek (answer, left, pauseParty)
	toPlay, free, asked bool
	// partyEntry: the entry of the stop this node took, wherever the person has moved it since.
	// ranOut: mpv moved on from the end of the entry it played, by itself
	partyEntry int
	ranOut     bool

	// playlist
	names []string
	// current: the entry mpv opened last, as the script says it (start); toward: the one this
	// node's move is heading to, until the script says it opened it, -1 for none. auto: the last
	// start was mpv's own, after a file's end or error
	current int
	toward  int
	auto    bool
}

// ourSeek is a seek this node sent, by the id of its go, which the script says with it
// (wentTo), on the entry it is for.
type ourSeek struct {
	id    int64
	at    float64
	entry int
}

// flight is a go the script hasn't acked, and what the ack is weighed against (landed): the
// stamp and the second this node held as it sent it; seek: it seeks, so the ack's second is
// from before mpv lands; still: mpv was stopped, so the ack's second is where it was.
type flight struct {
	id          int64
	at          float64
	seek, still bool
}

// handleEvent returns what goes out for one line from mpv: the line itself when it is
// this node's own change, and whatever must follow it. Each is tagged in the step that
// decided it, so its stamp is the one that change was given.
func (c *Client) handleEvent(event Event, line []byte) [][]byte {
	// mpv's own order, to read after a run; not its position while playing, one per frame
	if event.Name != timePos || c.paused {
		log.Printf("Watch: mpv said: %s\n", line)
	}
	out, err := c.react(event)
	if err == nil {
		out = append([][]byte{line}, out...)
	}
	if len(out) > 0 {
		// this node's own change, or what it adds to a peer's: the next stamp. Only the
		// person's own pause, seek or move leads; any other stop is free, and the first seek
		// leads.
		lead := event.Name == timePos || event.Name == pause && event.paused() && !c.asked ||
			event.Name == playlistPos && !c.ranOut
		c.stamp, c.by, c.free = c.stamp+1, "", !lead
	}
	if event.Name == pause {
		c.asked = false
	}
	for i, b := range out {
		out[i] = c.tag(b)
	}
	return out
}

func (c *Client) react(event Event) ([][]byte, error) {
	switch event.EventType {
	case propertyChange:
		if event.Data != "" {
			return c.propertyChanged(event)
		}
	}
	return nil, errDoNotSend
}

// scriptSaid is a message from the script (watchparty.lua): its name, once, or what mpv holds when
// it was sent, as reports to read as mpv's: the pause it holds, and the person's seek. While a
// go is on its way, what the script says is from before it, which the go overwrites, or from
// the go itself: none is the person's, and the ack says where mpv ended (landed).
func (c *Client) scriptSaid(args []string) []Event {
	if len(args) < 5 || args[0] != scriptName {
		return nil
	}
	if len(args) > 5 {
		c.read = args[5]
	}
	kind, said := args[1], args[2]
	if kind == "hello" {
		if c.script != "" {
			return nil
		}
		c.script = said
		// where mpv starts: every later entry it opens, the script says (start)
		if e, err := strconv.Atoi(entryOf(args)); err == nil && e >= 0 {
			c.current = e
		}
		c.seen[playlistPos] = true
		c.opens()
		// what mpv holds: the first pause this node hears, which only marks where it starts
		return []Event{{EventType: propertyChange, Name: pause, Data: args[3], fromScript: true}}
	}
	id, _ := strconv.ParseInt(said, 10, 64)
	paused, pos := args[3], args[4]
	at, err := strconv.ParseFloat(pos, 64)
	known := err == nil
	switch kind {
	case "refused":
		c.answered(id, outcome{Response: Response{Error: "mpv refused it"}})
		if c.ground(id) {
			c.paused = paused == "yes"
			// a key that moved mpv while the go was on its way, as at an ack
			return c.wentElsewhere(paused, at, known, entryOf(args))
		}
	case "ack":
		c.answered(id, outcome{Response: Response{Error: "success"}})
		return c.landed(id, paused, at, known, len(args) > 6 && args[6] == "over", entryOf(args))
	case "start":
		return c.started(said, entryOf(args))
	case "pause":
		if c.flying != nil {
			return nil
		}
		var out []Event
		// stopped, and still: a frame-step, which moves mpv without a seek (ORDER.md)
		if known && paused == "yes" && c.paused && len(c.marked) == 0 && !c.on(at) {
			out = c.wentTo(0, at)
		}
		if known && paused == "yes" {
			c.pos = at
		}
		return append([]Event{{EventType: propertyChange, Name: pause, Data: paused, fromScript: true}}, out...)
	case seek:
		if known {
			return c.wentTo(id, at)
		}
	}
	return nil
}

// entryOf is the entry a script message says mpv is on, or opened (start); "" if it says none.
func entryOf(args []string) string {
	if len(args) > 7 {
		return args[7]
	}
	return ""
}

// started is the script saying whose file mpv opened (watchparty.lua): this node's move, by its
// go's id; mpv's own (auto), after a file's end or error; or the person's. While a go is on its
// way, a start that isn't its own is weighed at its ack, which says the entry mpv ended on.
func (c *Client) started(label, entry string) []Event {
	n, err := strconv.Atoi(entry)
	if err != nil || n < 0 {
		return nil
	}
	c.auto = label == "auto"
	if _, err := strconv.ParseInt(label, 10, 64); err == nil {
		// this node's own move
		if c.toward == n {
			c.toward = -1
		}
		if n != c.current {
			c.current = n
			c.clear(n)
		}
		return nil
	}
	if c.flying != nil {
		return nil
	}
	return []Event{{EventType: propertyChange, Name: playlistPos, Data: entry, fromScript: true}}
}

// answered gives the script's answer to the goTo waiting on it.
func (c *Client) answered(id int64, o outcome) {
	if c.waiting[id] {
		c.replies[id] = o
	}
}

// ground forgets the go on its way once the script answered it or it failed, and whether id
// was that go.
func (c *Client) ground(id int64) bool {
	if c.flying == nil || c.flying.id != id {
		return false
	}
	c.flying = nil
	return true
}

// landed is the script's ack of the go on its way: what mpv holds once it has all of it. What
// differs from what this node holds is the person's, made while the go was on its way and not
// overwritten by it, and goes out as their report: nothing else of this node's goes out while
// a go is on its way, so the ack is always weighed against the state it was sent from. over: the go undid a key of the
// person's this node never read; made as the party changed, it would lose to that change
// anyway (stopped over playing), so it stays undone, and the person is told.
func (c *Client) landed(id int64, paused string, at float64, known, over bool, entry string) []Event {
	f := c.flying
	if !c.ground(id) {
		return nil
	}
	// it goes out first, as a move does
	out := c.wentElsewhere(paused, at, known, entry)
	if paused != holds(c.paused) {
		out = append(out, Event{EventType: propertyChange, Name: pause, Data: paused, fromScript: true})
	}
	// mpv stood where this node held it, with no seek of its own: anywhere else, the person's
	// seek ran just before the go, or folded into a seek of this node's landing meanwhile
	if known && f.still && !f.seek && len(c.marked) == 0 && math.Abs(at-f.at) > timePosTolerance {
		c.seeking = paused == "no"
		out = append(out, Event{EventType: propertyChange, Name: timePos, Data: strconv.FormatFloat(at, 'f', 6, 64), fromScript: true})
	} else if known && paused == "yes" && !f.seek {
		c.pos = at
	}
	if over && len(out) == 0 {
		c.later = append(c.later, func() { c.showText(overruled) })
	}
	return out
}

// wentElsewhere is the entry the script says mpv ended a go on, if it isn't the one this node
// heads to: the person's move, or mpv's own, while the go was on its way, from where mpv stopped.
func (c *Client) wentElsewhere(paused string, at float64, known bool, entry string) []Event {
	n, err := strconv.Atoi(entry)
	if err != nil || n < 0 || n == c.heading() {
		return nil
	}
	c.toward = -1
	move := Event{EventType: propertyChange, Name: playlistPos, Data: entry, fromScript: true}
	if known && paused == "yes" {
		// it may have played a moment there before the go stopped it
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

// propertyChanged is what a property-change with data means for the party: what to
// broadcast after it, or instead of it, and nil only if the event itself goes out.
func (c *Client) propertyChanged(event Event) ([][]byte, error) {
	extra, moved := c.record(event)
	first := !c.seen[event.Name]
	c.seen[event.Name] = true
	if first && event.Name == timePos {
		close(c.ready)
	}
	var err error
	switch event.Name {
	case pause:
		extra, err = c.pauseChanged(event.paused(), extra)
	case timePos:
		if !event.fromScript {
			// how far mpv has played; where it stops and lands, the script says
			return nil, errDoNotSend
		}
		err = c.positionChanged()
	case pausedForCache:
		c.stalled = event.paused()
		if c.stalled && !c.paused {
			c.pauseParty("buffering", false)
		}
		return nil, errDoNotSend
	case playlistPos:
		if !event.fromScript {
			return nil, errDoNotSend
		}
		err = c.entryChanged(moved)
	default:
		return nil, errDoNotSend
	}
	if err == nil && first {
		return nil, errDoNotSend
	}
	return extra, err
}

// record keeps what mpv reported, echo or not. It returns what a pause carries, and
// whether the playlist moved to another entry after the first file was open.
func (c *Client) record(event Event) (extra [][]byte, moved bool) {
	switch event.Name {
	case timePos:
		// stopped, mpv is where the script last said: a playing second it reported before
		// the stop can come after (MPV.md, [R6c]); and while a go is on its way, mpv may not
		// play yet, wherever the person took it
		if pos, err := strconv.ParseFloat(event.Data, 64); err == nil {
			c.reported = pos
			if event.fromScript || !c.paused && c.flying == nil {
				c.pos = pos
			}
		}
	case duration:
		if d, err := strconv.ParseFloat(event.Data, 64); err == nil {
			c.duration = d
		}
	case pause:
		if event.paused() {
			extra = c.pauseExtras()
		}
	case filename:
		name, _ := json.Marshal(event.Data)
		log.Printf("now playing: %s\n", name)
	case playlistPos:
		if n, err := strconv.Atoi(event.Data); err == nil && n >= 0 && n != c.current && event.fromScript {
			c.ranOut = c.auto
			c.current = n
			moved = c.seen[timePos]
			c.clear(n)
			if event.stoppedAt != nil {
				c.pos = *event.stoppedAt
			}
		}
	}
	return extra, moved
}

// clear forgets the second this node was on and the seeks it sent for other files, as mpv
// opens entry: the old second would go out with a pause, and a seek queued for the old file
// never lands.
func (c *Client) clear(entry int) {
	c.pos, c.seeking = 0, false
	c.marked = slices.DeleteFunc(c.marked, func(s ourSeek) bool { return s.entry != entry })
	c.landing = c.landing && len(c.marked) > 0
}

// pauseChanged is the user's own pause or resume.
func (c *Client) pauseChanged(paused bool, extra [][]byte) ([][]byte, error) {
	if c.paused == paused {
		return extra, errNoChange
	}
	if !paused && c.stalled {
		c.pauseParty("still buffering", false)
		return nil, errDoNotSend
	}
	if !paused && c.led() && c.heading() != c.partyEntry {
		// a follower resuming on the file the person moved to: as a move while playing, the
		// party stops there, on this node's second, and it leads. Everyone must load the
		// file first, and they would play that far apart.
		c.paused = false
		c.pauseParty("video changed", true)
		return nil, errDoNotSend
	}
	c.paused = paused
	if !paused {
		c.seeking, c.since = false, c.target()
	}
	return extra, nil
}

// positionChanged is the person's seek, as the script said where it went (wentTo): stopped, it
// goes out; playing, it pauses the party.
func (c *Client) positionChanged() error {
	if c.paused && c.led() {
		// the step taking the leader's state goes on: its seek overrides this one
		c.keepOnScreen()
		return errDoNotSend
	}
	// the person's seek goes out, or stops the party, newer: wherever this node was being
	// sent, this is where it is now. mpv lands nothing of a seek queued before it, and one
	// sent after it lands as the person's, newer, which is where mpv then is.
	c.marked, c.landing = nil, false
	if c.paused {
		return nil
	}
	if c.seeking {
		c.seeking = false
		c.pauseParty("seek", true)
	}
	return errDoNotSend
}

// keepOnScreen keeps a follower's seek or move on this screen until the party resumes, and
// tells the person, once for each state it takes, so a held key doesn't flood the feed.
func (c *Client) keepOnScreen() {
	if !c.told {
		c.told = true
		text := fmt.Sprintf("only %s can seek until resumed", c.leader)
		c.later = append(c.later, func() { c.showText(text) })
	}
}

// entryChanged is this node moving to another entry by itself: it goes out, and pauses
// the party if it was playing. A follower's stays on its screen, as its seek does: a move is a
// seek far enough to land in another file. mpv moving on from the end is nobody's choice.
func (c *Client) entryChanged(moved bool) error {
	if !moved {
		return errDoNotSend
	}
	if c.led() {
		c.keepOnScreen()
		return errDoNotSend
	}
	if !c.paused {
		c.pauseParty("video changed", !c.ranOut)
	}
	return nil
}

// state is what the party shares, and what every message says its sender holds: a stamp
// (a Lamport clock), the entry, stopped or playing, and where it is stopped or resumes from.
type state struct {
	stamp  int
	entry  int
	paused bool
	at     float64
	// free: a stop with no leader (Event.Free); not weighed by over
	free bool
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
		at, entry = c.since, c.partyEntry
	}
	paused := c.paused || c.reason != ""
	return state{stamp: c.stamp, entry: entry, paused: paused, at: at, free: paused && c.free}
}

// onAStop is a node stopped on a member's state (by). Not a joiner, whose stamp is still
// unheard, nor a node taking a member's resume: a seek the person makes before mpv plays is
// theirs, newer, and stops it (moved).
func (c *Client) onAStop() bool {
	return c.paused && c.reason == "" && c.by != "" && !c.toPlay && c.stamp >= 0
}

// led is a node following the leader of a stop: the member whose pause or seek it took. Only
// the leader seeks for the party; a seek the person makes here stays on their screen until
// the party resumes, from the resumer's screen. A stop nobody chose has no leader (free):
// anyone's seek goes out, and the first leads.
func (c *Client) led() bool {
	return c.onAStop() && !c.free
}

// tag says what state a message was sent from, so whoever gets it knows whether it is
// newer than theirs. Only this node knows, at the moment it sends.
func (c *Client) tag(b []byte) []byte {
	s := c.held()
	head := fmt.Appendf(nil, `{"stamp":%d,"paused":%t,"at":%.6f,`, s.stamp, s.paused, s.at)
	if s.free {
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
	s := state{stamp: e.Stamp, paused: *e.Paused, at: *e.At, free: e.Free && *e.Paused}
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
	if c.toward >= 0 {
		return c.toward
	}
	return c.current
}

func (c *Client) pauseExtras() [][]byte {
	if c.reason == "" {
		return nil
	}
	reason, _ := json.Marshal(Event{Name: pauseReason, Data: c.reason})
	c.reason = ""
	return [][]byte{position(timePos, c.pos), reason}
}

func position(name string, pos float64) []byte {
	b, _ := json.Marshal(Event{EventType: propertyChange, Name: name, Data: strconv.FormatFloat(pos, 'f', 6, 64)})
	return b
}

// pauseParty stops the party for why this node is out of step: led by it after the person's
// seek or move (lead), by nobody otherwise.
func (c *Client) pauseParty(reason string, lead bool) {
	c.reason = reason
	c.later = append(c.later, func() {
		c.asked = !lead
		err := c.pauseReq(true)
		if err != nil {
			log.Println("pauseParty:", err)
			c.reason = ""
			return
		}
		c.showText("paused for everyone: " + reason)
	})
}

// watch runs the loop until mpv's socket closes: a reader hands it mpv's lines in order,
// and it handles them and everything else that comes, one at a time.
func (c *Client) watch() error {
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := c.conn.scanner
		for scanner.Scan() {
			// the scanner reuses its buffer on the next Scan
			if !c.post(input{line: bytes.Clone(scanner.Bytes())}) {
				return
			}
		}
		err = scanner.Err()
		c.post(input{closed: true})
	}()
	c.runLater()
	for !c.gone {
		in, ok := c.next()
		if !ok {
			return c.ctx.Err()
		}
		c.handle(in)
	}
	<-done
	return err
}

// serve keeps taking inputs after mpv's socket closed, until the party ends: a peer still
// never waits on this node.
func (c *Client) serve() {
	for {
		in, ok := c.next()
		if !ok {
			return
		}
		c.handle(in)
	}
}

func (c *Client) post(in input) bool {
	if c.ctx.Err() != nil {
		return false
	}
	c.inbox.put(in)
	return true
}

// next is the loop's next input. What the transport has handed over comes first: its
// sender is done with it, so it came before whatever mpv says from here, and a message sent
// before the file loaded is heard while it loads. Read on the loop itself, never by a
// goroutine that would hold it a moment before passing it on.
func (c *Client) next() (input, bool) {
	for {
		select {
		case m := <-c.incoming:
			return input{msg: &m}, true
		default:
		}
		if in, ok := c.inbox.poll(); ok {
			return in, true
		}
		select {
		case m := <-c.incoming:
			return input{msg: &m}, true
		case <-c.inbox.ready:
		case <-c.ctx.Done():
			return input{}, false
		}
	}
}

// handle is one input, with whatever it starts. The only rule for order: a transport
// message is taken only when no other's change is still being applied, so each is applied
// to its end, as mpv answers it, before the next; mpv's lines are handled meanwhile.
func (c *Client) handle(in input) {
	switch {
	case in.closed:
		c.gone = true
		if c.opening {
			c.open(ErrClosed)
		}
	case in.expired != 0:
		if c.waiting[in.expired] {
			c.replies[in.expired] = outcome{expired: true}
		}
	case in.unsent != 0:
		if c.waiting[in.unsent] {
			c.replies[in.unsent] = outcome{unsent: in.err}
		}
	case in.msg != nil:
		if c.opening {
			c.hear(*in.msg)
		} else {
			c.queue = append(c.queue, *in.msg)
		}
	default:
		c.mpvSaid(in.line)
	}
	c.runLater()
	// the party's state is applied through the script, so it waits for its name too
	if c.opening && c.seen[timePos] && c.script != "" {
		c.join()
	}
	for c.busy == 0 && len(c.queue) > 0 {
		c.busy++
		m := c.queue[0]
		c.queue = c.queue[1:]
		c.apply(m)
		c.busy--
		c.runLater()
	}
	c.snapshot()
}

func (c *Client) runLater() {
	for len(c.later) > 0 && c.flying == nil {
		f := c.later[0]
		c.later = c.later[1:]
		f()
	}
}

// opens gives New its answer once all it waits for is there.
func (c *Client) opens() {
	if c.observed && !c.opening && c.script != "" {
		c.open(nil)
	}
}

// open gives New its answer, once.
func (c *Client) open(err error) {
	select {
	case c.opened <- err:
	default:
	}
}

// snapshot is what Position and Finished need, as of the last input.
type snapshot struct {
	current, entries int
	pos, duration    float64
}

func (c *Client) snapshot() {
	current, pos := c.current, c.reported
	if c.onAStop() {
		// the party's entry and second, wherever the person took this node's screen
		current, pos = c.partyEntry, c.since
	}
	c.snap.Store(&snapshot{current: current, entries: len(c.names), pos: pos, duration: c.duration})
}

// mpvSaid is one line from mpv: an answer to a request, or an event.
func (c *Client) mpvSaid(line []byte) {
	event := Event{}
	if err := json.Unmarshal(line, &event); err != nil {
		log.Println("Watch: unmarshal:", err, string(line))
		return
	}
	if event.RequestId > 0 && event.Error != "" {
		if c.waiting[event.RequestId] {
			c.replies[event.RequestId] = outcome{Response: event.Response, data: event.Data}
			log.Println("Watch: queued response:", string(line))
		} else {
			log.Println("Watch: dropped response:", string(line))
		}
	}
	if event.EventType == clientMessage {
		log.Printf("Watch: mpv said: %s\n", line)
		for _, report := range c.scriptSaid(event.Args) {
			line, _ := json.Marshal(report)
			c.broadcast(c.handleEvent(report, line))
		}
		return
	}
	if event.EventType == "" {
		return
	}
	c.broadcast(c.handleEvent(event, line))
}

func (c *Client) broadcast(out [][]byte) {
	for _, b := range out {
		select {
		case c.outgoing <- b:
			log.Printf("Watch: broadcasted event: %s\n", b)
		case <-c.ctx.Done():
			return
		}
	}
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
	if s, err := c.stateOf(event); err == nil && (c.early == nil || s.over(c.best) > 0) {
		c.early, c.best = &e, s
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
	c.busy++
	defer func() { c.busy-- }()
	if c.resumeAt > 0 {
		if err := c.goTo(c.paused, &c.resumeAt, nil, c.paused); err != nil {
			log.Println("continuing where it stopped:", err)
		}
	}
	if c.early != nil {
		c.apply(*c.early)
	}
	c.announce()
	for _, e := range c.notices {
		c.apply(e)
	}
	c.early, c.notices = nil, nil
}

// announce joins the party: the members pause and answer with where they are, and this
// node takes the first answer.
func (c *Client) announce() {
	c.stamp = unheard
	hello, _ := json.Marshal(Event{Name: joined})
	select {
	case c.outgoing <- hello:
	case <-c.ctx.Done():
	}
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
		data := "no"
		if c.held().paused {
			data = "yes"
		}
		check, _ = json.Marshal(Event{EventType: propertyChange, Name: pause, Data: data, Check: true})
		check = c.tag(check)
	}
	switch {
	case joiner:
		c.announce()
	case check != nil:
		select {
		case c.outgoing <- check:
		case <-c.ctx.Done():
		}
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
			if event.Data == e.HostName+HasLeft {
				err = errors.Join(err, c.left(e.HostName, e.From))
			}
		case pauseReason:
			err = c.showText(fmt.Sprintf("paused for %s: %s", e.HostName, event.Data))
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
	over, told := s.over(h), c.by == m.From
	// a resume reaching a node that plays: newer, this node missed the pause in between; a
	// tie from a member something this node sent never reached, that member resumed from
	// what it last heard; the state this node took from a member, sent again by it, to this
	// node alone, as what it sent looked lost: it arrived late; or a newer resume from a
	// member back from losing the relay, made where nobody heard it; or a tie from another
	// stop, more than the two seconds each could stop at from one. Not a joiner: it syncs
	// when its join is answered.
	elsewhere := !e.Again && !e.Check && math.Abs(s.at-c.since) > 2*timePosTolerance
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
		c.leader, c.told = hostname, false
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
		c.pos, c.marked, c.landing = 0, nil, false
	}
	a.there = c.on(s.at)
	a.seek = a.stop && !a.moving || !a.there && (a.moving || c.paused)
	a.still = !s.paused && (a.moving || c.stalled)
	paused := s.paused || a.still
	a.tell = a.seek || a.moving || paused != c.paused
	c.stamp, c.by, c.since, c.toPlay, c.free, c.partyEntry = s.stamp, from, s.at, !s.paused, s.free, s.entry
	if a.moving {
		c.toward = s.entry
	}
	if a.tell {
		c.paused = paused
	}
	return a
}

// adopt tells mpv to go where commit took this node, in one go: stopped, on the entry, on
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
		if err = c.goTo(c.paused, second, entry, a.was); err != nil {
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

// release lets go of what commit marked once mpv refused the go, as it does while it quits or
// between files: toward would head for a move mpv never makes, and a seek mark would put
// this node on a second it never reached (target).
func (c *Client) release(a adoption) {
	if a.moving && c.toward == a.entry {
		c.toward = -1
	}
	if a.seek {
		c.marked = slices.DeleteFunc(c.marked, func(s ourSeek) bool { return s.at == a.at })
		c.landing = c.landing && len(c.marked) > 0
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
		c.asked = true
		return c.pauseReq(true)
	}
	c.free = true
	// tagged now, not before the notice: what it says is where this node is when it goes out
	c.say(c.tag(position(timePos, c.held().at)))
	return nil
}

// say sends the party one message of this node's own.
func (c *Client) say(b []byte) {
	select {
	case c.outgoing <- b:
	case <-c.ctx.Done():
	}
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
	case c.stamp < 0 || c.by == e.From:
		// a joiner has no state yet; one taken from that member, it has
		c.reply(e.Reply, nil)
		return nil
	}
	data := "no"
	if c.held().paused {
		data = "yes"
	}
	again, _ := json.Marshal(Event{EventType: propertyChange, Name: pause, Data: data, Again: true})
	again = c.tag(again)
	if c.reply(e.Reply, again) {
		log.Printf("sent again, as %s missed something: %s\n", e.HostName, again)
	}
	return nil
}

func (c *Client) reply(to chan<- []byte, b []byte) bool {
	select {
	case to <- b:
		return true
	case <-c.ctx.Done():
		return false
	}
}

// left pauses the party for a member that left (from), a stop with no leader; a leader that
// left leaves its stop with none. By its id, not its name, which another member may share.
func (c *Client) left(hostname, from string) error {
	playing, ending := !c.paused, c.ending()
	if c.led() && from != "" && c.by == from {
		c.free = true
	}
	if !playing || ending {
		return nil
	}
	c.showText(fmt.Sprintf("paused for %s: left", hostname))
	c.asked = true
	return c.pauseReq(true)
}

func (c *Client) entryName(i int) string {
	if i < 0 || i >= len(c.names) {
		return "another video"
	}
	return c.names[i]
}

// pauseReq stops or plays mpv where it is.
func (c *Client) pauseReq(paused bool) error {
	return c.goTo(paused, nil, nil, c.paused)
}

// goTo has the script take mpv to a state in one message (watchparty.lua), and returns once mpv has
// all of it (landed): stopped or playing, on the entry if one is given, on the second if one
// is. still: mpv was stopped before. The person's key can only land before it, which it
// overwrites, or after it, which is theirs (ORDER.md).
func (c *Client) goTo(paused bool, second *float64, entry *int, still bool) error {
	id, at, to := rand.Int63(), "-", "-"
	if second != nil {
		at = strconv.FormatFloat(*second, 'f', 6, 64)
		c.marked = append(c.marked, ourSeek{id: id, at: *second, entry: c.heading()})
		c.marked = c.marked[max(0, len(c.marked)-maxSeeks):]
		c.landing = true
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
	c.flying = &flight{id: id, at: c.target(), seek: second != nil, still: still}
	// one go at a time: a message, or a command a line starts (later), waits for its ack, so
	// the ack is always weighed against the state it was sent from. The ack can come while mpv
	// answers the command itself.
	c.busy++
	defer func() { c.busy-- }()
	c.waiting[id] = true
	defer delete(c.waiting, id)
	err := c.do("script-message-to", c.script, scriptName+"-go", holds(paused), at, to, strconv.FormatInt(id, 10), c.read, on)
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

// refuseResume is a peer's resume reaching a node still waiting on its cache, which never
// reaches mpv: the node stops the party again, with where and why.
func (c *Client) refuseResume() error {
	c.reason = "still buffering"
	again, _ := json.Marshal(Event{EventType: propertyChange, Name: pause, Data: "yes"})
	out := append([][]byte{again}, c.pauseExtras()...)
	// this node's own pause, newer than the resume it refuses, a stop nobody chose
	c.stamp, c.by, c.free = c.stamp+1, "", true
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

// wentTo is a seek mpv runs to pos, as the script says at mpv's seek event (watchparty.lua), which
// is where it lands (ORDER.md). This node's own if it carries this node's id and is on its
// second; mpv runs seeks in order, so those marked before it were folded into it. Any other
// is the person's: their seek alone, or folded into this node's (a key's step added to it, or
// their second replacing it). It goes out as mpv's report of where it is, as a seek always did
// (positionChanged), unless it is where this node already is or heads.
func (c *Client) wentTo(id int64, pos float64) []Event {
	where := c.target()
	if i := slices.IndexFunc(c.marked, func(s ourSeek) bool { return s.id == id }); i >= 0 {
		at := c.marked[i].at
		c.marked = c.marked[i+1:]
		if math.Abs(pos-at) <= timePosTolerance {
			c.pos = pos
			c.landing = c.landing && len(c.marked) > 0
			return nil
		}
	}
	if c.flying != nil {
		// from before a go on its way, which overwrites it, or the ack says where it went
		return nil
	}
	if c.paused && math.Abs(pos-where) <= timePosTolerance {
		c.pos = pos
		return nil
	}
	c.seeking = !c.paused
	return []Event{{EventType: propertyChange, Name: timePos, Data: strconv.FormatFloat(pos, 'f', 6, 64), fromScript: true}}
}

// target is where this node is, or where a seek it sent will land: a member that just arrived
// is still on its way, and answering another joiner with the old spot would drag the party
// back to it.
func (c *Client) target() float64 {
	if c.landing && len(c.marked) > 0 {
		return c.marked[len(c.marked)-1].at
	}
	return c.pos
}

// Position is the entry and the second to continue the party from. Within
// endGraceSeconds of an entry's end that is the next entry's start: with keep-open=always
// mpv stays on the finished one, and continuing there would only end it again.
func (c *Client) Position() (int, float64) {
	s := c.snap.Load()
	if !s.ending() {
		return s.current, s.pos
	}
	return min(s.current+1, max(s.entries-1, 0)), 0
}

// Finished is the last entry, or the only file, played to its end: nothing is left to
// continue.
func (c *Client) Finished() bool {
	s := c.snap.Load()
	return s.ending() && s.current >= s.entries-1
}

func (c *Client) ending() bool {
	return (&snapshot{pos: c.pos, duration: c.duration}).ending()
}

func (s *snapshot) ending() bool {
	return s.duration > 0 && s.duration-s.pos <= endGraceSeconds
}

func (c *Client) showText(text string) error {
	return c.do(showText, text, showTextDurationSeconds*1000)
}

// New wires up one mpv. names are its playlist entries in order, and nil for a single file;
// start is the entry it opens on, which a peer's answer can arrive before mpv reports. A
// continued party seeks that entry to at once it is open (see join).
func New(c context.Context, cancel context.CancelFunc, nc net.Conn, outgoing chan<- []byte, incoming <-chan IncomingMessage, names []string, start int, at float64) (*Client, error) {
	conn := &connection{
		conn:    nc,
		scanner: bufio.NewScanner(nc),
		closed:  make(chan struct{}),
	}
	client := &Client{
		ctx:      c,
		ready:    make(chan struct{}),
		conn:     conn,
		outgoing: outgoing,
		paused:   true,
		seen:     map[string]bool{},
		missed:   map[string]bool{},
		names:    names,
		current:  start,
		toward:   -1,
		resumeAt: at,
		stamp:    unheard,
		incoming: incoming,
		inbox:    newMailbox[input](),
		replies:  map[int64]outcome{},
		waiting:  map[int64]bool{},
		opening:  true,
		opened:   make(chan error, 1),
	}
	client.snapshot()

	// the loop's first step, once the reader runs: observing makes mpv say what it holds
	// pause comes from the script (acked), with every change mark
	events := []string{filename, timePos, pausedForCache, duration}
	client.later = append(client.later, func() {
		// mpv makes a script's client before it runs any command (ORDER.md), so one sent to
		// the script's name fails only if there is none: mpv couldn't load it, as it has no Lua
		if err := client.do("script-message-to", scriptName, "watchparty-check"); err != nil {
			client.open(fmt.Errorf("%w (%v)", ErrNoScript, err))
			return
		}
		if err := client.do("script-message", "watchparty-hello"); err != nil {
			client.open(err)
			return
		}
		for i, event := range events {
			if err := client.do("observe_property_string", i+1, event); err != nil {
				client.open(err)
				return
			}
		}
		client.observed = true
		client.opens()
	})
	go func() {
		err := client.watch()
		if err != nil {
			log.Println("watch:", err)
		}
		conn.conn.Close()
		close(conn.closed)
		select {
		case <-client.ready:
			cancel()
		default:
		}
		client.serve()
	}()

	select {
	case err := <-client.opened:
		if err != nil {
			return nil, err
		}
		return client, nil
	case <-c.Done():
		return nil, c.Err()
	}
}
