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

// watch() must not hand the scanner's internal buffer to the outgoing channel:
// the consumer reads it after Scan() has already overwritten it.
func TestWatchDoesNotAliasScannerBuffer(t *testing.T) {
	var b strings.Builder
	want := 6
	for i := range want {
		data := "no"
		if i%2 == 0 {
			data = "yes"
		}
		b.WriteString(`{"event":"property-change","name":"pause","data":"` + data + `","pad":"` + strings.Repeat("x", i) + `"}` + "\n")
	}

	sc := bufio.NewScanner(&dripReader{s: b.String()})
	sc.Buffer(make([]byte, 128), 128) // small buffer => guarantees the shift

	outgoing := make(chan []byte, want)
	c := &Client{
		outgoing: outgoing,
		paused:   false,
		conn:     &connection{scanner: sc, resCh: make(chan Response, 1)},
	}

	if err := c.watch(); err != nil {
		t.Fatal(err)
	}
	close(outgoing)

	got := 0
	for msg := range outgoing {
		e := Event{}
		if err := json.Unmarshal(msg, &e); err != nil {
			t.Fatalf("broadcast event was corrupted before the consumer read it: %s (%v)", msg, err)
		}
		if e.Name != pause {
			t.Fatalf("garbled event: %s", msg)
		}
		got++
	}
	if got != want {
		t.Fatalf("broadcast %d events, want %d", got, want)
	}
}
