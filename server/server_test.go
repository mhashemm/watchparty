package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhashemm/watchparty/mpv"
)

// A failing peer makes broadcast post a notice onto incoming, which nobody drains
// until mpv has loaded the file. That send must not block, or BroadcastEvents stops for good.
func TestBroadcastDoesNotBlockOnFullIncoming(t *testing.T) {
	t.Parallel()
	incoming := make(chan mpv.IncomingMessage) // nobody is draining this
	s := New(context.Background(), incoming, "127.0.0.1:1", "me", "", time.Nanosecond)
	s.addresses["127.0.0.1:1"] = &peer{address: "127.0.0.1:1", Hostname: "dead"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.broadcast(context.Background(), false, func(context.Context, *peer, uint64) error {
			return errors.New("peer unreachable")
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast blocked sending the error notice; s.mu is now stuck held")
	}
}

// A peer that keeps failing has to be evicted, otherwise every later broadcast
// pays its 10s timeout. Any success in between resets the strike count.
func TestBroadcastEvictsPeerAfterMaxFails(t *testing.T) {
	t.Parallel()
	incoming := make(chan mpv.IncomingMessage, 8)
	s := New(context.Background(), incoming, "127.0.0.1:1", "me", "", time.Nanosecond)
	dead := &peer{address: "127.0.0.1:2", Hostname: "dead"}
	s.addresses[dead.address] = dead

	fail := func(context.Context, *peer, uint64) error { return errors.New("peer unreachable") }
	ok := func(context.Context, *peer, uint64) error { return nil }

	for range maxFails - 1 {
		s.broadcast(context.Background(), false, fail)
	}
	if _, exists := s.addresses[dead.address]; !exists {
		t.Fatalf("evicted after %d failures, want %d", maxFails-1, maxFails)
	}

	s.broadcast(context.Background(), false, ok)
	for range maxFails - 1 {
		s.broadcast(context.Background(), false, fail)
	}
	if _, exists := s.addresses[dead.address]; !exists {
		t.Fatal("a successful broadcast must reset the strike count")
	}

	s.broadcast(context.Background(), false, fail)
	if _, exists := s.addresses[dead.address]; exists {
		t.Fatalf("still registered after %d consecutive failures", maxFails)
	}
}

// Flaky Wi-Fi fails three sends in a burst of pauses without being gone. A peer
// is evicted only once it has also been failing for evictAfter.
func TestFailingPeerIsKeptUntilEvictAfter(t *testing.T) {
	t.Parallel()
	s := New(context.Background(), make(chan mpv.IncomingMessage, 8), "127.0.0.1:1", "me", "", time.Hour)
	flaky := &peer{address: "127.0.0.1:2", Hostname: "flaky"}
	s.addresses[flaky.address] = flaky
	fail := func(context.Context, *peer, uint64) error { return errors.New("peer unreachable") }

	for range maxFails + 2 {
		s.broadcast(context.Background(), false, fail)
	}
	if _, exists := s.addresses[flaky.address]; !exists {
		t.Fatalf("evicted after %d failures within the first hour", maxFails+2)
	}
	flaky.failingSince = flaky.failingSince.Add(-2 * time.Hour)
	s.broadcast(context.Background(), false, fail)
	if _, exists := s.addresses[flaky.address]; exists {
		t.Fatal("still registered after failing for longer than evictAfter")
	}
}

// A peer kept past maxFails can sit behind a 10s timeout on every send. The next
// event must not queue behind it, but a healthy peer is still waited for.
func TestSuspectPeerDoesNotHoldUpBroadcast(t *testing.T) {
	t.Parallel()
	s := New(context.Background(), make(chan mpv.IncomingMessage, 8), "127.0.0.1:1", "me", "", time.Hour)
	suspect := &peer{address: "127.0.0.1:2", Hostname: "suspect", fails: maxFails}
	healthy := &peer{address: "127.0.0.1:3", Hostname: "healthy"}
	s.addresses[suspect.address] = suspect
	s.addresses[healthy.address] = healthy

	release := make(chan struct{})
	reached := atomic.Bool{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.broadcast(context.Background(), true, func(_ context.Context, p *peer, _ uint64) error {
			if p == suspect {
				<-release
				return nil
			}
			reached.Store(true)
			return nil
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast waited for the suspect peer")
	}
	if !reached.Load() {
		t.Fatal("broadcast returned before the healthy peer was sent to")
	}
	close(release)
}

func TestBroadcastEventsStopsWithTheParty(t *testing.T) {
	t.Parallel()
	c, cancel := context.WithCancel(context.Background())
	s := New(c, make(chan mpv.IncomingMessage), "127.0.0.1:1", "me", "", time.Nanosecond)
	done := make(chan struct{})
	go func() {
		s.BroadcastEvents(make(chan []byte))
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("BroadcastEvents outlived the party")
	}
}

// /hi from a node playing another file is refused before it becomes a peer.
func TestHiRefusesADifferentFile(t *testing.T) {
	t.Parallel()
	s := New(context.Background(), make(chan mpv.IncomingMessage, 8), "127.0.0.1:1", "me", "abc", time.Nanosecond)
	for file, want := range map[string]int{"xyz": http.StatusConflict, "": http.StatusConflict, "abc": http.StatusOK} {
		req := httptest.NewRequest(http.MethodPost, "/hi", nil)
		req.Header.Set(addressHeaderKey, "127.0.0.1:2")
		req.Header.Set(fileHeaderKey, file)
		res := httptest.NewRecorder()
		s.Hi(res, req)
		_, added := s.addresses["127.0.0.1:2"]
		if res.Code != want || added != (want == http.StatusOK) {
			t.Errorf("file %q: got %d and added=%v, want %d", file, res.Code, added, want)
		}
		delete(s.addresses, "127.0.0.1:2")
	}
}

type trackedBody struct {
	io.Reader
	closed *atomic.Bool
}

func (b trackedBody) Close() error {
	b.closed.Store(true)
	return nil
}

type stubTransport struct {
	status int
	closed *atomic.Bool
}

func (t stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: t.status, Body: trackedBody{strings.NewReader("x"), t.closed}}, nil
}

// An unclosed body keeps its connection out of the pool for good, one per event.
func TestResponseBodiesAreClosed(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNoContent, http.StatusInternalServerError, http.StatusConflict} {
		closed := &atomic.Bool{}
		s := New(context.Background(), make(chan mpv.IncomingMessage, 8), "127.0.0.1:1", "me", "", time.Nanosecond)
		s.client = &http.Client{Transport: stubTransport{status, closed}}
		s.post(context.Background(), "127.0.0.1:2", "/event", nil, 1)
		if !closed.Load() {
			t.Errorf("%d: response body left open", status)
		}
	}
}
