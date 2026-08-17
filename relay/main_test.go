package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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
	}
}

func serve(t *testing.T, r *relay) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", r.handle)
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return "ws" + s.URL[len("http"):]
}

func join(t *testing.T, url, roomName, secret, hostname string) (*websocket.Conn, int) {
	t.Helper()
	conn, res, err := websocket.Dial(context.Background(),
		url+"/ws?room="+roomName+"&hostname="+hostname,
		&websocket.DialOptions{HTTPHeader: http.Header{secretHeaderKey: {secret}}})
	if err != nil {
		if res != nil {
			return nil, res.StatusCode
		}
		t.Fatalf("dial %s: %s", roomName, err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn, http.StatusSwitchingProtocols
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

	// a still gets b's and c's join notices, so only a playback frame counts
	if got, ok := recvEvent(t, a, 500*time.Millisecond); ok {
		t.Fatalf("the sender received its own event back: %+v", got)
	}
}

// recvNotices collects the show-text payloads a member is sent.
func recvNotices(t *testing.T, conn *websocket.Conn, want int) []string {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := []string{}
	for len(got) < want {
		_, b, err := conn.Read(c)
		if err != nil {
			return got
		}
		m := message{}
		json.Unmarshal(b, &m)
		e := struct {
			Name string `json:"name"`
			Data string `json:"data"`
		}{}
		json.Unmarshal(m.Event, &e)
		if e.Name == "show-text" {
			got = append(got, e.Data)
		}
	}
	return got
}

// The room hears about the joiner, and the joiner hears about the room. Without
// the second half the last node in would believe it was alone.
func TestJoinerLearnsWhoIsAlreadyThere(t *testing.T) {
	url := serve(t, newRelay(10, 10, 10))
	join(t, url, "movie", "s", "a")
	join(t, url, "movie", "s", "b")
	c, _ := join(t, url, "movie", "s", "c")

	got := recvNotices(t, c, 2)
	for _, want := range []string{"connected to a", "connected to b"} {
		if !slices.Contains(got, want) {
			t.Fatalf("last joiner was not told about the room: got %v, want %q", got, want)
		}
	}
}

func TestWrongSecretIsRejected(t *testing.T) {
	url := serve(t, newRelay(100, 10, 10))
	join(t, url, "movie", "right", "a")

	if _, code := join(t, url, "movie", "wrong", "b"); code != http.StatusForbidden {
		t.Fatalf("wrong secret got %d, want 403", code)
	}
	if _, code := join(t, url, "movie", "right", "b"); code != http.StatusSwitchingProtocols {
		t.Fatalf("right secret got %d, want an upgrade", code)
	}
}

// Joining an existing room must not cost quota, or a party of eight would lock
// itself out.
func TestOnlyRoomCreationIsRateLimited(t *testing.T) {
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

func TestMaxRoomsAndMaxConns(t *testing.T) {
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
	l := newLimiter()
	for i := range 10_000 {
		l.allow("172.16."+strconv.Itoa(i/256)+"."+strconv.Itoa(i%256), 10)
	}
	if !l.allow("203.0.113.9", 10) {
		t.Fatal("a fresh ip was rejected after only 10k others; cols is too small")
	}
}

func TestLimiterResetsOnWindowRollover(t *testing.T) {
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
