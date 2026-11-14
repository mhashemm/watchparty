package mpv

import (
	"bufio"
	"bytes"
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
	"time"
)

const (
	pause           = "pause"
	timePos         = "time-pos"
	pausedForCache  = "paused-for-cache"
	playlistPos     = "playlist-pos"
	filename        = "filename"
	fileLoaded      = "file-loaded"
	duration        = "duration"
	propertyChange  = "property-change"
	seek            = "seek"
	playbackRestart = "playback-restart"
	setProperty     = "set_property"
	showText        = "show-text"
	pauseReason     = "pause-reason"
	joined          = "joined"
	startPos        = "start-pos"
	pausedAt        = "paused-at"

	lostRelay = "paused: lost the relay"
	// unheard is the stamp of a node that hasn't heard the party yet: so far below any the
	// party can hold that nothing a joiner does before it is answered wins over the party,
	// while joiners still order what they do among themselves.
	unheard = -1 << 30
	// catchingUp is why the party stops for a node whose pause or seek was older than
	// what the party holds: it missed something, and is told where the party is.
	catchingUp = "catching up"

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
	HostName string
	Event    []byte
	// Opened, Lost and Back are set only by this node's own transport, never from the wire.
	// Opened: this node opened the room with the key, or started the mesh, so its position
	// is the party's.
	Opened bool
	// Lost: its connection to the relay dropped. Back: it is in the room again, and
	// didn't open it.
	Lost, Back bool
}

type connection struct {
	conn    net.Conn
	scanner *bufio.Scanner
	mu      sync.Mutex
	pending map[int64]chan Response
	closed  chan struct{}
}

func (c *connection) wait(id int64) chan Response {
	ch := make(chan Response, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	return ch
}

func (c *connection) done(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *connection) respond(res Response) bool {
	c.mu.Lock()
	ch, ok := c.pending[res.RequestId]
	c.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- res:
		return true
	default:
		return false
	}
}

func (c *connection) do(args ...any) error {
	req := Request{
		Command:   args,
		RequestId: rand.Int63(),
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ch := c.wait(req.RequestId)
	defer c.done(req.RequestId)
	_, err = c.conn.Write(append(body, '\n'))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrClosed, err)
	}
	log.Printf("do: sent request: %s\n", body)

	select {
	case res := <-ch:
		log.Printf("do: received response: [requestId=%d] [error=%s]\n", res.RequestId, res.Error)
		if res.Error != "success" {
			return errors.New(res.Error)
		}
		return nil
	case <-c.closed:
		return ErrClosed
	case <-time.After(5 * time.Second):
		log.Printf("do: response timeout: [requestId=%d]\n", req.RequestId)
		return errTimeout
	}
}

