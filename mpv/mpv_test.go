package mpv

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"
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
		outgoing: outgoing,
		paused:   false,
		applied:  map[string]string{},
		seen:     map[string]bool{},
		conn:     &connection{scanner: sc, resCh: make(chan Response, 1)},
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

	// mpv snaps a seek to a keyframe, so the echo can come back slightly off
	c.applied[timePos] = "12.000000"
	got = feed(t, c, outgoing, propertyChangeLine(timePos, "12.041000", 1))
	if len(got) != 0 {
		t.Fatalf("keyframe-snapped echo escaped the tolerance: %+v", got)
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
