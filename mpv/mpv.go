package mpv

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
)

const syncMargin = 30 //seconds

type Event struct {
	EventType string `json:"event"`
	Id        int    `json:"id"`
	Name      string `json:"name"`
	Data      string `json:"data"`
}

// { "command": ["set_property", "pause", true] }
// req := `{ "command": ["observe_property_string", 1, "pause"] }` + "\n"
// req := `{ "command": ["observe_property_string", 1, "playback-time"] }` + "\n"
// req := `{ "command": ["get_property", "filename"] }` + "\n"

type conn struct {
	cunt    net.Conn
	scanner *bufio.Scanner
	mu      sync.Mutex
}

func newConn(c context.Context, socket string) (*conn, error) {
	dialer := &net.Dialer{}
	cunt, err := dialer.DialContext(c, "unix", socket)
	if err != nil {
		return nil, err
	}

	return &conn{
		cunt:    cunt,
		scanner: bufio.NewScanner(cunt),
	}, nil
}

func (c *conn) request(req []byte) ([]byte, error) {
	req = append(req, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()

	_, err := c.cunt.Write(req)
	if err != nil {
		return nil, err
	}
	c.scanner.Scan()
	if c.scanner.Err() != nil {
		return nil, c.scanner.Err()
	}
	return c.scanner.Bytes(), nil
}

type Client struct {
	eventsConn        *conn
	conn              *conn
	mu                sync.Mutex
	paused            bool
	playbackRestarted bool
}

func (s *Client) Watch(outgoing chan []byte) error {
	s.eventsConn.mu.Lock()
	defer s.eventsConn.mu.Unlock()

	scanner := s.eventsConn.scanner
	for scanner.Scan() {
		outgoing <- scanner.Bytes()
		fmt.Println(scanner.Text())
	}

	return scanner.Err()
}

func (s *Client) ProccessIncomingEvents(incoming chan []byte) {
	for e := range incoming {
		event := Event{}
		err := json.Unmarshal(e, &event)
		if err != nil {
			log.Println(err)
			continue
		}

		switch event.Name {
		case "pause":
			err = s.pause(event)
		case "playback-time", "playback-restart":
			err = s.sync(event)
		}

		if err != nil {
			log.Printf("%+v %s", event, err)
			continue
		}
	}
}

func (s *Client) pause(event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	paused := event.Data == "yes"
	if s.paused == paused {
		return nil
	}
	req := fmt.Sprintf(`{ "command": ["set_property", "pause", %t] }`, paused)
	_, err := s.conn.request([]byte(req))
	return err
}

func (s *Client) sync(event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch event.Name {
	case "playback-restart":
		s.playbackRestarted = true
		return nil
	case "playback-time":
		if !s.playbackRestarted {
			return nil
		}
	}
	s.playbackRestarted = false
	req := fmt.Sprintf(`{ "command": ["set_property", "playback-time", %s] }`, event.Data)
	_, err := s.conn.request([]byte(req))
	return err
}

func New(c context.Context, socket string) (*Client, error) {
	eventsConn, err := newConn(c, socket)
	if err != nil {
		return nil, err
	}

	conn, err := newConn(c, socket)
	if err != nil {
		return nil, err
	}

	_, err = eventsConn.request([]byte(`{ "command": ["observe_property_string", 1, "pause"] }`))
	if err != nil {
		return nil, err
	}
	_, err = eventsConn.request([]byte(`{ "command": ["observe_property_string", 1, "playback-time"] }`))
	if err != nil {
		return nil, err
	}
	_, err = eventsConn.request([]byte(`{ "command": ["observe_property_string", 1, "playback-restart"] }`))
	if err != nil {
		return nil, err
	}

	return &Client{
		eventsConn: eventsConn,
		conn:       conn,
	}, nil
}
