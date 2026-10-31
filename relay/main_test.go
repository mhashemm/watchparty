package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func newRelay(perHour, maxRooms int, maxConns int64) *relay {
	return &relay{
		rooms:    map[string]*room{},
		limiter:  newLimiter(),
		perHour:  perHour,
		maxRooms: maxRooms,
		maxConns: maxConns,
		ping:     pingInterval,
		timeout:  writeTimeout,
	}
}

// relays maps each served url to its relay, so join can wait for registration
var relays sync.Map

func serve(t *testing.T, r *relay) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", r.handle)
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	url := "ws" + s.URL[len("http"):]
	relays.Store(url, r)
	return url
}

func members(r *relay, roomName string) []*member {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rm, ok := r.rooms[roomName]; ok {
		return slices.Collect(maps.Keys(rm.members))
	}
	return nil
}

// join returns once the relay has registered the member, not just once the
// handshake is done: add runs after Accept, so a frame sent in between misses it.
func join(t *testing.T, url, roomName, secret, hostname string, createKey ...string) (*websocket.Conn, int) {
	t.Helper()
	v, _ := relays.Load(url)
	r := v.(*relay)
	before := members(r, roomName)
	conn, res, err := websocket.Dial(context.Background(),
		url+"/ws?room="+roomName+"&hostname="+hostname,
		&websocket.DialOptions{HTTPHeader: http.Header{secretHeaderKey: {secret}, createHeaderKey: createKey}})
	if err != nil {
		if res != nil {
			return nil, res.StatusCode
		}
		t.Fatalf("dial %s: %s", roomName, err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	eventually(t, "the relay never registered "+hostname, func() bool {
		return slices.ContainsFunc(members(r, roomName), func(m *member) bool { return !slices.Contains(before, m) })
	})
	return conn, http.StatusSwitchingProtocols
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
	}
}

// mark sends a playback frame. Each member's queue is FIFO, so whatever the relay
// sent a member before the mark arrives before it: reading up to the mark proves
// an absence without waiting out a timeout.
func mark(t *testing.T, conn *websocket.Conn, from string) {
	t.Helper()
	b, _ := json.Marshal(message{Hostname: from, Event: json.RawMessage(`{"name":"mark"}`)})
	if err := conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// noticesBefore returns the notices that arrive ahead of the next playback frame.
func noticesBefore(t *testing.T, conn *websocket.Conn) []string {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := []string{}
	for {
		_, b, err := conn.Read(c)
		if err != nil {
			t.Fatalf("no playback frame after %v: %s", got, err)
		}
		m := message{}
		json.Unmarshal(b, &m)
		e := struct {
			Name string `json:"name"`
			Data string `json:"data"`
		}{}
		json.Unmarshal(m.Event, &e)
		if e.Name != "show-text" {
			return got
		}
		got = append(got, e.Data)
	}
}

// recvEvent reads until it sees a real playback frame, skipping the join/leave
// notices the relay synthesizes, or gives up after timeout.
func recvEvent(t *testing.T, conn *websocket.Conn, timeout time.Duration) (message, bool) {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		_, b, err := conn.Read(c)
		if err != nil {
			return message{}, false
		}
		m := message{}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("bad frame: %s", b)
		}
		e := struct {
			Name string `json:"name"`
		}{}
		json.Unmarshal(m.Event, &e)
		if e.Name == "show-text" {
			continue // join/leave notice
		}
		return m, true
	}
}