type Event struct {
	EventType string `json:"event,omitempty"`
	Name      string `json:"name"`
	Data      string `json:"data"`
	// Stamp, At and Entry are the state its sender was in when it sent this (tag)
	Stamp int      `json:"stamp,omitempty"`
	At    *float64 `json:"at,omitempty"`
	Entry *int     `json:"entry,omitempty"`
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

	// mu guards everything below, and is never held across conn.do
	mu sync.Mutex

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

	// joining: taking the party's position
	joining  bool
	resumeAt float64

	// settling: after a join or a leave, on the earliest stop
	settling bool
	earliest float64
	heard    bool
	pausedBy string

	// playlist
	names     []string
	current   int
	following []string
	loading   bool
	heldSeek  string
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
// this node's own change, and whatever must follow it. Each is tagged under the lock that
// decided it, so its stamp is the one that change was given.
func (c *Client) handleEvent(event Event, line []byte) [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out, err := c.react(event)
	if err == nil {
		out = append([][]byte{line}, out...)
	}
	if len(out) > 0 {
		// this node's own change, or what it adds to a peer's: the next stamp
		c.stamp++
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
	case fileLoaded:
		c.fileLoaded()
	case propertyChange:
		if event.Data != "" {
			return c.propertyChanged(event)
		}
	}
	return nil, errDoNotSend
}

// fileLoaded lets go of the seek held for the entry this node was heading to.
func (c *Client) fileLoaded() {
	// a file on the way to where this node is headed: a seek let go here lands on it
	if c.heading() != c.current {
		return
	}
	c.loading, c.following = false, nil
	if held := c.heldSeek; held != "" {
		// still on its way until seekTo has marked the seek, or target sees neither
		pos, _ := strconv.ParseFloat(held, 64)
		c.heldSeek, c.peerSeek, c.landing, c.seeks = "", true, true, append(c.seeks, pos)
		go func() {
			if err := c.seekTo(held); err != nil {
				log.Println("seek after loading:", err)
			}
		}()
	}
}

// propertyChanged is what a property-change with data means for the party: what to
// broadcast after it, or instead of it, and nil only if the event itself goes out.
func (c *Client) propertyChanged(event Event) ([][]byte, error) {
	extra, moved := c.record(event)
	if c.isEcho(event) {
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
		if n, err := strconv.Atoi(event.Data); err == nil && n >= 0 && n != c.current {
			c.current, c.pos, c.seeking, c.heard, c.landing = n, 0, false, false, false
			moved = c.seen[timePos]
			if moved {
				c.seeks = []float64{0}
			}
		}
	}
	return extra, moved
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
		c.seeks, c.seeking = nil, false
		c.resumed()
		return extra, nil
	}
	// stopped from playing: where it is now, whatever seek was on its way
	c.pausedBy, c.landing = "", false
	if len(extra) == 0 {
		extra = [][]byte{position(pausedAt, c.pos)}
	}
	if at, ok := c.earlierStop(); ok {
		go func() {
			if err := c.seekTo(at); err != nil {
				log.Println("settling:", err)
			}
		}()
	}
	return extra, nil
}

