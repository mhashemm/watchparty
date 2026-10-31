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
	"strconv"
	"sync"
	"time"
)

const (
	pause          = "pause"
	timePos        = "time-pos"
	pausedForCache = "paused-for-cache"
	propertyChange = "property-change"
	seek           = "seek"
	setProperty    = "set_property"
	showText       = "show-text"
	pauseReason    = "pause-reason"

	showTextDurationSeconds = 5
	timePosTolerance        = 0.1
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
		return err
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
	mu       sync.Mutex
	paused   bool
	applied  map[string]string
	seeks    []float64
	seen     map[string]bool
	pos      float64
	seeking  bool
	stalled  bool
	reason   string
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
		if !c.paused {
			c.seeking = true
		}
		return nil, errDoNotSend

	case propertyChange:
		if event.Data == "" {
			return nil, errDoNotSend
		}
		var extra [][]byte
		switch event.Name {
		case timePos:
			if pos, err := strconv.ParseFloat(event.Data, 64); err == nil {
				c.pos = pos
			}
		case pause:
			if event.paused() {
				extra = c.pauseExtras()
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
				c.seeking = false
			}
		case timePos:
			if !c.paused {
				if c.seeking {
					c.seeking = false
					c.pauseParty("seek")
				}
				return nil, errDoNotSend
			}
		case pausedForCache:
			c.stalled = event.paused()
			if c.stalled && !c.paused {
				c.pauseParty("buffering")
			}
			return nil, errDoNotSend
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

func (c *Client) pauseExtras() [][]byte {
	if c.reason == "" {
		return nil
	}
	c.seeks = append(c.seeks, c.pos)
	pos, _ := json.Marshal(Event{EventType: propertyChange, Name: timePos, Data: strconv.FormatFloat(c.pos, 'f', 6, 64)})
	reason, _ := json.Marshal(Event{Name: pauseReason, Data: c.reason})
	c.reason = ""
	return [][]byte{pos, reason}
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
	event := Event{}
	err := json.Unmarshal(e.Event, &event)
	if err == nil {
		switch event.Name {
		case pause:
			err = c.pause(event, e.HostName)
		case timePos:
			err = c.sync(event, e.HostName)
		case showText:
			err = c.showText(event.Data)
		case pauseReason:
			err = c.showText(fmt.Sprintf("paused for %s: %s", e.HostName, event.Data))
		}
	}
	if err != nil {
		log.Printf("ProcessIncomingEvents: %s [event=%s] [host=%s]\n", err, e.Event, e.HostName)
	}
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
			case c.outgoing <- b:
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
	pos, err := strconv.ParseFloat(event.Data, 64)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.seeks = append(c.seeks, pos)
	c.seeks = c.seeks[max(0, len(c.seeks)-maxSeeks):]
	c.mu.Unlock()

	err = c.conn.do(setProperty, timePos, event.Data)
	if err != nil {
		return err
	}

	c.showText(fmt.Sprintf("synced by %s", hostname))
	return nil
}

func (c *Client) showText(text string) error {
	return c.conn.do(showText, text, showTextDurationSeconds*1000)
}

func New(c context.Context, cancel context.CancelFunc, nc net.Conn, outgoing chan<- []byte, incoming <-chan IncomingMessage) (*Client, error) {
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

	events := []string{pause, timePos, pausedForCache}
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
