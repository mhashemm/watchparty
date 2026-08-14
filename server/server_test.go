package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mhashemm/watchparty/mpv"
)

// broadcast holds s.mu.RLock until wg.Wait returns, so nothing inside the
// per-peer goroutines may block on a channel: a backed-up incoming would
// wedge every handler waiting on s.mu.
func TestBroadcastDoesNotBlockOnFullIncoming(t *testing.T) {
	incoming := make(chan mpv.IncomingMessage) // nobody is draining this
	s := New(context.Background(), incoming, "127.0.0.1:1", "me")
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
	incoming := make(chan mpv.IncomingMessage, 8)
	s := New(context.Background(), incoming, "127.0.0.1:1", "me")
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