// The sender must not receive its own frame back. This is what makes the
// counter/staleness machinery unnecessary in relay mode; if it regresses, every
// event loops forever.
func TestBroadcastReachesEveryoneButTheSender(t *testing.T) {
	t.Parallel()
	url := serve(t, newRelay(10, 10, 10))
	a, _ := join(t, url, "movie", "s3cret", "a")
	b, _ := join(t, url, "movie", "s3cret", "b")
	c, _ := join(t, url, "movie", "s3cret", "c")

	sent, _ := json.Marshal(message{Hostname: "a", Event: json.RawMessage(`{"name":"pause","data":"yes"}`)})
	if err := a.Write(context.Background(), websocket.MessageText, sent); err != nil {
		t.Fatal(err)
	}

	for name, conn := range map[string]*websocket.Conn{"b": b, "c": c} {
		got, ok := recvEvent(t, conn, 2*time.Second)
		if !ok {
			t.Fatalf("%s never received a's event", name)
		}
		if got.Hostname != "a" || string(got.Event) != `{"name":"pause","data":"yes"}` {
			t.Fatalf("%s got %+v", name, got)
		}
	}

	mark(t, b, "b")
	if got, ok := recvEvent(t, a, 2*time.Second); !ok || got.Hostname != "b" {
		t.Fatalf("the sender received %+v before b's mark, want its own event never to come back", got)
	}
}

// The room hears about the joiner, and the joiner hears about the room. Without
// the second half the last node in would believe it was alone.
func TestJoinerLearnsWhoIsAlreadyThere(t *testing.T) {
	t.Parallel()
	url := serve(t, newRelay(10, 10, 10))
	a, _ := join(t, url, "movie", "s", "a")
	join(t, url, "movie", "s", "b")
	c, _ := join(t, url, "movie", "s", "c")

	// c's mark reaches a only from c's read loop, which starts after c's notices are queued
	mark(t, c, "c")
	noticesBefore(t, a)
	mark(t, a, "a")
	got := noticesBefore(t, c)
	for _, want := range []string{"connected to a", "connected to b"} {
		if !slices.Contains(got, want) {
			t.Fatalf("last joiner was not told about the room: got %v, want %q", got, want)
		}
	}
}

// A node whose connection died on its side but not on the relay's redials with the
// same session. The relay must swap it in silently rather than keep the stale
// member until the ping reaps it and tell the room it joined twice.
func TestReconnectReplacesStaleMember(t *testing.T) {
	t.Parallel()
	r := newRelay(10, 10, 10)
	url := serve(t, r)
	a, _ := join(t, url, "movie", "s", "a")
	stale, _ := join(t, url, "movie", "s", "b&session=x")
	fresh, _ := join(t, url, "movie", "s", "b&session=x")

	c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		if _, _, err := stale.Read(c); err != nil {
			if c.Err() != nil {
				t.Fatal("the stale connection was left open")
			}
			break
		}
	}
	// the stale handler's leave runs before it releases its conn slot
	eventually(t, "the stale handler never finished", func() bool { return r.conns.Load() == 2 })
	mark(t, fresh, "b")
	if got := noticesBefore(t, a); !slices.Equal(got, []string{"b has joined"}) {
		t.Fatalf("room was told %v, want one join and no leave", got)
	}
	mark(t, a, "a")
	if got := noticesBefore(t, fresh); !slices.Contains(got, "connected to a") || slices.Contains(got, "connected to b") {
		t.Fatalf("reconnected node was told %v, want a and not itself", got)
	}
}

func queued(m *member) []string {
	got := []string{}
	for {
		select {
		case b := <-m.ch:
			msg := message{}
			json.Unmarshal(b, &msg)
			e := struct {
				Data string `json:"data"`
			}{}
			json.Unmarshal(msg.Event, &e)
			got = append(got, e.Data)
		default:
			slices.Sort(got)
			return got
		}
	}
}

// "has joined" goes to the members add saw, so a later joiner is never told about
// someone it already found in the room, and a reconnect never hears of itself.
func TestJoinNoticesGoToTheRoomAddSaw(t *testing.T) {
	t.Parallel()
	r := newRelay(10, 10, 10)
	r.rooms["movie"] = &room{members: map[*member]struct{}{}}
	mk := func(hostname, session string) *member {
		return &member{hostname: hostname, session: session, ch: make(chan []byte, sendBuffer)}
	}
	b, a, c := mk("b", ""), mk("a", "x"), mk("c", "")
	r.add("movie", b)
	r.add("movie", a)
	r.add("movie", c)
	if got := queued(b); !slices.Equal(got, []string{"a has joined", "c has joined"}) {
		t.Fatalf("b got %v", got)
	}
	if got := queued(a); !slices.Equal(got, []string{"c has joined", "connected to b"}) {
		t.Fatalf("a got %v", got)
	}
	if got := queued(c); !slices.Equal(got, []string{"connected to a", "connected to b"}) {
		t.Fatalf("c got %v, want no join for someone already there", got)
	}

	fresh := mk("a", "x")
	if stale := r.add("movie", fresh); stale != a {
		t.Fatal("the reconnect did not replace the stale member")
	}
	if got := queued(fresh); !slices.Equal(got, []string{"connected to b", "connected to c"}) {
		t.Fatalf("reconnect got %v, want the room and not itself", got)
	}
	for _, m := range []*member{b, c} {
		if got := queued(m); len(got) != 0 {
			t.Fatalf("%s was told %v about a reconnect", m.hostname, got)
		}
	}
}

