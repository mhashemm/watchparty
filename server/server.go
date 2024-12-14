package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"maps"
	"net/http"
	"sync"
)

const addressHeaderKey = "hit-me-up"

type Server struct {
	c         context.Context
	addresses map[string]struct{}
	mu        sync.RWMutex
	incoming  chan<- []byte
	client    *http.Client
	myAddress string
}

func (s *Server) Init(res http.ResponseWriter, req *http.Request) {
	addr := req.Header.Get(addressHeaderKey)
	s.mu.Lock()
	s.addresses[addr] = struct{}{}
	s.mu.Unlock()

	res.WriteHeader(http.StatusNoContent)
}

func (s *Server) Event(res http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		log.Printf("%s: %s\n", req.RemoteAddr, err)
		res.WriteHeader(http.StatusInternalServerError)
		return
	}
	s.incoming <- body
	res.WriteHeader(http.StatusNoContent)
}

func (s *Server) AddAddress(addr string) error {
	err := s.request(addr, "/init", nil)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.addresses[addr] = struct{}{}
	s.mu.Unlock()
	return nil
}

func (s *Server) Broadcast(outgoing chan []byte) {
	wg := sync.WaitGroup{}
	for event := range outgoing {
		s.mu.RLock()
		addrs := maps.Keys(s.addresses)
		s.mu.RUnlock()
		wg.Add(len(s.addresses))
		for addr := range addrs {
			go func() {
				defer wg.Done()
				err := s.request(addr, "/event", event)
				if err != nil {
					log.Printf("%s: %s\n", addr, err)
				}
			}()
		}
		wg.Wait()
	}
}

func (s *Server) request(addr string, endpoint string, data []byte) error {
	req, err := http.NewRequestWithContext(s.c, http.MethodPost, "http://"+addr+endpoint, bytes.NewBuffer(data))
	if err != nil {
		return err
	}
	req.Header.Add(addressHeaderKey, s.myAddress)
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return errors.New(res.Status)
	}
	return nil
}

func New(c context.Context, incoming chan []byte, myAddress string) *Server {
	return &Server{
		c:         c,
		incoming:  incoming,
		client:    &http.Client{},
		addresses: make(map[string]struct{}),
		myAddress: myAddress,
	}
}
