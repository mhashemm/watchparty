package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"hash/maphash"
	"log"
	"maps"
	"net"
	"net/http"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/mhashemm/watchparty/mpv"
)

const (
	secretHeaderKey = "room-secret"

	sendBuffer    = 64
	pingInterval  = 30 * time.Second
	readLimit     = 4096
	limiterWindow = time.Hour

	rows = 4
	cols = 16384
)

// declared again in the node's ws.go; both are package main so neither can import
// the other, and four lines beat a package that exists only to share them
type message struct {
	Hostname string          `json:"hostname"`
	Event    json.RawMessage `json:"event"`
}

// ponytail: count-min sketch, fixed 128KB; collisions over-count so a busy shared NAT
// may hit the cap early. Bump cols if that shows up in practice.
type limiter struct {
	mu     sync.Mutex
	seeds  [rows]maphash.Seed
	counts [rows][cols]uint16
	window time.Time
}

func newLimiter() *limiter {
	l := &limiter{window: time.Now()}
	for i := range l.seeds {
		l.seeds[i] = maphash.MakeSeed()
	}
	return l
}

func (l *limiter) allow(key string, cap int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Since(l.window) > limiterWindow {
		for i := range l.counts {
			clear(l.counts[i][:])
		}
		l.window = time.Now()
	}
	est := uint16(0xffff)
	for i := range l.counts {
		h := maphash.String(l.seeds[i], key) % cols
		if l.counts[i][h] < 0xffff {
			l.counts[i][h]++
		}
		est = min(est, l.counts[i][h])
	}
	return int(est) <= cap
}

type member struct {
	hostname string
	ch       chan []byte
	conn     *websocket.Conn
}

type room struct {
	secret  []byte
	members map[*member]struct{}
}

type relay struct {
	mu    sync.Mutex
	rooms map[string]*room

	limiter  *limiter
	conns    atomic.Int64
	perHour  int
	maxRooms int
	maxConns int64
	trusted  bool
}

