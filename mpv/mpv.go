package mpv

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
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
	pause           = "pause"
	timePos         = "time-pos"
	pausedForCache  = "paused-for-cache"
	playlistPos     = "playlist-pos"
	filename        = "filename"
	startFile       = "start-file"
	fileLoaded      = "file-loaded"
	duration        = "duration"
	propertyChange  = "property-change"
	seek            = "seek"
	playbackRestart = "playback-restart"
	setProperty     = "set_property"
	showText        = "show-text"
	pauseReason     = "pause-reason"
	joined          = "joined"

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
	errEcho      = errors.New("echo")
	errTimeout   = errors.New("mpv did not answer within 5s")

	ErrClosed = errors.New("mpv closed the IPC connection")
)

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
	if c.gone {
		return ErrClosed
	}
	req := Request{
		Command:   args,
		RequestId: rand.Int63(),
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	id := req.RequestId
	if c.out == nil {
		c.out = newMailbox[request]()
		go c.writer(c.out)
	}
	c.out.put(request{id: id, body: body})
	c.waiting[id] = true
	defer delete(c.waiting, id)
	expire := time.AfterFunc(5*time.Second, func() { c.post(input{expired: id}) })
	defer expire.Stop()
	for {
		if r, ok := c.replies[id]; ok {
			delete(c.replies, id)
			if r.unsent != nil {
				return fmt.Errorf("%w: %v", ErrClosed, r.unsent)
			}
			if r.expired {
				log.Printf("do: response timeout: [requestId=%d]\n", id)
				return errTimeout
			}
			log.Printf("do: received response: [requestId=%d] [error=%s]\n", r.RequestId, r.Error)
			if r.Error != "success" {
				return errors.New(r.Error)
			}
			return nil
		}
		if c.gone {
			return ErrClosed
		}
		in, ok := c.next()
		if !ok {
			return ErrClosed
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
	// EntryID: the playlist entry a start-file opens, counted from 1
	EntryID int `json:"playlist_entry_id,omitempty"`
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
	// snap is what Position and Finished read from another goroutine
	snap atomic.Pointer[snapshot]
	// opened is New's one answer: nil once the first file loaded and mpv took every
	// observe (observed), or why it never will
	opened   chan error
	observed bool

	// what mpv last reported
	paused   bool
	pos      float64
	duration float64
	stalled  bool
	seen     map[string]bool

	// echo: what this node set itself, so mpv's report of it isn't broadcast, and what
	// it is about to pause the party for
	applied  map[string]string
	seeks    []float64
	peerSeek bool
	// landing: mpv hasn't reported the last seek this node applied, so it is not yet on
	// the second it is heading to. Not peerSeek: a playback-restart ends that, and one from
	// a slow load can arrive after the first seek was sent.
	landing bool
	seeking bool
	reason  string

	// stamp orders what the party shares (a Lamport clock): the highest this node has
	// made or followed. A joiner starts at unheard.
	stamp int
	// missed: members (From) that something this node sent failed to reach
	missed map[string]bool

	resumeAt float64
	// by: the member (From) whose state this node last adopted
	by string

	// playlist
	names     []string
	current   int
	following []string
	loading   bool
	heldSeek  string
	// started, loaded: the entry mpv opens and the one it has loaded, by start-file and
	// file-loaded, counted from 1 as mpv does; 0 for none. cleared: what this node had
	// marked for the last file is gone, for the one started
	started, loaded int
	cleared         bool
	// stray: mpv opened a file this node isn't on or heading to, and hasn't reported its
	// entry: a key press, which goes out once it does
	stray bool
}

func (c *Client) isEcho(event Event) bool {
	if event.Name == timePos {
		got, err := strconv.ParseFloat(event.Data, 64)
		if err != nil {
			return false
		}
		for i, want := range c.seeks {
			if math.Abs(got-want) <= timePosTolerance {
				// landed once mpv reports the second of the last one, whichever it matched
				c.landing = c.landing && math.Abs(got-c.seeks[len(c.seeks)-1]) > timePosTolerance
				c.seeks = c.seeks[i:]
				c.seeks[0] = got
				return true
			}
		}
		return false
	}
	if event.Name == playlistPos {
		i := slices.Index(c.following, event.Data)
		if i < 0 {
			// someone pressed a key before our set reached mpv, so the party must hear
			// every report from here to where it ends up. mpv's first report isn't that.
			if c.seen[playlistPos] {
				c.following = nil
			}
			return false
		}
		c.following = c.following[i+1:]
		return true
	}
	applied, ok := c.applied[event.Name]
	if !ok {
		return false
	}
	delete(c.applied, event.Name)
	return applied == event.Data
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
		// this node's own change, or what it adds to a peer's: the next stamp
		c.stamp, c.by = c.stamp+1, ""
	}
	for i, b := range out {
		out[i] = c.tag(b)
	}
	return out
}

func (c *Client) react(event Event) ([][]byte, error) {
	switch event.EventType {
	case seek:
		if !c.paused && !c.peerSeek {
			c.seeking = true
		}
	case playbackRestart:
		c.peerSeek = false
	case startFile:
		c.started, c.loaded, c.cleared = event.EntryID, 0, false
		c.stray = event.EntryID != c.heading()+1
	case fileLoaded:
		c.loaded = c.started
		c.fileLoaded()
	case propertyChange:
		if event.Data != "" {
			out, err := c.propertyChanged(event)
			if event.Name == playlistPos {
				c.fileLoaded()
			}
			return out, err
		}
	}
	return nil, errDoNotSend
}

// fileLoaded lets go of the seek held for the entry this node was heading to, once mpv has
// loaded it and said so. mpv reports a property only once its event queue is empty
// (client.h), so its report of the entry can come before file-loaded or after it; events
// keep their order, and start-file names the entry.
func (c *Client) fileLoaded() {
	// a file on the way to where this node is headed: a seek let go here lands on it
	if c.loaded == 0 || c.loaded != c.heading()+1 {
		return
	}
	c.loading = false
	if c.current+1 == c.loaded {
		// whatever else it was heading to, mpv never reported (MPV.md)
		c.following = nil
	} else {
		// the report is still to come: a seek let go now must survive it
		c.clear()
	}
	if held := c.heldSeek; held != "" {
		// still on its way until seekTo has marked the seek, or target sees neither
		pos, _ := strconv.ParseFloat(held, 64)
		c.heldSeek, c.peerSeek, c.landing, c.seeks = "", true, true, append(c.seeks, pos)
		c.later = append(c.later, func() {
			if err := c.seekTo(held); err != nil {
				log.Println("seek after loading:", err)
			}
		})
	}
}

// propertyChanged is what a property-change with data means for the party: what to
// broadcast after it, or instead of it, and nil only if the event itself goes out.
func (c *Client) propertyChanged(event Event) ([][]byte, error) {
	extra, moved := c.record(event)
	if c.isEcho(event) {
		if event.Name == pause {
			// from here mpv plays, or doesn't: its next time-pos can come before the reply
			// that tells the one applying it, and while stopped that would read as a seek
			c.paused = event.paused()
		}
		return extra, errEcho
	}
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
		err = c.positionChanged()
	case pausedForCache:
		c.stalled = event.paused()
		if c.stalled && !c.paused {
			c.pauseParty("buffering")
		}
		return nil, errDoNotSend
	case playlistPos:
		err = c.entryChanged(moved && !first)
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
		if pos, err := strconv.ParseFloat(event.Data, 64); err == nil {
			c.pos = pos
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
		c.stray = false
		if n, err := strconv.Atoi(event.Data); err == nil && n >= 0 && n != c.current {
			c.current = n
			moved = c.seen[timePos]
			if !c.cleared || c.started != n+1 {
				c.clear()
			}
		}
	}
	return extra, moved
}

// clear forgets the second this node was on and the seeks it marked, as mpv opens another
// file: the old second would go out with a pause, and the new file's 0 as a seek.
func (c *Client) clear() {
	c.pos, c.seeking, c.landing, c.cleared = 0, false, false, true
	if c.seen[timePos] {
		c.seeks = []float64{0}
	}
}

// pauseChanged is the user's own pause or resume.
func (c *Client) pauseChanged(paused bool, extra [][]byte) ([][]byte, error) {
	if c.paused == paused {
		return extra, errNoChange
	}
	if !paused && c.stalled {
		c.pauseParty("still buffering")
		return nil, errDoNotSend
	}
	c.paused = paused
	if !paused {
		c.seeks, c.seeking, c.landing = nil, false, false
		return extra, nil
	}
	// stopped from playing: where it is now, whatever seek was on its way
	c.landing = false
	return extra, nil
}

// positionChanged is a seek while paused, which goes out, or playback moving on, which
// doesn't, unless it is where a seek while playing landed: that pauses the party.
func (c *Client) positionChanged() error {
	if c.paused {
		// its own seek: wherever it was being sent, this is where it is now
		c.landing = false
		return nil
	}
	if c.seeking {
		c.seeking = false
		c.pauseParty("seek")
	}
	return errDoNotSend
}

// entryChanged is this node moving to another entry by itself: it goes out, and pauses
// the party if it was playing.
func (c *Client) entryChanged(moved bool) error {
	if !moved {
		return errDoNotSend
	}
	if !c.paused {
		c.pauseParty("video changed")
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
	return state{stamp: c.stamp, entry: c.heading(), paused: c.paused || c.reason != "", at: c.target()}
}

// tag says what state a message was sent from, so whoever gets it knows whether it is
// newer than theirs. Only this node knows, at the moment it sends.
func (c *Client) tag(b []byte) []byte {
	s := c.held()
	head := fmt.Appendf(nil, `{"stamp":%d,"paused":%t,"at":%.6f,`, s.stamp, s.paused, s.at)
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
	s := state{stamp: e.Stamp, paused: *e.Paused, at: *e.At}
	if len(c.names) > 0 {
		if s.entry = *e.Entry; s.entry < 0 || s.entry >= len(c.names) {
			return s, errors.New("no such entry")
		}
	}
	return s, nil
}

// on is whether pos is the second this node is on, or on its way to. Not at: that also
// matches every seek it has applied since the last resume, to know mpv's echo of one, and
// a second it has since left is not where it is.
func (c *Client) on(pos float64) bool {
	return math.Abs(pos-c.target()) <= timePosTolerance
}

// heading is the entry this node is on, or on its way to.
func (c *Client) heading() int {
	if n := len(c.following); n > 0 {
		if i, err := strconv.Atoi(c.following[n-1]); err == nil {
			return i
		}
	}
	return c.current
}

func (c *Client) pauseExtras() [][]byte {
	if c.reason == "" {
		return nil
	}
	c.seeks = append(c.seeks, c.pos)
	reason, _ := json.Marshal(Event{Name: pauseReason, Data: c.reason})
	c.reason = ""
	return [][]byte{position(timePos, c.pos), reason}
}

func position(name string, pos float64) []byte {
	b, _ := json.Marshal(Event{EventType: propertyChange, Name: name, Data: strconv.FormatFloat(pos, 'f', 6, 64)})
	return b
}

func (c *Client) pauseParty(reason string) {
	c.reason = reason
	c.later = append(c.later, func() {
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
	if c.opening && c.seen[timePos] {
		c.join()
	}
	for c.busy == 0 && len(c.queue) > 0 {
		m := c.queue[0]
		c.queue = c.queue[1:]
		c.busy++
		c.apply(m)
		c.busy--
	}
	c.snapshot()
}

func (c *Client) runLater() {
	for len(c.later) > 0 {
		f := c.later[0]
		c.later = c.later[1:]
		f()
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
	c.snap.Store(&snapshot{current: c.current, entries: len(c.names), pos: c.pos, duration: c.duration})
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
			c.replies[event.RequestId] = outcome{Response: event.Response}
			log.Println("Watch: queued response:", string(line))
		} else {
			log.Println("Watch: dropped response:", string(line))
		}
	}
	if event.EventType == "" {
		return
	}
	for _, b := range c.handleEvent(event, line) {
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
	if c.observed {
		c.open(nil)
	}
	c.busy++
	defer func() { c.busy-- }()
	if c.resumeAt > 0 {
		if err := c.seekTo(strconv.FormatFloat(c.resumeAt, 'f', 6, 64)); err != nil {
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
				err = errors.Join(err, c.left(e.HostName))
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
	// member back from losing the relay, made where nobody heard it. Not a joiner: it syncs
	// when its join is answered.
	apart := !s.paused && h.stamp >= 0 && s.entry == h.entry &&
		(!h.paused && (over > 0 || over == 0 && (c.missed[m.From] || e.Again && told)) || e.Check && over > 0)
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
	// seek: not on its second yet; now: mpv is told at once, or it waits for the file
	stop, moving, seek, now bool
}

// commit takes a newer state in the step that weighed it: the stamp with the entry, the
// stop and the second it stands for, all marked before mpv is told. Taken apart, this node
// would hold the party's stamp on its own entry or second, a tie it could win; and whatever
// it says meanwhile already names where it is going.
func (c *Client) commit(s state, from string) adoption {
	a := adoption{state: s, moving: s.entry != c.heading(), from: c.entryName(c.heading())}
	// a file opens stopped, and a seek is made stopped: one that plays on stays where it is
	a.stop = (a.moving || s.paused) && !c.paused
	there := a.moving && s.at <= timePosTolerance || !a.moving && c.on(s.at)
	// a seek now would land on a file mpv opened by itself, and go out as this node's own
	// once mpv reports it; that move goes out newer than s, from the file's start
	away := !a.moving && c.stray && c.started != c.heading()+1
	a.seek = !there && !away && (a.stop || c.paused)
	c.stamp, c.by = s.stamp, from
	if a.stop {
		c.applied[pause] = "yes"
	}
	if a.moving {
		// a seek held for the entry it was heading to is not for this one
		c.loading, c.heldSeek = true, ""
		c.following = append(c.following, strconv.Itoa(s.entry))
	}
	if a.seek {
		a.now = c.mark(s.at)
	}
	return a
}

// adopt tells mpv to go where commit took this node: stopped, on the entry, on the second,
// then playing if the state is. One that plays on another entry stops the party instead:
// loading it, this node can't be in step. A change this node makes while mpv answers a step
// is newer than the state, and goes out: the steps left would undo it here alone.
func (c *Client) adopt(a adoption, name, hostname string) (err error) {
	// what mpv never did, as it refused a step
	left := a
	overtaken := func() bool {
		if c.stamp == a.stamp {
			return false
		}
		c.release(left)
		return true
	}
	defer func() {
		if err == nil {
			return
		}
		c.release(left)
		// it holds the party's stamp where mpv isn't: asked again, the party says where.
		// Not once mpv is gone: this node is leaving.
		if !errors.Is(err, ErrClosed) {
			c.announce()
		}
	}()
	if a.stop {
		if err = c.pauseReq(true); err != nil {
			return err
		}
		left.stop = false
		c.paused = true
		c.showText(fmt.Sprintf("paused by %s", hostname))
	}
	if overtaken() {
		return nil
	}
	if a.moving {
		if err = c.do(setProperty, playlistPos, a.entry); err != nil {
			return err
		}
		left.moving = false
		c.showText(fmt.Sprintf("from %s to %s, by %s", a.from, c.names[a.entry], hostname))
	}
	if a.now {
		if err = c.seekReq(a.at); err != nil {
			return err
		}
		left.now = false
	}
	if a.seek && name != pause {
		c.showText(fmt.Sprintf("synced by %s", hostname))
	}
	if a.paused || overtaken() {
		return nil
	}
	if a.moving {
		return c.answer(hostname, "")
	}
	return c.resume(hostname)
}

// release lets go of what commit marked for the steps mpv refused, as it does while it
// quits or between files: a pause marker would swallow the user's next pause, loading
// would hold every later seek, and a seek marker would put this node on a second it never
// reached, and take the user's own seek there for mpv's echo. One held for a file mpv never
// opens is refused with the move.
func (c *Client) release(a adoption) {
	if a.stop {
		delete(c.applied, pause)
	}
	if a.moving {
		c.loading, c.heldSeek = false, ""
		if n := slices.Index(c.following, strconv.Itoa(a.entry)); n >= 0 {
			c.following = slices.Delete(c.following, n, n+1)
		}
	}
	if a.now || a.moving && a.seek {
		c.seeks = slices.DeleteFunc(c.seeks, func(pos float64) bool { return pos == a.at })
	}
	if a.now {
		c.peerSeek, c.landing = false, false
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
	joiner, playing := c.stamp < 0, !c.paused
	var where []byte
	if joiner || !playing {
		where = c.tag(position(timePos, c.held().at))
	}
	if joiner {
		select {
		case c.outgoing <- where:
		case <-c.ctx.Done():
		}
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
	if playing {
		if err := c.pauseReq(true); err != nil {
			return err
		}
	}
	if where != nil {
		select {
		case c.outgoing <- where:
		case <-c.ctx.Done():
		}
	}
	return nil
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

func (c *Client) left(hostname string) error {
	playing, ending := !c.paused, c.ending()
	if !playing || ending {
		return nil
	}
	c.showText(fmt.Sprintf("paused for %s: left", hostname))
	return c.pauseReq(true)
}

func (c *Client) entryName(i int) string {
	if i < 0 || i >= len(c.names) {
		return "another video"
	}
	return c.names[i]
}

func (c *Client) pauseReq(paused bool) error {
	return c.do(setProperty, pause, paused)
}

// resume applies a peer's resume. One that reaches a node still waiting on its cache never
// reaches mpv: the node stops the party again, with where and why.
func (c *Client) resume(hostname string) error {
	if !c.paused {
		return nil
	}
	if c.stalled {
		c.reason = "still buffering"
		again, _ := json.Marshal(Event{EventType: propertyChange, Name: pause, Data: "yes"})
		out := append([][]byte{again}, c.pauseExtras()...)
		// this node's own pause, newer than the resume it refuses
		c.stamp, c.by = c.stamp+1, ""
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
	c.applied[pause] = "no"

	err := c.pauseReq(false)
	if err != nil {
		// no echo is coming for a set mpv refused; if it only answered late, its report
		// goes out as this node's own, which peers already in that state ignore
		delete(c.applied, pause)
		return err
	}
	c.paused, c.landing = false, false

	c.showText(fmt.Sprintf("resumed by %s", hostname))
	return nil
}

func (c *Client) seekTo(data string) error {
	pos, err := strconv.ParseFloat(data, 64)
	if err != nil {
		return err
	}
	now := c.mark(pos)
	if !now {
		return nil
	}
	if err := c.seekReq(pos); err != nil {
		c.release(adoption{state: state{at: pos}, now: true})
		return err
	}
	return nil
}

// mark makes a seek this node applies its own before mpv is told: where it is heading, and
// not to be broadcast when mpv reports it. While a move loads it is held for the file it
// opens (false: not for mpv yet).
func (c *Client) mark(pos float64) bool {
	c.seeks = append(c.seeks, pos)
	c.seeks = c.seeks[max(0, len(c.seeks)-maxSeeks):]
	if c.loading {
		c.heldSeek = strconv.FormatFloat(pos, 'f', 6, 64)
		return false
	}
	c.peerSeek, c.landing = true, true
	return true
}

func (c *Client) seekReq(pos float64) error {
	return c.do(setProperty, timePos, strconv.FormatFloat(pos, 'f', 6, 64))
}

// target is where this node is, or where a seek it applied will land: a member that
// just arrived is still on its way, and answering another joiner with the old spot
// would drag the party back to it. A file still loading opens at its start, unless a seek
// waits for it: the second it left, read as where it is, makes a seek there look done.
func (c *Client) target() float64 {
	held := c.heldSeek
	if held == "" && c.loading {
		return 0
	}
	if held == "" && c.landing && len(c.seeks) > 0 {
		return c.seeks[len(c.seeks)-1]
	}
	if pos, err := strconv.ParseFloat(held, 64); err == nil {
		return pos
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
		applied:  map[string]string{},
		seen:     map[string]bool{},
		missed:   map[string]bool{},
		names:    names,
		current:  start,
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
	events := []string{filename, pause, timePos, pausedForCache, duration}
	if len(names) > 0 {
		events = append(events, playlistPos)
	}
	client.later = append(client.later, func() {
		for i, event := range events {
			if err := client.do("observe_property_string", i+1, event); err != nil {
				client.open(err)
				return
			}
		}
		client.observed = true
		if !client.opening {
			client.open(nil)
		}
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
