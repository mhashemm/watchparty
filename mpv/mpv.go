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

	// scriptName is the name mpv gives the script, from ScriptFile. If another script has
	// that file name, it takes the name and this one gets a number after it (ORDER.md)
	scriptName = "watchparty"

	lostRelay = "lost the relay, reconnecting"
	// unheard is the stamp of a node that hasn't heard the party yet. It is far below any
	// stamp the party can hold, so nothing a joiner does before it is answered wins over the
	// party, while joiners can still order their own changes among themselves.
	unheard = -1 << 30
	// catchingUp is why the party stops for a node whose pause or seek was older than the
	// party's state: it missed something, and is told where the party is.
	catchingUp = "catching up"
	// outOfSync is why the party stops when a resume reaches a node that is playing: one of
	// the two missed a pause or a resume, and plays from another second.
	outOfSync = "out of sync"

	showTextDurationSeconds = 5
	timePosTolerance        = 0.1
	endGraceSeconds         = 5
	maxSeeks                = 16
)

// How long the node waits on mpv before giving up (errTimeout): replyLimit for a reply or a
// set's ack; moveLimit for a set that moves to another file, acked once the file starts,
// which mpv gives up on after its --network-timeout (60s; 81s seen, ORDER.md).
const (
	replyLimit = 5 * time.Second
	moveLimit  = 3 * time.Minute
	// a member that ran out of cache reports its seconds ready again after that many
	// seconds, but no sooner than reportMin and no later than reportMax
	reportMin = 5 * time.Second
	reportMax = time.Minute
	// mpv's screen shows a member's seconds ready the first time, then only when they reach
	// twice the last shown and at least twice screenFrom: showing every report would cover
	// the screen for most of a stall's first minute, hiding every other notice
	screenFrom = 5
)

var (
	errTimeout = errors.New("mpv did not answer in time")

	ErrClosed = errors.New("mpv closed the IPC connection")
	// ErrNoScript: mpv didn't load Script because it was built without Lua
	ErrNoScript = errors.New("this mpv can't run the sync script: install an mpv built with Lua")
)

// Script is the Lua script the node controls mpv through; party.Run passes it to mpv as
// ScriptFile with --script.
//
//go:embed watchparty.lua
var Script []byte

// ScriptFile is the file name Script is written to: mpv names a script after its file.
const ScriptFile = scriptName + ".lua"

// IncomingMessage is what the transport hands the node: a peer's message, or news about the
// transport itself.
type IncomingMessage struct {
	// HostName is the sender's name, as people know them. From tells members apart, which
	// names may not: the mesh address, or the relay client's id. Notices have no From.
	HostName string
	From     string
	Event    []byte
	// Opened, Lost and Back are set only by this node's own transport, never from the wire.
	// Opened: this node created the room or started the mesh, so its position is the party's.
	Opened bool
	// Lost: the relay connection dropped. Back: it is in the room again (not as its
	// creator); whatever was sent meanwhile is gone.
	Lost, Back bool
	// Missed: something this node sent failed to reach From, maybe the latest state it
	// heard. Resend: send this node's state on Reply, or nil for none; the transport
	// delivers it to From alone.
	Missed, Resend bool
	Reply          chan<- []byte
	// typed: a chat message this node's person typed (Config.Chat), say. Unexported, so no
	// transport can make one: it rides the queue only to be handled in turn.
	typed bool
	say   string
}

// Config is what New needs besides mpv's connection.
type Config struct {
	// Outgoing takes what this node sends the party; Incoming hands it the transport's messages.
	Outgoing chan<- []byte
	Incoming <-chan IncomingMessage
	// Chat is what this node's person types to the party, one message each. Heard, if not nil,
	// is given each chat message mpv shows: who ("you" for this node's own) and the text.
	Chat  <-chan string
	Heard func(who, text string)
	// Names are the playlist entries in order, nil for a single file. Start is the entry mpv
	// opens on, needed because a peer's answer can arrive before mpv reports it. A continued
	// party seeks that entry to At once it is open (join).
	Names []string
	Start int
	At    float64
}