func (r *relay) ip(req *http.Request) string {
	if r.trusted {
		// last element, not first: with one proxy appending, the last entry is the
		// proxy's own view of the direct peer and a client cannot forge it
		if fwd := req.Header.Values("X-Forwarded-For"); len(fwd) > 0 {
			parts := strings.Split(strings.Join(fwd, ","), ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
				return last
			}
		}
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

func notice(hostname, text string) []byte {
	event, _ := json.Marshal(mpv.Event{Name: "show-text", Data: text})
	b, _ := json.Marshal(message{Hostname: hostname, Event: event})
	return b
}

func (r *relay) broadcast(name string, from *member, b []byte) {
	r.mu.Lock()
	rm, ok := r.rooms[name]
	if !ok {
		r.mu.Unlock()
		return
	}
	members := slices.Collect(maps.Keys(rm.members))
	r.mu.Unlock()

	for _, m := range members {
		if m == from {
			continue
		}
		select {
		case m.ch <- b:
		default:
			// 64 frames behind is not a slow client, it is a dead one
			log.Printf("%s: send buffer full, dropping\n", m.hostname)
			m.conn.CloseNow()
		}
	}
}

// admit enforces the caps in cost order, creating the room if it is new. It runs
// before the upgrade so a refused caller never becomes a websocket.
func (r *relay) admit(name string, secret []byte, ip string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	rm, exists := r.rooms[name]
	if exists {
		if subtle.ConstantTimeCompare(rm.secret, secret) != 1 {
			// a wrong secret is the only lock on the door, so grind attempts cost
			// quota and eventually stop being answered at all
			if !r.limiter.allow(ip, r.perHour) {
				return http.StatusTooManyRequests, fmt.Errorf("too many failed attempts")
			}
			return http.StatusForbidden, fmt.Errorf("bad secret")
		}
		return 0, nil
	}
	if !r.limiter.allow(ip, r.perHour) {
		return http.StatusTooManyRequests, fmt.Errorf("room creation rate limit")
	}
	if len(r.rooms) >= r.maxRooms {
		return http.StatusServiceUnavailable, fmt.Errorf("relay is full")
	}
	r.rooms[name] = &room{secret: secret, members: map[*member]struct{}{}}
	return 0, nil
}

// add registers the member only once its conn is set, so broadcast can never
// reach a member whose conn is still nil. It returns who was already in the room.
func (r *relay) add(name string, m *member) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	rm, ok := r.rooms[name]
	if !ok {
		return nil
	}
	present := make([]string, 0, len(rm.members))
	for other := range rm.members {
		present = append(present, other.hostname)
	}
	rm.members[m] = struct{}{}
	return present
}

// leave removes m and reaps the room once it is empty. A nil m only reaps.
func (r *relay) leave(name string, m *member) {
	r.mu.Lock()
	rm, ok := r.rooms[name]
	if !ok {
		r.mu.Unlock()
		return
	}
	if m != nil {
		delete(rm.members, m)
	}
	if len(rm.members) == 0 {
		delete(r.rooms, name)
	}
	r.mu.Unlock()
	if m != nil {
		r.broadcast(name, m, notice(m.hostname, fmt.Sprintf("%s has left", m.hostname)))
	}
}

func (r *relay) write(c context.Context, m *member) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.Done():
			return
		case b := <-m.ch:
			ctx, cancel := context.WithTimeout(c, 10*time.Second)
			err := m.conn.Write(ctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				return
			}
		case <-ticker.C:
			// a party sits idle for hours between pauses; without this, NAT and
			// proxy idle timeouts kill the connection silently
			ctx, cancel := context.WithTimeout(c, 10*time.Second)
			err := m.conn.Ping(ctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (r *relay) handle(res http.ResponseWriter, req *http.Request) {
	// reserve first: a flood would otherwise all pass a Load() check before any of
	// them incremented
	if r.conns.Add(1) > r.maxConns {
		r.conns.Add(-1)
		http.Error(res, "too many connections", http.StatusServiceUnavailable)
		return
	}
	defer r.conns.Add(-1)

	name := req.URL.Query().Get("room")
	hostname := req.URL.Query().Get("hostname")
	if name == "" || hostname == "" {
		http.Error(res, "room and hostname are required", http.StatusBadRequest)
		return
	}
	ip := r.ip(req)

	code, err := r.admit(name, []byte(req.Header.Get(secretHeaderKey)), ip)
	if err != nil {
		log.Printf("%s %s: %s\n", ip, name, err)
		http.Error(res, err.Error(), code)
		return
	}

	m := &member{hostname: hostname, ch: make(chan []byte, sendBuffer)}
	conn, err := websocket.Accept(res, req, nil)
	if err != nil {
		// admit may have just created the room; drop it again if it is still empty
		r.leave(name, nil)
		log.Printf("%s: %s\n", ip, err)
		return
	}
	conn.SetReadLimit(readLimit)
	m.conn = conn
	present := r.add(name, m)

	c, cancel := context.WithCancel(req.Context())
	defer func() {
		cancel()
		conn.CloseNow()
		r.leave(name, m)
	}()

	go r.write(c, m)
	log.Printf("%s joined %s from %s\n", hostname, name, ip)
	// the room learns about the joiner, and the joiner learns about the room --
	// without the second half the last one in would think it was alone
	r.broadcast(name, m, notice(hostname, fmt.Sprintf("%s has joined", hostname)))
	for _, other := range present {
		select {
		case m.ch <- notice(other, fmt.Sprintf("connected to %s", other)):
		default:
		}
	}

	for {
		_, b, err := conn.Read(c)
		if err != nil {
			log.Printf("%s: %s\n", hostname, err)
			return
		}
		r.broadcast(name, m, b)
	}
}

func main() {
	c, cancel := signal.NotifyContext(context.TODO(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	addr := flag.String("addr", ":8080", "listen address")
	roomsPerHour := flag.Int("roomsPerHour", 10, "new rooms one ip may create per hour")
	maxRooms := flag.Int("maxRooms", 10000, "rooms held at once before new ones are refused")
	maxConns := flag.Int64("maxConns", 5000, "connections held at once before new ones are refused")
	trustProxy := flag.Bool("trustProxy", false, "take the client ip from the last X-Forwarded-For element")
	flag.Parse()

	r := &relay{
		rooms:    map[string]*room{},
		limiter:  newLimiter(),
		perHour:  *roomsPerHour,
		maxRooms: *maxRooms,
		maxConns: *maxConns,
		trusted:  *trustProxy,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", r.handle)
	s := &http.Server{
		Addr:    *addr,
		Handler: mux,
		// no ReadTimeout on purpose: it would put an absolute deadline on the
		// hijacked conn and kill every long-lived websocket
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8192,
		BaseContext:       func(net.Listener) context.Context { return c },
	}
	go func() {
		<-c.Done()
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		s.Shutdown(shutdown)
	}()

	log.Printf("relay listening on %s\n", *addr)
	log.Println(s.ListenAndServe())
}