func TestWrongSecretIsRejected(t *testing.T) {
	t.Parallel()
	url := serve(t, newRelay(100, 10, 10))
	join(t, url, "movie", "right", "a")

	if _, code := join(t, url, "movie", "wrong", "b"); code != http.StatusForbidden {
		t.Fatalf("wrong secret got %d, want 403", code)
	}
	if _, code := join(t, url, "movie", "right", "b"); code != http.StatusSwitchingProtocols {
		t.Fatalf("right secret got %d, want an upgrade", code)
	}
}

// The room keeps its creator's file fingerprint, and a joiner playing another file
// is refused before the upgrade.
func TestDifferentFileIsRejected(t *testing.T) {
	t.Parallel()
	url := serve(t, newRelay(100, 10, 10))
	join(t, url, "movie", "s", "a&file=abc")

	if _, code := join(t, url, "movie", "s", "b&file=xyz"); code != http.StatusConflict {
		t.Fatalf("a different file got %d, want 409", code)
	}
	if _, code := join(t, url, "movie", "s", "b"); code != http.StatusConflict {
		t.Fatalf("no file got %d, want 409", code)
	}
	if _, code := join(t, url, "movie", "s", "b&file=abc"); code != http.StatusSwitchingProtocols {
		t.Fatalf("the same file got %d, want an upgrade", code)
	}
}

// Joining an existing room must not cost quota, or a party of eight would lock
// itself out.
func TestOnlyRoomCreationIsRateLimited(t *testing.T) {
	t.Parallel()
	url := serve(t, newRelay(2, 100, 100))

	for i := range 2 {
		if _, code := join(t, url, "room"+strconv.Itoa(i), "s", "a"); code != http.StatusSwitchingProtocols {
			t.Fatalf("room %d got %d, want an upgrade", i, code)
		}
	}
	if _, code := join(t, url, "room2", "s", "a"); code != http.StatusTooManyRequests {
		t.Fatalf("third new room got %d, want 429", code)
	}
	for i := range 2 {
		if _, code := join(t, url, "room"+strconv.Itoa(i), "s", "b"); code != http.StatusSwitchingProtocols {
			t.Fatalf("joining existing room %d got %d, want an upgrade", i, code)
		}
	}
}

func TestSecretGrindingIsRateLimited(t *testing.T) {
	t.Parallel()
	url := serve(t, newRelay(3, 100, 100))
	join(t, url, "movie", "right", "a")

	codes := map[int]int{}
	for range 10 {
		_, code := join(t, url, "movie", "wrong", "b")
		codes[code]++
	}
	if codes[http.StatusTooManyRequests] == 0 {
		t.Fatalf("brute force was never throttled: %v", codes)
	}
}

