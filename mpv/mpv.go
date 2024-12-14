package mpv

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
)

const syncMargin = 30 //seconds

type Event struct {
	EventType string `json:"event"`
	Id        int    `json:"id"`
	Name      string `json:"name"`
	Data      string `json:"data"`
}

type Client struct {
	eventsConn        *connection
	conn              *connection
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
	eventsConn, err := newConnection(c, socket)
	if err != nil {
		return nil, err
	}

	conn, err := newConnection(c, socket)
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