// Event is a line from mpv, or a message between nodes: raw mpv JSON plus the sender's state.
type Event struct {
	EventType string `json:"event,omitempty"`
	Name      string `json:"name"`
	Data      string `json:"data"`
	// Stamp, Paused, At and Entry are the state its sender held when it sent this (tag)
	Stamp  int      `json:"stamp,omitempty"`
	Paused *bool    `json:"paused,omitempty"`
	At     *float64 `json:"at,omitempty"`
	Entry  *int     `json:"entry,omitempty"`
	// Again: the sender's state, sent again to a member the mesh failed to reach.
	// Check: the sender's state, sent when it is back in the room after losing the relay.
	Again bool `json:"again,omitempty"`
	Check bool `json:"check,omitempty"`
	// Free: a stop nobody chose (a join, catching up, a leave): it has no leader, and the
	// first seek leads
	Free bool `json:"free,omitempty"`
	// Args: a script's message, in a client-message
	Args []string `json:"args,omitempty"`
	// fromScript: a report built from a script message (scriptSaid), never sent as is.
	// stoppedAt: for a move found in an ack, where mpv stopped on the new file
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

	// The loop: the only goroutine that reads or writes what follows. It takes the
	// transport's messages from incoming, everything else from inbox, in the order they came.
	incoming <-chan IncomingMessage
	// chat: what the person types (Config.Chat); heard is given each chat message mpv shows
	chat  <-chan string
	heard func(who, text string)
	inbox *mailbox[input]
	out   *mailbox[request]
	// queue: transport messages waiting while another's change is applied (applying)
	queue    []IncomingMessage
	applying int
	// later: commands an input started, run once it is handled
	later []func()
	// replies: answers kept for the do() waiting on them
	replies map[int64]outcome
	waiting map[int64]bool
	// gone: mpv's socket closed
	gone bool
	startup
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

	// What mpv last reported.
	paused         bool
	pos            float64
	duration       float64
	waitingOnCache bool
	seen           map[string]bool
	slowReport

	// This node's seeks and sets.
	// ourSeeks: seeks this node sent that mpv hasn't landed, oldest first (wentTo)
	ourSeeks []ourSeek
	// seekPending: mpv hasn't landed the last of ourSeeks, so it isn't yet on the second it
	// heads to (target)
	seekPending bool
	// pending: the set the script hasn't acked (setState), one at a time
	pending *pendingSet
	// lastScriptMsg: the number of the last script message read, which a set says, so the
	// script can tell what it undoes
	lastScriptMsg string
	// seekWhilePlaying: the person's seek went while playing
	seekWhilePlaying bool
	// stopReason: why this node is stopping the party (pauseParty)
	stopReason string

	// The party.
	// stamp orders what the party shares (a Lamport clock): the highest this node has made
	// or followed. A joiner starts at unheard.
	stamp int
	// resumedFrom: the second the resume this node plays went from, its own or one it took
	resumedFrom float64
	// missed: members (From) that something this node sent failed to reach
	missed map[string]bool
	// resumeAt: where a continued party starts (join)
	resumeAt float64
	// leaderID: the member (From) whose state this node last took; leaderName: its name
	leaderID, leaderName string
	// toldWhoLeads: the person was told who holds the seek, for that state
	toldWhoLeads bool
	// tookResume: the state this node last took from a member plays: it follows no pause
	tookResume bool
	// leaderless: the stop this node holds has no leader, as nobody chose it
	leaderless bool
	// pausedByNode: this node paused mpv itself, for a reason other than the person's seek
	// (answer, left, pauseParty)
	pausedByNode bool
	// partyEntry: the entry of the stop this node took, wherever the person has moved since
	partyEntry int

	// The playlist.
	names []string
	// current: the entry mpv opened last, as the script says it (start)
	current int
	// headingTo: the entry this node's move heads to until the script says it opened it, -1
	// for none
	headingTo int
	// lastStartAuto: the last start was mpv's own, after a file's end or error
	lastStartAuto bool
	// advancedAtEnd: mpv moved on from the end of the entry it played, by itself
	advancedAtEnd bool
}

