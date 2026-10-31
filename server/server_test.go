package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mhashemm/watchparty/mpv"
)

// A failing peer makes broadcast post a notice onto incoming, which nobody drains
// until mpv has loaded the file. That send must not block, or BroadcastEvents stops for good.
func TestBroadcastDoesNotBlockOnFullIncoming(t *testing.T) {
	t.Parallel()
	incoming := make(chan mpv.IncomingMessage) // nobody is draining this
	s := New(context.Background(), incoming, "127.0.0.1:1", "me", "")
	s.addresses["127.0.0.1:1"] = &peer{address: "127.0.0.1:1", Hostname: "dead"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.broadcast(context.Background(), func(context.Context, *peer, uint64) error {
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
	s := New(context.Background(), incoming, "127.0.0.1:1", "me", "")
	dead := &peer{address: "127.0.0.1:2", Hostname: "dead"}
	s.addresses[dead.address] = dead

	fail := func(context.Context, *peer, uint64) error { return errors.New("peer unreachable") }
	ok := func(context.Context, *peer, uint64) error { return nil }

	for range maxFails - 1 {
		s.broadcast(context.Background(), fail)
	}
	if _, exists := s.addresses[dead.address]; !exists {
		t.Fatalf("evicted after %d failures, want %d", maxFails-1, maxFails)
	}

	s.broadcast(context.Background(), ok)
	for range maxFails - 1 {
		s.broadcast(context.Background(), fail)
	}
	if _, exists := s.addresses[dead.address]; !exists {
		t.Fatal("a successful broadcast must reset the strike count")
	}

	s.broadcast(context.Background(), fail)
	if _, exists := s.addresses[dead.address]; exists {
		t.Fatalf("still registered after %d consecutive failures", maxFails)
	}
}

func TestBroadcastEventsStopsWithTheParty(t *testing.T) {
	t.Parallel()
	c, cancel := context.WithCancel(context.Background())
	s := New(c, make(chan mpv.IncomingMessage), "127.0.0.1:1", "me", "")
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
	s := New(context.Background(), make(chan mpv.IncomingMessage, 8), "127.0.0.1:1", "me", "abc")
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
