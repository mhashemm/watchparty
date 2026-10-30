package party

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mhashemm/watchparty/mpv"
)

// stub serves /ws with handle and returns the ws:// address a relayConn dials.
func stub(t *testing.T, handle func(n int, w http.ResponseWriter, r *http.Request)) string {
	t.Helper()
	n := atomic.Int32{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handle(int(n.Add(1)), w, r)
	}))
	t.Cleanup(s.Close)
	return "ws" + strings.TrimPrefix(s.URL, "http")
}

func accept(t *testing.T, w http.ResponseWriter, r *http.Request) *websocket.Conn {
	t.Helper()
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		t.Error(err)
		return nil
	}
	return conn
}

func newRelayConn(addr string, incoming chan mpv.IncomingMessage) *relayConn {
	return &relayConn{addr: addr, room: "room", secret: "secret", hostname: "me", session: "s1", incoming: incoming, retry: time.Millisecond}
}

func waitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal(what)
	}
}

// A wrong secret never becomes right, and retrying it only burns the relay's rate
// limiter, so 403 and 400 must end the party instead of looping.
func TestRejectionEndsTheParty(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusForbidden, http.StatusBadRequest} {
		addr := stub(t, func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
		r := newRelayConn(addr, nil)

		_, err := r.dial(context.Background())
		if !errors.Is(err, errRejected) {
			t.Fatalf("%d: got %v, want errRejected", status, err)
		}

		c, cancel := context.WithCancel(context.Background())
		returned := make(chan struct{})
		go func() {
			r.Connect(c, cancel)
			close(returned)
		}()
		waitDone(t, returned, "Connect kept retrying a rejection")
		if c.Err() == nil {
			t.Fatalf("%d: Connect gave up without ending the party", status)
		}
	}
}

// 401 is a guest who arrived before the host opened the room. They must wait, not
// exit, and get in once the room exists.
func TestNotOpenYetKeepsRetrying(t *testing.T) {
	t.Parallel()
	joined := make(chan struct{})
	addr := stub(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn := accept(t, w, r)
		close(joined)
		conn.Read(r.Context())
	})
	r := newRelayConn(addr, nil)
	c, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Connect(c, cancel)

	waitDone(t, joined, "never redialled after 401")
	if c.Err() != nil {
		t.Fatal("401 ended the party")
	}
}

// After a dropped connection the node redials with the same session, which is
// what lets the relay swap it in silently, and frames from both connections
// reach mpv. The secret travels in a header, never the query string.
func TestReconnectKeepsSessionAndDeliversFrames(t *testing.T) {
	t.Parallel()
	sessions := make(chan string, 2)
	addr := stub(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(secretHeaderKey) != "secret" || r.URL.Query().Has("secret") {
			t.Errorf("secret sent as %q, query %q", r.Header.Get(secretHeaderKey), r.URL.RawQuery)
		}
		sessions <- r.URL.Query().Get("session")
		conn := accept(t, w, r)
		frame := `{"hostname":"peer","event":{"event":"property-change","name":"pause","data":"yes"}}`
		conn.Write(r.Context(), websocket.MessageText, []byte(frame))
		if n == 1 {
			conn.CloseNow()
			return
		}
		conn.Read(r.Context())
	})
	incoming := make(chan mpv.IncomingMessage, 2)
	r := newRelayConn(addr, incoming)
	c, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Connect(c, cancel)

	for i := range 2 {
		select {
		case m := <-incoming:
			e := mpv.Event{}
			if err := json.Unmarshal(m.Event, &e); err != nil || m.HostName != "peer" || e.Name != "pause" {
				t.Fatalf("frame %d arrived as %+v (%v)", i, m, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("frame %d never reached incoming", i)
		}
	}
	if a, b := <-sessions, <-sessions; a != "s1" || b != "s1" {
		t.Fatalf("sessions %q, %q, want s1 twice", a, b)
	}
	if c.Err() != nil {
		t.Fatal("a dropped connection ended the party")
	}
}

// Broadcast wraps each raw mpv event in the envelope the relay forwards, and stops
// when the party does.
func TestBroadcastSendsEnvelopeAndStopsWithTheParty(t *testing.T) {
	t.Parallel()
	got := make(chan []byte, 1)
	addr := stub(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		conn := accept(t, w, r)
		_, b, err := conn.Read(r.Context())
		if err == nil {
			got <- b
		}
		conn.Read(r.Context())
	})
	r := newRelayConn(addr, nil)
	conn, err := r.dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	r.set(conn)

	c, cancel := context.WithCancel(context.Background())
	outgoing := make(chan []byte, 1)
	returned := make(chan struct{})
	go func() {
		r.Broadcast(c, outgoing)
		close(returned)
	}()
	event := `{"event":"property-change","name":"pause","data":"no"}`
	outgoing <- []byte(event)

	select {
	case b := <-got:
		want := `{"hostname":"me","event":` + event + `}`
		if string(b) != want {
			t.Fatalf("sent %s, want %s", b, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event never reached the relay")
	}

	cancel()
	waitDone(t, returned, "Broadcast outlived the party")
}

// An invite pasted as wss://host/ must not dial //ws, which the relay's mux
// redirects instead of upgrading.
func TestTrailingSlashOnRelayAddress(t *testing.T) {
	t.Parallel()
	addr := stub(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws" {
			http.NotFound(w, r)
			return
		}
		conn := accept(t, w, r)
		conn.Read(r.Context())
	})
	conn, err := newRelayConn(addr+"/", nil).dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	conn.CloseNow()
}
