package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"net/http"
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
	fileHeaderKey     = "file"
	secretHeaderKey   = "secret"

	maxFails = 3
	maxBody  = 64 << 10
)

var (
	ErrDifferentFile = errors.New("your video is not the same file as the party's, or only one of you passed -playlist")
	ErrWrongSecret   = errors.New("the party refused your secret")
)

type peer struct {
	Counter      uint64     `json:"counter,string"`
	Hostname     string     `json:"hostname"`
	mu           sync.Mutex `json:"-"`
	address      string     `json:"-"`
	fails        int        `json:"-"`
	failingSince time.Time  `json:"-"`
	// reached is open while an event this node sent failed to reach the peer, and is
	// closed by the next that does
	reached chan struct{} `json:"-"`
	tried   time.Time     `json:"-"`
}

func (p *peer) String() string {
	return fmt.Sprintf("%s (%s)", p.address, p.Hostname)
}

type Server struct {
	c          context.Context
	addresses  map[string]*peer
	mu         sync.RWMutex
	incoming   chan<- mpv.IncomingMessage
	client     *http.Client
	myAddress  string
	counter    uint64
	hostname   string
	file       string
	secret     string
	evictAfter time.Duration
	// chaseFrom and chaseTo bound how often a peer is sent this node's state again
	chaseFrom, chaseTo time.Duration
}

func (s *Server) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get(secretHeaderKey)), []byte(s.secret)) != 1 {
			log.Printf("refused %s: wrong secret\n", req.Header.Get(addressHeaderKey))
			res.WriteHeader(http.StatusForbidden)
			return
		}
		req.Body = http.MaxBytesReader(res, req.Body, maxBody)
		next.ServeHTTP(res, req)
	})
}

// notify shows a notice about the peer at from: a leave tells its followers their leader left,
// whatever its name.
func (s *Server) notify(hostname, from, text string) {
	event, _ := json.Marshal(mpv.Event{Name: "show-text", Data: text})
	select {
	case s.incoming <- mpv.IncomingMessage{HostName: hostname, From: from, Event: event}:
	default:
	}
}

func (s *Server) Hi(res http.ResponseWriter, req *http.Request) {
	addr := req.Header.Get(addressHeaderKey)
	counter, _ := strconv.ParseUint(req.Header.Get(counterHeaderKey), 10, 64)
	hostname := req.Header.Get(hostnameHeaderKey)
	if req.Header.Get(fileHeaderKey) != s.file {
		log.Printf("refused %s with ip %s: not the same file\n", hostname, addr)
		res.WriteHeader(http.StatusConflict)
		return
	}
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
	s.notify(hostname, addr, hostname+mpv.HasJoined)
}

func (s *Server) Event(res http.ResponseWriter, req *http.Request) {
	addr := req.Header.Get(addressHeaderKey)
	counter, _ := strconv.ParseUint(req.Header.Get(counterHeaderKey), 10, 64)
	body, err := io.ReadAll(req.Body)
	if err != nil {
		log.Printf("%s: %s\n", addr, err)
		if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
			res.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
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
	case s.incoming <- mpv.IncomingMessage{HostName: hostname, From: addr, Event: body}:
		res.WriteHeader(http.StatusNoContent)
	case <-req.Context().Done():
		res.WriteHeader(http.StatusServiceUnavailable)
	}
}

func (s *Server) Bye(res http.ResponseWriter, req *http.Request) {
	addr := req.Header.Get(addressHeaderKey)
	counter, _ := strconv.ParseUint(req.Header.Get(counterHeaderKey), 10, 64)
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
	s.notify(peer.Hostname, addr, peer.Hostname+mpv.HasLeft)
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
	s.add(p)

	s.mu.RLock()
	for a := range addresses {
		_, exists := s.addresses[a]
		if exists || a == addr || a == s.myAddress {
			delete(addresses, a)
		}
	}
	s.mu.RUnlock()

	// together: one by one, each peer that vanished without a /bye held the join up
	// for its own timeout
	var wg sync.WaitGroup
	for a := range addresses {
		wg.Go(func() {
			p, _, err := s.hi(a, counter)
			if err != nil {
				log.Println(err)
				return
			}
			s.add(p)
		})
	}
	wg.Wait()
	return nil
}

func (s *Server) add(p *peer) {
	s.mu.Lock()
	s.addresses[p.address] = p
	s.mu.Unlock()
	s.notify(p.Hostname, p.address, mpv.ConnectedTo+p.Hostname)
}

func (s *Server) Shutdown() {
	s.broadcast(context.WithoutCancel(s.c), false, func(c context.Context, p *peer, _ uint64) error {
		return s.post(c, p.address, "/bye", nil, math.MaxUint64)
	})
}

func (s *Server) BroadcastEvents(outgoing chan []byte) {
	for {
		var event []byte
		select {
		case event = <-outgoing:
		case <-s.c.Done():
			return
		}
		s.broadcast(s.c, true, func(c context.Context, p *peer, counter uint64) error {
			return s.track(p, s.post(c, p.address, "/event", event, counter))
		})
	}
}

// track follows whether a peer has every event this node sent. Each is the sender's whole
// state, so the first that fails leaves the peer behind on what the party holds: mpv is
// told, and asked to send its state again (chase) until one gets through.
func (s *Server) track(p *peer, err error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tried = time.Now()
	switch {
	case err != nil && p.reached == nil:
		p.reached = make(chan struct{})
		go s.chase(p, p.reached)
	case err == nil && p.reached != nil:
		close(p.reached)
		p.reached = nil
	}
	return err
}

