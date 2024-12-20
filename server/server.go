package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	addressHeaderKey  = "hit-me-up"
	counterHeaderKey  = "counter"
	hostnameHeaderKey = "hostname"
)

type peer struct {
	Counter  uint64     `json:"counter,string"`
	Hostname string     `json:"hostname"`
	mu       sync.Mutex `json:"-"`
	address  string     `json:"-"`
}

func (p *peer) String() string {
	return fmt.Sprintf("%s (%s)", p.address, p.Hostname)
}

type Server struct {
	c         context.Context
	addresses map[string]*peer
	mu        sync.RWMutex
	incoming  chan<- []byte
	client    *http.Client
	myAddress string
	counter   uint64
	hostname  string
}

func (s *Server) Hi(res http.ResponseWriter, req *http.Request) {
	addr := req.Header.Get(addressHeaderKey)
	counter, _ := strconv.ParseUint(req.Header.Get(counterHeaderKey), 10, 64)
	hostname := req.Header.Get(hostnameHeaderKey)
	s.mu.Lock()
	myCounter := s.counter
	resBody, _ := json.Marshal(s.addresses)
	s.addresses[addr] = &peer{
		Counter:  counter,
		Hostname: hostname,
		address:  addr,
	}
	s.mu.Unlock()

	res.Header().Add(counterHeaderKey, strconv.FormatUint(myCounter, 10))
	res.Header().Add(hostnameHeaderKey, s.hostname)
	_, err := res.Write(resBody)
	if err != nil {
		log.Printf("%s %s: %s\n", addr, hostname, err)
	}
	res.WriteHeader(http.StatusOK)
}

func (s *Server) Event(res http.ResponseWriter, req *http.Request) {
	addr := req.Header.Get(addressHeaderKey)
	counter, _ := strconv.ParseUint(req.Header.Get(counterHeaderKey), 10, 64)
	body, err := io.ReadAll(req.Body)
	if err != nil {
		log.Printf("%s: %s\n", addr, err)
		res.WriteHeader(http.StatusInternalServerError)
		return
	}
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
	if counter <= peer.Counter {
		log.Printf("skipped event from %s\n", peer.String())
		res.WriteHeader(http.StatusNoContent)
		return
	}
	peer.Counter = counter
	s.incoming <- body
	res.WriteHeader(http.StatusNoContent)
}

func (s *Server) Bye(res http.ResponseWriter, req *http.Request) {
	addr := req.Header.Get(addressHeaderKey)
	s.mu.Lock()
	delete(s.addresses, addr)
	s.mu.Unlock()
	res.WriteHeader(http.StatusNoContent)
}

func (s *Server) AddAddress(addr string) error {
	s.mu.RLock()
	counter := s.counter
	s.mu.RUnlock()
	res, err := s.request(addr, "/hi", nil, counter)
	if err != nil {
		return err
	}
	resBody, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	addresses := map[string]*peer{}
	err = json.Unmarshal(resBody, &addresses)
	if err != nil {
		return err
	}
	peerCounter, _ := strconv.ParseUint(res.Header.Get(counterHeaderKey), 10, 64)
	hostname := res.Header.Get(hostnameHeaderKey)
	s.mu.Lock()
	for addr, peer := range addresses {
		_, exists := s.addresses[addr]
		if exists {
			continue
		}
		_, err := s.request(addr, "/hi", nil, counter)
		if err != nil {
			log.Println(err)
			continue
		}
		s.addresses[addr] = peer
	}
	s.addresses[addr] = &peer{
		Counter:  peerCounter,
		Hostname: hostname,
		address:  addr,
	}
	s.mu.Unlock()
	return nil
}

func (s *Server) Shutdown() {
	s.broadcast(func(p *peer, _ uint64) error {
		_, err := s.request(p.address, "/bye", nil, math.MaxUint64)
		return err
	})
}

func (s *Server) BroadcastEvents(outgoing chan []byte) {
	for event := range outgoing {
		s.broadcast(func(p *peer, counter uint64) error {
			_, err := s.request(p.address, "/event", event, counter)
			return err
		})
	}
}

func (s *Server) broadcast(f func(*peer, uint64) error) {
	wg := sync.WaitGroup{}
	s.mu.Lock()
	s.counter += 1
	counter := s.counter
	s.mu.Unlock()

	s.mu.RLock()
	for _, peer := range s.addresses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f(peer, counter)
			if err != nil {
				log.Printf("%s: %s\n", peer.String(), err)
			}
		}()
	}
	wg.Wait()
	s.mu.RUnlock()
}

func (s *Server) request(addr string, endpoint string, data []byte, counter uint64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(s.c, http.MethodPost, "http://"+addr+endpoint, bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	req.Header.Add(addressHeaderKey, s.myAddress)
	req.Header.Add(counterHeaderKey, strconv.FormatUint(counter, 10))
	req.Header.Add(hostnameHeaderKey, s.hostname)
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fmt.Errorf("%s%s: %s", addr, endpoint, res.Status)
	}
	return res, nil
}

func New(c context.Context, incoming chan []byte, myAddress string) *Server {
	hostname, _ := os.Hostname()
	return &Server{
		c:        c,
		incoming: incoming,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		addresses: map[string]*peer{},
		myAddress: myAddress,
		hostname:  hostname,
	}
}
