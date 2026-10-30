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
	propertyChange = "property-change"
	seek           = "seek"
	setProperty    = "set_property"
	showText       = "show-text"

	showTextDurationSeconds = 5
	// ponytail: ±0.5s swallows a deliberate sub-second seek too; tighten only if
	// frame-stepping ever needs to sync
	timePosTolerance = 0.5
)

var (
	errNoChange  = errors.New("no change")
	errDoNotSend = errors.New("do not send")
	errEcho      = errors.New("echo")
	errTimeout   = errors.New("mpv did not answer within 5s")
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
	outgoing chan<- []byte
	conn     *connection
	mu       sync.Mutex
	paused   bool
	applied  map[string]string
	seen     map[string]bool
}

func (c *Client) isEcho(event Event) bool {
	applied, ok := c.applied[event.Name]
	if !ok {
		return false
	}
	if event.Name != timePos {
		delete(c.applied, event.Name)
		return applied == event.Data
	}
	got, err := strconv.ParseFloat(event.Data, 64)
	if err != nil {
		return false
	}
	want, err := strconv.ParseFloat(applied, 64)
	if err != nil {
		return false
	}
	if math.Abs(got-want) > timePosTolerance {
		return false
	}
	c.applied[timePos] = event.Data
	return true
}

func (c *Client) handleEvent(event Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch event.EventType {
	// case seek:
	// 	err := s.pauseReq(true)
	// 	if err != nil {
	// 		return err
	// 	}
	// 	s.paused = true
	// 	return errDoNotSend

	case propertyChange:
		if c.isEcho(event) {
			return errEcho
		}
		first := !c.seen[event.Name]
		c.seen[event.Name] = true
		switch event.Name {
		case pause:
			if c.paused == event.paused() {
				return errNoChange
			}
			c.paused = event.paused()
			if !c.paused {
				delete(c.applied, timePos)
			}
		case timePos:
			if !c.paused {
				return errDoNotSend
			}
		default:
			return errDoNotSend
		}
		if first {
			return errDoNotSend
		}
	default:
		return errDoNotSend
	}

	return nil
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
		switch err = c.handleEvent(event); err {
		case nil:
		case errNoChange, errDoNotSend, errEcho:
			continue
		default:
			log.Println("Watch:", err)
			continue
		}
		select {
		case c.outgoing <- bytes.Clone(scanner.Bytes()):
			log.Println("Watch: broadcasted event:", scanner.Text())
		case <-c.ctx.Done():
			return c.ctx.Err()
		}
	}

	return scanner.Err()
}

func (c *Client) ProcessIncomingEvents(incoming <-chan IncomingMessage) {
	for {
		var e IncomingMessage
		select {
		case e = <-incoming:
		case <-c.ctx.Done():
			return
		}
		event := Event{}
		err := json.Unmarshal(e.Event, &event)
		if err != nil {
			log.Printf("ProcessIncomingEvents: %s [event=%s] [host=%s]\n", err, e.Event, e.HostName)
			continue
		}

		switch event.Name {
		case pause:
			err = c.pause(event, e.HostName)
		case timePos:
			err = c.sync(event, e.HostName)
		case showText:
			err = c.showText(event.Data)
		}

		if err != nil {
			log.Printf("ProcessIncomingEvents: %s [event=%s] [host=%s]\n", err, e.Event, e.HostName)
			continue
		}
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

	c.mu.Lock()
	c.applied[timePos] = event.Data
	c.mu.Unlock()

	err := c.conn.do(setProperty, timePos, event.Data)
	if err != nil {
		return err
	}

	c.showText(fmt.Sprintf("synced by %s", hostname))
	return nil
}

func (c *Client) showText(text string) error {
	return c.conn.do(showText, text, showTextDurationSeconds*1000)
}

func New(c context.Context, cancel context.CancelFunc, socket string, outgoing chan<- []byte) (*Client, error) {
	conn, err := newConnection(c, socket)
	if err != nil {
		return nil, err
	}
	conn.scanner = bufio.NewScanner(conn.conn)
	conn.pending = map[int64]chan Response{}
	client := &Client{
		ctx:      c,
		conn:     conn,
		outgoing: outgoing,
		paused:   true,
		applied:  map[string]string{},
		seen:     map[string]bool{},
	}

	go func() {
		defer conn.conn.Close()
		err := client.watch()
		if err != nil {
			log.Println("watch:", err)
		}
		cancel()
	}()

	events := []string{pause, timePos}
	for i, event := range events {
		err := conn.do("observe_property_string", i+1, event)
		if err != nil {
			return nil, err
		}
	}

	return client, nil
}
