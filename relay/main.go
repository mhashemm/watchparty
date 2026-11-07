package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"hash/maphash"
	"log"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/coder/websocket"
	"github.com/mhashemm/watchparty/mpv"
)

const (
	secretHeaderKey  = "room-secret"
	createHeaderKey  = "create-key"
	openedHeaderKey  = "room-opened"
	waitingHeaderKey = "room-waiting"

	// close codes for a member held until the room opens, declared again in party/ws.go
	statusRoomOpen   websocket.StatusCode = 4001
	statusNotOpened  websocket.StatusCode = 4002
	waitForKeyHolder                      = 30 * time.Minute

	sendBuffer    = 64
	pingInterval  = 30 * time.Second
	writeTimeout  = 10 * time.Second
	readLimit     = 4096
	limiterWindow = time.Hour

	rows = 4
	cols = 1 << 20
)

// declared again in party/ws.go; importing party would drag upnp, server and mpv into
// the relay binary, and four lines beat a package that exists only to share them
type message struct {
	Hostname string          `json:"hostname"`
	Event    json.RawMessage `json:"event"`
}

// count-min sketch, a fixed 8MB however many addresses it sees
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
	id       string
	hostname string
	session  string
	ch       chan []byte
	conn     *websocket.Conn
	log      *slog.Logger
}

type room struct {
	secret  []byte
	file    string
	members map[*member]struct{}
}

// opening is a remembered room that keyless members are waiting in; open closes when a
// key holder creates it.
type opening struct {
	open    chan struct{}
	waiting int
}

type relay struct {
	mu      sync.Mutex
	rooms   map[string]*room
	opening map[string]*opening

	limiter    *limiter
	createKey  []byte
	remembered *remembered
	conns      atomic.Int64
	perHour    int
	maxRooms   int
	maxConns   int64
	maxWaiting int
	trusted    bool
	ping       time.Duration
	timeout    time.Duration
	waitFor    time.Duration
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

func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
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
			m.log.Warn("send buffer full, closing")
			m.conn.CloseNow()
		}
	}
}

// admit enforces the caps in cost order, creating the room if it is new. It runs
// before the upgrade so a refused caller never becomes a websocket. old is the room as
// it was remembered: a keyless member of it that can wait gets a channel that closes
// when a key holder opens it, since only a key holder creates a room. Whoever gets one
// must call stopWaiting.
func (r *relay) admit(name string, secret, key []byte, file, ip string, old *known, canWait bool) (bool, <-chan struct{}, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	rm, exists := r.rooms[name]
	if exists {
		if subtle.ConstantTimeCompare(rm.secret, secret) != 1 {
			// a wrong secret is the only lock on the door, so grind attempts cost
			// quota and eventually stop being answered at all
			if !r.limiter.allow(ip, r.perHour) {
				return false, nil, http.StatusTooManyRequests, fmt.Errorf("too many failed attempts")
			}
			return false, nil, http.StatusForbidden, fmt.Errorf("bad secret")
		}
		if rm.file != file {
			return false, nil, http.StatusConflict, fmt.Errorf("not the same file")
		}
		return false, nil, 0, nil
	}
	if r.createKey != nil && len(key) == 0 {
		// no key for a room never opened is a member early for the party, not a
		// guess, so it costs no quota and the node keeps retrying
		if old == nil || !canWait {
			return false, nil, http.StatusUnauthorized, fmt.Errorf("room not open yet")
		}
		sum := sha256.Sum256(secret)
		if subtle.ConstantTimeCompare(old.secret[:], sum[:]) != 1 {
			if !r.limiter.allow(ip, r.perHour) {
				return false, nil, http.StatusTooManyRequests, fmt.Errorf("too many failed attempts")
			}
			return false, nil, http.StatusForbidden, fmt.Errorf("bad secret")
		}
		if old.file != file {
			return false, nil, http.StatusConflict, fmt.Errorf("not the same file")
		}
		o, ok := r.opening[name]
		if !ok {
			o = &opening{open: make(chan struct{})}
			r.opening[name] = o
		}
		// anyone with the secret could otherwise hold MAX_CONNS connections for 30 minutes each
		if o.waiting >= r.maxWaiting {
			return false, nil, http.StatusServiceUnavailable, fmt.Errorf("too many waiting for this room")
		}
		o.waiting++
		return false, o.open, 0, nil
	}
	if !r.limiter.allow(ip, r.perHour) {
		return false, nil, http.StatusTooManyRequests, fmt.Errorf("room creation rate limit")
	}
	if r.createKey != nil && subtle.ConstantTimeCompare(r.createKey, key) != 1 {
		return false, nil, http.StatusForbidden, fmt.Errorf("bad create key")
	}
	if len(r.rooms) >= r.maxRooms {
		return false, nil, http.StatusServiceUnavailable, fmt.Errorf("relay is full")
	}
	r.rooms[name] = &room{secret: secret, file: file, members: map[*member]struct{}{}}
	if o, ok := r.opening[name]; ok {
		close(o.open)
		delete(r.opening, name)
	}
	return true, nil, 0, nil
}

