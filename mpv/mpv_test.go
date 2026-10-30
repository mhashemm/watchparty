package mpv

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// dripReader hands back a few bytes per Read, like the mpv socket does, which
// forces bufio.Scanner to refill and shift its buffer mid-stream.
type dripReader struct {
	s string
	i int
}

func (d *dripReader) Read(p []byte) (int, error) {
	if d.i >= len(d.s) {
		return 0, io.EOF
	}
	n := copy(p[:min(7, len(p))], d.s[d.i:])
	d.i += n
	return n, nil
}

func newTestClient(outgoing chan []byte, sc *bufio.Scanner) *Client {
	return &Client{
		ctx:      context.Background(),
		outgoing: outgoing,
		paused:   false,
		applied:  map[string]string{},
		seen:     map[string]bool{},
		conn:     &connection{scanner: sc, pending: map[int64]chan Response{}},
	}
}

// feed drives watch() over lines and returns what it decided to broadcast.
func feed(t *testing.T, c *Client, outgoing chan []byte, lines string) []Event {
	t.Helper()
	sc := bufio.NewScanner(&dripReader{s: lines})
	sc.Buffer(make([]byte, 128), 128) // small buffer => guarantees the shift
	c.conn.scanner = sc
	if err := c.watch(); err != nil {
		t.Fatal(err)
	}
	got := []Event{}
	for {
		select {
		case msg := <-outgoing:
			e := Event{}
			if err := json.Unmarshal(msg, &e); err != nil {
				t.Fatalf("broadcast event was corrupted before the consumer read it: %s (%v)", msg, err)
			}
			got = append(got, e)
		default:
			return got
		}
	}
}

func propertyChangeLine(name, data string, pad int) string {
	return `{"event":"property-change","name":"` + name + `","data":"` + data + `","pad":"` + strings.Repeat("x", pad) + `"}` + "\n"
}

// watch() must not hand the scanner's internal buffer to the outgoing channel:
// the consumer reads it after Scan() has already overwritten it.
func TestWatchDoesNotAliasScannerBuffer(t *testing.T) {
	var b strings.Builder
	lines := 6
	for i := range lines {
		data := "no"
		if i%2 == 0 {
			data = "yes"
		}
		b.WriteString(propertyChangeLine(pause, data, i))
	}

	outgoing := make(chan []byte, lines)
	c := newTestClient(outgoing, nil)
	got := feed(t, c, outgoing, b.String())

	for _, e := range got {
		if e.Name != pause {
			t.Fatalf("garbled event: %+v", e)
		}
	}
	// the first property-change per property is the observe_property echo, so
	// only 5 of the 6 alternating pauses are broadcast
	if len(got) != lines-1 {
		t.Fatalf("broadcast %d events, want %d", len(got), lines-1)
	}
}

// mpv fires one property-change per observed property the moment it is
// registered. A fresh node is paused, so an unswallowed initial time-pos would be
// broadcast as time-pos=0 and rewind the whole party.
func TestInitialPropertyChangeIsNotBroadcast(t *testing.T) {
	outgoing := make(chan []byte, 4)
	c := newTestClient(outgoing, nil)
	c.paused = true

	got := feed(t, c, outgoing, propertyChangeLine(timePos, "0.000000", 0))
	if len(got) != 0 {
		t.Fatalf("broadcast the observe_property echo: %+v", got)
	}

	got = feed(t, c, outgoing, propertyChangeLine(timePos, "77.000000", 1))
	if len(got) != 1 {
		t.Fatalf("swallowed a real seek after the initial one: %+v", got)
	}
}

