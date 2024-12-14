package mpv

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"math/rand"
	"net"
	"sync"
)

const (
	eventPause           = "pause"
	eventPlaybackTime    = "playback-time"
	eventPlaybackRestart = "playback-restart"
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

type Request struct {
	Command   []any `json:"command"`
	RequestId int64 `json:"request_id"`
}

type Client struct {
	outgoing          chan<- []byte
	conn              *connection
	mu                sync.Mutex
	paused            bool
	playbackRestarted bool
}

func (s *Client) handleState(event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch event.Name {
	case eventPause:
		s.paused = event.Data == "yes"
	}
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
		switch event.Name {
		case eventPause:
			s.handleState(event)
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
		case "pause":
			err = s.pause(event)
		case "playback-time", "playback-restart":
			err = s.sync(event)
		}

		if err != nil {
			log.Printf("%s | %s\n", e, err)
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
	req := Request{
		Command:   []any{"set_property", "pause", paused},
		RequestId: rand.Int63(),
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	err = s.conn.request(body)
	if err != nil {
		return err
	}
	s.paused = paused
	return nil
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
	req := Request{
		Command:   []any{"set_property", "playback-time", event.Data},
		RequestId: rand.Int63(),
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	err = s.conn.request(body)
	if err != nil {
		return err
	}
	s.playbackRestarted = false
	return nil
}

func (s *Client) Observe() error {
	events := []string{eventPause, eventPlaybackTime, eventPlaybackRestart}
	for i, event := range events {
		req := Request{
			Command:   []any{"observe_property_string", i + 1, event},
			RequestId: rand.Int63(),
		}
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