func (r *relay) stopWaiting(name string, open <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if o, ok := r.opening[name]; ok && o.open == open {
		if o.waiting--; o.waiting == 0 {
			delete(r.opening, name)
		}
	}
}

// hold keeps a keyless member of a closed room until a key holder opens it, then tells
// it to join; after waitFor it tells it the room didn't open. It relays nothing.
func (r *relay) hold(c context.Context, conn *websocket.Conn, open <-chan struct{}, l *slog.Logger) {
	c = conn.CloseRead(c)
	timer, ticker := time.NewTimer(r.waitFor), time.NewTicker(r.ping)
	defer timer.Stop()
	defer ticker.Stop()
	start := time.Now()
	for {
		select {
		case <-open:
			l.Info("room opened, sent to join", "seconds", int(time.Since(start).Seconds()))
			conn.Close(statusRoomOpen, "room open")
			return
		case <-timer.C:
			l.Info("room did not open", "seconds", int(time.Since(start).Seconds()))
			conn.Close(statusNotOpened, "room did not open")
			return
		case <-c.Done():
			l.Info("stopped waiting", "seconds", int(time.Since(start).Seconds()))
			return
		case <-ticker.C:
			// hours behind NAT and proxies are no different from a member's
			ctx, cancel := context.WithTimeout(c, r.timeout)
			err := conn.Ping(ctx)
			cancel()
			if err != nil {
				l.Info("stopped waiting", "reason", err)
				conn.CloseNow()
				return
			}
		}
	}
}

// add registers the member only once its conn is set, so broadcast can never
// reach a member whose conn is still nil. It returns the stale member m replaced
// if this is the same node reconnecting, and false if the room was reaped after
// m's admit. The notices are queued under the same
// lock, so "has joined" reaches exactly the members m found, and they are the
// ones m is told it is connected to.
func (r *relay) add(name string, m *member) (*member, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rm, ok := r.rooms[name]
	if !ok {
		return nil, false
	}
	var stale *member
	for other := range rm.members {
		if m.session != "" && other.session == m.session {
			delete(rm.members, other)
			stale = other
		}
	}
	// a reconnect is not news to the room, which never heard it leave
	joined := notice(m.hostname, fmt.Sprintf("%s has joined", m.hostname))
	for other := range rm.members {
		if stale == nil {
			queue(other, joined)
		}
		queue(m, notice(other.hostname, fmt.Sprintf("connected to %s", other.hostname)))
	}
	rm.members[m] = struct{}{}
	return stale, true
}

// queue drops a notice rather than block under r.mu; notices are cosmetic
func queue(m *member, b []byte) {
	select {
	case m.ch <- b:
	default:
	}
}

