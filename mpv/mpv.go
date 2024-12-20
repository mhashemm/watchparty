package mpv

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log"
	"math/rand"
	"net"
	"slices"
	"sync"
)

const (
	pause          = "pause"
	timePos        = "time-pos"
	propertyChange = "property-change"
	slave          = "slave"
	seek           = "seek"
	setProperty    = "set_property"
)

var (
	errNoChange  = errors.New("no change")
	errDonotSend = errors.New("do not send")
	errSlave     = errors.New("slave")

	skippedErrs = []error{errNoChange, errDonotSend, errSlave}
)

type connection struct {
	conn    net.Conn
	scanner *bufio.Scanner
	mu      sync.Mutex
}

func (c *connection) request(req []byte) error {
	req = append(req, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	log.Printf("%s", req)
	_, err := c.conn.Write(req)
	return err
}

type Event struct {
	EventType string `json:"event"`
	Id        int    `json:"id"`
	Name      string `json:"name"`
	Data      string `json:"data"`
}

func (e Event) paused() bool {
	return e.Data == "yes"
}

type Request struct {
	Command   []any `json:"command"`
	RequestId int64 `json:"request_id"`
}

func NewRequest(args ...any) Request {
	return Request{
		Command:   args,
		RequestId: rand.Int63(),
	}
}

type Client struct {
	outgoing chan<- []byte
	conn     *connection
	mu       sync.Mutex
	paused   bool
	role     string
}

func (s *Client) handleEvent(event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch event.EventType {
	case seek:
		if s.paused {
			return errDonotSend
		}
		err := s.pauseReq(true)
		if err != nil {
			return err
		}
		s.paused = true
		return errDonotSend

	case propertyChange:
		switch event.Name {
		case pause:
			s.paused = event.paused()
			if !s.paused {
				s.role = ""
			}
		case timePos:
			if !s.paused {
				return errDonotSend
			}
		default:
			return errDonotSend
		}
	default:
		return errDonotSend
	}

	if s.role == slave {
		return errSlave
	}
	return nil
}

func (s *Client) Watch() error {
	scanner := s.conn.scanner
	for scanner.Scan() {
		event := Event{}
		json.Unmarshal(scanner.Bytes(), &event)
		if event.EventType == "" {
			log.Println(scanner.Text())
			continue
		}
		err := s.handleEvent(event)
		if slices.Contains(skippedErrs, err) {
			continue
		}
		if err != nil {
			log.Printf("%s %s", scanner.Bytes(), err)
			continue
		}
		s.outgoing <- scanner.Bytes()
	}

	return scanner.Err()
}

func (s *Client) ProccessIncomingEvents(incoming <-chan []byte) {
	for e := range incoming {
		event := Event{}
		err := json.Unmarshal(e, &event)
		if err != nil {
			log.Printf("%s | %s\n", e, err)
			continue
		}

		switch event.Name {
		case pause:
			err = s.pause(event)
		case timePos:
			err = s.sync(event)
		}

		if err != nil {
			log.Printf("%s | %s\n", e, err)
			continue
		}
	}
}

func (s *Client) pauseReq(paused bool) error {
	req := NewRequest(setProperty, pause, paused)
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	err = s.conn.request(body)
	if err != nil {
		return err
	}
	return nil
}

func (s *Client) pause(event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	paused := event.paused()
	if paused {
		s.role = slave
	} else {
		s.role = ""
	}
	if s.paused == paused {
		return nil
	}
	err := s.pauseReq(paused)
	if err != nil {
		return err
	}
	s.paused = paused
	return nil
}

func (s *Client) sync(event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.paused {
		return nil
	}

	if event.Data == "" {
		return nil
	}

	req := NewRequest(setProperty, timePos, event.Data)
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	err = s.conn.request(body)
	if err != nil {
		return err
	}
	return nil
}

func (s *Client) Observe() error {
	events := []string{pause, timePos}
	for i, event := range events {
		req := NewRequest("observe_property_string", i+1, event)
		body, err := json.Marshal(req)
		if err != nil {
			return err
		}
		err = s.conn.request(body)
		if err != nil {
			return err
		}
	}
	return nil
}

func New(c context.Context, socket string, outgoing chan<- []byte) (*Client, error) {
	conn, err := newConnection(c, socket)
	if err != nil {
		return nil, err
	}

	return &Client{
		conn:     conn,
		outgoing: outgoing,
		paused:   true,
	}, nil
}
