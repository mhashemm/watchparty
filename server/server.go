package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mhashemm/watchparty/mpv"
)

const (
	addressHeaderKey  = "hit-me-up"
	counterHeaderKey  = "counter"
	hostnameHeaderKey = "hostname"

	// ponytail: 3 strikes then drop, make it a flag if flaky wifi evicts people mid-movie
	maxFails = 3
)

type peer struct {
	Counter  uint64     `json:"counter,string"`
	Hostname string     `json:"hostname"`
	mu       sync.Mutex `json:"-"`
	address  string     `json:"-"`
	fails    int        `json:"-"`
}

func (p *peer) String() string {
	return fmt.Sprintf("%s (%s)", p.address, p.Hostname)
}

type Server struct {
	c         context.Context
	addresses map[string]*peer
	mu        sync.RWMutex
	incoming  chan<- mpv.IncomingMessage
	client    *http.Client
	myAddress string
	counter   uint64
	hostname  string
}

func (s *Server) notify(hostname string, text string) {
	event, _ := json.Marshal(mpv.Event{Name: "show-text", Data: text})
	select {
	case s.incoming <- mpv.IncomingMessage{HostName: hostname, Event: event}:
	default:
	}
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
	res.WriteHeader(http.StatusOK)
	_, err := res.Write(resBody)
	if err != nil {
		log.Printf("%s %s: %s\n", addr, hostname, err)
	}
	log.Printf("connected to %s with ip %s", hostname, addr)
	s.notify(hostname, fmt.Sprintf("%s has joined", hostname))
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
	peer, exists := s.addresses[addr]
	if !exists {
		s.mu.RUnlock()
		log.Printf("%s: does not exists\n", addr)
		res.WriteHeader(http.StatusBadRequest)
		return
	}
	peer.mu.Lock()
	if counter <= peer.Counter {
		peer.mu.Unlock()
		s.mu.RUnlock()
		log.Printf("skipped event from %s\n", peer)
		res.WriteHeader(http.StatusNoContent)
		return
	}
	peer.Counter = counter
	hostname := peer.Hostname
	peer.mu.Unlock()
	s.mu.RUnlock()

	select {
	case s.incoming <- mpv.IncomingMessage{HostName: hostname, Event: body}:
		res.WriteHeader(http.StatusNoContent)
	case <-req.Context().Done():
		res.WriteHeader(http.StatusServiceUnavailable)
	}
}

func (s *Server) Bye(res http.ResponseWriter, req *http.Request) {
	addr := req.Header.Get(addressHeaderKey)
	counter, _ := strconv.ParseUint(req.Header.Get(counterHeaderKey), 10, 64)
	// ponytail: identity is whatever the headers claim, add signing if this ever leaves trusted peers
	s.mu.Lock()
	peer, exists := s.addresses[addr]
	if !exists {
		s.mu.Unlock()
		log.Printf("%s: does not exists\n", addr)
		res.WriteHeader(http.StatusBadRequest)
		return
	}
	peer.mu.Lock()
	stale := counter <= peer.Counter
	peer.mu.Unlock()
	if stale {
		s.mu.Unlock()
		log.Printf("skipped bye from %s\n", peer)
		res.WriteHeader(http.StatusNoContent)
		return
	}
	delete(s.addresses, addr)
	s.mu.Unlock()
	res.WriteHeader(http.StatusNoContent)
	s.notify(peer.Hostname, fmt.Sprintf("%s has left", peer.Hostname))
}

func (s *Server) hi(addr string, counter uint64) (*peer, map[string]*peer, error) {
	c, cancel := context.WithTimeout(s.c, 30*time.Second)
	defer cancel()

	res, err := s.request(c, addr, "/hi", nil, counter)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()

	addresses := map[string]*peer{}
	err = json.NewDecoder(res.Body).Decode(&addresses)
	if err != nil {
		return nil, nil, err
	}
	peerCounter, _ := strconv.ParseUint(res.Header.Get(counterHeaderKey), 10, 64)
	return &peer{
		Counter:  peerCounter,
		Hostname: res.Header.Get(hostnameHeaderKey),
		address:  addr,
	}, addresses, nil
}

func (s *Server) AddAddress(addr string) error {
	addr, hostname, _ := strings.Cut(addr, "|")
	s.mu.RLock()
	counter := s.counter
	s.mu.RUnlock()

	p, addresses, err := s.hi(addr, counter)
	if err != nil {
		return err
	}
	if hostname != "" {
		p.Hostname = hostname
	}
	added := []*peer{p}

	s.mu.RLock()
	for a := range addresses {
		_, exists := s.addresses[a]
		if exists || a == addr || a == s.myAddress {
			delete(addresses, a)
		}
	}
	s.mu.RUnlock()

	for a, p := range addresses {
		_, _, err := s.hi(a, counter)
		if err != nil {
			log.Println(err)
			continue
		}
		p.address = a
		added = append(added, p)
	}

	s.mu.Lock()
	for _, p := range added {
		s.addresses[p.address] = p
	}
	s.mu.Unlock()

	for _, p := range added {
		s.notify(p.Hostname, fmt.Sprintf("connected to %s", p.Hostname))
	}
	return nil
}

func (s *Server) Shutdown() {
	s.broadcast(context.WithoutCancel(s.c), func(c context.Context, p *peer, _ uint64) error {
		_, err := s.request(c, p.address, "/bye", nil, math.MaxUint64)
		return err
	})
}

func (s *Server) BroadcastEvents(outgoing chan []byte) {
	for event := range outgoing {
		s.broadcast(s.c, func(c context.Context, p *peer, counter uint64) error {
			_, err := s.request(c, p.address, "/event", event, counter)
			return err
		})
	}
}

func (s *Server) broadcast(parent context.Context, f func(context.Context, *peer, uint64) error) {
	s.mu.Lock()
	s.counter += 1
	counter := s.counter
	peers := slices.Collect(maps.Values(s.addresses))
	s.mu.Unlock()

	c, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	errs := make([]error, len(peers))
	wg := sync.WaitGroup{}
	wg.Add(len(peers))
	for i, p := range peers {
		go func() {
			defer wg.Done()
			errs[i] = f(c, p, counter)
		}()
	}
	wg.Wait()

	for i, p := range peers {
		p.mu.Lock()
		if errs[i] == nil {
			p.fails = 0
			p.mu.Unlock()
			continue
		}
		p.fails += 1
		dropped := p.fails >= maxFails
		p.mu.Unlock()

		log.Printf("%s: %s\n", p, errs[i])
		if !dropped {
			continue
		}
		s.mu.Lock()
		if s.addresses[p.address] == p {
			delete(s.addresses, p.address)
		}
		s.mu.Unlock()
		s.notify(p.Hostname, fmt.Sprintf("dropped %s", p))
	}
}

func (s *Server) request(c context.Context, addr string, endpoint string, data []byte, counter uint64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(c, http.MethodPost, "http://"+addr+endpoint, bytes.NewBuffer(data))
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

func New(c context.Context, incoming chan mpv.IncomingMessage, myAddress string, hostname string) *Server {
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	return &Server{
		c:         c,
		incoming:  incoming,
		client:    &http.Client{},
		addresses: map[string]*peer{},
		myAddress: myAddress,
		hostname:  hostname,
	}
}
