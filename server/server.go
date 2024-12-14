package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"maps"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	addressHeaderKey = "hit-me-up"
	counterHeaderKey = "counter"
)

type peer struct {
	counter uint64
	mu      sync.Mutex
}

type Server struct {
	c         context.Context
	addresses map[string]*peer
	mu        sync.RWMutex
	incoming  chan<- []byte
	client    *http.Client
	myAddress string
	counter   uint64
}

func (s *Server) Init(res http.ResponseWriter, req *http.Request) {
	addr := req.Header.Get(addressHeaderKey)
	counter, _ := strconv.ParseUint(req.Header.Get(counterHeaderKey), 10, 64)
	s.mu.Lock()
	myCounter := s.counter
	s.addresses[addr] = &peer{
		counter: counter,
	}
	s.mu.Unlock()

	res.Header().Add(counterHeaderKey, strconv.FormatUint(myCounter, 10))
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
	addr := req.Header.Get(addressHeaderKey)
	counter, _ := strconv.ParseUint(req.Header.Get(counterHeaderKey), 10, 64)
	s.mu.RLock()
	defer s.mu.RUnlock()
	peer, exists := s.addresses[addr]
	if !exists {
		log.Printf("%s: does not exists\n", addr)
		res.WriteHeader(http.StatusBadRequest)
		return
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if counter <= peer.counter {
		log.Printf("skipped event from %s\n", addr)
		res.WriteHeader(http.StatusNoContent)
		return
	}
	peer.counter = counter
	s.incoming <- body
	res.WriteHeader(http.StatusNoContent)
}

func (s *Server) AddAddress(addr string) error {
	s.mu.RLock()
	counter := s.counter
	s.mu.RUnlock()
	res, err := s.request(addr, "/init", nil, counter)
	if err != nil {
		return err
	}
	peerCounter, _ := strconv.ParseUint(res.Header.Get(counterHeaderKey), 10, 64)
	s.mu.Lock()
	s.addresses[addr] = &peer{
		counter: peerCounter,
	}
	s.mu.Unlock()
	return nil
}

func (s *Server) Broadcast(outgoing chan []byte) {
	wg := sync.WaitGroup{}
	for event := range outgoing {
		s.mu.Lock()
		addrs := maps.Keys(s.addresses)
		s.counter += 1
		counter := s.counter
		s.mu.Unlock()
		wg.Add(len(s.addresses))
		for addr := range addrs {
			go func() {
				defer wg.Done()
				_, err := s.request(addr, "/event", event, counter)
				if err != nil {
					log.Printf("%s: %s\n", addr, err)
				}
			}()
		}
		wg.Wait()
	}
}

func (s *Server) request(addr string, endpoint string, data []byte, counter uint64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(s.c, http.MethodPost, "http://"+addr+endpoint, bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	req.Header.Add(addressHeaderKey, s.myAddress)
	req.Header.Add(counterHeaderKey, strconv.FormatUint(counter, 10))
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, errors.New(res.Status)
	}
	return res, nil
}

func New(c context.Context, incoming chan []byte, myAddress string) *Server {
	return &Server{
		c:        c,
		incoming: incoming,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		addresses: make(map[string]*peer),
		myAddress: myAddress,
	}
}