// Only the key holder opens rooms; everyone else joins with the room secret alone.
// A keyless join before the room exists is a member early for the party, so it
// must not cost quota, but a wrong key is a guess and must.
func TestCreateKey(t *testing.T) {
	t.Parallel()
	r := newRelay(2, 100, 100)
	r.createKey = []byte("k")
	url := serve(t, r)

	for range 5 {
		if _, code := join(t, url, "movie", "s", "b"); code != http.StatusUnauthorized {
			t.Fatalf("keyless create got %d, want 401", code)
		}
	}
	if _, code := join(t, url, "movie", "s", "a", "wrong"); code != http.StatusForbidden {
		t.Fatalf("wrong key got %d, want 403", code)
	}
	if _, code := join(t, url, "movie", "s", "a", "k"); code != http.StatusSwitchingProtocols {
		t.Fatalf("right key got %d after keyless retries, want an upgrade", code)
	}
	if _, code := join(t, url, "movie", "s", "b"); code != http.StatusSwitchingProtocols {
		t.Fatalf("keyless join of an open room got %d, want an upgrade", code)
	}
	if _, code := join(t, url, "other", "s", "a", "k"); code != http.StatusTooManyRequests {
		t.Fatalf("third create got %d, want 429: the wrong key must have cost quota", code)
	}
}

func TestMaxRoomsAndMaxConns(t *testing.T) {
	t.Parallel()
	r := newRelay(100, 1, 100)
	url := serve(t, r)
	join(t, url, "first", "s", "a")
	if _, code := join(t, url, "second", "s", "b"); code != http.StatusServiceUnavailable {
		t.Fatalf("second room got %d, want 503", code)
	}

	r2 := newRelay(100, 100, 2)
	url2 := serve(t, r2)
	join(t, url2, "movie", "s", "a")
	join(t, url2, "movie", "s", "b")
	if _, code := join(t, url2, "movie", "s", "c"); code != http.StatusServiceUnavailable {
		t.Fatalf("third conn got %d, want 503", code)
	}
}

// The whole reason the limiter is a sketch and not a map: memory must not grow
// with the number of distinct source addresses.
func TestLimiterDoesNotAllocatePerIP(t *testing.T) {
	l := newLimiter()
	for i := range 200_000 {
		l.allow("10."+strconv.Itoa(i%256)+"."+strconv.Itoa(i/256%256)+"."+strconv.Itoa(i/65536), 10)
	}
	if n := testing.AllocsPerRun(100, func() { l.allow("192.0.2.1", 10) }); n != 0 {
		t.Fatalf("limiter allocated %.1f times per check under load", n)
	}
}

// A sketch may over-count on collisions but must never under-count, or an
// attacker could collide their way past the cap.
func TestLimiterNeverUnderCounts(t *testing.T) {
	t.Parallel()
	l := newLimiter()
	const want = 50
	for i := range want {
		l.allow("198.51.100.7", 1_000_000)
		for j := range 100 {
			l.allow("10.0."+strconv.Itoa(i)+"."+strconv.Itoa(j), 1_000_000)
		}
	}
	if l.allow("198.51.100.7", want-2) {
		t.Fatal("estimate fell below the true count; an attacker could collide past the cap")
	}
}

// The accepted cost of fixed memory is occasional false rejection, so keep the
// sketch wide enough that it does not happen at realistic load.
func TestLimiterDoesNotFalselyRejectAtRealisticLoad(t *testing.T) {
	t.Parallel()
	l := newLimiter()
	for i := range 10_000 {
		l.allow("172.16."+strconv.Itoa(i/256)+"."+strconv.Itoa(i%256), 10)
	}
	if !l.allow("203.0.113.9", 10) {
		t.Fatal("a fresh ip was rejected after only 10k others; cols is too small")
	}
}

func TestLimiterResetsOnWindowRollover(t *testing.T) {
	t.Parallel()
	l := newLimiter()
	for range 20 {
		l.allow("203.0.113.1", 5)
	}
	if l.allow("203.0.113.1", 5) {
		t.Fatal("expected the ip to be over its cap")
	}
	l.window = time.Now().Add(-2 * limiterWindow)
	if !l.allow("203.0.113.1", 5) {
		t.Fatal("window rollover did not clear the counts")
	}
}

func TestMemberThatStopsAnsweringPingsIsRemoved(t *testing.T) {
	t.Parallel()
	r := newRelay(10, 10, 10)
	r.ping, r.timeout = 50*time.Millisecond, 50*time.Millisecond
	join(t, serve(t, r), "deaf", "s", "a")

	eventually(t, "a member that never answers a ping still holds its room", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.rooms) == 0
	})
}