// positionChanged is a seek while paused, which goes out, or playback moving on, which
// doesn't, unless it is where a seek while playing landed: that pauses the party.
func (c *Client) positionChanged() error {
	if c.paused {
		// its own seek: wherever it was being sent, this is where it is now
		c.joining, c.landing = false, false
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
	c.joining = false
	return nil
}

// tag says what state a message was sent from: its stamp, where this node is stopped or
// resumes from, and in a playlist the entry, so a peer that has moved on can drop it. Only
// this node knows, at the moment it sends.
func (c *Client) tag(b []byte) []byte {
	at := c.pos
	if c.paused {
		at = c.target()
	}
	return c.tagAs(b, c.current, at)
}

func (c *Client) tagAs(b []byte, entry int, at float64) []byte {
	head := fmt.Appendf(nil, `{"stamp":%d,"at":%.6f,`, c.stamp, at)
	if len(c.names) > 0 {
		head = fmt.Appendf(head, `"entry":%d,`, entry)
	}
	return append(head, b[1:]...)
}

// holds is where this node is as an answer says it: the entry it is heading to and its
// second on that one, which is the start of a file still loading unless a seek waits for
// it. What mpv reports is tagged with the file it is on (tag); an answer made while a move
// loads would otherwise name the new entry with the old file's second.
func (c *Client) holds() (entry int, at float64) {
	if entry = c.heading(); entry != c.current && c.heldSeek == "" {
		return entry, 0
	}
	return entry, c.target()
}

// How a peer's pause or seek stands against what this node holds.
const (
	older = iota // its sender is behind: catch it up
	loses        // sent at the same moment as this node's, which wins: tell it
	same         // nothing to do
	newer        // take it
)

// weigh compares a peer's pause or seek with what this node holds. A higher stamp is
// newer. With the same stamp the two were made at once and the safer wins, for everyone
// alike: stopped over playing, then the earlier second.
func (c *Client) weigh(stamp int, paused bool, at float64) int {
	if stamp < c.stamp {
		return older
	}
	if stamp > c.stamp {
		return newer
	}
	if paused != c.paused {
		if paused {
			return newer
		}
		return loses
	}
	if !paused || c.on(at) {
		return same
	}
	if at < c.target() {
		return newer
	}
	return loses
}

// on is whether pos is the second this node is on, or on its way to. Not at: that also
// matches every seek it has applied since the last resume, to know mpv's echo of one, and
// a second it has since left is not where it is.
func (c *Client) on(pos float64) bool {
	return math.Abs(pos-c.target()) <= timePosTolerance
}

// beats is whether an entry a peer named wins over the one this node is heading to: a
// newer stamp does, and of two with the same stamp the higher entry. Everyone applies it
// alike, so two who moved at once, or joined at once, end on the same one.
func (c *Client) beats(stamp, entry int) bool {
	return stamp > c.stamp || stamp == c.stamp && entry > c.heading()
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

// fromAnotherEntry is a position or a resume sent before its sender moved, or before
// we did. A pause still counts, since stopping is always safe.
func (c *Client) fromAnotherEntry(event Event) bool {
	if event.Entry == nil || (event.Name != timePos && event.Name != pausedAt && event.Name != startPos && (event.Name != pause || event.paused())) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return *event.Entry != c.heading()
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
	go func() {
		err := c.pauseReq(true)
		if err != nil {
			log.Println("pauseParty:", err)
			c.mu.Lock()
			c.reason = ""
			c.mu.Unlock()
			return
		}
		c.showText("paused for everyone: " + reason)
	}()
}

func (c *Client) watch() error {
	scanner := c.conn.scanner
	for scanner.Scan() {
		event := Event{}
		err := json.Unmarshal(scanner.Bytes(), &event)
		if err != nil {
			log.Println("Watch: unmarshal:", err, scanner.Text())
			continue
		}
		if event.RequestId > 0 && event.Error != "" {
			if c.conn.respond(event.Response) {
				log.Println("Watch: queued response:", scanner.Text())
			} else {
				log.Println("Watch: dropped response:", scanner.Text())
			}
		}
		if event.EventType == "" {
			continue
		}
		for _, b := range c.handleEvent(event, bytes.Clone(scanner.Bytes())) {
			select {
			case c.outgoing <- b:
				log.Printf("Watch: broadcasted event: %s\n", b)
			case <-c.ctx.Done():
				return c.ctx.Err()
			}
		}
	}

	return scanner.Err()
}

// ProcessIncomingEvents drains incoming from the start, so peers never wait on a
// file that is slow to load. Until it has loaded, only the latest pause and
// time-pos are kept; position goes first because mpv starts paused.
func (c *Client) ProcessIncomingEvents(incoming <-chan IncomingMessage) {
	latest := map[string]IncomingMessage{}
	notices := []IncomingMessage{}
	for loading := true; loading; {
		select {
		case e := <-incoming:
			event := Event{}
			err := json.Unmarshal(e.Event, &event)
			if err == nil && (event.Name == pause || event.Name == timePos) {
				latest[event.Name] = e
			} else if err == nil && event.Name == startPos {
				// a position for someone else's join: this node hasn't said joined yet
			} else {
				notices = append(notices, e)
			}
		case <-c.ready:
			loading = false
		case <-c.ctx.Done():
			return
		}
	}
	// a continued party seeks only the file it continues on, before the party's position
	// is applied and before joining, so whatever the party says lands after it. Not
	// --start: mpv applies it to every later entry too. If it fails, the file is at 0.
	if c.resumeAt > 0 {
		if err := c.seekTo(strconv.FormatFloat(c.resumeAt, 'f', 6, 64)); err != nil {
			log.Println("continuing where it stopped:", err)
		}
	}
	for _, name := range []string{timePos, pause} {
		if e, ok := latest[name]; ok {
			c.apply(e)
		}
	}
	c.announce()
	for _, e := range notices {
		c.apply(e)
	}
	for {
		select {
		case e := <-incoming:
			c.apply(e)
		case <-c.ctx.Done():
			return
		}
	}
}

// announce joins the party: the members pause and answer with where they are, and this
// node takes the first answer. Also after a dropped relay connection, since whatever it
// sent or was sent meanwhile is gone.
func (c *Client) announce() {
	c.mu.Lock()
	c.joining, c.stamp = true, unheard
	c.mu.Unlock()
	hello, _ := json.Marshal(Event{Name: joined})
	select {
	case c.outgoing <- hello:
	case <-c.ctx.Done():
	}
}

// lost stops this node the moment its relay connection drops: nothing it does can reach
// the party until it is back, and playing on would only take it further out of step. Not
// within endGraceSeconds of the end, like a leave: it would stop just short of it.
func (c *Client) lost() error {
	c.mu.Lock()
	playing, ending := !c.paused, c.ending()
	c.mu.Unlock()
	if !playing || ending {
		return nil
	}
	c.showText(lostRelay)
	return c.pauseReq(true)
}

// back joins the party again, unless this node is playing out the end, which lost let it
// do: a join would stop everyone else just short of it.
func (c *Client) back() {
	c.mu.Lock()
	finishing := !c.paused && c.ending()
	c.mu.Unlock()
	if !finishing {
		c.announce()
	}
}

func (c *Client) apply(e IncomingMessage) {
	if e.Opened {
		c.mu.Lock()
		c.joining, c.stamp = false, max(c.stamp, 1)
		c.mu.Unlock()
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
	event := Event{}
	err := json.Unmarshal(e.Event, &event)
	if err == nil && c.behind(event) {
		if err := c.answer(e.HostName, catchingUp, timePos); err != nil {
			log.Printf("ProcessIncomingEvents: %s [event=%s] [host=%s]\n", err, e.Event, e.HostName)
		}
		return
	}
	if err == nil && c.elsewhere(event) {
		// a pause or a seek that wins was made on its sender's entry: go there first, or
		// this node would hold the party's stamp on a file the party has left
		err = c.followEntry(Event{Data: strconv.Itoa(*event.Entry), Stamp: event.Stamp, At: event.At, Entry: event.Entry}, e.HostName)
	}
	if err == nil && c.fromAnotherEntry(event) {
		log.Printf("ProcessIncomingEvents: from another entry [event=%s] [host=%s]\n", e.Event, e.HostName)
		return
	}
	if err == nil {
		switch event.Name {
		case pause:
			err = c.pause(event, e.HostName)
		case timePos:
			err = c.sync(event, e.HostName)
		case showText:
			err = c.showText(event.Data)
			if event.Data == e.HostName+HasLeft {
				err = errors.Join(err, c.left(e.HostName))
			}
		case pauseReason:
			err = c.showText(fmt.Sprintf("paused for %s: %s", e.HostName, event.Data))
		case joined:
			err = c.answer(e.HostName, "joining", startPos)
		case startPos:
			err = c.arrive(event, e.HostName)
		case pausedAt:
			err = c.align(event, e.HostName)
		case playlistPos:
			err = c.followEntry(event, e.HostName)
		}
	}
	if err != nil {
		log.Printf("ProcessIncomingEvents: %s [event=%s] [host=%s]\n", err, e.Event, e.HostName)
	}
}

// behind is a pause or a seek older than what this node holds: its sender missed
// something, whatever entry it thinks it is on.
func (c *Client) behind(event Event) bool {
	if event.Name != pause && event.Name != timePos {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return event.Stamp < c.stamp
}

// elsewhere is a pause or a seek made on another entry than the one this node is heading
// to, which wins over what this node holds.
func (c *Client) elsewhere(event Event) bool {
	if event.Name != pause && event.Name != timePos || event.Entry == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return *event.Entry != c.heading() && c.beats(event.Stamp, *event.Entry)
}

// answer tells a node that isn't where the party is where that is: its entry, then its
// second, as name. For a reason, a joiner or a node that fell behind, the party stops
// first and settles like after a leave. With none, the node's change was made at the same
// moment as one that won, which stopped the party already.
func (c *Client) answer(hostname, reason, name string) error {
	c.mu.Lock()
	joining, playing := c.joining, !c.paused
	var at string
	var earlier bool
	if !joining && reason != "" {
		at, earlier = c.settle()
	}
	var entry []byte
	if len(c.names) > 0 {
		heading, second := c.holds()
		entry, _ = json.Marshal(Event{EventType: propertyChange, Name: playlistPos, Data: strconv.Itoa(heading)})
		entry = c.tagAs(entry, heading, second)
	}
	c.mu.Unlock()
	if entry != nil {
		select {
		case c.outgoing <- entry:
		case <-c.ctx.Done():
			return nil
		}
	}
	if joining {
		return nil
	}
	if earlier {
		if err := c.seekTo(at); err != nil {
			return err
		}
	}
	// for a node that fell behind it says why first, like a leave: the GUI's feed takes the
	// pause that follows for this one, not for a pause of the person's own
	if reason == catchingUp {
		c.showText(fmt.Sprintf("paused for %s: %s", hostname, reason))
	}
	if playing {
		err := c.pauseReq(true)
		if err != nil {
			return err
		}
	}
	if reason != "" && reason != catchingUp {
		c.showText(fmt.Sprintf("paused for %s: %s", hostname, reason))
	}
	c.mu.Lock()
	heading, second := c.holds()
	b := c.tagAs(position(name, second), heading, second)
	c.mu.Unlock()
	select {
	case c.outgoing <- b:
	case <-c.ctx.Done():
	}
	return nil
}

func (c *Client) left(hostname string) error {
	c.mu.Lock()
	playing, ending := !c.paused, c.ending()
	at, earlier := c.settle()
	c.mu.Unlock()
	if earlier {
		return c.seekTo(at)
	}
	if !playing || ending {
		return nil
	}
	c.showText(fmt.Sprintf("paused for %s: left", hostname))
	return c.pauseReq(true)
}

// followEntry takes the entry a peer moved to, or answered with, if it beats what this
// node holds. A peer that knows of a later move has the higher stamp, so an answer sent
// before its sender heard of one moves nobody back. Its sender stopped for it, or is about
// to, and says where: taking the stamp without that would leave this node holding the
// party's stamp on its own second, a tie it could win.
func (c *Client) followEntry(event Event, hostname string) error {
	i, err := strconv.Atoi(event.Data)
	if err != nil {
		return err
	}
	if i < 0 || i >= len(c.names) {
		return errors.New("no such entry")
	}
	c.mu.Lock()
	beats := c.beats(event.Stamp, i)
	c.mu.Unlock()
	if !beats {
		return nil
	}
	err = c.setPause(true, hostname)
	if err != nil {
		return err
	}
	c.mu.Lock()
	// again: this node's own move can have happened during the pause
	if !c.beats(event.Stamp, i) {
		c.mu.Unlock()
		return nil
	}
	c.stamp = event.Stamp
	moving, from := i != c.heading(), c.entryName(c.heading())
	if moving {
		// a seek held for the entry it was heading to is not for this one
		c.loading, c.heldSeek = true, ""
		c.following = append(c.following, event.Data)
	}
	// a file opens at its start, and anywhere else on this one is already there
	at, known := c.where(event, i)
	there := !known || moving && at <= timePosTolerance || !moving && c.on(at)
	c.mu.Unlock()
	if moving {
		err = c.conn.do(setProperty, playlistPos, i)
		if err != nil {
			c.mu.Lock()
			c.loading = false
			if n := slices.Index(c.following, event.Data); n >= 0 {
				c.following = slices.Delete(c.following, n, n+1)
			}
			c.mu.Unlock()
			return err
		}
		c.showText(fmt.Sprintf("from %s to %s, by %s", from, c.names[i], hostname))
	}
	if there {
		return nil
	}
	return c.seekBy(strconv.FormatFloat(at, 'f', 6, 64), hostname)
}

// where is the second a message says its sender is on, if that is on entry: a node still
// loading the entry it names, or pausing while a move reaches it, is on another file.
func (c *Client) where(event Event, entry int) (float64, bool) {
	if event.At == nil || len(c.names) > 0 && (event.Entry == nil || *event.Entry != entry) {
		return 0, false
	}
	return *event.At, true
}

func (c *Client) entryName(i int) string {
	if i < 0 || i >= len(c.names) {
		return "another video"
	}
	return c.names[i]
}

func (c *Client) arrive(event Event, hostname string) error {
	pos, err := strconv.ParseFloat(event.Data, 64)
	if err != nil {
		return err
	}
	c.mu.Lock()
	joining := c.joining
	c.joining = false
	c.settling = c.settling || joining
	if joining {
		// what it takes is the party's, as of this stamp
		c.stamp = max(c.stamp, event.Stamp)
	}
	there := c.at(pos)
	c.mu.Unlock()
	if !joining {
		return c.align(event, hostname)
	}
	if there {
		return nil
	}
	err = c.setPause(true, hostname)
	if err != nil {
		return err
	}
	return c.seekBy(event.Data, hostname)
}

func (c *Client) pauseReq(paused bool) error {
	return c.conn.do(setProperty, pause, paused)
}

// pause is a peer's pause or resume, which says where it stopped or resumes from, so it
// is enough by itself when the seek that came with it was lost.
func (c *Client) pause(event Event, hostname string) error {
	paused := event.paused()
	c.mu.Lock()
	at, there := c.target(), true
	if said, ok := c.where(event, c.heading()); ok {
		at, there = said, c.on(said)
	}
	w := c.weigh(event.Stamp, paused, at)
	if w == newer {
		c.stamp = event.Stamp
	}
	stopped := c.paused
	c.mu.Unlock()
	switch w {
	case older:
		return c.answer(hostname, catchingUp, timePos)
	case loses:
		return c.answer(hostname, "", timePos)
	case same:
		return nil
	}
	data := strconv.FormatFloat(at, 'f', 6, 64)
	if !paused && stopped && !there {
		if err := c.seekTo(data); err != nil {
			return err
		}
	}
	if err := c.setPause(paused, hostname); err != nil {
		return err
	}
	if paused && !there {
		return c.seekTo(data)
	}
	return nil
}

// setPause applies a pause or a resume as hostname's.
func (c *Client) setPause(paused bool, hostname string) error {
	data, did := "no", "resumed"
	if paused {
		data, did = "yes", "paused"
	}
	c.mu.Lock()
	if c.paused == paused {
		c.mu.Unlock()
		return nil
	}
	if !paused && c.stalled {
		c.reason = "still buffering"
		again, _ := json.Marshal(Event{EventType: propertyChange, Name: pause, Data: "yes"})
		out := append([][]byte{again}, c.pauseExtras()...)
		// this node's own pause, newer than the resume it refuses
		c.stamp++
		for i, b := range out {
			out[i] = c.tag(b)
		}
		c.mu.Unlock()
		for _, b := range out {
			select {
			case c.outgoing <- b:
			case <-c.ctx.Done():
				return nil
			}
		}
		return c.showText("paused for everyone: still buffering")
	}
	c.applied[pause] = data
	c.mu.Unlock()

	err := c.pauseReq(paused)
	if err != nil {
		// no echo is coming for a set mpv refused; if it only answered late, its report
		// goes out as this node's own, which peers already in that state ignore
		c.mu.Lock()
		delete(c.applied, pause)
		c.mu.Unlock()
		return err
	}
	c.mu.Lock()
	c.paused = paused
	if paused {
		c.pausedBy, c.landing = hostname, false
	} else {
		c.resumed()
	}
	c.mu.Unlock()

	c.showText(fmt.Sprintf("%s by %s", did, hostname))
	return nil
}

// sync is a peer's seek. It was made while stopped, so a node still playing stops first:
// that seek and its own resume were made at the same moment, or the pause before it was lost.
func (c *Client) sync(event Event, hostname string) error {
	pos, err := strconv.ParseFloat(event.Data, 64)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	w := c.weigh(event.Stamp, true, pos)
	if w == newer {
		c.stamp = event.Stamp
	}
	playing, told := !c.paused, c.pausedBy == hostname
	c.mu.Unlock()
	switch w {
	case older:
		return c.answer(hostname, catchingUp, timePos)
	case loses:
		return c.answer(hostname, "", timePos)
	case same:
		// where the pause it came with already took this node
		if told {
			c.showText(fmt.Sprintf("synced by %s", hostname))
		}
		return nil
	}
	if playing {
		if err := c.setPause(true, hostname); err != nil {
			return err
		}
	}
	return c.seekBy(event.Data, hostname)
}

func (c *Client) seekBy(data, hostname string) error {
	err := c.seekTo(data)
	if err != nil {
		return err
	}
	c.showText(fmt.Sprintf("synced by %s", hostname))
	return nil
}

func (c *Client) seekTo(data string) error {
	pos, err := strconv.ParseFloat(data, 64)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.seeks = append(c.seeks, pos)
	c.seeks = c.seeks[max(0, len(c.seeks)-maxSeeks):]
	c.joining = false
	if c.loading {
		c.heldSeek = data
		c.mu.Unlock()
		return nil
	}
	c.peerSeek, c.landing = true, true
	c.mu.Unlock()
	err = c.conn.do(setProperty, timePos, data)
	if err != nil {
		c.mu.Lock()
		c.peerSeek, c.landing = false, false
		c.mu.Unlock()
	}
	return err
}

func (c *Client) align(event Event, hostname string) error {
	pos, err := strconv.ParseFloat(event.Data, 64)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if event.Stamp < c.stamp {
		// where a node that is behind stopped: the party is not there
		c.mu.Unlock()
		return nil
	}
	if !c.heard || pos < c.earliest {
		c.earliest, c.heard = pos, true
	}
	// while settling only an earlier stop counts, even from the peer whose pause we applied:
	// its paused-at can land after another member's earlier one
	follow := c.paused && !c.at(pos) && (c.settling && pos < c.target() || !c.settling && c.pausedBy == hostname)
	c.mu.Unlock()
	if !follow {
		return nil
	}
	return c.seekTo(event.Data)
}

// target is where this node is, or where a seek it applied will land: a member that
// just arrived is still on its way, and answering another joiner with the old spot
// would drag the party back to it.
func (c *Client) target() float64 {
	held := c.heldSeek
	if held == "" && c.landing && len(c.seeks) > 0 {
		return c.seeks[len(c.seeks)-1]
	}
	if pos, err := strconv.ParseFloat(held, 64); err == nil {
		return pos
	}
	return c.pos
}

// settle starts settling, after a join or a leave: members stop a moment apart, so until
// the next resume this node follows only an earlier stop. It returns one heard already.
func (c *Client) settle() (string, bool) {
	c.settling = true
	return c.earlierStop()
}

// resumed ends joining and settling: the party is playing again.
func (c *Client) resumed() {
	c.joining, c.settling, c.heard, c.landing = false, false, false, false
}

// earlierStop is the earliest stop a peer sent since the last resume, if this node is
// settling and stopped later. It can have arrived before this node knew to settle: a
// member that applied someone's pause ignores everyone else's position until a leave or
// a join makes it settle.
func (c *Client) earlierStop() (string, bool) {
	if !c.paused || !c.settling || !c.heard || c.earliest >= c.target() || c.at(c.earliest) {
		return "", false
	}
	return strconv.FormatFloat(c.earliest, 'f', 6, 64), true
}

func (c *Client) at(pos float64) bool {
	near := func(x float64) bool { return math.Abs(pos-x) <= timePosTolerance }
	return near(c.pos) || slices.ContainsFunc(c.seeks, near)
}

// Position is the entry and the second to continue the party from. Within
// endGraceSeconds of an entry's end that is the next entry's start: with keep-open=always
// mpv stays on the finished one, and continuing there would only end it again.
func (c *Client) Position() (int, float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ending() {
		return c.current, c.pos
	}
	return min(c.current+1, max(len(c.names)-1, 0)), 0
}

// Finished is the last entry, or the only file, played to its end: nothing is left to
// continue.
func (c *Client) Finished() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ending() && c.current >= len(c.names)-1
}

func (c *Client) ending() bool {
	return c.duration > 0 && c.duration-c.pos <= endGraceSeconds
}

func (c *Client) showText(text string) error {
	return c.conn.do(showText, text, showTextDurationSeconds*1000)
}

// New wires up one mpv. names are its playlist entries in order, and nil for a single file;
// start is the entry it opens on, which a peer's answer can arrive before mpv reports. A
// continued party seeks that entry to at once it is open (see ProcessIncomingEvents).
func New(c context.Context, cancel context.CancelFunc, nc net.Conn, outgoing chan<- []byte, incoming <-chan IncomingMessage, names []string, start int, at float64) (*Client, error) {
	conn := &connection{
		conn:    nc,
		scanner: bufio.NewScanner(nc),
		pending: map[int64]chan Response{},
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
		names:    names,
		current:  start,
		resumeAt: at,
		stamp:    unheard,
	}

	go client.ProcessIncomingEvents(incoming)
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
	}()

	events := []string{filename, pause, timePos, pausedForCache, duration}
	if len(names) > 0 {
		events = append(events, playlistPos)
	}
	for i, event := range events {
		err := conn.do("observe_property_string", i+1, event)
		if err != nil {
			return nil, err
		}
	}

	select {
	case <-client.ready:
		return client, nil
	case <-conn.closed:
		return nil, ErrClosed
	case <-c.Done():
		return nil, c.Err()
	}
}
