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
	"strings"
	"sync"
	"time"

	"github.com/mhashemm/watchparty/types"
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
	incoming  chan<- types.IncomingMessage
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
	res.WriteHeader(http.StatusOK)
	_, err := res.Write(resBody)
	if err != nil {
		log.Printf("%s %s: %s\n", addr, hostname, err)
	}
	log.Printf("connected to %s with ip %s", hostname, addr)
	s.incoming <- types.IncomingMessage{
		HostName: hostname,
		Event:    fmt.Appendf(nil, `{"name":"show-text",data:"%s has joined"}`, hostname),
	}
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
	msg := types.IncomingMessage{
		HostName: peer.Hostname,
		Event:    body,
	}
	s.incoming <- msg
	res.WriteHeader(http.StatusNoContent)
}

func (s *Server) Bye(res http.ResponseWriter, req *http.Request) {
	addr := req.Header.Get(addressHeaderKey)
	s.mu.Lock()
	peer, exists := s.addresses[addr]
	if !exists {
		s.mu.Unlock()
		log.Printf("%s: does not exists\n", addr)
		res.WriteHeader(http.StatusBadRequest)
		return
	}
	delete(s.addresses, addr)
	s.mu.Unlock()
	res.WriteHeader(http.StatusNoContent)
	s.incoming <- types.IncomingMessage{
		HostName: peer.Hostname,
		Event:    fmt.Appendf(nil, `{"name":"show-text",data:"%s has left"}`, peer.Hostname),
	}
}

func (s *Server) AddAddress(addr string) error {
	c, cancel := context.WithTimeout(s.c, 30*time.Second)
	defer cancel()

	addr, hostname, _ := strings.Cut(addr, "|")
	s.mu.RLock()
	counter := s.counter
	s.mu.RUnlock()
	res, err := s.request(c, addr, "/hi", nil, counter)
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
	if hostname == "" {
		hostname = res.Header.Get(hostnameHeaderKey)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for addr, peer := range addresses {
		_, exists := s.addresses[addr]
		if exists {
			continue
		}
		c, cancel := context.WithTimeout(s.c, 30*time.Second)
		defer cancel()
		_, err := s.request(c, addr, "/hi", nil, counter)
		if err != nil {
			log.Println(err)
			continue
		}
		s.addresses[addr] = peer
		s.incoming <- types.IncomingMessage{
			HostName: hostname,
			Event:    fmt.Appendf(nil, `{"name":"show-text",data:"connected to %s"}`, peer.Hostname),
		}
	}
	s.addresses[addr] = &peer{
		Counter:  peerCounter,
		Hostname: hostname,
		address:  addr,
	}
	s.incoming <- types.IncomingMessage{
		HostName: hostname,
		Event:    fmt.Appendf(nil, `{"name":"show-text",data:"connected to %s"}`, hostname),
	}
	return nil
}

func (s *Server) Shutdown() {
	s.broadcast(func(c context.Context, p *peer, _ uint64) error {
		_, err := s.request(c, p.address, "/bye", nil, math.MaxUint64)
		return err
	})
}

func (s *Server) BroadcastEvents(outgoing chan []byte) {
	for event := range outgoing {
		go s.broadcast(func(c context.Context, p *peer, counter uint64) error {
			_, err := s.request(c, p.address, "/event", event, counter)
			return err
		})
	}
}

func (s *Server) broadcast(f func(context.Context, *peer, uint64) error) {
	wg := sync.WaitGroup{}
	defer wg.Wait()

	s.mu.Lock()
	s.counter += 1
	counter := s.counter
	s.mu.Unlock()

	s.mu.RLock()
	defer s.mu.RUnlock()

	wg.Add(len(s.addresses))
	c, cancel := context.WithTimeout(s.c, 10*time.Second)
	defer cancel()
	for _, peer := range s.addresses {
		go func() {
			defer wg.Done()
			err := f(c, peer, counter)
			if err != nil {
				log.Printf("%s: %s\n", peer.String(), err)
			}
		}()
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

func New(c context.Context, incoming chan types.IncomingMessage, myAddress string, hostname string) *Server {
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
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
