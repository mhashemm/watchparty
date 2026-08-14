package mpv

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/mhashemm/watchparty/types"
)

const (
	pause          = "pause"
	timePos        = "time-pos"
	propertyChange = "property-change"
	slave          = "slave"
	seek           = "seek"
	setProperty    = "set_property"
	showText       = "show-text"

	showTextDurationSeconds = 5
)

var (
	errNoChange  = errors.New("no change")
	errDoNotSend = errors.New("do not send")
	errSlave     = errors.New("slave")

	skippedErrs = []error{errNoChange, errDoNotSend, errSlave}
)

type connection struct {
	mu      sync.Mutex
	conn    net.Conn
	scanner *bufio.Scanner
	ctx     context.Context
	reqCh   chan Request
	resCh   chan Response
}

func (c *connection) doRequests() {
	for req := range c.reqCh {
		body, err := json.Marshal(req)
		if err != nil {
			log.Println("DoRequests:", err)
			continue
		}
		body = append(body, '\n')
		_, err = c.conn.Write(body)
		log.Printf("DoRequests: %s", body)
		if err != nil {
			log.Println("DoRequests:", err)
			continue
		}
	}
}

func (c *connection) do(args ...any) error {
	req := Request{
		Command:   args,
		RequestId: rand.Int63(),
	}

	c.reqCh <- req
	for {
		select {
		case res := <-c.resCh:
			if res.RequestId != req.RequestId {
				log.Printf("do: received wrong response: [requestId=%d] [expectedRequestId=%d]\n", res.RequestId, req.RequestId)
				continue
			}
			log.Printf("do: received response: [requestId=%d] [error=%s]\n", res.RequestId, res.Error)
			if res.Error != "success" {
				return errors.New(res.Error)
			}
			return nil
		case <-time.After(5 * time.Second):
			log.Printf("do: response timeout: [requestId=%d]\n", req.RequestId)
			return context.Canceled
		}
	}
}

type Event struct {
	EventType string `json:"event,omitempty"`
	Id        int    `json:"id,omitempty"`
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
	outgoing chan<- []byte
	conn     *connection
	mu       sync.Mutex
	paused   bool
	role     string
}

func (c *Client) handleEvent(event Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch event.EventType {
	// case seek:
	// 	if s.paused && s.role == slave {
	// 		return errDoNotSend
	// 	}
	// 	err := s.pauseReq(true)
	// 	if err != nil {
	// 		return err
	// 	}
	// 	s.paused = true
	// 	return errDoNotSend

	case propertyChange:
		switch event.Name {
		case pause:
			if c.paused == event.paused() {
				return errNoChange
			}
			c.paused = event.paused()
			if !c.paused {
				c.role = ""
			}
		case timePos:
			if !c.paused {
				return errDoNotSend
			}
		default:
			return errDoNotSend
		}
	default:
		return errDoNotSend
	}

	if c.role == slave {
		return errSlave
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
			select {
			case c.conn.resCh <- event.Response:
				log.Println("Watch: queued response:", scanner.Text())
			default:
				log.Println("Watch: dropped response:", scanner.Text())
			}
		}
		if event.EventType == "" {
			continue
		}
		err = c.handleEvent(event)
		if slices.Contains(skippedErrs, err) {
			continue
		}
		if err != nil {
			log.Println("Watch:", err)
			continue
		}
		c.outgoing <- bytes.Clone(scanner.Bytes())
		log.Println("Watch: broadcasted event:", scanner.Text())
	}

	return scanner.Err()
}

func (c *Client) ProcessIncomingEvents(incoming <-chan types.IncomingMessage) {
	for e := range incoming {
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
	if paused {
		c.role = slave
	} else {
		c.role = ""
	}
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

func New(c context.Context, cancel context.CancelFunc, socket string, outgoing chan<- []byte, startAsSlave bool) (*Client, error) {
	conn, err := newConnection(c, socket)
	if err != nil {
		return nil, err
	}
	conn.scanner = bufio.NewScanner(conn.conn)
	conn.ctx = c
	conn.reqCh = make(chan Request, 10)
	conn.resCh = make(chan Response, 10)
	client := &Client{
		conn:     conn,
		outgoing: outgoing,
		paused:   true,
	}
	if startAsSlave {
		client.role = slave
	}

	go conn.doRequests()

	go func() {
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