// startup is what a node keeps while its first file loads (opening): the newest state heard, by
// stamp, and the notices, in order (hear, join).
type startup struct {
	opening     bool
	newestHeard *IncomingMessage
	newestState state
	notices     []IncomingMessage
}

// slowReport is a stalled stream's seconds ready (ahead): after mpv ran out of cache on a
// stream (onNetwork), until this node plays, it tells the party how many seconds it has
// ready (readySent), next at reportAt. readyOnScreen and peerReadyOnScreen are the last
// shown on mpv's screen, this node's (-1: none yet) and each peer's by From (louder).
type slowReport struct {
	onNetwork         bool
	slowStream        bool
	readySent         int
	reportAt          time.Time
	readyOnScreen     int
	peerReadyOnScreen map[string]int
}

// opens gives New its answer once everything it waits for has arrived.
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

// snapshot is what Position, Buffering and Finished read from other goroutines, as of the
// last input.
type snapshot struct {
	current, entries int
	pos, duration    float64
	buffering        bool
}

func (c *Client) snapshot() {
	current, pos := c.current, c.reported
	if c.onAStop() {
		// the party's entry and second, wherever the person took this node's screen
		current, pos = c.partyEntry, c.resumedFrom
	}
	c.snap.Store(&snapshot{current: current, entries: len(c.names), pos: pos, duration: c.duration, buffering: c.waitingOnCache})
}

// Position is the entry and second to continue the party from. Within endGraceSeconds of
// an entry's end it is the next entry's start: with keep-open=always mpv stays on the
// finished entry, and continuing there would only end it again.
func (c *Client) Position() (int, float64) {
	s := c.snap.Load()
	if !s.ending() {
		return s.current, s.pos
	}
	return min(s.current+1, max(s.entries-1, 0)), 0
}

// observed are the properties New observes, with ids from 1
var observed = []string{filename, timePos, pausedForCache, duration, cacheFill, viaNetwork, cacheAhead}

// Buffering is whether mpv is waiting on its cache (paused-for-cache).
func (c *Client) Buffering() bool {
	return c.snap.Load().buffering
}

// Finished is whether the last entry, or the only file, played to its end, leaving nothing
// to continue.
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

// New connects to one mpv and returns once its first file is open.
func New(c context.Context, cancel context.CancelFunc, nc net.Conn, cfg Config) (*Client, error) {
	conn := &connection{
		conn:    nc,
		scanner: bufio.NewScanner(nc),
		closed:  make(chan struct{}),
	}
	client := &Client{
		ctx:        c,
		ready:      make(chan struct{}),
		conn:       conn,
		outgoing:   cfg.Outgoing,
		chat:       cfg.Chat,
		heard:      cfg.Heard,
		paused:     true,
		seen:       map[string]bool{},
		missed:     map[string]bool{},
		slowReport: slowReport{peerReadyOnScreen: map[string]int{}},
		names:      cfg.Names,
		current:    cfg.Start,
		headingTo:  -1,
		resumeAt:   cfg.At,
		stamp:      unheard,
		incoming:   cfg.Incoming,
		inbox:      newMailbox[input](),
		replies:    map[int64]outcome{},
		waiting:    map[int64]bool{},
		startup:    startup{opening: true},
		opened:     make(chan error, 1),
	}
	client.snapshot()

	// the loop's first step, once the reader runs. Observing makes mpv report each
	// property's value; pause comes from the script instead (hello).
	client.later = append(client.later, func() {
		// mpv creates a script's client before running any command (ORDER.md), so a message
		// to the script's name fails only if mpv couldn't load it: it has no Lua
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