// leave removes m and reaps the room once it is empty. A nil m only reaps, and so
// does an m that add already replaced: the room was never told it left. It reports
// whether m was still a member.
func (r *relay) leave(name string, m *member) bool {
	r.mu.Lock()
	rm, ok := r.rooms[name]
	if !ok {
		r.mu.Unlock()
		return false
	}
	_, present := rm.members[m]
	delete(rm.members, m)
	reaped := len(rm.members) == 0
	if reaped {
		delete(r.rooms, name)
	}
	r.mu.Unlock()
	if reaped && r.remembered != nil {
		if err := r.remembered.touch(name); err != nil {
			slog.Error("remembering a room failed", "room", clean(name), "err", err)
		}
	}
	if present {
		r.broadcast(name, m, notice(m.hostname, fmt.Sprintf("%s has left", m.hostname)))
	}
	return present
}

func (r *relay) counts(name string) (members, rooms int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rm, ok := r.rooms[name]; ok {
		members = len(rm.members)
	}
	return members, len(r.rooms)
}

func (r *relay) write(c context.Context, m *member) {
	ticker := time.NewTicker(r.ping)
	defer ticker.Stop()
	defer m.conn.CloseNow()
	for {
		select {
		case <-c.Done():
			return
		case b := <-m.ch:
			ctx, cancel := context.WithTimeout(c, r.timeout)
			err := m.conn.Write(ctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				m.log.Info("write failed", "err", err)
				return
			}
		case <-ticker.C:
			// a party sits idle for hours between pauses; without this, NAT and
			// proxy idle timeouts kill the connection silently
			ctx, cancel := context.WithTimeout(c, r.timeout)
			err := m.conn.Ping(ctx)
			cancel()
			if err != nil {
				m.log.Info("ping failed", "err", err)
				return
			}
		}
	}
}

func refuse(res http.ResponseWriter, l *slog.Logger, code int, reason string) {
	l.Info("refused", "status", code, "reason", reason)
	http.Error(res, reason, code)
}

func (r *relay) handle(res http.ResponseWriter, req *http.Request) {
	// random, not a counter: the same id must not mean two connections across a restart
	id := strconv.FormatUint(rand.Uint64(), 36)
	// reserve first: a flood would otherwise all pass a Load() check before any of
	// them incremented
	if r.conns.Add(1) > r.maxConns {
		r.conns.Add(-1)
		refuse(res, slog.With("conn", id, "ip", r.ip(req)), http.StatusServiceUnavailable, "too many connections")
		return
	}
	defer r.conns.Add(-1)

	name := req.URL.Query().Get("room")
	hostname := clean(req.URL.Query().Get("hostname"))
	ip := r.ip(req)
	l := slog.With("conn", id, "ip", ip, "room", clean(name), "hostname", hostname)
	if name == "" || hostname == "" {
		refuse(res, l, http.StatusBadRequest, "room and hostname are required")
		return
	}

	secret, key, file := []byte(req.Header.Get(secretHeaderKey)), []byte(req.Header.Get(createHeaderKey)), req.URL.Query().Get("file")
	var old *known
	if r.remembered != nil && r.createKey != nil && len(key) == 0 {
		// read before admit takes r.mu, so a free keyless retry never holds every room up on the disk
		var err error
		if old, err = r.remembered.lookup(name); err != nil {
			l.Error("reading remembered rooms failed", "err", err)
			refuse(res, l, http.StatusServiceUnavailable, "relay storage failed")
			return
		}
	}
	created, open, code, err := r.admit(name, secret, key, file, ip, old, req.URL.Query().Get("wait") == "1")
	if err != nil {
		refuse(res, l, code, err.Error())
		return
	}
	if created && r.remembered != nil {
		if err := r.remembered.remember(name, secret, file); err != nil {
			l.Error("remembering a room failed", "err", err)
		}
	}
	if open != nil {
		defer r.stopWaiting(name, open)
		res.Header().Set(waitingHeaderKey, "yes")
		conn, err := websocket.Accept(res, req, nil)
		if err != nil {
			l.Info("upgrade failed", "err", err)
			return
		}
		l.Info("waiting for a key holder")
		r.hold(req.Context(), conn, open, l)
		return
	}
	// a node told it opened the room answers joiners with its position: only a key
	// holder creates a room, and on a relay without CREATE_KEY nobody's is the party's
	if created && r.createKey != nil {
		res.Header().Set(openedHeaderKey, "yes")
	}

	m := &member{id: id, hostname: hostname, session: req.URL.Query().Get("session"), ch: make(chan []byte, sendBuffer), log: l}
	conn, err := websocket.Accept(res, req, nil)
	if err != nil {
		// admit may have just created the room; drop it again if it is still empty
		r.leave(name, nil)
		l.Info("upgrade failed", "err", err)
		return
	}
	conn.SetReadLimit(readLimit)
	m.conn = conn
	stale, ok := r.add(name, m)
	if !ok {
		// the redial goes through admit again, which recreates the room or waits
		l.Info("room closed before the join landed, sent back to redial")
		conn.Close(websocket.StatusTryAgainLater, "room closed")
		return
	}
	replaces := ""
	if stale != nil {
		stale.conn.CloseNow()
		replaces = stale.id
	}

	start, frames := time.Now(), 0
	var readErr error
	c, cancel := context.WithCancel(req.Context())
	defer func() {
		cancel()
		conn.CloseNow()
		msg := "replaced by its reconnect"
		if r.leave(name, m) {
			msg = "left"
		}
		members, rooms := r.counts(name)
		l.Info(msg, "seconds", int(time.Since(start).Seconds()), "frames", frames, "reason", readErr, "members", members, "rooms", rooms)
	}()

	go r.write(c, m)
	members, rooms := r.counts(name)
	l.Info("joined", "replaces", replaces, "members", members, "rooms", rooms)

	for {
		var b []byte
		_, b, readErr = conn.Read(c)
		if readErr != nil {
			return
		}
		frames++
		r.broadcast(name, m, b)
	}
}