// Observed before the file loads, time-pos first comes with no data, then the real
// position once it has loaded. The empty one must not count as the first change,
// or the real 0 is broadcast and rewinds the party. Quitting while paused also
// sends an empty one, which must not go out either.
func TestDatalessPropertyChangeIsIgnored(t *testing.T) {
	outgoing := make(chan []byte, 4)
	c := newTestClient(outgoing, nil)
	c.paused = true

	empty := `{"event":"property-change","id":2,"name":"time-pos"}` + "\n"
	got := feed(t, c, outgoing, empty+propertyChangeLine(timePos, "0.000000", 0))
	if len(got) != 0 {
		t.Fatalf("broadcast the position the file loaded at: %+v", got)
	}

	got = feed(t, c, outgoing, propertyChangeLine(timePos, "42.000000", 1)+empty)
	if len(got) != 1 || got[0].Data != "42.000000" {
		t.Fatalf("want only the real seek broadcast, got %+v", got)
	}
}

// b applies a's seek, mpv echoes the change back, and b must not rebroadcast it
// to a. Without this the event ping-pongs; relay mode has no counters to stop it.
func TestAppliedValueIsNotRebroadcast(t *testing.T) {
	outgoing := make(chan []byte, 4)
	c := newTestClient(outgoing, nil)
	c.paused = true
	c.seen[timePos] = true

	// sync() marks the value before the IPC call; this is the echo coming back
	c.applied[timePos] = "77.000000"
	got := feed(t, c, outgoing, propertyChangeLine(timePos, "77.000000", 0))
	if len(got) != 0 {
		t.Fatalf("rebroadcast the event it had just applied: %+v", got)
	}

	// a seek's echo can land a frame off the applied value
	c.applied[timePos] = "12.000000"
	got = feed(t, c, outgoing, propertyChangeLine(timePos, "12.041000", 1))
	if len(got) != 0 {
		t.Fatalf("a frame-off echo escaped the tolerance: %+v", got)
	}

	// but this peer's own seek is not an echo and must reach everyone else
	got = feed(t, c, outgoing, propertyChangeLine(timePos, "42.000000", 2))
	if len(got) != 1 {
		t.Fatalf("swallowed a genuine seek, want 1 broadcast: %+v", got)
	}
}

func TestAppliedPauseIsNotRebroadcast(t *testing.T) {
	outgoing := make(chan []byte, 4)
	c := newTestClient(outgoing, nil)
	c.paused = false
	c.seen[pause] = true

	c.applied[pause] = "yes"
	got := feed(t, c, outgoing, propertyChangeLine(pause, "yes", 0))
	if len(got) != 0 {
		t.Fatalf("rebroadcast the pause it had just applied: %+v", got)
	}
	if c.paused {
		t.Fatal("suppressing the echo must not skip the state update")
	}

	// the echo consumed the marker, so pausing again later is a real event
	c.paused = false
	got = feed(t, c, outgoing, propertyChangeLine(pause, "yes", 1))
	if len(got) != 1 {
		t.Fatalf("stale marker swallowed a genuine pause: %+v", got)
	}
}

// Two do() calls in flight must each get their own reply, even when mpv answers
// them out of order. A shared response channel let one steal the other's reply
// and leave it to time out.
func TestConcurrentRequestsGetTheirOwnResponse(t *testing.T) {
	ours, theirs := net.Pipe()
	defer ours.Close()
	defer theirs.Close()
	c := newTestClient(make(chan []byte, 1), bufio.NewScanner(ours))
	c.conn.conn = ours
	go c.watch()

	go func() {
		sc := bufio.NewScanner(theirs)
		ids := []int64{}
		for len(ids) < 2 && sc.Scan() {
			req := Request{}
			json.Unmarshal(sc.Bytes(), &req)
			ids = append(ids, req.RequestId)
		}
		for i := len(ids) - 1; i >= 0; i-- {
			fmt.Fprintf(theirs, `{"request_id":%d,"error":"success"}`+"\n", ids[i])
		}
	}()

	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- c.conn.do("get_property", pause) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestProcessIncomingEventsStopsWithTheParty(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := newTestClient(nil, nil)
	c.ctx = ctx
	done := make(chan struct{})
	go func() {
		c.ProcessIncomingEvents(make(chan IncomingMessage))
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ProcessIncomingEvents outlived the party")
	}
}
