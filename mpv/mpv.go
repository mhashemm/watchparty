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

	showTextDurationSeconds = 5
	timePosTolerance        = 0.1
	endGraceSeconds         = 5
	maxSeeks                = 16
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
	// Opened is set only by this node's relay client, never from the wire: this node
	// opened the room with the key, so its position is the party's.
	Opened bool
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
	Entry     *int   `json:"entry,omitempty"`
	From      *int   `json:"from,omitempty"`
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
	ctx        context.Context
	ready      chan struct{}
	outgoing   chan<- []byte
	conn       *connection
	mu         sync.Mutex
	paused     bool
	applied    map[string]string
	seeks      []float64
	seen       map[string]bool
	pos        float64
	duration   float64
	seeking    bool
	peerSeek   bool
	stalled    bool
	reason     string
	joining    bool
	pausedBy   string
	names      []string
	current    int
	movedFrom  int
	following  []string
	joinedWith []string
	heardParty bool
	loading    bool
	heldSeek   string
	settling   bool
	earliest   float64
	heard      bool
}

func (c *Client) isEcho(event Event) bool {
	if event.Name == timePos {
		got, err := strconv.ParseFloat(event.Data, 64)
		if err != nil {
			return false
		}
		for i, want := range c.seeks {
			if math.Abs(got-want) <= timePosTolerance {
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

func (c *Client) handleEvent(event Event) ([][]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch event.EventType {
	case seek:
		if !c.paused && !c.peerSeek {
			c.seeking = true
		}
		return nil, errDoNotSend

	case playbackRestart:
		c.peerSeek = false
		return nil, errDoNotSend

	case fileLoaded:
		// a file on the way to where this node is headed: a seek let go here lands on it
		if c.heading() != c.current {
			return nil, errDoNotSend
		}
		c.loading, c.following = false, nil
		if held := c.heldSeek; held != "" {
			// still on its way until seekTo has marked the seek, or target sees neither
			pos, _ := strconv.ParseFloat(held, 64)
			c.heldSeek, c.peerSeek, c.seeks = "", true, append(c.seeks, pos)
			go func() {
				if err := c.seekTo(held); err != nil {
					log.Println("seek after loading:", err)
				}
			}()
		}
		return nil, errDoNotSend

	case propertyChange:
		if event.Data == "" {
			return nil, errDoNotSend
		}
		var extra [][]byte
		changed, previous := false, c.heading()
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
				c.current, c.pos, c.seeking, c.heard = n, 0, false, false
				changed = c.seen[timePos]
				if changed {
					c.seeks = []float64{0}
				}
			}
		}
		if c.isEcho(event) {
			return extra, errEcho
		}
		first := !c.seen[event.Name]
		c.seen[event.Name] = true
		if first && event.Name == timePos {
			close(c.ready)
		}
		switch event.Name {
		case pause:
			if c.paused == event.paused() {
				return extra, errNoChange
			}
			if !event.paused() && c.stalled {
				c.pauseParty("still buffering")
				return nil, errDoNotSend
			}
			c.paused = event.paused()
			if !c.paused {
				c.seeks = nil
				c.seeking, c.joining, c.settling, c.heard = false, false, false, false
			} else {
				c.pausedBy = ""
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
			}
		case timePos:
			if !c.paused {
				if c.seeking {
					c.seeking = false
					c.pauseParty("seek")
				}
				return nil, errDoNotSend
			}
			c.joining = false
		case pausedForCache:
			c.stalled = event.paused()
			if c.stalled && !c.paused {
				c.pauseParty("buffering")
			}
			return nil, errDoNotSend
		case playlistPos:
			if !changed || first {
				return nil, errDoNotSend
			}
			if !c.paused {
				c.pauseParty("video changed")
			}
			c.movedFrom, c.joining = previous, false
			b, _ := json.Marshal(Event{EventType: propertyChange, Name: playlistPos, Data: event.Data, From: &previous})
			return [][]byte{b}, errDoNotSend
		default:
			return nil, errDoNotSend
		}
		if first {
			return nil, errDoNotSend
		}
		return extra, nil
	}
	return nil, errDoNotSend
}

// tagged says which entry a message belongs to, so a peer that has moved on can drop
// it. Only this node knows, at the moment it sends.
func (c *Client) tagged(b []byte) []byte {
	if len(c.names) == 0 {
		return b
	}
	c.mu.Lock()
	current := c.current
	c.mu.Unlock()
	return append(fmt.Appendf(nil, `{"entry":%d,`, current), b[1:]...)
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
		extra, err := c.handleEvent(event)
		out := extra
		if err == nil {
			out = append([][]byte{bytes.Clone(scanner.Bytes())}, extra...)
		}
		for _, b := range out {
			b = c.tagged(b)
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
			if json.Unmarshal(e.Event, &event) == nil && (event.Name == pause || event.Name == timePos) {
				latest[event.Name] = e
			} else {
				notices = append(notices, e)
			}
		case <-c.ready:
			loading = false
		case <-c.ctx.Done():
			return
		}
	}
	for _, name := range []string{timePos, pause} {
		if e, ok := latest[name]; ok {
			c.apply(e)
		}
	}
	c.mu.Lock()
	c.joining = true
	c.mu.Unlock()
	hello, _ := json.Marshal(Event{Name: joined})
	select {
	case c.outgoing <- hello:
	case <-c.ctx.Done():
		return
	}
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

func (c *Client) apply(e IncomingMessage) {
	if e.Opened {
		c.mu.Lock()
		c.joining = false
		c.mu.Unlock()
		return
	}
	event := Event{}
	err := json.Unmarshal(e.Event, &event)
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
			if event.Data == e.HostName+" has left" {
				err = errors.Join(err, c.left(e.HostName))
			}
		case pauseReason:
			err = c.showText(fmt.Sprintf("paused for %s: %s", e.HostName, event.Data))
		case joined:
			err = c.welcome(e.HostName)
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

func (c *Client) welcome(hostname string) error {
	c.mu.Lock()
	joining, playing := c.joining, !c.paused
	if joining {
		c.joinedWith = append(c.joinedWith, hostname)
	} else {
		c.settling = true
	}
	at, earlier := c.earlierStop()
	var entry []byte
	if len(c.names) > 0 {
		entry, _ = json.Marshal(Event{EventType: propertyChange, Name: playlistPos, Data: strconv.Itoa(c.heading())})
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
	if playing {
		err := c.pauseReq(true)
		if err != nil {
			return err
		}
	}
	c.showText(fmt.Sprintf("paused for %s: joining", hostname))
	c.mu.Lock()
	b := position(startPos, c.target())
	c.mu.Unlock()
	select {
	case c.outgoing <- c.tagged(b):
	case <-c.ctx.Done():
	}
	return nil
}

func (c *Client) left(hostname string) error {
	c.mu.Lock()
	c.settling = true
	playing := !c.paused
	ending := c.duration > 0 && c.duration-c.pos <= endGraceSeconds
	at, earlier := c.earlierStop()
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

func (c *Client) followEntry(event Event, hostname string) error {
	i, err := strconv.Atoi(event.Data)
	if err != nil {
		return err
	}
	if i < 0 || i >= len(c.names) {
		return errors.New("no such entry")
	}
	c.mu.Lock()
	heading := c.heading()
	lost := event.From != nil && *event.From == c.movedFrom && i < heading
	joiner := slices.Contains(c.joinedWith, hostname)
	// once the party has answered, a fellow joiner's entry is only where it was
	notForUs := event.From == nil && (!c.joining || joiner && (i < heading || c.heardParty))
	if event.From == nil && c.joining && !joiner {
		c.heardParty = true
	}
	c.mu.Unlock()
	if i == heading || lost || notForUs {
		return nil
	}
	err = c.pause(Event{Name: pause, Data: "yes"}, hostname)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if i == c.heading() {
		c.mu.Unlock()
		return nil
	}
	from := c.entryName(c.heading())
	c.loading = true
	c.following = append(c.following, event.Data)
	c.movedFrom = heading
	if event.From != nil {
		c.movedFrom = *event.From
	}
	c.mu.Unlock()
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
	return c.showText(fmt.Sprintf("from %s to %s, by %s", from, c.names[i], hostname))
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
	there := c.at(pos)
	c.mu.Unlock()
	if !joining {
		return c.align(event, hostname)
	}
	if there {
		return nil
	}
	err = c.pause(Event{Name: pause, Data: "yes"}, hostname)
	if err != nil {
		return err
	}
	return c.sync(Event{Name: timePos, Data: event.Data}, hostname)
}

func (c *Client) pauseReq(paused bool) error {
	return c.conn.do(setProperty, pause, paused)
}

func (c *Client) pause(event Event, hostname string) error {
	paused := event.paused()
	c.mu.Lock()
	if c.paused == paused {
		c.mu.Unlock()
		return nil
	}
	if !paused && c.stalled {
		c.reason = "still buffering"
		again, _ := json.Marshal(Event{EventType: propertyChange, Name: pause, Data: "yes"})
		out := append([][]byte{again}, c.pauseExtras()...)
		c.mu.Unlock()
		for _, b := range out {
			select {
			case c.outgoing <- c.tagged(b):
			case <-c.ctx.Done():
				return nil
			}
		}
		return c.showText("paused for everyone: still buffering")
	}
	c.applied[pause] = event.Data
	c.mu.Unlock()

	err := c.pauseReq(paused)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.paused = paused
	if paused {
		c.pausedBy = hostname
	} else {
		c.joining, c.settling, c.heard = false, false, false
	}
	c.mu.Unlock()

	actionText := "resumed"
	if paused {
		actionText = "paused"
	}
	c.showText(fmt.Sprintf("%s by %s", actionText, hostname))
	return nil
}

func (c *Client) sync(event Event, hostname string) error {
	c.mu.Lock()
	paused := c.paused
	c.mu.Unlock()
	if !paused {
		return nil
	}

	if event.Data == "" {
		return nil
	}
	err := c.seekTo(event.Data)
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
	c.peerSeek = true
	c.mu.Unlock()
	err = c.conn.do(setProperty, timePos, data)
	if err != nil {
		c.mu.Lock()
		c.peerSeek = false
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
	if !c.heard || pos < c.earliest {
		c.earliest, c.heard = pos, true
	}
	stopped := c.stopped()
	// while settling only an earlier stop counts, even from the peer whose pause we applied:
	// its paused-at can land after another member's earlier one
	follow := c.paused && !c.at(pos) && (c.settling && pos < stopped || !c.settling && c.pausedBy == hostname)
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
	if held == "" && c.peerSeek && len(c.seeks) > 0 {
		return c.seeks[len(c.seeks)-1]
	}
	if pos, err := strconv.ParseFloat(held, 64); err == nil {
		return pos
	}
	return c.pos
}

func (c *Client) stopped() float64 {
	if n := len(c.seeks); n > 0 {
		return c.seeks[n-1]
	}
	return c.pos
}

// earlierStop is the earliest stop a peer sent since the last resume, if this node is
// settling and stopped later. It can have arrived before this node knew to settle: a
// member that applied someone's pause ignores everyone else's position until a leave or
// a join makes it settle.
func (c *Client) earlierStop() (string, bool) {
	if !c.paused || !c.settling || !c.heard || c.earliest >= c.stopped() || c.at(c.earliest) {
		return "", false
	}
	return strconv.FormatFloat(c.earliest, 'f', 6, 64), true
}

func (c *Client) at(pos float64) bool {
	near := func(x float64) bool { return math.Abs(pos-x) <= timePosTolerance }
	return near(c.pos) || slices.ContainsFunc(c.seeks, near)
}

// ClearStart undoes --start once the first file is open: mpv would start every later
// entry, and every return to this one, there too.
func (c *Client) ClearStart() error {
	return c.conn.do(setProperty, "start", "none")
}

// Position is where this node is in its entry, for continuing the party later. Within
// endGraceSeconds of the end it is 0: continuing there would only end the file again.
func (c *Client) Position() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ending() {
		return 0
	}
	return c.pos
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
// start is the entry it opens on, which a peer's answer can arrive before mpv reports.
func New(c context.Context, cancel context.CancelFunc, nc net.Conn, outgoing chan<- []byte, incoming <-chan IncomingMessage, names []string, start int) (*Client, error) {
	conn := &connection{
		conn:    nc,
		scanner: bufio.NewScanner(nc),
		pending: map[int64]chan Response{},
		closed:  make(chan struct{}),
	}
	client := &Client{
		ctx:       c,
		ready:     make(chan struct{}),
		conn:      conn,
		outgoing:  outgoing,
		paused:    true,
		applied:   map[string]string{},
		seen:      map[string]bool{},
		names:     names,
		current:   start,
		movedFrom: -1,
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