// chase tells mpv a peer missed something, then, each time it waited longer, sends the peer
// its state again, until a send reaches it, it is dropped, or the party ends. Not at once:
// that would fail as the send did. A wait in which this node sent the peer something
// anyway, which carried its state, sends nothing: each send to a peer that is down is a
// strike. Not from the send that failed: BroadcastEvents is waiting for it, and mpv can be
// blocked on outgoing meanwhile.
func (s *Server) chase(p *peer, reached chan struct{}) {
	if !s.tell(mpv.IncomingMessage{HostName: p.Hostname, From: p.address, Missed: true}) {
		return
	}
	for wait := s.chaseFrom; ; wait = min(wait*2, s.chaseTo) {
		select {
		case <-reached:
			return
		case <-time.After(wait):
		case <-s.c.Done():
			return
		}
		s.mu.RLock()
		gone := s.addresses[p.address] != p
		s.mu.RUnlock()
		p.mu.Lock()
		quiet := time.Since(p.tried) >= wait
		p.mu.Unlock()
		if gone || quiet && !s.resend(p) {
			return
		}
	}
}

// resend asks mpv for its state and sends it to p alone: the rest of the party has it, and
// to a member holding it, a resume sent again says it got the first one late.
func (s *Server) resend(p *peer) bool {
	reply := make(chan []byte, 1)
	if !s.tell(mpv.IncomingMessage{HostName: p.Hostname, From: p.address, Resend: true, Reply: reply}) {
		return false
	}
	var event []byte
	select {
	case event = <-reply:
	case <-s.c.Done():
		return false
	}
	if event == nil {
		return true
	}
	s.mu.Lock()
	s.counter += 1
	counter := s.counter
	s.mu.Unlock()
	c, cancel := context.WithTimeout(s.c, 10*time.Second)
	defer cancel()
	s.record(p, s.track(p, s.post(c, p.address, "/event", event, counter)))
	return true
}

func (s *Server) tell(m mpv.IncomingMessage) bool {
	select {
	case s.incoming <- m:
		return true
	case <-s.c.Done():
		return false
	}
}

// broadcast waits for every peer, except that with detach it does not wait for a
// peer already at maxFails: it may be kept for evictAfter, and the next event must
// not queue behind its timeout for that long.
func (s *Server) broadcast(parent context.Context, detach bool, f func(context.Context, *peer, uint64) error) {
	s.mu.Lock()
	s.counter += 1
	counter := s.counter
	peers := slices.Collect(maps.Values(s.addresses))
	s.mu.Unlock()

	wg := sync.WaitGroup{}
	for _, p := range peers {
		send := func() {
			c, cancel := context.WithTimeout(parent, 10*time.Second)
			defer cancel()
			s.record(p, f(c, p, counter))
		}
		p.mu.Lock()
		suspect := p.fails >= maxFails
		p.mu.Unlock()
		if detach && suspect {
			go send()
			continue
		}
		wg.Go(send)
	}
	wg.Wait()
}

func (s *Server) record(p *peer, err error) {
	p.mu.Lock()
	if err == nil {
		p.fails = 0
		p.failingSince = time.Time{}
		p.mu.Unlock()
		return
	}
	if p.fails == 0 {
		p.failingSince = time.Now()
	}
	p.fails += 1
	dropped := p.fails >= maxFails && time.Since(p.failingSince) >= s.evictAfter
	p.mu.Unlock()

	log.Printf("%s: %s\n", p, err)
	if !dropped {
		return
	}
	s.mu.Lock()
	evicted := s.addresses[p.address] == p
	if evicted {
		delete(s.addresses, p.address)
	}
	s.mu.Unlock()
	if evicted {
		s.notify(p.Hostname, p.address, mpv.Dropped+p.String())
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
	req.Header.Add(fileHeaderKey, s.file)
	req.Header.Add(secretHeaderKey, s.secret)
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusConflict {
		res.Body.Close()
		return nil, fmt.Errorf("%s%s: %w", addr, endpoint, ErrDifferentFile)
	}
	if res.StatusCode == http.StatusForbidden {
		res.Body.Close()
		return nil, fmt.Errorf("%s%s: %w", addr, endpoint, ErrWrongSecret)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		res.Body.Close()
		return nil, fmt.Errorf("%s%s: %s", addr, endpoint, res.Status)
	}
	return res, nil
}

func (s *Server) post(c context.Context, addr string, endpoint string, data []byte, counter uint64) error {
	res, err := s.request(c, addr, endpoint, data, counter)
	if err != nil {
		return err
	}
	return res.Body.Close()
}

func New(c context.Context, incoming chan mpv.IncomingMessage, myAddress string, hostname string, file string, secret string, evictAfter time.Duration) *Server {
	return &Server{
		c:          c,
		incoming:   incoming,
		client:     &http.Client{},
		addresses:  map[string]*peer{},
		myAddress:  myAddress,
		hostname:   hostname,
		file:       file,
		secret:     secret,
		evictAfter: evictAfter,
		chaseFrom:  time.Second,
		chaseTo:    30 * time.Second,
	}
}
