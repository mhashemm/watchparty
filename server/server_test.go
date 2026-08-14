package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mhashemm/watchparty/types"
)

// broadcast holds s.mu.RLock until wg.Wait returns, so nothing inside the
// per-peer goroutines may block on a channel: a backed-up incoming would
// wedge every handler waiting on s.mu.
func TestBroadcastDoesNotBlockOnFullIncoming(t *testing.T) {
	incoming := make(chan types.IncomingMessage) // nobody is draining this
	s := New(context.Background(), incoming, "127.0.0.1:1", "me")
	s.addresses["127.0.0.1:1"] = &peer{address: "127.0.0.1:1", Hostname: "dead"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.broadcast(func(context.Context, *peer, uint64) error {
			return errors.New("peer unreachable")
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast blocked sending the error notice; s.mu is now stuck held")
	}
}