func str(s string) (string, error) { return s, nil }

func env[T any](key string, def T, parse func(string) (T, error)) T {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	t, err := parse(v)
	if err != nil {
		log.Fatalf("%s=%q: %v", key, v, err)
	}
	return t
}

func main() {
	c, cancel := signal.NotifyContext(context.TODO(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// also routes the log package, so net/http's own errors land here too
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	addr := env("ADDR", ":8080", str)
	r := &relay{
		rooms:      map[string]*room{},
		limiter:    newLimiter(),
		perHour:    env("ROOMS_PER_HOUR", 10, strconv.Atoi),
		maxRooms:   env("MAX_ROOMS", 10000, strconv.Atoi),
		maxConns:   int64(env("MAX_CONNS", 5000, strconv.Atoi)),
		maxWaiting: env("MAX_WAITING", 20, strconv.Atoi),
		trusted:    env("TRUST_PROXY", false, strconv.ParseBool),
		opening:    map[string]*opening{},
		ping:       pingInterval,
		timeout:    writeTimeout,
		waitFor:    waitForKeyHolder,
	}
	if k := env("CREATE_KEY", "", str); k != "" {
		r.createKey = []byte(k)
		// without a key anyone may open any room, so there is nothing to bring back
		db := env("DB", "relay.db", str)
		var err error
		if r.remembered, err = openRemembered(db); err != nil {
			log.Fatalf("DB=%q: %v", db, err)
		}
		defer r.remembered.db.Close()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", r.handle)
	s := &http.Server{
		Addr:    addr,
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

	slog.Info("listening", "addr", addr, "create_key", r.createKey != nil, "remembers_rooms", r.remembered != nil, "trust_proxy", r.trusted,
		"rooms_per_hour", r.perHour, "max_rooms", r.maxRooms, "max_conns", r.maxConns, "max_waiting", r.maxWaiting)
	slog.Info("stopped", "reason", s.ListenAndServe())
}
