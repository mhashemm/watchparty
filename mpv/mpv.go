package mpv

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"net"
	"sync/atomic"
	"time"
)

const (
	pause          = "pause"
	timePos        = "time-pos"
	pausedForCache = "paused-for-cache"
	cacheFill      = "cache-buffering-state"
	cacheAhead     = "demuxer-cache-duration"
	viaNetwork     = "demuxer-via-network"
	buffered       = "buffered"
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
	// a member that ran out of cache tells the party how much it has ready once it had that
	// many seconds, never more often than reportMin nor less than reportMax
	reportMin = 5 * time.Second
	reportMax = time.Minute
	// mpv's screen shows a member's seconds ready the first time, then only once they are
	// twice the last shown, and at least twice screenFrom: every report would keep text up
	// for most of a stall's first minute, over any other notice
	screenFrom = 5
)

var (
	errTimeout = errors.New("mpv did not answer in time")

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
	// slow: mpv ran out of cache on a stream (network) since this node last played, so while
	// paused it tells the party how many seconds it has ready (readySaid), next at reportAt
	network   bool
	slow      bool
	readySaid int
	reportAt  time.Time
	// the seconds ready last shown on mpv's screen, this node's (-1: none yet) and each
	// peer's by its From, until the party plays (louder)
	readyShown int
	shownOf    map[string]int

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
	buffering        bool
}

func (c *Client) snapshot() {
	current, pos := c.current, c.reported
	if c.onAStop() {
		// the party's entry and second, wherever the person took this node's screen
		current, pos = c.partyEntry, c.since
	}
	c.snap.Store(&snapshot{current: current, entries: len(c.names), pos: pos, duration: c.duration, buffering: c.stalled})
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

// observed are the properties New observes, as ids 1 and up
var observed = []string{filename, timePos, pausedForCache, duration, cacheFill, viaNetwork, cacheAhead}

// Buffering is mpv waiting on its cache (paused-for-cache).
func (c *Client) Buffering() bool {
	return c.snap.Load().buffering
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
		shownOf:  map[string]int{},
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
		for i, event := range observed {
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
