package mpv

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
		ready:    make(chan struct{}),
		outgoing: outgoing,
		paused:   false,
		applied:  map[string]string{},
		seen:     map[string]bool{},
		missed:   map[string]bool{},
		conn:     &connection{scanner: sc, closed: make(chan struct{})},
		inbox:    newMailbox[input](),
		replies:  map[int64]outcome{},
		waiting:  map[int64]bool{},
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
	t.Parallel()
	var b strings.Builder
	lines := 6
	for i := range lines {
		data := "no"
		if i%2 == 0 {
			data = "yes"
		}
		b.WriteString(propertyChangeLine(pause, data, i))
	}

	outgoing := make(chan []byte, 2*lines)
	c := newTestClient(outgoing, nil)
	got := feed(t, c, outgoing, b.String())

	pauses := 0
	for _, e := range got {
		switch e.Name {
		case pause:
			pauses++
		default:
			t.Fatalf("garbled event: %+v", e)
		}
	}
	// the first property-change per property is the observe_property echo, so
	// only 5 of the 6 alternating pauses are broadcast
	if pauses != lines-1 {
		t.Fatalf("broadcast %d pauses, want %d", pauses, lines-1)
	}
}

// watch closes the socket when mpv hangs up, so the next request can fail on the write
// before anyone has seen conn.closed. party.Run needs ErrClosed to read mpv's exit status.
func TestARequestOnAClosedSocketIsErrClosed(t *testing.T) {
	t.Parallel()
	nc, peer := net.Pipe()
	defer peer.Close()
	nc.Close()
	c := newTestClient(nil, nil)
	c.conn.conn = nc
	if err := c.do("observe_property_string", 1, pause); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v, want ErrClosed", err)
	}
}

// Someone can add to the playlist from inside mpv, so the entry it reports may be one we never named.
func TestAnEntryWeDidNotNameDoesNotPanic(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: []string{"e1", "e2"}})
		n.mpvSays(propertyChangeLine(playlistPos, "5", 0))
		n.broadcasts()
		n.peerSays("a", move(5, 0))
		if got := n.mpvWasTold(); !slices.Contains(got, "show-text from another video to e1, by a 5000") {
			t.Fatalf("told mpv %q, want the entry it never named called another video", got)
		}
	})
}

func TestFileNameIsNeverBroadcast(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(propertyChangeLine(filename, "a.mkv", 0), propertyChangeLine(filename, "b.mkv", 0))
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("a file name went over the wire: %+v", got)
		}
	})
}

// mpv fires one property-change per observed property the moment it is
// registered. A fresh node is paused, so an unswallowed initial time-pos would be
// broadcast as time-pos=0 and rewind the whole party.
func TestInitialPropertyChangeIsNotBroadcast(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{})
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q, want only joined: mpv's answer to the observe went out", got)
		}
		n.mpvSays(mpvAt(77))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=77.000000]" {
			t.Fatalf("broadcast %q, want the real seek after the initial one", got)
		}
	})
}

// Observed before the file loads, time-pos first comes with no data, then the real
// position once it has loaded. The empty one must not count as the first change,
// or the real 0 is broadcast and rewinds the party. Quitting while paused also
// sends an empty one, which must not go out either.
func TestDatalessPropertyChangeIsIgnored(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		empty := `{"event":"property-change","id":2,"name":"time-pos"}`
		n := newNode(t, setup{first: []string{empty + "\n"}})
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q, want only joined: the position the file loaded at went out", got)
		}
		n.mpvSays(mpvAt(42), empty)
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=42.000000]" {
			t.Fatalf("broadcast %q, want only the real seek", got)
		}
	})
}

// b applies a's seek, mpv echoes the change back, and b must not rebroadcast it
// to a. Without this the event ping-pongs; relay mode has no counters to stop it.
func TestAppliedValueIsNotRebroadcast(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("a", stamped(5, seekEvent(77)))
		n.mpvSays(mpvAt(77))
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("rebroadcast the seek it had just applied: %q", said(got))
		}
		// a seek's echo can land a frame off the applied value
		n.peerSays("a", stamped(6, seekEvent(12)))
		n.mpvSays(mpvAt(12.041))
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("a frame-off echo escaped the tolerance: %q", said(got))
		}
		// but the person's own seek is not an echo: a follower keeps it on its screen
		n.mpvWasTold()
		n.mpvSays(mpvAt(42))
		n.keepsTheSeek("a")
	})
}

// b applies 30 and then 40 before mpv's echo of 30 is back. That late echo must
// not go out as b's own seek, or the whole party jumps back to 30.
func TestLateEchoOfAnEarlierSeekIsNotRebroadcast(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("a", stamped(5, seekEvent(30)))
		n.peerSays("a", stamped(6, seekEvent(40)))
		n.mpvSays(mpvAt(30), mpvAt(40))
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("rebroadcast an echo: %q", said(got))
		}
		// 40's echo means mpv is done with 30, so seeking back there is the user's own
		n.mpvWasTold()
		n.mpvSays(mpvAt(30))
		n.keepsTheSeek("a")
	})
}

// --hr-seek=yes is pinned, so an echo is at most a frame off. A deliberate seek
// a fraction of a second away is still the user's own.
func TestSmallSeekIsNotAnEcho(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("a", stamped(5, seekEvent(12)))
		n.mpvWasTold()
		n.mpvSays(mpvAt(12.3))
		n.keepsTheSeek("a")
	})
}

// A stop nobody chose has no leader, and the first seek leads: one that answers a join or a
// leave, as each node's own. A joiner is answered with it, so its seek goes out (e2e [5]);
// a leader that left leaves its stop with none, told by its id, as names may be shared.
func TestAStopNobodyChoseHasNoLeader(t *testing.T) {
	t.Parallel()
	free := func(t *testing.T, got []Event, name string) {
		t.Helper()
		for _, e := range got {
			if e.Name == name && e.Free {
				return
			}
		}
		t.Fatalf("broadcast %+v, want its %s marked free", got, name)
	}
	seekGoesOut := func(n *node) {
		n.t.Helper()
		n.mpvWasTold()
		n.mpvSays(mpvAt(50))
		// not free: whoever takes it follows this node, the first to seek
		if got := n.broadcasts(); fmt.Sprint(said(got)) != "[time-pos=50.000000]" || got[0].Free {
			n.t.Fatalf("broadcast %+v, want the person's seek, which leads", got)
		}
	}
	t.Run("a joiner answered", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{joiner: true})
			n.peerSays("a", `{"stamp":3,"paused":true,"at":30,"free":true,"event":"property-change","name":"time-pos","data":"30.000000"}`)
			n.mpvObeys()
			n.broadcasts()
			seekGoesOut(n)
		})
	})
	t.Run("answering a join, stopped", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(3, true, 30)
			n.peerSays("j", `{"name":"joined"}`)
			free(t, n.broadcasts(), timePos)
			seekGoesOut(n)
		})
	})
	t.Run("answering a join, playing", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.plays()
			n.peerSays("j", `{"name":"joined"}`)
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
			free(t, n.broadcasts(), pause)
		})
	})
	t.Run("a leave, playing", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.plays()
			n.peerSays("x", `{"name":"show-text","data":"x has left"}`)
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
			free(t, n.broadcasts(), pause)
		})
	})
	t.Run("the leader leaves", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(3, true, 30)
			n.peerSays("z", `{"name":"show-text","data":"z has left"}`)
			seekGoesOut(n)
		})
	})
	t.Run("another member of the leader's name leaves", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(3, true, 30)
			n.transportSays(IncomingMessage{HostName: "z", From: "z2", Event: []byte(`{"name":"show-text","data":"z has left"}`)})
			n.mpvWasTold()
			n.mpvSays(mpvAt(50))
			n.keepsTheSeek("z")
		})
	})
	t.Run("another member leaves", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(3, true, 30)
			n.peerSays("x", `{"name":"show-text","data":"x has left"}`)
			n.mpvWasTold()
			n.mpvSays(mpvAt(50))
			n.keepsTheSeek("z")
		})
	})
}

// A joiner is sent to 30 by an answer, which it doesn't follow, and the person seeks to 39
// and back to 30. Their seek to 39 means mpv is done with 30 (MPV.md), so the way back is
// theirs, not the answer's echo.
func TestASeekBackToWhereAnAnswerSentItIsThePersonsOwn(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{joiner: true})
		n.peerSays("a", `{"stamp":3,"paused":true,"at":30,"free":true,"event":"property-change","name":"time-pos","data":"30.000000"}`)
		n.mpvObeys()
		n.mpvSays(mpvAt(39))
		n.broadcasts()
		n.mpvSays(mpvAt(30))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=30.000000]" {
			t.Fatalf("broadcast %q, want the seek back to where the answer had sent it", got)
		}
	})
}

// A peer sends this node to 30, and the person seeks to 39 and back to 30: a follower keeps
// both on its screen, and tells the person once for that state, so a held key doesn't flood
// the GUI's feed; again once it takes the leader's next seek.
func TestASeekBackToWhereAPeerSentItIsThePersonsOwn(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("a", stamped(5, seekEvent(30)))
		n.mpvObeys()
		n.mpvWasTold()
		n.mpvSays(mpvAt(39))
		n.keepsTheSeek("a")
		n.mpvSays(mpvAt(30))
		if got, told := said(n.broadcasts()), n.mpvWasTold(); len(got) != 0 || len(told) != 0 {
			t.Fatalf("broadcast %q and told mpv %q, want nothing more", got, told)
		}
		n.peerSays("a", stamped(6, seekEvent(50)))
		n.mpvObeys()
		n.mpvWasTold()
		n.mpvSays(mpvAt(55))
		n.keepsTheSeek("a")
	})
}

// A peer sends this node to 20 and resumes the party; later the person stops and seeks back
// to 20. A resume taken from a peer ends the seeks this node applied, as its own does.
func TestASeekBackAfterAPeersResumeIsThePersonsOwn(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("a", stamped(5, seekEvent(20)))
		n.mpvObeys()
		n.peerSays("a", stamped(6, `{"event":"property-change","name":"pause","data":"no","at":20}`))
		n.mpvObeys()
		n.mpvSays(mpvAt(25), propertyChangeLine(pause, "yes", 0))
		n.broadcasts()
		n.mpvSays(`{"event":"seek"}`, mpvAt(20), `{"event":"playback-restart"}`)
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=20.000000]" {
			t.Fatalf("broadcast %q, want the seek back to where the peer had sent it", got)
		}
	})
}

// b applies a's pause, and mpv reports it before it answers the set: that report is the
// echo, and it is also when b stopped, so a position mpv reports before its answer is a
// seek while paused, not playback.
func TestAppliedPauseIsNotRebroadcast(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvAnswersLate()
		n.peerSays("a", stamped(5, `{"event":"property-change","name":"pause","data":"yes","at":0}`))
		n.mpvObeys(mpvAt(55))
		// only the seek made once stopped, which a follower keeps: not the echo, and not the
		// seek lost as playback
		n.keepsTheSeek("a")
		// the echo used up the marker, so pausing again later is a real event
		n.plays()
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes]" {
			t.Fatalf("broadcast %q, want the genuine pause", got)
		}
	})
}

// Two do() calls in flight must each get their own reply, in whichever order mpv answers
// them. A shared response channel let one steal the other's reply and leave it to time
// out. Two are in flight when one waits inside the other: a line mpv sends meanwhile starts
// a second request (as a stall starts pauseParty's) before the first is answered.
func TestConcurrentRequestsGetTheirOwnResponse(t *testing.T) {
	t.Parallel()
	for _, reversed := range []bool{false, true} {
		t.Run(fmt.Sprintf("answered reversed %v", reversed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ours, theirs := net.Pipe()
				defer ours.Close()
				defer theirs.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				c := newTestClient(make(chan []byte, 1), bufio.NewScanner(ours))
				c.ctx, c.conn.conn = ctx, ours
				go func() {
					sc := bufio.NewScanner(theirs)
					ids := []int64{}
					for len(ids) < 2 && sc.Scan() {
						req := Request{}
						json.Unmarshal(sc.Bytes(), &req)
						ids = append(ids, req.RequestId)
						if len(ids) == 1 {
							fmt.Fprint(theirs, `{"event":"seek"}`+"\n")
						}
					}
					if reversed {
						slices.Reverse(ids)
					}
					for _, id := range ids {
						fmt.Fprintf(theirs, `{"request_id":%d,"error":"success"}`+"\n", id)
					}
				}()
				errs := make(chan error, 2)
				c.later = append(c.later, func() {
					// the second, once the line that comes while the first waits is handled
					c.later = append(c.later, func() { errs <- c.do("get_property", pause) })
					errs <- c.do("get_property", pause)
				})
				go c.watch()
				for range 2 {
					if err := <-errs; err != nil {
						t.Fatal(err)
					}
				}
			})
		})
	}
}

func TestTheLoopStopsWithTheParty(t *testing.T) {
	t.Parallel()
	ours, theirs := net.Pipe()
	defer ours.Close()
	defer theirs.Close()
	ctx, cancel := context.WithCancel(context.Background())
	c := newTestClient(nil, bufio.NewScanner(ours))
	c.ctx, c.incoming = ctx, make(chan IncomingMessage)
	done := make(chan struct{})
	go func() {
		c.watch()
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the loop outlived the party")
	}
}

// fakeMpv answers every request on conn with success and then sends events; with
// closeAfter > 0 it hangs up after that many requests instead.
func fakeMpv(conn net.Conn, closeAfter int, events ...string) {
	sc := bufio.NewScanner(conn)
	for n := 1; sc.Scan(); n++ {
		req := Request{}
		json.Unmarshal(sc.Bytes(), &req)
		if n == closeAfter {
			conn.Close()
			return
		}
		fmt.Fprintf(conn, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
		if n == 2 {
			for _, e := range events {
				fmt.Fprint(conn, e+"\n")
			}
		}
	}
}

// New returns only once the file is loaded, which is the first time-pos that has
// data. The empty one mpv sends before that doesn't count.
func TestNewWaitsForTheFileToLoad(t *testing.T) {
	t.Parallel()
	ours, theirs := net.Pipe()
	defer ours.Close()
	loaded := make(chan struct{})
	go fakeMpv(theirs, 0, `{"event":"property-change","id":1,"name":"pause","data":"yes"}`,
		`{"event":"property-change","id":2,"name":"time-pos"}`)

	c, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		New(c, func() {}, ours, make(chan []byte, 1), make(chan IncomingMessage), nil, 0, 0)
		close(loaded)
	}()
	select {
	case <-loaded:
		t.Fatal("New returned before time-pos had a value")
	case <-time.After(300 * time.Millisecond):
	}
	fmt.Fprint(theirs, `{"event":"property-change","id":2,"name":"time-pos","data":"0.000000"}`+"\n")
	select {
	case <-loaded:
	case <-time.After(time.Second):
		t.Fatal("New never returned after the file loaded")
	}
}

// mpv that dies before the file loads (a bad file) closes the socket mid-request.
// New must return ErrClosed at once, not after do()'s 5s timeout, and must not
// cancel the party: that would kill mpv and hide its real exit status.
func TestNewReturnsAtOnceWhenMpvGoesAway(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ours, theirs := net.Pipe()
		defer ours.Close()
		go fakeMpv(theirs, 2)
		cancelled := atomic.Bool{}
		start := time.Now()
		c, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := New(c, func() { cancelled.Store(true) }, ours, make(chan []byte, 1), make(chan IncomingMessage), nil, 0, 0)
		if !errors.Is(err, ErrClosed) || time.Since(start) > time.Second {
			t.Fatalf("got %v after %s, want ErrClosed at once", err, time.Since(start))
		}
		synctest.Wait()
		if cancelled.Load() {
			t.Fatal("cancelled the party before the video was ready")
		}
	})
}

// An answer mpv sent before hanging up is an answer: the command was done, whichever the
// node reads first, as both come in the socket's order.
func TestAnAnswerBeforeAHangUpCountsAsDone(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ours, theirs := net.Pipe()
		defer ours.Close()
		go func() {
			sc := bufio.NewScanner(theirs)
			if sc.Scan() {
				req := Request{}
				json.Unmarshal(sc.Bytes(), &req)
				fmt.Fprintf(theirs, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
			}
			theirs.Close()
		}()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c := newTestClient(make(chan []byte, 1), bufio.NewScanner(ours))
		c.ctx, c.conn.conn = ctx, ours
		errs := make(chan error, 1)
		c.later = append(c.later, func() { errs <- c.do(setProperty, pause, true) })
		go c.watch()
		if err := <-errs; err != nil {
			t.Fatalf("got %v, want the command done", err)
		}
	})
}

// What the transport has handed over comes before mpv's next line, whatever ran first: a
// message sent before the file loaded is heard while it loads, so an older one isn't
// applied after a newer one, as if its sender had fallen behind (#89, at the loop).
func TestWhatTheTransportHandedOverComesBeforeMpvsNextLine(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ours, theirs := net.Pipe()
		defer ours.Close()
		defer theirs.Close()
		told := make(chan string, 16)
		go func() {
			sc := bufio.NewScanner(theirs)
			for sc.Scan() {
				req := Request{}
				json.Unmarshal(sc.Bytes(), &req)
				told <- strings.TrimSpace(fmt.Sprintln(req.Command...))
				fmt.Fprintf(theirs, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
			}
		}()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c := newTestClient(make(chan []byte, 8), bufio.NewScanner(ours))
		c.ctx, c.conn.conn, c.paused, c.stamp, c.opening = ctx, ours, true, unheard, true
		incoming := make(chan IncomingMessage, 2)
		incoming <- peer("a", `{"stamp":5,"at":30,"name":"pause","data":"yes"}`)
		incoming <- peer("b", stamped(3, seekEvent(10)))
		c.incoming = incoming
		// both are waiting when the loop first looks: the line in its inbox, the messages
		// in the transport's channel
		c.inbox.put(input{line: []byte(mpvAt(0))})
		go c.watch()
		synctest.Wait()
		got := []string{}
		for len(told) > 0 {
			got = append(got, <-told)
		}
		if fmt.Sprint(got) != "[set_property time-pos 30.000000]" {
			t.Fatalf("told mpv %q, want only the newer of the two", got)
		}
	})
}

// The party starts only with everything observed: an observe mpv refuses, even after the
// file loaded, fails New, or the party would run without a rule it relies on (a stall
// unobserved never pauses anyone).
func TestNewFailsWhenAnObserveIsRefusedAfterTheFileLoaded(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ours, theirs := net.Pipe()
		defer ours.Close()
		defer theirs.Close()
		go func() {
			sc := bufio.NewScanner(theirs)
			for n := 1; sc.Scan(); n++ {
				req := Request{}
				json.Unmarshal(sc.Bytes(), &req)
				if n == 4 {
					fmt.Fprintf(theirs, `{"request_id":%d,"error":"property not found"}`+"\n", req.RequestId)
					continue
				}
				fmt.Fprintf(theirs, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
				if n == 3 {
					fmt.Fprint(theirs, mpvAt(0))
				}
			}
		}()
		c, cancel := context.WithCancel(context.Background())
		defer cancel()
		if _, err := New(c, func() {}, ours, make(chan []byte, 8), make(chan IncomingMessage), nil, 0, 0); err == nil || err.Error() != "property not found" {
			t.Fatalf("got %v, want the refused observe", err)
		}
	})
}

// mpv closing after it took every observe, with the file never loaded (a URL that never
// opens), still ends New with ErrClosed, at once.
func TestNewReturnsWhenMpvGoesAwayBeforeTheFileLoads(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ours, theirs := net.Pipe()
		defer ours.Close()
		go func() {
			sc := bufio.NewScanner(theirs)
			for n := 1; n <= 5 && sc.Scan(); n++ {
				req := Request{}
				json.Unmarshal(sc.Bytes(), &req)
				fmt.Fprintf(theirs, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
			}
			theirs.Close()
		}()
		c, cancel := context.WithCancel(context.Background())
		defer cancel()
		start := time.Now()
		if _, err := New(c, func() {}, ours, make(chan []byte, 8), make(chan IncomingMessage), nil, 0, 0); !errors.Is(err, ErrClosed) || time.Since(start) > time.Second {
			t.Fatalf("got %v after %s, want ErrClosed at once", err, time.Since(start))
		}
	})
}

// mpv that loads the file and quits right after (a person closing it at once) started the
// party: New succeeds, and the party then learns mpv is gone. The load came first on the
// socket, so it counts first.
func TestNewSucceedsWhenMpvLoadsThenQuitsAtOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ours, theirs := net.Pipe()
		defer ours.Close()
		go func() {
			sc := bufio.NewScanner(theirs)
			for n := 1; n <= 5 && sc.Scan(); n++ {
				req := Request{}
				json.Unmarshal(sc.Bytes(), &req)
				fmt.Fprintf(theirs, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
			}
			fmt.Fprint(theirs, mpvAt(0))
			theirs.Close()
		}()
		cancelled := atomic.Bool{}
		c, cancel := context.WithCancel(context.Background())
		defer cancel()
		if _, err := New(c, func() { cancelled.Store(true) }, ours, make(chan []byte, 8), make(chan IncomingMessage), nil, 0, 0); err != nil {
			t.Fatalf("got %v, want the party started", err)
		}
		synctest.Wait()
		if !cancelled.Load() {
			t.Fatal("mpv quit and the party never heard")
		}
	})
}

// Once the file has loaded, mpv hanging up is how the party learns mpv quit: the cancel
// New was given is called (party.Run tells a quit from other stops through it).
func TestMpvHangingUpAfterLoadingEndsTheParty(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ours, theirs := net.Pipe()
		defer ours.Close()
		go fakeMpv(theirs, 0, propertyChangeLine(pause, "yes", 0), mpvAt(0))
		cancelled := atomic.Bool{}
		c, cancel := context.WithCancel(context.Background())
		defer cancel()
		if _, err := New(c, func() { cancelled.Store(true) }, ours, make(chan []byte, 1), make(chan IncomingMessage), nil, 0, 0); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if cancelled.Load() {
			t.Fatal("cancelled the party while mpv was still there")
		}
		theirs.Close()
		synctest.Wait()
		if !cancelled.Load() {
			t.Fatal("mpv hung up and the party never heard")
		}
	})
}

// A reply that never comes is given up on after 5s, like a refusal: the node lets go of
// what it marked, asks the party again, and goes on with what comes next.
func TestAReplyThatNeverComesIsGivenUpOn(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvAnswersLate()
		n.peerSays("a", stamped(5, seekEvent(20)))
		if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 20.000000]" {
			t.Fatalf("seeks %q, want the seek sent", got)
		}
		time.Sleep(4 * time.Second)
		if got := said(n.broadcasts()); len(got) != 0 {
			t.Fatalf("broadcast %q before 5s, want nothing yet", got)
		}
		time.Sleep(2 * time.Second)
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q after 5s, want it to ask the party again", got)
		}
		n.peerSays("a", stamped(6, seekEvent(30)))
		if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 30.000000]" {
			t.Fatalf("seeks %q, want the next message handled", got)
		}
	})
}

// The person seeks while mpv still answers a step of taking a member's state: that seek is
// newer than the state, and goes out stopped; the steps left would leave this node alone
// where the member was, playing or on its second, as the party takes the seek
// (SIM_SEED=10 found the first).
func TestAChangeMadeWhileTakingAStateIsNotUndone(t *testing.T) {
	t.Run("the resume after a seek", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.mpvAnswersLate()
			n.peerSays("a", stamped(5, `{"event":"property-change","name":"pause","data":"no","at":34}`))
			n.mpvObeys(`{"event":"seek"}`, mpvAt(20), `{"event":"playback-restart"}`)
			if got := n.mpvWasTold(); slices.Contains(got, "set_property pause false") {
				t.Fatalf("told mpv %q, want it never told to play", got)
			}
			if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=20.000000]" {
				t.Fatalf("broadcast %q, want its own seek", got)
			}
		})
	})
	t.Run("the stop over a resume", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.plays()
			n.mpvWasTold()
			n.mpvAnswersLate()
			n.peerSays("a", stamped(5, `{"event":"property-change","name":"pause","data":"yes","at":0}`))
			n.mpvObeys(propertyChangeLine(pause, "no", 0))
			n.broadcasts()
			n.mpvSays(mpvAt(1))
			if got := said(n.broadcasts()); len(got) != 0 {
				t.Fatalf("a frame while playing broadcast %q: the node took itself for stopped", got)
			}
		})
	})
	t.Run("the resume over a pause", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.mpvAnswersLate()
			n.peerSays("a", stamped(5, `{"event":"property-change","name":"pause","data":"no","at":0}`))
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
			n.broadcasts()
			n.mpvSays(`{"event":"seek"}`, mpvAt(30), `{"event":"playback-restart"}`)
			if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=30.000000]" {
				t.Fatalf("a seek while stopped broadcast %q, want it to go out: the node took itself for playing", got)
			}
		})
	})
	t.Run("a seek while playing, before the peer's seek is sent", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.plays()
			n.mpvWasTold()
			n.mpvAnswersLate()
			n.peerSays("a", stamped(5, `{"event":"property-change","name":"pause","data":"yes","at":40}`))
			n.mpvObeys(propertyChangeLine(pause, "no", 0), `{"event":"seek"}`, mpvAt(90), `{"event":"playback-restart"}`)
			if got := said(n.broadcasts()); !slices.Contains(got, "pause-reason=seek") {
				t.Fatalf("broadcast %q, want its seek while playing to stop the party", got)
			}
		})
	})
	t.Run("the seek after a stop", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.plays()
			n.mpvWasTold()
			n.mpvAnswersLate()
			n.peerSays("a", stamped(5, `{"event":"property-change","name":"pause","data":"yes","at":40}`))
			n.mpvObeys(`{"event":"seek"}`, mpvAt(50), `{"event":"playback-restart"}`)
			// stopped on the member's state, it follows: the person's seek stays here, and the
			// stop's seek to the member's second still goes to mpv
			told := n.mpvWasTold()
			if got := seeksIn(told); fmt.Sprint(got) != "[set_property time-pos 40.000000]" {
				t.Fatalf("seeks %q, want the member's second", got)
			}
			if !slices.Contains(told, "show-text only a can seek until resumed 5000") {
				t.Fatalf("told mpv %q, want the person told who holds the seek", told)
			}
			if got := said(n.broadcasts()); len(got) != 0 {
				t.Fatalf("broadcast %q, want nothing", got)
			}
		})
	})
}

// A peer must never wait on a file that is slow to load (a stream): every send lands
// while mpv is still loading, and only the newest of what the party said is applied once
// it has. Each says the whole of it: a resume says where from.
func TestIncomingWhileLoadingKeepsOnlyTheLatest(t *testing.T) {
	t.Parallel()
	ours, theirs := net.Pipe()
	defer ours.Close()
	c, cancel := context.WithCancel(context.Background())
	defer cancel()
	load := make(chan struct{})
	sent := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(theirs)
		for n := 1; sc.Scan(); n++ {
			req := Request{}
			json.Unmarshal(sc.Bytes(), &req)
			fmt.Fprintf(theirs, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
			if n == 2 {
				<-load
				fmt.Fprint(theirs, `{"event":"property-change","id":2,"name":"time-pos","data":"0.000000"}`+"\n")
			} else if req.Command[0] != "observe_property_string" {
				sent <- strings.TrimSpace(fmt.Sprintln(req.Command...))
			}
		}
	}()

	incoming := make(chan IncomingMessage)
	loaded := make(chan error, 1)
	go func() {
		_, err := New(c, func() {}, ours, make(chan []byte, 8), incoming, nil, 0, 0)
		loaded <- err
	}()
	stamp := 0
	at := "0"
	send := func(name, data string) {
		t.Helper()
		stamp++
		if name == timePos {
			at = data
		}
		select {
		case incoming <- peer("a", stamped(stamp, `{"at":`+at+`,"name":"`+name+`","data":"`+data+`"}`)):
		case <-time.After(time.Second):
			t.Fatalf("a peer blocked on %s=%s", name, data)
		}
	}
	for i := range 1000 {
		send(timePos, fmt.Sprint(i))
		send(pause, map[bool]string{true: "yes", false: "no"}[i%2 == 0])
	}
	close(load)
	if err := <-loaded; err != nil {
		t.Fatal(err)
	}
	send(showText, "mark")

	got := []string{}
	for s := ""; s != "show-text mark 5000"; {
		select {
		case s = <-sent:
			got = append(got, s)
		case <-time.After(time.Second):
			t.Fatalf("the mark never reached mpv, applied %q", got)
		}
	}
	// the last resume says where from, so it is the whole of what the party said
	want := []string{"set_property time-pos 999.000000", "set_property pause false",
		"show-text resumed by a 5000", "show-text mark 5000"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("applied %q, want %q", got, want)
	}
}

// A seek while playing would leave every peer where it was, since positions only
// sync while paused. The node pauses the party first, and peers get the pause, then
// where it landed, then why.
func TestSeekWhilePlayingPausesTheParty(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvSays(mpvAt(10), mpvAt(10.041), `{"event":"seek"}`, mpvAt(40))
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true show-text paused for everyone: seek 5000]" {
			t.Fatalf("told mpv %q, want the node to pause itself and say why", got)
		}
		n.mpvSays(mpvAt(40.041), propertyChangeLine(pause, "yes", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes time-pos=40.041000 pause-reason=seek]" {
			t.Fatalf("broadcast %q, want the pause, where it paused, then why", got)
		}
		// mpv's frame-landing second value is the position just sent, not a new seek
		n.mpvSays(mpvAt(40.042), mpvAt(50))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=50.000000]" {
			t.Fatalf("broadcast %q, want only the next real seek", got)
		}
	})
}

// Someone seeks and resumes at once, and both reach this node together. mpv reports the
// seek after the resume, which must not look like someone seeking while playing.
func TestPeerSeekReportedAfterAResumeIsNotASeek(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("a", stamped(5, seekEvent(40)))
		n.peerSays("a", stamped(6, `{"event":"property-change","name":"pause","data":"no","at":40}`))
		n.mpvWasTold()
		n.mpvSays(propertyChangeLine(pause, "no", 0), `{"event":"seek"}`, mpvAt(40), `{"event":"playback-restart"}`, mpvAt(40.041), mpvAt(40.2), mpvAt(40.5))
		if got, told := said(n.broadcasts()), n.mpvWasTold(); len(got) != 0 || len(told) != 0 {
			t.Fatalf("broadcast %q and told mpv %q for the peer's own seek", got, told)
		}
		n.mpvSays(`{"event":"seek"}`, mpvAt(50))
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true show-text paused for everyone: seek 5000]" {
			t.Fatalf("told mpv %q, want a real seek to pause the party", got)
		}
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes time-pos=50.000000 pause-reason=seek]" {
			t.Fatalf("broadcast %q, want the party paused at 50 only", got)
		}
	})
}

// Playback moves time-pos on every frame, and none of it may go out or pause anyone.
func TestPlaybackIsNotASeek(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		for i := range 48 {
			n.mpvSays(mpvAt(10 + float64(i)/24))
		}
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes]" {
			t.Fatalf("broadcast %q, want only the later pause", got)
		}
		if got := n.mpvWasTold(); len(got) != 0 {
			t.Fatalf("told mpv %q during playback", got)
		}
	})
}

// A node waiting on its cache falls behind everyone still playing. It pauses the
// party instead, and stays paused for a manual resume once the cache refills.
func TestBufferingPausesTheParty(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvSays(mpvAt(20), propertyChangeLine(pausedForCache, "yes", 0))
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true show-text paused for everyone: buffering 5000]" {
			t.Fatalf("told mpv %q, want the node to pause itself", got)
		}
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes time-pos=20.000000 pause-reason=buffering]" {
			t.Fatalf("broadcast %q", got)
		}
		n.mpvWasTold()
		// a resume while still stalled is not passed on, and pauses again
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true show-text paused for everyone: still buffering 5000]" {
			t.Fatalf("told mpv %q, want the node to pause itself again", got)
		}
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=20.000000 pause-reason=still buffering]" {
			t.Fatalf("broadcast %q", got)
		}
	})
}

// A peer's resume reaching a node that is still stalled is refused: the node never
// plays, and tells the party it is paused again and where. Applying the resume and
// pausing at once doesn't work, because mpv folds the two into no event at all.
func TestRemoteResumeWhileBufferingIsRefused(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(mpvAt(20), propertyChangeLine(pausedForCache, "yes", 0))
		n.broadcasts()
		n.peerSays("a", stamped(9, `{"event":"property-change","name":"pause","data":"no","at":20}`))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes time-pos=20.000000 pause-reason=still buffering]" {
			t.Fatalf("broadcast %q", got)
		}
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text paused for everyone: still buffering 5000]" {
			t.Fatalf("told mpv %q, want the resume refused", got)
		}
	})
}

// mpv can refuse a set (the property is unavailable while it quits or between files).
// The echo marker for a pause that never happened matched the user's own next pause,
// which then never went out.
func TestARefusedPauseLeavesNoMarker(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvRefuses("set_property pause", "property unavailable")
		n.peerSays("a", stamped(5, `{"event":"property-change","name":"pause","data":"yes","at":0}`))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q, want it to ask the party where it is", got)
		}
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes]" {
			t.Fatalf("broadcast %q, want the user's own pause", got)
		}
	})
}

// peer is a message from host as a node sends it, whole state included, so a test writes
// only what it is about. Left out, paused follows the event (a pause's data; stopped for a
// seek or a move), a seek is at its second, and a move is to the start of its entry. Where
// a pause stopped, and the entry of a pause or a seek, a test always says.
func peer(host, event string) IncomingMessage {
	m := map[string]any{}
	if json.Unmarshal([]byte(event), &m) == nil {
		name, _ := m["name"].(string)
		data, _ := m["data"].(string)
		or := func(k string, v any) {
			if _, ok := m[k]; !ok {
				m[k] = v
			}
		}
		switch name {
		case pause:
			or("paused", data == "yes")
		case timePos:
			or("paused", true)
			if at, err := strconv.ParseFloat(data, 64); err == nil {
				or("at", at)
			}
		case playlistPos:
			or("paused", true)
			or("at", 0)
			if entry, err := strconv.Atoi(data); err == nil {
				or("entry", entry)
			}
		}
		b, _ := json.Marshal(m)
		event = string(b)
	}
	return IncomingMessage{HostName: host, From: host, Event: []byte(event)}
}

// Once its file has loaded, a node says so, and takes where the party says it is.
func TestJoinerAnnouncesItselfOnceLoaded(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{joiner: true})
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q, want joined", got)
		}
		n.peerSays("a", stamped(1, seekEvent(40)))
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property time-pos 40.000000 show-text synced by a 5000]" {
			t.Fatalf("told mpv %q, want the party's position", got)
		}
		n.mpvObeys()
		// every member answers, and the rest say the same
		n.peerSays("b", stamped(1, seekEvent(40)))
		if got, told := n.broadcasts(), n.mpvWasTold(); len(got) != 0 || len(told) != 0 {
			t.Fatalf("broadcast %q and told mpv %q for the same answer again", said(got), told)
		}
	})
}

// What the party says while this node loads is applied once it has, before it says joined:
// the newest, which says the entry and where on it.
func TestWhatIsHeardWhileLoadingIsAppliedBeforeJoining(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{names: []string{"e1", "e2", "e3"}, joiner: true, late: true, heard: []IncomingMessage{
			peer("a", move(1, 2)),
			peer("a", stamped(2, tagged(2, seekEvent(50)))),
		}})
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property playlist-pos 2]" {
			t.Fatalf("told mpv %q, want the party's entry", got)
		}
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q before mpv took the party's entry", said(got))
		}
		// the seek waits for the file it is about
		n.mpvObeys()
		// the notices and the seek let go for the file are told side by side
		got := n.mpvWasTold()
		slices.Sort(got)
		if fmt.Sprint(got) != "[set_property time-pos 50.000000 show-text from e1 to e3, by a 5000 show-text synced by a 5000]" {
			t.Fatalf("told mpv %q once e3 loaded, want the party's second", got)
		}
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q, want joined", got)
		}
	})
}

// A node already in the party stops for a joiner, and its own pause is its answer: it
// says where, with a stamp newer than anything it could say before mpv had stopped.
func TestJoinPausesThePartyAndSaysWhere(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		resumed := n.broadcasts()[0].Stamp
		n.mpvSays(mpvAt(33.5))
		n.peerSays("j", `{"name":"joined"}`)
		// why first: the GUI's feed takes the pause that follows for this one
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text paused for j: joining 5000 set_property pause true]" {
			t.Fatalf("told mpv %q", got)
		}
		n.mpvObeys()
		got := n.broadcasts()
		if len(got) != 1 || got[0].Name != pause || got[0].Data != "yes" || got[0].At == nil || *got[0].At != 33.5 || got[0].Stamp <= resumed {
			t.Fatalf("broadcast %+v, want only its own pause, where it stopped, newer than its resume: a position beside it could carry the stamp from before", got)
		}
	})
}

// A node already paused, often by another node's pause for the same joiner, still
// says why the party is waiting.
func TestJoinWhilePausedStillShowsWhy(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("j", `{"name":"joined"}`)
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text paused for j: joining 5000]" {
			t.Fatalf("told mpv %q, want the notice and no pause", got)
		}
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != timePos {
			t.Fatalf("broadcast %q, want the position", said(got))
		}
	})
}

// Everyone gets the notice at their own moment, so everyone pauses themselves, and any
// one of those pauses is enough for a node that missed the notice.
func TestMemberLeavingPausesTheParty(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvSays(mpvAt(33.5))
		n.peerSays("x", `{"name":"show-text","data":"x has left"}`)
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text x has left 5000 show-text paused for x: left 5000 set_property pause true]" {
			t.Fatalf("told mpv %q, want the notice, the reason, then the party paused", got)
		}
		n.mpvObeys()
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != pause || got[0].Data != "yes" || got[0].At == nil || *got[0].At != 33.5 {
			t.Fatalf("broadcast %+v, want the pause, where it stopped", got)
		}
	})
}

func TestMemberLeavingChangesNothingWhileAlreadyPaused(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("x", `{"name":"show-text","data":"x has left"}`)
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text x has left 5000]" {
			t.Fatalf("told mpv %q, want the notice and no pause", got)
		}
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q", said(got))
		}
	})
}

// The video ending is how a party normally finishes. Each player reaches the end at
// its own moment, and whoever gets there first leaves; a pause would hold the rest a
// moment before the end, with nothing telling them why.
func TestMemberLeavingNearTheEndDoesNotPause(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvSays(propertyChangeLine(duration, "100.000000", 0), mpvAt(96))
		n.peerSays("x", `{"name":"show-text","data":"x has left"}`)
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text x has left 5000]" {
			t.Fatalf("told mpv %q, want no pause", got)
		}
	})
}

// Only the notice about its own sender counts, which is the shape both transports send.
func TestOnlyALeaveNoticePauses(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.peerSays("x", `{"name":"show-text","data":"y has left"}`)
		n.peerSays("x", `{"name":"show-text","data":"x has joined"}`)
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text y has left 5000 show-text x has joined 5000]" {
			t.Fatalf("told mpv %q, want no pause", got)
		}
	})
}

// Everyone stops a moment apart for a leave or a join, each with the same stamp, so they
// settle on the earliest stop, or they would resume that far apart and stay so. A joiner
// takes the first it hears, then the earliest too.
func TestStopsAMomentApartSettleOnTheEarliest(t *testing.T) {
	t.Parallel()
	stop := func(at float64) string {
		return fmt.Sprintf(`{"stamp":2,"at":%f,"name":"pause","data":"yes"}`, at)
	}
	t.Run("a member", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(2, true, 40)
			n.peerSays("a", stop(41))
			n.peerSays("b", stop(39))
			if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 39.000000]" {
				t.Fatalf("seeks %q, want only the earlier stop followed", got)
			}
		})
	})
	t.Run("a joiner", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{joiner: true})
			n.peerSays("a", stop(40))
			n.peerSays("b", stop(39))
			if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 40.000000 set_property time-pos 39.000000]" {
				t.Fatalf("seeks %q, want the first, then the earlier", got)
			}
		})
	})
}

// Two people start at once on different entries, and each answers the other. Neither
// knows anything yet, so their stamps tie and the higher entry wins, as it does for two
// moves: each following the other would swap them, for good. A party's answer has a
// stamp, and that is followed, lower or not.
func TestTwoJoinersSettleOnTheHigherEntry(t *testing.T) {
	t.Parallel()
	names := []string{"e1", "e2", "e3"}
	answer := func(i int) string { return move(unheard, i) }
	for _, tc := range []struct {
		name     string
		start    int
		messages []string
		want     string
	}{
		{"the higher one stays", 2, []string{`{"name":"joined"}`, answer(0)}, ""},
		{"the lower one follows", 0, []string{`{"name":"joined"}`, answer(2)}, "set_property playlist-pos 2"},
		{"the party's answer is followed, lower or not", 2, []string{move(1, 0)}, "set_property playlist-pos 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{names: names, start: tc.start, joiner: true})
				for _, m := range tc.messages {
					n.peerSays("a", m)
				}
				moved := ""
				for _, s := range n.mpvWasTold() {
					if strings.HasPrefix(s, "set_property playlist-pos") {
						moved = s
					}
				}
				if moved != tc.want {
					t.Fatalf("told mpv %q, want %q", moved, tc.want)
				}
			})
		})
	}
}

// Members answer a joiner with where each stopped, a moment apart and with one stamp. The
// joiner takes the first, then an earlier one, and not a later one after that.
func TestAJoinerFollowsAnEarlierAnswer(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{joiner: true})
		n.peerSays("a", stamped(1, seekEvent(40)))
		n.peerSays("b", stamped(1, seekEvent(39)))
		n.peerSays("c", stamped(1, seekEvent(40)))
		if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 40.000000 set_property time-pos 39.000000]" {
			t.Fatalf("seeks %q", got)
		}
	})
}

// A joiner already where the party is takes its stamp and seeks nowhere.
func TestAnAnswerWhereItAlreadyIsSeeksNowhere(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{joiner: true})
		n.mpvSays(mpvAt(40))
		n.broadcasts()
		n.peerSays("a", stamped(1, seekEvent(40.05)))
		n.peerSays("b", stamped(1, seekEvent(55)))
		if got := seeksIn(n.mpvWasTold()); len(got) != 0 {
			t.Fatalf("seeks %q, want neither answer to seek: it is there, and the later one loses", got)
		}
		// its next change is newer than the party's answer
		n.plays()
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		if got := n.broadcasts(); len(got) != 1 || got[0].Stamp != 3 {
			t.Fatalf("broadcast %+v, want its pause at stamp 3: it took the party's 1, then resumed at 2", got)
		}
	})
}

// A pause says where the pauser stopped. Over a slow link the others stop a round trip
// later; without this they resume that far apart and stay so.
func TestOwnPauseSaysWhereItStopped(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvSays(mpvAt(61.25), propertyChangeLine(pause, "yes", 0))
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != pause || got[0].At == nil || *got[0].At != 61.25 {
			t.Fatalf("broadcast %+v, want the pause, where it stopped", got)
		}
	})
}

// A node that loses the relay plays on, as the party most likely does, and says so. Back
// in the room it says, quietly, the state it holds; the party stops only for a state that
// isn't its own (take). A joiner that was never answered has none, and joins again.
func TestLosingTheRelayPlaysOnAndComingBackSaysWhereItIs(t *testing.T) {
	t.Parallel()
	t.Run("a member", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(4, false, 10)
			n.transportSays(IncomingMessage{Lost: true})
			if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text lost the relay, reconnecting 5000]" {
				t.Fatalf("told mpv %q, want only the notice", got)
			}
			n.transportSays(IncomingMessage{Back: true})
			got := n.broadcasts()
			if len(got) != 1 || !got[0].Check || got[0].Name != pause || got[0].Stamp != 4 || got[0].Paused == nil || *got[0].Paused || got[0].At == nil || *got[0].At != 10 {
				t.Fatalf("broadcast %+v, want its state, playing from 10 at stamp 4, as a check", got)
			}
		})
	})
	t.Run("a joiner never answered", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{joiner: true})
			n.transportSays(IncomingMessage{Lost: true})
			n.transportSays(IncomingMessage{Back: true})
			if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
				t.Fatalf("broadcast %q, want it to join again", got)
			}
		})
	})
}

// The last seconds are played out, as when a member leaves: a state that stops the party
// would hold everyone else just short of the end.
func TestComingBackNearTheEndSaysNothing(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(4, false, 117)
		n.transportSays(IncomingMessage{Lost: true})
		n.transportSays(IncomingMessage{Back: true})
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q, want nothing this close to the end", said(got))
		}
		// stopped by hand there, it says where like anywhere else
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		n.broadcasts()
		n.transportSays(IncomingMessage{Back: true})
		if got := n.broadcasts(); len(got) != 1 || !got[0].Check {
			t.Fatalf("broadcast %+v, want its state", got)
		}
	})
}

// What a member back from losing the relay says is weighed like any state, but a newer
// resume: made where nobody heard it, nobody knows how far it has played since.
func TestAStateSentBackAfterLosingTheRelay(t *testing.T) {
	t.Parallel()
	check := func(stamp int, paused bool, at float64) string {
		return fmt.Sprintf(`{"stamp":%d,"paused":%t,"at":%f,"check":true,"event":"property-change","name":"pause","data":"%s"}`, stamp, paused, at, map[bool]string{true: "yes", false: "no"}[paused])
	}
	for _, tc := range []struct {
		name   string
		paused bool
		event  string
		want   []string
	}{
		{"the party's own, playing", false, check(4, false, 30), nil},
		{"the party's own, stopped", true, check(4, true, 30), nil},
		{"older, while the party moved on", false, check(3, false, 10), []string{"show-text paused for c: catching up 5000", "set_property pause true"}},
		{"a newer pause", false, check(5, true, 25), []string{"set_property pause true", "show-text paused by c 5000", "set_property time-pos 25.000000"}},
		{"a newer resume, the party stopped", true, check(5, false, 30), []string{"show-text paused: out of sync with c 5000"}},
		{"a newer resume, the party playing", false, check(5, false, 30), []string{"show-text paused: out of sync with c 5000", "set_property pause true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.takes(4, tc.paused, 30)
				n.peerSays("c", tc.event)
				if got := n.mpvWasTold(); !slices.Equal(got, tc.want) {
					t.Fatalf("told mpv %q, want %q", got, tc.want)
				}
			})
		})
	}
}

// Moving on while playing, at the end of an entry or by hand, stops the party on the
// next one. Peers hear which entry first, so the pause and position land on it.
func TestMovingToAnEntryWhilePlayingPausesTheParty(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.plays()
		n.mpvSays(mpvAt(90))
		n.mpvSays(propertyChangeLine(playlistPos, "1", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[playlist-pos=1]" {
			t.Fatalf("broadcast %q, want the entry", got)
		}
		if got := n.mpvWasTold(); len(got) == 0 || got[0] != "set_property pause true" {
			t.Fatalf("told mpv %q, want the party paused", got)
		}
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes time-pos=0.000000 pause-reason=video changed]" {
			t.Fatalf("broadcast %q, want the pause, the new entry's start (not the old position), and why", got)
		}
	})
}

// A move is a seek far enough to land in another file: a follower's stays on its screen, and
// the party is still where it was. Resumed there, the party stops on that file and second,
// as for a move while playing, and this node leads: the others must load it first, and
// would play that far apart.
func TestAFollowersMoveStaysOnItsScreen(t *testing.T) {
	t.Parallel()
	t.Run("kept", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos})
			n.takes(3, true, 30)
			n.mpvSays(propertyChangeLine(playlistPos, "1", 0))
			n.keepsTheSeek("z")
			// a joiner is told the party's entry and second, not this screen's
			n.peerSays("j", `{"name":"joined"}`)
			got := n.broadcasts()
			if len(got) != 1 || *got[0].Entry != 0 || *got[0].At != 30 {
				t.Fatalf("answered %+v, want the party's entry 0 at 30", got)
			}
			// and where the party continues from, if it ends now
			if entry, at := n.c.Position(); entry != 0 || at != 30 {
				t.Fatalf("Position %d at %v, want the party's entry 0 at 30", entry, at)
			}
		})
	})
	t.Run("resumed there", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos})
			n.takes(3, true, 30)
			n.mpvSays(propertyChangeLine(playlistPos, "1", 0), mpvAt(12))
			n.keepsTheSeek("z")
			n.mpvSays(propertyChangeLine(pause, "no", 0))
			if got := n.broadcasts(); len(got) != 0 {
				t.Fatalf("broadcast %q, want no resume", said(got))
			}
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
			got := n.broadcasts()
			if fmt.Sprint(said(got)) != "[pause=yes time-pos=12.000000 pause-reason=video changed]" || *got[0].Entry != 1 || *got[0].At != 12 || got[0].Free {
				t.Fatalf("broadcast %+v, want the party stopped on entry 1 at 12, led by this node", got)
			}
		})
	})
}

// Only the person's own pause, seek or move leads a stop, while playing too: the party stops
// for it first. Any other stop is free, and the first seek leads: mpv moving on from the end
// of an entry (two players reach the end together, and the tie's winner would lead, e2e
// [R15c]), or running out of cache.
func TestOnlyThePersonsPauseOrSeekLeads(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		does func(n *node)
		free bool
	}{
		"the person's pause": {func(n *node) {
			n.plays()
			n.mpvSays(propertyChangeLine(pause, "yes", 0))
		}, false},
		"the person's seek": {func(n *node) { n.mpvSays(mpvAt(30)) }, false},
		"a seek while playing": {func(n *node) {
			n.plays()
			n.mpvSays(`{"event":"seek"}`, mpvAt(50), `{"event":"playback-restart"}`)
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
		}, false},
		"a move at the end": {func(n *node) {
			n.plays()
			n.mpvSays(mpvAt(118), propertyChangeLine(playlistPos, "1", 0))
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
		}, true},
		"a move by hand": {func(n *node) {
			n.plays()
			n.mpvSays(mpvAt(90), propertyChangeLine(playlistPos, "1", 0))
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
		}, false},
		"a move while stopped": {func(n *node) { n.mpvSays(propertyChangeLine(playlistPos, "1", 0)) }, false},
		"the person's pause after a stop nobody chose": {func(n *node) {
			n.plays()
			n.mpvSays(propertyChangeLine(pausedForCache, "yes", 0))
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
			n.mpvSays(propertyChangeLine(pausedForCache, "no", 0))
			n.plays()
			n.mpvSays(propertyChangeLine(pause, "yes", 0))
		}, false},
		"a peer's resume refused while out of cache": {func(n *node) {
			n.plays()
			n.mpvSays(propertyChangeLine(pausedForCache, "yes", 0))
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
			n.broadcasts()
			n.peerSays("a", stamped(20, `{"event":"property-change","name":"pause","data":"no","at":0,"entry":0}`))
		}, true},
		"running out of cache": {func(n *node) {
			n.plays()
			n.mpvSays(propertyChangeLine(pausedForCache, "yes", 0))
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{names: threeVideos})
				c.does(n)
				got := n.broadcasts()
				if len(got) == 0 {
					t.Fatal("broadcast nothing")
				}
				for _, e := range got {
					if e.Free != c.free {
						t.Fatalf("broadcast %+v, want free %v", e, c.free)
					}
				}
			})
		})
	}
}

// A new file reports time-pos 0 while paused. Sent on, it would seek a peer still
// playing the end of the last entry.
func TestMovingWhilePausedSendsOnlyTheEntry(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.mpvSays(propertyChangeLine(playlistPos, "2", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[playlist-pos=2]" {
			t.Fatalf("broadcast %q, want the entry", got)
		}
		n.mpvSays(mpvAt(0), propertyChangeLine(pause, "no", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=no]" {
			t.Fatalf("broadcast %q, want nothing before the resume", got)
		}
	})
}

// mpv reports where it starts as soon as it is observed, and -1 once the list runs out.
// Neither is someone moving.
func TestPlaylistPositionThatIsNotAMoveStaysLocal(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 2})
		n.mpvSays(propertyChangeLine(playlistPos, "-1", 0), propertyChangeLine(playlistPos, "0", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[playlist-pos=0]" {
			t.Fatalf("broadcast %q, want only the move to the first", got)
		}
	})
}

// mpv can report an entry before the file is loaded: the one it starts on, or one a key
// pressed while it loads took it to. That isn't a move, and the file's first time-pos must
// still count as loaded.
func TestStartingEntryBeforeTheFileLoadsIsNotAMove(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		// newNode returns once the node knows the file loaded
		n := newNode(t, setup{names: threeVideos, opening: []string{
			strings.TrimSpace(propertyChangeLine(playlistPos, "0", 0)), strings.TrimSpace(propertyChangeLine(playlistPos, "1", 0)), strings.TrimSpace(propertyChangeLine(pause, "yes", 0)), strings.TrimSpace(mpvAt(0)),
		}})
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q, want only joined", got)
		}
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != pause || got[0].Entry == nil || *got[0].Entry != 1 {
			t.Fatalf("broadcast %+v, want the resume, on entry 1", got)
		}
	})
}

// A peer's move opens the next file stopped, and mpv's report of it isn't sent back.
func TestFollowingAnEntryStopsFirstAndStaysQuiet(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.plays()
		n.peerSays("a", move(5, 2))
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true show-text paused by a 5000 set_property playlist-pos 2 show-text from e1 to e3, by a 5000]" {
			t.Fatalf("told mpv %q, want it stopped, then moved", got)
		}
		n.mpvObeys()
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=no]" {
			t.Fatalf("broadcast %q, want nothing before the resume", got)
		}
	})
}

// Every member answers a joiner, and two nodes can reach the end together.
func TestEntryAlreadyHeadedThereIsNotRepeated(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 1})
		n.peerSays("a", move(2, 1))
		n.peerSays("a", move(3, 2))
		n.peerSays("b", move(3, 2))
		n.peerSays("b", move(4, 3))
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property playlist-pos 2 show-text from e2 to e3, by a 5000]" {
			t.Fatalf("told mpv %q, want one move, and none to an entry it has no file for", got)
		}
	})
}

// A seek right after a move would land on the old file, which then ends and loses it.
// A joiner hits this every time: it is told the entry, then the position.
func TestSeekAfterFollowingAnEntryWaitsForTheFile(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.peerSays("a", move(5, 1))
		n.peerSays("a", stamped(6, tagged(1, seekEvent(40))))
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property playlist-pos 1 show-text from e1 to e2, by a 5000 show-text synced by a 5000]" {
			t.Fatalf("told mpv %q, want the move and no seek yet", got)
		}
		n.mpvObeys()
		if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 40.000000]" {
			t.Fatalf("seeks %q once e2 loaded, want the held seek", got)
		}
	})
}

// While a move loads, this node is at the new file's start, not on the second it left: a
// seek there on the new file is still to make. Seed 436890989886 of
// TestAJoinCrossingAMoveEndsOnThePartysEntry found it.
func TestASeekWhileLoadingToTheSecondItLeftIsMade(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.takes(3, true, 30)
		n.peerSays("a", move(5, 1))
		n.peerSays("a", stamped(6, tagged(1, seekEvent(30))))
		n.mpvObeys()
		if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 30.000000]" {
			t.Fatalf("seeks %q once e2 loaded, want the seek to 30", got)
		}
	})
}

// A key press opens a file before mpv reports the entry. A stop taken meanwhile is for the
// file this node left: its seek would land on the new one and go out as this node's own,
// after its move, so the party would follow it there. The move alone goes out, from 0.
// Seed 436890989886 of TestAJoinCrossingAMoveEndsOnThePartysEntry found it.
func TestAStopTakenAsAKeyPressOpensAFileDoesNotSeekIt(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.mpvSays(started(1), `{"event":"file-loaded"}`)
		// a stop nobody chose: one that leads would keep the person's move on this screen
		n.peerSays("b", `{"stamp":3,"at":30,"entry":0,"free":true,"event":"property-change","name":"pause","data":"yes"}`)
		n.mpvSays(propertyChangeLine(playlistPos, "1", 0), mpvAt(0))
		n.mpvObeys()
		got := n.broadcasts()
		if fmt.Sprint(said(got)) != "[playlist-pos=1]" || got[0].Stamp <= 3 || got[0].At == nil || *got[0].At != 0 {
			t.Fatalf("broadcast %+v, want only the move, newer than the stop, from 0", got)
		}
		if got := seeksIn(n.mpvWasTold()); len(got) != 0 {
			t.Fatalf("seeks %q, want none", got)
		}
	})
}

// A node that hasn't heard the party answers too, with what it holds: its stamp is below
// any the party has, so only a fellow joiner can take it.
func TestJoiningNodeAnswersWithAnUnheardStamp(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 2, joiner: true})
		n.mpvSays(mpvAt(7))
		n.broadcasts()
		n.peerSays("j", `{"name":"joined"}`)
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != timePos || got[0].Stamp >= 0 || got[0].Entry == nil || *got[0].Entry != 2 || got[0].At == nil || *got[0].At != 7 {
			t.Fatalf("broadcast %+v, want its state, unheard", got)
		}
	})
}

// An answer is one message: the entry, and where on it.
func TestJoinAnswerSaysTheEntryAndWhereOnIt(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 2})
		n.mpvSays(mpvAt(33))
		n.broadcasts()
		n.peerSays("j", `{"name":"joined"}`)
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != timePos || got[0].Data != "33.000000" || got[0].Entry == nil || *got[0].Entry != 2 {
			t.Fatalf("broadcast %+v, want the entry and the position", got)
		}
	})
}

// Everyone hears the answer to a joiner, and it can arrive after someone has moved on. It
// carries the stamp its sender had, so one sent just before two people crossed is older
// than both moves and sends nobody to the other's entry. One from someone who knows of a
// later move is followed by anyone, joiner or not.
func TestAnAnswerIsFollowedOnlyIfItIsNewer(t *testing.T) {
	t.Parallel()
	t.Run("an older answer reaching a member", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos, start: 1})
			n.takes(3, true, 0)
			n.peerSays("a", move(2, 2))
			if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text paused for a: catching up 5000]" {
				t.Fatalf("told mpv %q, want the party stopped for a, and no move", got)
			}
		})
	})
	t.Run("a fellow joiner's, to a joiner who moved by hand", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos, start: 1, joiner: true})
			n.mpvSays(propertyChangeLine(playlistPos, "0", 0))
			n.broadcasts()
			n.peerSays("a", move(unheard, 2))
			if got := n.mpvWasTold(); len(got) != 0 {
				t.Fatalf("told mpv %q, want nothing", got)
			}
		})
	})
	t.Run("a newer answer reaching a member", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos, start: 1})
			n.takes(3, true, 0)
			// an answer is a stop nobody chose: this node doesn't follow it, and its move goes out
			n.peerSays("a", `{"stamp":4,"free":true,"event":"property-change","name":"playlist-pos","data":"0"}`)
			if got := n.mpvWasTold(); len(got) == 0 || got[0] != "set_property playlist-pos 0" {
				t.Fatalf("told mpv %q, want to follow it", got)
			}
			n.mpvObeys()
			n.mpvSays(propertyChangeLine(playlistPos, "2", 0))
			if got := n.broadcasts(); len(got) != 1 || got[0].Stamp != 5 {
				t.Fatalf("moved with %+v, want stamp 5: one past the one it followed", got)
			}
		})
	})
}

// A newer stamp for the entry this node is already on moves nothing, and is still taken:
// its next move must beat it.
func TestANewerStampForTheSameEntryIsTaken(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 1})
		// mpv moving a on from the end: nobody leads, so this node's move goes out
		n.peerSays("a", `{"stamp":5,"free":true,"event":"property-change","name":"playlist-pos","data":"1"}`)
		if got := n.mpvWasTold(); len(got) != 0 {
			t.Fatalf("told mpv %q, want nothing for the entry it is on", got)
		}
		n.mpvSays(propertyChangeLine(playlistPos, "2", 0))
		if got := n.broadcasts(); len(got) != 1 || got[0].Stamp != 6 {
			t.Fatalf("moved with %+v, want one past the highest it heard", got)
		}
	})
}

// A joiner knows nothing, whatever it knew before, and whoever opened the party is the
// party from the start. Its own first change shows the stamp it holds.
func TestAJoinForgetsTheStampAndOpeningTakesOne(t *testing.T) {
	t.Parallel()
	stampOfASeek := func(n *node) int {
		n.t.Helper()
		n.mpvSays(mpvAt(9))
		got := n.broadcasts()
		if len(got) != 1 {
			n.t.Fatalf("broadcast %+v, want the seek", got)
		}
		return got[0].Stamp
	}
	t.Run("a joiner", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			if got := stampOfASeek(ready(t, setup{joiner: true})); got != unheard+1 {
				t.Fatalf("stamp %d, want one past unheard", got)
			}
		})
	})
	t.Run("an opener", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			if got := stampOfASeek(ready(t, setup{})); got != 2 {
				t.Fatalf("stamp %d, want one past 1", got)
			}
		})
	})
	t.Run("one who reopens keeps its stamp", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(7, true, 0)
			n.transportSays(IncomingMessage{Opened: true})
			// a follower's seek stays on its screen: its resume shows the stamp
			n.mpvSays(propertyChangeLine(pause, "no", 0))
			if got := n.broadcasts(); len(got) != 1 || got[0].Stamp != 8 {
				t.Fatalf("broadcast %+v, want its resume, one past 7", got)
			}
		})
	})
	t.Run("one that joins again forgets it", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(7, true, 0)
			// mpv refusing a step it was told is what makes a member ask the party again
			n.mpvRefuses("set_property time-pos", "property unavailable")
			n.peerSays("a", stamped(8, seekEvent(20)))
			if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
				t.Fatalf("broadcast %q, want joined", got)
			}
			if got := stampOfASeek(n); got != unheard+1 {
				t.Fatalf("stamp %d, want one past unheard", got)
			}
		})
	})
}

// A seek mpv refused never happened: the user's own seek to that second, once mpv takes
// seeks again, is no echo and goes out. So does one held for a file whose move mpv refused.
func TestARefusedSeekLeavesNoMarker(t *testing.T) {
	t.Parallel()
	userSeeks := func(n *node, pos float64) {
		n.mpvTakesAgain()
		n.mpvSays(`{"event":"seek"}`, mpvAt(pos), `{"event":"playback-restart"}`)
		if got := said(n.broadcasts()); fmt.Sprint(got) != fmt.Sprintf("[time-pos=%f]", pos) {
			t.Fatalf("broadcast %q, want the user's seek", got)
		}
	}
	t.Run("a seek", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(7, true, 0)
			n.mpvRefuses("set_property time-pos", "property unavailable")
			n.peerSays("a", stamped(8, seekEvent(20)))
			n.broadcasts()
			userSeeks(n, 20)
		})
	})
	t.Run("a seek held for a move", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos})
			n.takes(7, true, 0)
			n.mpvRefuses("set_property playlist-pos", "property unavailable")
			n.peerSays("a", `{"stamp":8,"at":20,"entry":1,"event":"property-change","name":"time-pos","data":"20.000000"}`)
			n.broadcasts()
			userSeeks(n, 20)
		})
	})
	t.Run("a seek let go once the file loaded", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos})
			n.takes(7, true, 0)
			n.mpvRefuses("set_property time-pos", "property unavailable")
			n.peerSays("a", `{"stamp":8,"at":20,"entry":1,"event":"property-change","name":"time-pos","data":"20.000000"}`)
			n.mpvObeys()
			if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 20.000000]" {
				t.Fatalf("seeks %q, want the held seek let go", got)
			}
			n.broadcasts()
			// a follower keeps the person's seek, and the notice says it wasn't taken for an echo
			n.mpvTakesAgain()
			n.mpvWasTold()
			n.mpvSays(`{"event":"seek"}`, mpvAt(20), `{"event":"playback-restart"}`)
			n.keepsTheSeek("a")
		})
	})
}

func tagged(entry int, event string) string {
	return fmt.Sprintf(`{"entry":%d,%s`, entry, event[1:])
}

// stamped is a peer's message as it arrives from a party: with the stamp of the change it
// is part of. Without one it is as old as a node that has heard nothing.
func stamped(stamp int, event string) string {
	return fmt.Sprintf(`{"stamp":%d,%s`, stamp, event[1:])
}

// Everything a node sends says its stamp, which each change of its own raises, where it
// is, and in a playlist which entry it belongs to.
func TestMessagesSayWhichEntryTheyBelongTo(t *testing.T) {
	t.Parallel()
	t.Run("a playlist", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos})
			n.mpvSays(propertyChangeLine(playlistPos, "2", 0))
			if got := n.broadcasts(); len(got) != 1 || got[0].Name != playlistPos || got[0].Stamp != 2 || got[0].Entry == nil || *got[0].Entry != 2 {
				t.Fatalf("broadcast %+v, want a move to 2 with a new stamp", got)
			}
			n.mpvSays(mpvAt(0), mpvAt(9))
			if got := n.broadcasts(); len(got) != 1 || got[0].Name != timePos || got[0].Stamp != 3 || got[0].At == nil || *got[0].At != 9 || got[0].Entry == nil || *got[0].Entry != 2 {
				t.Fatalf("broadcast %+v, want a seek on entry 2, at 9, with the next stamp", got)
			}
		})
	})
	t.Run("a single file", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.mpvSays(mpvAt(9))
			if got := n.broadcasts(); len(got) != 1 || got[0].Entry != nil || got[0].Stamp != 2 {
				t.Fatalf("broadcast %+v, want a stamp and no entry", got)
			}
		})
	})
}

// A seek or a resume made at the same moment as the move that brought this node here
// carries the move's stamp and a lower entry: it belongs to a file the party has left. A
// pause still stops us: stopping is always safe.
func TestMessagesFromAnEntryThePartyLeftAreDropped(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 1})
		n.takes(2, true, 0)
		n.peerSays("a", stamped(2, tagged(0, seekEvent(40))))
		n.peerSays("a", stamped(2, tagged(0, `{"name":"pause","data":"no"}`)))
		n.peerSays("a", stamped(3, tagged(1, seekEvent(41))))
		if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 41.000000]" {
			t.Fatalf("seeks %q, want only the seek on its entry", got)
		}
	})
}

// A newer pause or seek from another entry means the party is there: this node missed the
// move. It goes there first, then to the second it was told, once the file has loaded.
func TestANewerSeekFromAnotherEntryBringsThisNodeThere(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.peerSays("a", `{"stamp":3,"entry":2,"event":"property-change","name":"time-pos","data":"40.000000"}`)
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property playlist-pos 2 show-text from e1 to e3, by a 5000 show-text synced by a 5000]" {
			t.Fatalf("told mpv %q", got)
		}
		n.mpvObeys()
		if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 40.000000]" {
			t.Fatalf("seeks %q, want the second it was told, on the new file", got)
		}
	})
}

// It loses to the higher entry, and this node, playing, answers by stopping where it is.
func TestAPauseFromAnotherEntryStillStops(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 1})
		n.plays()
		n.mpvSays(mpvAt(5))
		n.peerSays("a", `{"stamp":2,"at":30,"entry":0,"name":"pause","data":"yes"}`)
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true]" {
			t.Fatalf("told mpv %q, want the pause, and no seek", got)
		}
	})
}

// While a move is on its way, the entry it is heading to is the one that counts.
func TestMessagesAreCheckedAgainstWhereWeAreHeading(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.peerSays("a", move(2, 1))
		n.peerSays("a", stamped(2, tagged(0, seekEvent(40))))
		n.peerSays("a", stamped(3, tagged(1, seekEvent(41))))
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property playlist-pos 1 show-text from e1 to e2, by a 5000 show-text synced by a 5000]" {
			t.Fatalf("told mpv %q", got)
		}
		n.mpvObeys()
		if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 41.000000]" {
			t.Fatalf("seeks %q, want the seek on the new entry", got)
		}
	})
}

// move is a peer's playlist entry as it arrives: a move, or an answer to a joiner.
func move(stamp, to int) string {
	return fmt.Sprintf(`{"stamp":%d,"event":"property-change","name":"playlist-pos","data":"%d"}`, stamp, to)
}

// Two people move at once, one forward and one back, so their moves carry the same
// stamp. Without a rule each follows the other and they swap. Everyone applies the same
// rule, so they end up together on the higher one.
func TestTwoMovesFromTheSameEntrySettleOnTheHigher(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ mine, theirs int }{
		"we went higher": {2, 0},
		"we went lower":  {0, 2},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{names: threeVideos, start: 1})
				n.mpvSays(propertyChangeLine(playlistPos, fmt.Sprint(c.mine), 0))
				n.broadcasts()
				n.peerSays("a", move(2, c.theirs))
				moved := ""
				for _, s := range n.mpvWasTold() {
					if strings.HasPrefix(s, "set_property playlist-pos") {
						moved = s
					}
				}
				if want := map[bool]string{true: "", false: fmt.Sprintf("set_property playlist-pos %d", c.theirs)}[c.mine > c.theirs]; moved != want {
					t.Fatalf("told mpv %q, want %q: the higher one", moved, want)
				}
			})
		})
	}
}

// A third member hears both moves, in either order, and must land in the same place.
func TestAThirdMemberSettlesOnTheHigherInEitherOrder(t *testing.T) {
	t.Parallel()
	for name, moves := range map[string][2]int{"higher first": {2, 0}, "lower first": {0, 2}} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{names: threeVideos, start: 1})
				n.peerSays("a", move(2, moves[0]))
				n.peerSays("b", move(2, moves[1]))
				last := ""
				for _, s := range n.mpvWasTold() {
					if strings.HasPrefix(s, "set_property playlist-pos") {
						last = s
					}
				}
				if last != "set_property playlist-pos 2" {
					t.Fatalf("settled with %q, want the higher one", last)
				}
			})
		})
	}
}

// Someone behind everyone else hears a tie too, and what it compares is the stamps, not
// where it happens to be.
func TestATieIsSeenFromAnyEntry(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.peerSays("a", move(2, 2))
		n.peerSays("b", move(2, 1))
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property playlist-pos 2 show-text from e1 to e3, by a 5000]" {
			t.Fatalf("told mpv %q, want the higher one alone", got)
		}
	})
}

// The higher entry only settles a tie. A newer move is followed, lower or not, so someone
// who missed a message catches up on the next one instead of staying apart.
func TestALowerMoveThatIsNotATieIsFollowed(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 2})
		n.peerSays("a", move(2, 0))
		if got := n.mpvWasTold(); len(got) == 0 || got[0] != "set_property playlist-pos 0" {
			t.Fatalf("told mpv %q, want it followed", got)
		}
	})
}

// Two players reach the end of an entry together. One decides to follow the other, and
// its own move arrives while it is pausing. The set that follows would change nothing, so
// mpv would never answer it with an event, and it would wait for a load that has passed,
// holding every later seek forever. mpv reports a property only once its event queue is
// empty (client.h), so its report of the entry can come before file-loaded or after it,
// and the load can be over before the move arrives. A seek held for the file and let go at
// its file-loaded must still be marked when the report comes: its echo is no one's seek.
func TestOwnMoveWhileFollowingDoesNotHoldLaterSeeks(t *testing.T) {
	t.Parallel()
	started := strings.TrimSpace(started(2))
	loaded := `{"event":"file-loaded"}`
	reported := strings.TrimSpace(propertyChangeLine(playlistPos, "2", 0))
	for _, tc := range []struct {
		name                            string
		before, pausing, after, seeking []string
	}{
		{"reported, then loaded", nil, []string{started, reported, loaded}, nil, nil},
		{"loaded, then reported", nil, []string{started, loaded, reported}, nil, nil},
		{"loaded before the move came", []string{started, loaded}, []string{reported}, nil, nil},
		{"loaded with a seek held, then reported", nil, nil, []string{started, loaded}, []string{reported}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{names: threeVideos, start: 1})
				n.plays()
				n.mpvAnswersLate()
				n.mpvSays(tc.before...)
				n.peerSays("a", move(5, 2))
				// what mpv says while it pauses, before it answers
				n.mpvSays(tc.pausing...)
				n.mpvAnswers()
				n.peerSays("a", stamped(6, tagged(2, seekEvent(5))))
				n.mpvSays(tc.after...)
				// setting the entry mpv reached by itself is a no-op (MPV.md), and loading, set
				// before mpv was told, was cleared by that file's file-loaded
				if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 5.000000]" {
					t.Fatalf("seeks %q, want the seek let go", got)
				}
				// the person resumes once the seek has landed: what goes out first
				n.mpvSays(append(tc.seeking, `{"event":"seek"}`, mpvAt(5), `{"event":"playback-restart"}`, propertyChangeLine(pause, "no", 0))...)
				n.mpvAnswers()
				if got := said(n.broadcasts()); len(got) == 0 || got[0] != "pause=no" {
					t.Fatalf("broadcast %q, want the person's resume first", got)
				}
			})
		})
	}
}

// mpv's first report of where it started can land after a peer already sent this node to
// another video. That report isn't the echo of the move, so it mustn't use up the marker,
// or the real echo goes out as this node moving the party.
func TestAnEarlierPositionDoesNotUseUpTheMarker(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, opening: []string{strings.TrimSpace(propertyChangeLine(pause, "yes", 0)), strings.TrimSpace(mpvAt(0))}})
		n.peerSays("a", move(5, 1))
		if got := n.mpvWasTold(); len(got) == 0 || got[0] != "set_property playlist-pos 1" {
			t.Fatalf("told mpv %q", got)
		}
		n.mpvSays(propertyChangeLine(playlistPos, "0", 0), propertyChangeLine(playlistPos, "1", 0), propertyChangeLine(pause, "no", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=no]" {
			t.Fatalf("broadcast %q, want nothing before the resume", got)
		}
	})
}

// Where a node stopped is where a continued party starts, except right at an entry's
// end (mpv's keep-open=always stays there): then it is the next entry, from its start.
func TestPositionToContinueFrom(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		names    []string
		current  int
		pos, dur float64
		entry    int
		at       float64
	}{
		{nil, 0, 50, 100, 0, 50},
		{nil, 0, 97, 100, 0, 0},
		{nil, 0, 12, 0, 0, 12},
		{threeVideos, 1, 50, 100, 1, 50},
		{threeVideos, 1, 97, 100, 2, 0},
		{threeVideos, 2, 97, 100, 2, 0},
	} {
		t.Run(fmt.Sprintf("%d entries, on %d at %v of %v", len(c.names), c.current, c.pos, c.dur), func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{names: c.names, start: c.current})
				n.mpvSays(propertyChangeLine(duration, fmt.Sprintf("%f", c.dur), 0), mpvAt(c.pos))
				if entry, at := n.c.Position(); entry != c.entry || at != c.at {
					t.Fatalf("entry %d at %v, want %d at %v", entry, at, c.entry, c.at)
				}
			})
		})
	}
}

// Only the end of the last entry, or of the only file, finishes the party: any other
// entry has a next one to continue from.
func TestFinishedOnlyAtTheEndOfTheLastEntry(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		names    []string
		current  int
		pos      float64
		finished bool
	}{{nil, 0, 97, true}, {nil, 0, 50, false}, {threeVideos, 2, 97, true}, {threeVideos, 1, 97, false}, {threeVideos, 2, 50, false}} {
		t.Run(fmt.Sprintf("%d entries, on %d at %v", len(c.names), c.current, c.pos), func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{names: c.names, start: c.current})
				n.mpvSays(propertyChangeLine(duration, "100.000000", 0), mpvAt(c.pos))
				if got := n.c.Finished(); got != c.finished {
					t.Fatalf("finished %v, want %v", got, c.finished)
				}
			})
		})
	}
}

// A node that opens on a later entry can hear the party name that entry before mpv has
// reported where it is. Taking itself for entry 0, it set the entry it was already on,
// which mpv answers with nothing, and waited for that load forever.
func TestJoinerKnowsItsStartingEntryBeforeMpvSaysSo(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{names: threeVideos, start: 1, joiner: true,
			heard:   []IncomingMessage{peer("a", `{"event":"property-change","name":"playlist-pos","data":"1"}`)},
			opening: []string{strings.TrimSpace(propertyChangeLine(pause, "yes", 0)), strings.TrimSpace(mpvAt(0))},
		})
		if got := n.mpvWasTold(); len(got) != 0 {
			t.Fatalf("told mpv %q, want nothing: it is already on that entry", got)
		}
	})
}

// A continued party seeks the file it continues on once it is open, and only it. Then
// whatever the party sent while it loaded is applied, so the party's position wins.
// The seek is its own echo, so nobody else is moved by it.
func TestContinuingSeeksBeforeThePartyIsHeard(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{at: 40, joiner: true, heard: []IncomingMessage{peer("a", stamped(1, seekEvent(55)))}})
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property time-pos 40.000000 set_property time-pos 55.000000 show-text synced by a 5000]" {
			t.Fatalf("told mpv %q, want its own spot, then the party's", got)
		}
		n.mpvObeys()
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q: continuing moved the party", got)
		}
	})
}

// A pause says where its sender stopped, and a seek that it was stopped: in a mesh a send
// can fail, and either one alone must leave the node that got it stopped on that second.
func TestAPauseSaysWhereAndASeekSaysStopped(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ event, want string }{
		"a pause alone": {`{"stamp":5,"at":40,"name":"pause","data":"yes"}`, "[set_property pause true show-text paused by a 5000 set_property time-pos 40.000000]"},
		"a seek alone":  {`{"stamp":5,"name":"time-pos","data":"40.000000"}`, "[set_property pause true show-text paused by a 5000 set_property time-pos 40.000000 show-text synced by a 5000]"},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.plays()
				n.mpvSays(mpvAt(30))
				n.peerSays("a", c.event)
				if got := n.mpvWasTold(); fmt.Sprint(got) != c.want {
					t.Fatalf("told mpv %q, want %q", got, c.want)
				}
			})
		})
	}
}

// A resume says where from, so a node that missed the seek before it starts there too.
func TestAResumeSaysWhereFrom(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(mpvAt(30))
		n.peerSays("a", `{"stamp":5,"at":40,"name":"pause","data":"no"}`)
		if got := n.mpvWasTold(); len(got) < 2 || fmt.Sprint(got[:2]) != "[set_property time-pos 40.000000 set_property pause false]" {
			t.Fatalf("told mpv %q, want to seek where the resume is from, then play", got)
		}
	})
}

// A pause, a resume or a seek older than what this node holds comes from a node that
// missed something. It isn't applied: the party stops if it was playing, says why, and
// tells that node where it is, as a seek, which that node takes as newer than its own.
func TestAnOlderChangeIsAnsweredNotApplied(t *testing.T) {
	t.Parallel()
	for name, old := range map[string]string{
		"a resume": `{"stamp":2,"at":10,"name":"pause","data":"no"}`,
		"a seek":   `{"stamp":2,"name":"time-pos","data":"10.000000"}`,
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.takes(3, true, 30)
				n.peerSays("b", old)
				if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text paused for b: catching up 5000]" {
					t.Fatalf("told mpv %q, want only why", got)
				}
				if got := n.broadcasts(); len(got) != 1 || got[0].Name != timePos || got[0].Data != "30.000000" || got[0].Stamp != 3 {
					t.Fatalf("answered %+v, want where the party is, with its stamp", got)
				}
			})
		})
	}
	t.Run("a pause, to a party playing", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(3, false, 30)
			n.peerSays("b", `{"stamp":2,"at":10,"name":"pause","data":"yes"}`)
			// why first, so the GUI's feed doesn't take the pause for the person's own
			if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text paused for b: catching up 5000 set_property pause true]" {
				t.Fatalf("told mpv %q", got)
			}
			n.mpvObeys()
			// its own pause is the answer: newer, and where it stopped
			if got := n.broadcasts(); len(got) != 1 || got[0].Name != pause || got[0].Stamp != 4 || got[0].At == nil || *got[0].At != 30 {
				t.Fatalf("broadcast %+v, want its own pause, newer, where it stopped", got)
			}
		})
	})
}

// Two changes with the same stamp were made at the same moment, and everyone must pick
// the same one: stopped over playing, then the earlier second. The one that loses is
// told, quietly, since its sender may never have heard the winner.
func TestChangesMadeAtOnceGoToTheSaferOne(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		event, does string
		answers     bool
	}{
		"a resume loses to a seek":    {`{"stamp":3,"at":30,"name":"pause","data":"no"}`, "", true},
		"a later seek loses":          {`{"stamp":3,"name":"time-pos","data":"50.000000"}`, "", true},
		"a later stop loses":          {`{"stamp":3,"at":50,"name":"pause","data":"yes"}`, "", true},
		"an earlier seek wins":        {`{"stamp":3,"name":"time-pos","data":"20.000000"}`, "set_property time-pos 20.000000", false},
		"an earlier stop wins":        {`{"stamp":3,"at":20,"name":"pause","data":"yes"}`, "set_property time-pos 20.000000", false},
		"the same second is the same": {`{"stamp":3,"name":"time-pos","data":"40.000000"}`, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.takes(3, true, 40)
				n.peerSays("b", c.event)
				if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != fmt.Sprint(slices.DeleteFunc([]string{c.does}, func(s string) bool { return s == "" })) {
					t.Fatalf("seeks %q, want %q", got, c.does)
				}
				got := n.broadcasts()
				if c.answers && (len(got) != 1 || got[0].Name != timePos || got[0].Data != "40.000000" || got[0].Stamp != 3) {
					t.Fatalf("answered %+v, want where this node is", got)
				}
				if !c.answers && len(got) != 0 {
					t.Fatalf("broadcast %q, want nothing", said(got))
				}
			})
		})
	}
	t.Run("a seek made as a node resumed stops it", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(3, false, 40)
			n.peerSays("b", `{"stamp":3,"name":"time-pos","data":"50.000000"}`)
			if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true show-text paused by b 5000 set_property time-pos 50.000000 show-text synced by b 5000]" {
				t.Fatalf("told mpv %q", got)
			}
		})
	})
}

// A joiner takes the party's stamp with its position. Left unheard, the first thing it did
// would be older than what the party holds, and nobody would follow it.
func TestAJoinerTakesThePartysStampWithItsPosition(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{joiner: true})
		n.peerSays("a", stamped(5, seekEvent(40)))
		if got := n.mpvWasTold(); len(got) == 0 || got[0] != "set_property time-pos 40.000000" {
			t.Fatalf("told mpv %q, want the party's position", got)
		}
		n.mpvObeys()
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != pause || got[0].Stamp != 6 {
			t.Fatalf("resumed with %+v, want a stamp newer than the party's", got)
		}
	})
}

// Where this node is, for a tie, is where it is heading: not any second it has been sent
// to since the last resume, which is kept only to know mpv's echo of that seek.
func TestATieIsWeighedAgainstWhereThisNodeIsHeading(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("a", `{"stamp":2,"name":"time-pos","data":"10.000000"}`)
		n.peerSays("a", `{"stamp":3,"name":"time-pos","data":"20.000000"}`)
		n.peerSays("b", `{"stamp":3,"at":10,"name":"pause","data":"yes"}`)
		if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 10.000000 set_property time-pos 20.000000 set_property time-pos 10.000000]" {
			t.Fatalf("seeks %q, want the earlier of two stops made at once, though this node was there before", got)
		}
	})
}

// An entry a peer answers with says where on it, so a node that takes the party's stamp
// from it takes the party's second too. A node already on that entry does as well.
func TestAnEntryAnswerSaysWhereOnIt(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 1})
		n.mpvSays(mpvAt(10))
		n.peerSays("a", `{"stamp":4,"at":30,"event":"property-change","name":"playlist-pos","data":"1"}`)
		if got := n.mpvWasTold(); len(got) == 0 || got[0] != "set_property time-pos 30.000000" {
			t.Fatalf("told mpv %q, want the party's second on the entry it is already on", got)
		}
	})
}

// A node that answers while a move it followed is still loading names the entry it is
// heading to, and the start of that file, or the seek waiting for it: not the second it is
// on in the file it is leaving.
func TestAnAnswerWhileAMoveLoadsIsAboutTheNewEntry(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.mpvSays(mpvAt(30))
		n.broadcasts()
		n.peerSays("a", move(3, 1))
		n.peerSays("j", `{"name":"joined"}`)
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != timePos || got[0].Data != "0.000000" || got[0].Entry == nil || *got[0].Entry != 1 || got[0].Stamp != 3 {
			t.Fatalf("answered %+v, want entry 1 at its start, with the move's stamp", got)
		}
	})
}

// mpv says playback-restart when a file is ready, and on a slow stream that can come after
// this node was already sent somewhere. It doesn't mean the seek has landed: until mpv
// reports that second, this node is still heading to it. Taken for where mpv last was, the
// start, a joiner lost a tie to the next member's stop and told the party it was at 0, and
// the party went there.
func TestASeekIsStillOnItsWayAfterAStrayPlaybackRestart(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{joiner: true})
		n.peerSays("a", `{"stamp":8,"at":53.1,"name":"pause","data":"yes"}`)
		n.mpvSays(`{"event":"playback-restart"}`)
		n.peerSays("b", `{"stamp":8,"at":53.3,"name":"pause","data":"yes"}`)
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property time-pos 53.100000]" {
			t.Fatalf("told mpv %q, want the party's second and nothing more", got)
		}
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=53.100000]" {
			t.Fatalf("answered %q, want the earlier of the two stops, where it is heading", got)
		}
	})
}

// What is kept while loading is the newest by its stamp, not the last to arrive: members'
// messages come in any order, and an older one applied after a newer one would be answered
// as if its sender had fallen behind, by a node that hasn't even joined.
func TestTheNewestHeardWhileLoadingIsByStampNotArrival(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{joiner: true, heard: []IncomingMessage{
			peer("a", `{"stamp":5,"at":30,"name":"pause","data":"yes"}`),
			peer("b", `{"stamp":3,"event":"property-change","name":"time-pos","data":"10.000000"}`),
		}})
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property time-pos 30.000000]" {
			t.Fatalf("told mpv %q, want only the newer of the two", got)
		}
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q, want joined and nothing before it", got)
		}
	})
}

// What a joiner does before it has heard the party is not a member falling behind: its
// join is being answered, so nobody stops again or says so.
func TestAJoinersOwnChangeIsNotCaughtUp(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(3, true, 30)
		n.peerSays("j", fmt.Sprintf(`{"stamp":%d,"event":"property-change","name":"time-pos","data":"10.000000"}`, unheard+1))
		n.peerSays("j", fmt.Sprintf(`{"stamp":%d,"at":10,"name":"pause","data":"no"}`, unheard+2))
		if got, told := n.broadcasts(), n.mpvWasTold(); len(got) != 0 || len(told) != 0 {
			t.Fatalf("answered %q and told mpv %q, want nothing", said(got), told)
		}
	})
}

// Its user's own change while a move it followed loads is about the entry it is heading
// to. Tagged with the file mpv is still on, it carried the party's newest stamp on the
// entry the party left, and dragged everyone back there.
func TestOwnChangeWhileAMoveLoadsIsAboutTheNewEntry(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.mpvSays(mpvAt(30))
		n.broadcasts()
		n.peerSays("a", move(3, 1))
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != pause || got[0].Entry == nil || *got[0].Entry != 1 || got[0].At == nil || *got[0].At != 0 || got[0].Stamp != 4 {
			t.Fatalf("resumed with %+v, want entry 1 from its start, newer than the move", got)
		}
	})
}

// A newer resume from another entry means this node missed the move. It goes there
// stopped, as the file has to load, and stops the party instead of playing that far behind.
func TestANewerResumeFromAnotherEntryStopsTheParty(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.plays()
		n.mpvSays(mpvAt(5))
		n.peerSays("a", `{"stamp":3,"at":10,"entry":2,"name":"pause","data":"no"}`)
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true show-text paused by a 5000 set_property playlist-pos 2 show-text from e1 to e3, by a 5000]" {
			t.Fatalf("told mpv %q", got)
		}
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != timePos || got[0].Data != "10.000000" || got[0].Stamp != 3 || got[0].Entry == nil || *got[0].Entry != 2 {
			t.Fatalf("answered %+v, want where it holds the party is, stopped", got)
		}
	})
}

// Every node sends its whole state, so a message missing part of it is refused rather than
// filled in from this node's own: each of these is newer, and would move it.
func TestAMessageWithoutItsStateIsRefused(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.mpvSays(mpvAt(40))
		n.broadcasts()
		// raw, not through peer, which fills in what a node always sends
		for _, e := range []string{
			`{"stamp":3,"at":10,"entry":0,"name":"time-pos","data":"10.000000"}`,
			`{"stamp":3,"paused":true,"entry":0,"name":"pause","data":"yes"}`,
			`{"stamp":3,"paused":true,"at":10,"name":"time-pos","data":"10.000000"}`,
			`{"stamp":3,"paused":true,"at":0,"entry":3,"name":"playlist-pos","data":"3"}`,
			`{"stamp":3,"paused":true,"at":1e999,"entry":0,"name":"time-pos","data":"10.000000"}`,
		} {
			n.transportSays(IncomingMessage{HostName: "a", From: "a", Event: []byte(e)})
		}
		if got, told := n.broadcasts(), n.mpvWasTold(); len(got) != 0 || len(told) != 0 {
			t.Fatalf("broadcast %q and told mpv %q for messages without their state", said(got), told)
		}
	})
}

// A move while playing stops the party, so it says stopped before mpv has: whoever takes it
// opens the file stopped instead of answering a node it thinks plays on.
func TestAMoveWhilePlayingSaysStopped(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.plays()
		n.mpvSays(propertyChangeLine(playlistPos, "2", 0))
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != playlistPos || got[0].Paused == nil || !*got[0].Paused {
			t.Fatalf("broadcast %+v, want the move, stopped", got)
		}
	})
}

// A joiner that pressed play before it was answered is not out of sync with the party: it
// isn't in it yet, and stopping the party with its own second would drag the party there.
func TestAJoinerPlayingIsNotOutOfSync(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{joiner: true})
		n.plays()
		n.mpvSays(mpvAt(40))
		n.transportSays(IncomingMessage{HostName: "a", From: "a", Missed: true})
		n.peerSays("a", `{"stamp":2,"at":30,"name":"pause","data":"no"}`)
		if got := n.mpvWasTold(); slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "out of sync") }) {
			t.Fatalf("told mpv %q, want nothing until its join is answered", got)
		}
	})
}

// A resume reaching a node that plays says it is out of step: newer, it missed the pause
// in between, which no stamp shows; a tie from a member something this node sent never
// reached, it resumed from what it last heard. The party stops, each where it is, and
// settles on the earliest; nobody is moved by the resume.
func TestAResumeToANodeThatPlaysStopsTheParty(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		resume string
		missed bool
	}{
		"newer":                     {`{"stamp":4,"at":30,"name":"pause","data":"no"}`, false},
		"newer, from where it is":   {`{"stamp":4,"at":40,"name":"pause","data":"no"}`, false},
		"a tie, from one it missed": {`{"stamp":3,"at":40,"name":"pause","data":"no"}`, true},
		"a tie, from just ahead":    {`{"stamp":3,"at":40.15,"name":"pause","data":"no"}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.takes(3, false, 40)
				if c.missed {
					n.transportSays(IncomingMessage{HostName: "a", From: "a", Missed: true})
				}
				n.peerSays("a", c.resume)
				if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text paused: out of sync with a 5000 set_property pause true]" {
					t.Fatalf("told mpv %q", got)
				}
				n.mpvObeys()
				var r Event
				json.Unmarshal([]byte(c.resume), &r)
				if got := n.broadcasts(); len(got) != 1 || got[0].Name != pause || got[0].At == nil || *got[0].At != 40 || got[0].Stamp != r.Stamp+1 {
					t.Fatalf("broadcast %+v, want its own pause where it is, newer than the resume", got)
				}
			})
		})
	}
}

// A tie with the resume this node plays, from a member that heard it and resumed from the
// same stop, is the same resume made at once. Each stopped within timePosTolerance of the
// party's second, so the two can name seconds twice that apart.
func TestATieFromAMemberThatMissedNothingMovesNothing(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(3, false, 40)
		n.peerSays("a", `{"stamp":3,"at":39.82,"name":"pause","data":"no"}`)
		if got := n.mpvWasTold(); len(got) != 0 {
			t.Fatalf("told mpv %q, want nothing for the same resume", got)
		}
	})
}

// Two resumes with one stamp from different seconds were made from different stops: the
// person at one node resumed while it was still being moved to where the other stopped
// (SIM_SEED=10223). Neither is newer, so each would play on, that far apart for good. The
// party stops, each where it is, and settles on the earliest. The resume this node plays
// is its own or one it took. Sent again or back from losing the relay, a resume says where
// its sender is now, not where it resumed from, so it is weighed by the rules for those.
func TestATieFromAnotherSecondStopsTheParty(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		own    bool
		resume string
		want   string
	}{
		"taken":                  {false, `{"stamp":3,"at":30,"name":"pause","data":"no"}`, "[show-text paused: out of sync with a 5000 set_property pause true]"},
		"its own":                {true, `{"stamp":3,"at":40,"name":"pause","data":"no"}`, "[show-text paused: out of sync with a 5000 set_property pause true]"},
		"from ahead":             {false, `{"stamp":3,"at":50,"name":"pause","data":"no"}`, "[show-text paused: out of sync with a 5000 set_property pause true]"},
		"back from losing relay": {false, `{"stamp":3,"at":30,"check":true,"name":"pause","data":"no"}`, "[]"},
		"sent again":             {false, `{"stamp":3,"at":30,"again":true,"name":"pause","data":"no"}`, "[]"},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				if c.own {
					// the person moves on from the stop it took, which a follower keeps, then resumes
					// from there
					n.takes(2, true, 40)
					n.mpvSays(mpvAt(45))
					n.mpvSays(propertyChangeLine(pause, "no", 0))
					if got := n.broadcasts(); len(got) != 1 || got[0].Stamp != 3 || *got[0].At != 45 {
						t.Fatalf("broadcast %+v, want its resume from 45 with stamp 3", got)
					}
					n.mpvWasTold()
				} else {
					n.takes(3, false, 40)
				}
				n.peerSays("a", c.resume)
				if got := n.mpvWasTold(); fmt.Sprint(got) != c.want {
					t.Fatalf("told mpv %q, want %s", got, c.want)
				}
			})
		})
	}
}

// The state sent again to a member the mesh missed goes to everyone, and can cross a newer
// change on its way. Older, it is let go: its sender only hasn't heard that change yet. An
// older message that isn't sent again still says its sender is behind.
func TestAnOlderStateSentAgainIsLetGo(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		event string
		want  string
	}{
		"sent again": {`{"stamp":2,"at":30,"again":true,"name":"pause","data":"yes"}`, "[]"},
		"not":        {`{"stamp":2,"at":30,"name":"pause","data":"yes"}`, "[show-text paused for a: catching up 5000]"},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.takes(3, true, 40)
				n.peerSays("a", c.event)
				if got := n.mpvWasTold(); fmt.Sprint(got) != c.want {
					t.Fatalf("told mpv %q, want %s", got, c.want)
				}
			})
		})
	}
}

// A member that something this node sent never reached is sent its state again, as a
// pause marked again, each time the transport asks, and to that member alone: it goes
// back to the transport, not out to the party. Being told it missed something sends
// nothing: that comes right after the send that failed. A joiner has no state yet, and a
// state taken from that very member it has.
func TestAMemberThatMissedSomethingIsSentTheStateAgain(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		paused bool
		joiner bool
		by     string
		want   string
	}{
		"stopped":       {true, false, "z", "yes"},
		"playing":       {false, false, "z", "no"},
		"taken from b":  {false, false, "b", "no"},
		"joining":       {true, true, "", ""},
		"taken from it": {false, false, "a", ""},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{joiner: c.joiner})
				if c.joiner {
					n.mpvSays(mpvAt(40))
					n.broadcasts()
				} else {
					n.takesFrom(c.by, 3, c.paused, 40)
				}
				replies := make(chan []byte, 2)
				n.transportSays(IncomingMessage{HostName: "a", From: "a", Missed: true})
				n.transportSays(IncomingMessage{HostName: "a", From: "a", Resend: true, Reply: replies})
				n.transportSays(IncomingMessage{HostName: "a", From: "a", Resend: true, Reply: replies})
				if got := n.mpvWasTold(); len(got) != 0 {
					t.Fatalf("told mpv %q, want nothing", got)
				}
				for range 2 {
					b := <-replies
					if c.want == "" {
						if b != nil {
							t.Fatalf("handed back %s, want nothing to send", b)
						}
						continue
					}
					e := Event{}
					json.Unmarshal(b, &e)
					if e.Name != pause || !e.Again || e.Data != c.want || e.Stamp != 3 || *e.Paused != c.paused || *e.At != 40 {
						t.Fatalf("handed back %+v, want its state again", e)
					}
				}
				if got := n.broadcasts(); len(got) != 0 {
					t.Fatalf("broadcast %q, want it sent to a alone", said(got))
				}
			})
		})
	}
}

// What was sent to this node looked lost to its sender, which sends it again: the same
// resume, reaching a node that already plays the one it took from that sender. So the
// first one got here late, and this node plays behind: the party stops. One that plays a
// resume taken from someone else got it on time.
func TestAResumeSentAgainThatThisNodeTookFromItsSenderStopsTheParty(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		by   string
		want string
	}{"taken from a": {"a", "[show-text paused: out of sync with a 5000 set_property pause true]"}, "taken from c": {"c", "[]"}} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.takesFrom(c.by, 2, false, 30)
				n.peerSays("a", `{"stamp":2,"at":40,"again":true,"name":"pause","data":"no"}`)
				if got := n.mpvWasTold(); fmt.Sprint(got) != c.want {
					t.Fatalf("told mpv %q, want %s", got, c.want)
				}
			})
		})
	}
}

// Two members can share a name. Every rule about one member keys on From, so what one
// missed, or the state taken from one, says nothing of the other; only what is shown
// uses the name.
func TestMembersWithOneNameAreToldApart(t *testing.T) {
	t.Parallel()
	laptop := func(id, event string) IncomingMessage {
		m := peer(id, event)
		m.HostName = "laptop"
		return m
	}
	t.Run("missed", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(3, false, 40)
			n.transportSays(IncomingMessage{HostName: "laptop", From: "l1", Missed: true})
			n.transportSays(laptop("l2", `{"stamp":3,"at":40,"name":"pause","data":"no"}`))
			if got := n.mpvWasTold(); len(got) != 0 {
				t.Fatalf("told mpv %q for l2, which missed nothing", got)
			}
			n.transportSays(laptop("l1", `{"stamp":3,"at":40,"name":"pause","data":"no"}`))
			if got := n.mpvWasTold(); len(got) == 0 || got[0] != "show-text paused: out of sync with laptop 5000" {
				t.Fatalf("told mpv %q, want the party stopped for l1", got)
			}
		})
	})
	t.Run("synced by", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(3, true, 40)
			n.transportSays(laptop("l1", `{"stamp":4,"at":40,"name":"pause","data":"yes"}`))
			n.transportSays(laptop("l2", `{"stamp":4,"at":40,"name":"time-pos","data":"40.000000"}`))
			n.transportSays(laptop("l1", `{"stamp":4,"at":40,"name":"time-pos","data":"40.000000"}`))
			if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text synced by laptop 5000]" {
				t.Fatalf("told mpv %q, want one synced by, for l1, whose state it took", got)
			}
		})
	})
}

// A member that missed something stays missed until it sends something newer than this
// node holds, which it heard since or made, or joins again. Not when a send reaches it: a
// tie it sent before can still be on its way. Something older says nothing of it. Missed
// shows in what a tie from it does to a node that plays: it stops the party.
func TestAMemberThatMissedSomethingIsForgottenOnceItSendsNews(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		via    IncomingMessage
		missed bool
	}{
		"newer":    {peer("a", `{"stamp":4,"at":30,"name":"pause","data":"yes"}`), false},
		"rejoined": {peer("a", `{"name":"joined"}`), false},
		"a tie":    {peer("a", `{"stamp":3,"at":40,"name":"pause","data":"yes"}`), true},
		"older":    {peer("a", `{"stamp":2,"at":40,"name":"pause","data":"yes"}`), true},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.takes(3, true, 40)
				n.transportSays(IncomingMessage{HostName: "a", From: "a", Missed: true})
				n.transportSays(c.via)
				n.mpvObeys()
				n.broadcasts()
				n.mpvWasTold()
				n.mpvSays(propertyChangeLine(pause, "no", 0))
				resumed := n.broadcasts()[0]
				n.peerSays("a", fmt.Sprintf(`{"stamp":%d,"at":%f,"name":"pause","data":"no"}`, resumed.Stamp, *resumed.At))
				stopped := slices.Contains(n.mpvWasTold(), "show-text paused: out of sync with a 5000")
				if stopped != c.missed {
					t.Fatalf("a tie from a stopped the party: %v, want %v (a still missed)", stopped, c.missed)
				}
			})
		})
	}
}

// mpv refuses a set while it quits or between files. commit marked every step before mpv
// was told, so whatever mpv never did is let go of: a pause marker would swallow the
// user's next pause, loading would hold every later seek, a seek marker would say this
// node is on a second it never reached. It holds the party's stamp where mpv isn't, so it
// asks the party where it is; not once mpv is gone.
func TestARefusedStepLeavesNoMarker(t *testing.T) {
	t.Parallel()
	t.Run("the pause before a move", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos})
			n.plays()
			n.mpvSays(mpvAt(5))
			n.mpvRefuses(setProperty, "property unavailable")
			n.peerSays("a", `{"stamp":3,"paused":true,"at":30,"entry":2,"name":"time-pos","data":"30.000000"}`)
			if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
				t.Fatalf("broadcast %q, want it to ask the party where it is", got)
			}
			n.mpvTakesAgain()
			// no pause marker: the person's own pause goes out; no move on its way: nor does a seek
			n.mpvSays(propertyChangeLine(pause, "yes", 0), mpvAt(12))
			if got := n.broadcasts(); len(got) != 2 || got[0].Name != pause || got[1].Name != timePos || got[1].Entry == nil || *got[1].Entry != 0 {
				t.Fatalf("broadcast %+v, want the person's pause and seek, on entry 0", got)
			}
		})
	})
	t.Run("a seek", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos})
			n.mpvSays(mpvAt(5))
			n.broadcasts()
			n.mpvRefuses(setProperty, "property unavailable")
			n.peerSays("a", `{"stamp":3,"paused":true,"at":30,"entry":0,"name":"time-pos","data":"30.000000"}`)
			if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
				t.Fatalf("broadcast %q, want it to ask the party where it is", got)
			}
			// not on its way to 30: asked where it is, it is still at 5, and the person's seek goes out
			n.peerSays("j", `{"name":"joined"}`)
			if got := n.broadcasts(); len(got) != 1 || got[0].At == nil || *got[0].At != 5 {
				t.Fatalf("answered %+v, want 5", got)
			}
			n.mpvSays(mpvAt(12))
			if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=12.000000]" {
				t.Fatalf("broadcast %q, want the person's seek", got)
			}
		})
	})
	t.Run("anything, with mpv gone", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos})
			n.plays()
			n.mpvQuitsOn(setProperty)
			n.peerSays("a", `{"stamp":3,"paused":true,"at":30,"entry":2,"name":"time-pos","data":"30.000000"}`)
			if got := n.broadcasts(); len(got) != 0 {
				t.Fatalf("broadcast %q, want nothing: it is leaving", said(got))
			}
		})
	})
}

// A member's change is applied to its end, each step as mpv takes it, before the next
// message is weighed: weighed in between, it would meet a node half way there.
func TestAMessageWaitsForTheChangeBeforeIt(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvAnswersLate()
		n.peerSays("a", `{"stamp":5,"at":30,"name":"pause","data":"yes"}`)
		n.peerSays("b", `{"stamp":6,"at":40,"name":"pause","data":"yes"}`)
		n.mpvObeys()
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true show-text paused by a 5000 set_property time-pos 30.000000 set_property time-pos 40.000000]" {
			t.Fatalf("told mpv %q, want a's change whole, then b's", got)
		}
	})
}

// mpv hanging up while a member's change waits on it asks nobody anything, then or later:
// the node is leaving, and a request it never got an answer to is not a refusal.
func TestMpvGoneMidChangeAsksNobodyEvenLater(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvQuitsOn(setProperty)
		n.peerSays("a", `{"stamp":3,"at":30,"name":"pause","data":"yes"}`)
		time.Sleep(10 * time.Second)
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q, want nothing: it is leaving", said(got))
		}
	})
}

// A message that comes while a joiner applies what it heard waits for all of it: the seek
// to where it continues, the party's state, the join, and the notices heard while loading.
func TestAMessageDuringTheJoinWaitsForTheNotices(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{at: 40, joiner: true, late: true, heard: []IncomingMessage{peer("a", `{"name":"show-text","data":"heard"}`)}})
		n.peerSays("a", `{"name":"show-text","data":"later"}`)
		n.mpvObeys()
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property time-pos 40.000000 show-text heard 5000 show-text later 5000]" {
			t.Fatalf("told mpv %q, want the notice heard while loading first", got)
		}
	})
}

// Two players reach the end of a file together, and each stops the party for it. One
// adopts the other's pause, then sends its own reason with the next stamp; the other's
// position, sent at the same moment, arrives after it. That message is older, but it
// says the state this node holds: its sender missed nothing, so the party isn't stopped.
func TestAnOlderMessageThatAgreesIsNotCaughtUp(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 2})
		n.takes(11, true, 0)
		n.peerSays("a", `{"stamp":10,"at":0,"entry":2,"name":"time-pos","data":"0.000000"}`)
		n.peerSays("a", `{"stamp":10,"at":0,"entry":2,"name":"pause","data":"yes"}`)
		if got := n.mpvWasTold(); len(got) != 0 {
			t.Fatalf("told mpv %q, want nothing for an older message that says what this node holds", got)
		}
	})
}

// mpv folds a seek into the one right behind it: a peer sends this node to 70, and before
// mpv gets there the person seeks back to 3, where mpv already was. mpv says it began a
// seek and played back, and reports no second, as it never changed (MPV.md); or reports 3,
// which a marker of the person's earlier seek there would swallow (SIM_SEED=60097). Asked
// where it is, mpv says 3: the person's seek, which a follower keeps on its screen.
func TestAFoldedSeekBackIsThePersonsOwn(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		playing bool     // the person's first seek to 3 is made while playing, which marks it
		reports []string // what mpv says once the two seeks fold
	}{
		"nothing reported":     {false, []string{`{"event":"seek"}`, `{"event":"playback-restart"}`}},
		"reported on a marker": {true, []string{`{"event":"seek"}`, mpvAt(3), `{"event":"playback-restart"}`}},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				if c.playing {
					n.plays()
					n.mpvSays(`{"event":"seek"}`, mpvAt(3), `{"event":"playback-restart"}`)
					n.mpvObeys()
				} else {
					n.mpvSays(mpvAt(3))
				}
				got := n.broadcasts()
				if len(got) == 0 {
					t.Fatal("broadcast nothing for the person's seek to 3")
				}
				stamp := got[0].Stamp + 1
				n.mpvWasTold()
				n.peerSays("a", stamped(stamp, seekEvent(70)))
				if told := n.mpvWasTold(); len(told) == 0 || told[0] != "set_property time-pos 70.000000" {
					t.Fatalf("told mpv %q, want the peer's seek", told)
				}
				n.mpvIsAt(3)
				n.mpvSays(c.reports...)
				// the person's seek back to 3, which a follower keeps
				n.keepsTheSeek("a")
			})
		})
	}
	// the person seeks to 5 while mpv answers: theirs is kept, and what mpv answered was
	// before it
	t.Run("moved meanwhile", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.peerSays("a", stamped(5, seekEvent(70)))
			n.mpvIsAt(3)
			n.mpvHoldsWhere()
			n.mpvSays(`{"event":"seek"}`, `{"event":"playback-restart"}`, `{"event":"seek"}`, mpvAt(5))
			n.mpvSaysWhere()
			n.keepsTheSeek("a")
		})
	})
	// a node that plays has moved on from where the seek landed, and a resume taken from a
	// peer ends the seek it was landing: it isn't asked
	t.Run("playing", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.mpvSays(mpvAt(30))
			n.broadcasts()
			n.peerSays("a", `{"stamp":5,"at":70,"name":"pause","data":"no"}`)
			n.mpvWasTold()
			n.mpvIsAt(70.4)
			n.mpvSays(`{"event":"seek"}`, `{"event":"playback-restart"}`)
			if got := n.mpvWasTold(); slices.Contains(got, "get_property_string time-pos") {
				t.Fatalf("told mpv %q, want it not asked while playing", got)
			}
		})
	})
	// mpv began a seek, its own or the person's before this node's, and played back: asked,
	// it is where this node sent it, so nothing goes out
	t.Run("landed where sent", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.peerSays("a", stamped(5, seekEvent(70)))
			n.mpvIsAt(70)
			n.mpvSays(`{"event":"seek"}`, `{"event":"playback-restart"}`)
			if got := n.broadcasts(); len(got) != 0 {
				t.Fatalf("broadcast %q, want nothing", said(got))
			}
			if got := n.mpvWasTold(); !slices.Contains(got, "get_property_string time-pos") {
				t.Fatalf("told mpv %q, want it asked where it is", got)
			}
			// mpv's report of 70 after it is still the echo
			n.mpvSays(mpvAt(70))
			if got := n.broadcasts(); len(got) != 0 {
				t.Fatalf("broadcast %q, want the echo swallowed", said(got))
			}
		})
	})
}

// A peer's stop at 60 reaches this node while it plays on 60, so it only pauses; but mpv
// plays on before the pause lands, and stops at 60.3. Once mpv reports the stop, every
// position before it has been read: off the stop's second, the node seeks there, as its
// own step, and says nothing (SIM_SEED=514736544449). Within a frame, it stays.
func TestAStopThatLandsOffItsSecondIsSeekedBack(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		played []string
		seeks  []string
	}{
		"0.3s on":    {[]string{mpvAt(60.1), mpvAt(60.2), mpvAt(60.3)}, []string{"set_property time-pos 60.000000"}},
		"a frame on": {[]string{mpvAt(60.04)}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.mpvSays(mpvAt(60))
				n.plays()
				n.mpvAnswersLate()
				n.peerSays("a", `{"stamp":9,"at":60,"name":"pause","data":"yes"}`)
				if told := n.mpvWasTold(); len(told) == 0 || told[0] != "set_property pause true" {
					t.Fatalf("told mpv %q, want the stop", told)
				}
				n.mpvSays(c.played...)
				n.mpvObeys()
				if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != fmt.Sprint(c.seeks) {
					t.Fatalf("seeks %q, want %q", got, c.seeks)
				}
				if got := n.broadcasts(); len(got) != 0 {
					t.Fatalf("broadcast %q, want nothing", said(got))
				}
			})
		})
	}
}

// The person seeks to 60 while mpv still holds the play of a peer's resume, so mpv plays
// after the person's seek, which went out newer: the party holds it, by its stamp, stopped
// at 60, so the play is undone on this node alone, quietly (SIM_SEED=467222401814).
func TestAPlayMpvRunsAfterThePersonsChangeIsUndone(t *testing.T) {
	t.Parallel()
	for name, reportFirst := range map[string]bool{"a play reported before its answer": true, "a play reported after its answer": false} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.takes(2, true, 30)
				n.mpvAnswersLate()
				n.peerSays("a", `{"stamp":3,"at":30,"name":"pause","data":"no"}`)
				if told := n.mpvWasTold(); fmt.Sprint(told) != "[set_property pause false]" {
					t.Fatalf("told mpv %q, want the play", told)
				}
				n.mpvSays(`{"event":"seek"}`, mpvAt(60), `{"event":"playback-restart"}`)
				if got := n.broadcasts(); fmt.Sprint(said(got)) != "[time-pos=60.000000]" || got[0].Stamp != 4 || !*got[0].Paused {
					t.Fatalf("broadcast %+v, want the person's seek, stopped, newer", got)
				}
				if reportFirst {
					n.mpvObeys()
				} else {
					n.mpvAnswers()
					n.mpvSays(propertyChangeLine(pause, "no", 0))
				}
				n.mpvObeys()
				if told := n.mpvWasTold(); fmt.Sprint(told) != "[set_property pause true]" {
					t.Fatalf("told mpv %q, want it stopped again, quietly", told)
				}
				if got := n.broadcasts(); len(got) != 0 {
					t.Fatalf("broadcast %q, want nothing: the party holds the person's seek", said(got))
				}
			})
		})
	}
	// a peer's newer resume is taken before mpv reports the late play: that is where this
	// node is now, playing, and nothing is undone
	t.Run("a newer state taken since", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(2, true, 30)
			n.mpvAnswersLate()
			n.peerSays("a", `{"stamp":3,"at":30,"name":"pause","data":"no"}`)
			n.mpvSays(`{"event":"seek"}`, mpvAt(60), `{"event":"playback-restart"}`)
			n.broadcasts()
			n.mpvWasTold()
			n.peerSays("b", `{"stamp":5,"at":60,"name":"pause","data":"no"}`)
			n.mpvAnswers()
			n.mpvSays(propertyChangeLine(pause, "no", 0))
			n.mpvAnswers()
			if told := n.mpvWasTold(); slices.Contains(told, "set_property pause true") {
				t.Fatalf("told mpv %q, want it left playing", told)
			}
			if got := n.broadcasts(); len(got) != 0 {
				t.Fatalf("broadcast %q, want nothing", said(got))
			}
		})
	})
}

// The person resumes while mpv still holds a peer's seek to 60, which runs after the
// resume: mpv plays from 60, so the resume says so, as this node was heading there. Said
// from 30, the others played from 30 and this node from 60.
func TestAResumeWhileASeekLandsPlaysFromItsSecond(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(2, true, 30)
		n.mpvAnswersLate()
		n.peerSays("a", stamped(3, seekEvent(60)))
		if told := n.mpvWasTold(); fmt.Sprint(told) != "[set_property time-pos 60.000000]" {
			t.Fatalf("told mpv %q, want the seek", told)
		}
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		if got := n.broadcasts(); fmt.Sprint(said(got)) != "[pause=no]" || *got[0].At != 60 {
			t.Fatalf("broadcast %+v, want the person's resume, from 60", got)
		}
		n.mpvObeys()
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q, want nothing", said(got))
		}
	})
}

// A peer's stop at 60 reaches this node while it plays on 30: mpv is told to stop first,
// and the seek to 60 is only marked. The person presses play before mpv answers the stop:
// the step that would send the seek stops (moved), so the resume plays from where mpv is.
// Said from 60, it played on from 30 while everyone else played from 60
// (SIM_SEED=736469209410).
func TestAResumeBeforeASeekIsSentPlaysFromWhereMpvIs(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(2, false, 30)
		n.mpvAnswersLate()
		n.peerSays("a", `{"stamp":3,"at":60,"name":"pause","data":"yes"}`)
		if told := n.mpvWasTold(); fmt.Sprint(told) != "[set_property pause true]" {
			t.Fatalf("told mpv %q, want only the stop", told)
		}
		// mpv stops, the person resumes, then mpv answers the stop
		n.mpvObeys(propertyChangeLine(pause, "no", 0))
		if got := n.broadcasts(); fmt.Sprint(said(got)) != "[pause=no]" || *got[0].At != 30 {
			t.Fatalf("broadcast %+v, want the person's resume, from 30", got)
		}
		if got := seeksIn(n.mpvWasTold()); len(got) != 0 {
			t.Fatalf("seeks %q, want none", got)
		}
	})
}

// A peer sent this node to 38, then to 7, and the person seeked back to 38: mpv folds the
// two, reports 38, which reads as the first seek's echo, and plays back. The person presses
// play before mpv says where it is: mpv has run the seek to 7 already, so the resume plays
// from where mpv is (SIM_SEED=388032164346). Said from 7, it played on from 38.
func TestAResumeAfterAFoldedSeekPlaysFromWhereMpvIs(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(2, true, 30)
		n.peerSays("a", stamped(3, seekEvent(38)))
		n.mpvObeys()
		n.mpvWasTold()
		n.mpvAnswersLate()
		n.peerSays("a", stamped(4, seekEvent(7)))
		if told := n.mpvWasTold(); fmt.Sprint(told) != "[set_property time-pos 7.000000]" {
			t.Fatalf("told mpv %q, want the seek", told)
		}
		n.mpvHoldsWhere()
		n.mpvSays(`{"event":"seek"}`, mpvAt(38), `{"event":"playback-restart"}`, propertyChangeLine(pause, "no", 0))
		if got := n.broadcasts(); fmt.Sprint(said(got)) != "[pause=no]" || *got[0].At != 38 {
			t.Fatalf("broadcast %+v, want the person's resume, from 38", got)
		}
	})
}

// The person presses play while a peer's seek to 60 lands, then seeks to 42 while it plays,
// and mpv folds the two: it reports 42 and plays on. Asked where it is, mpv says 42.1. That
// is the person's seek while playing, so it pauses the party, and the pause says where mpv
// stopped, not the second read while it played on (SIM_SEED=831497494691).
func TestAFoldedSeekWhilePlayingPausesTheParty(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(2, true, 30)
		n.mpvAnswersLate()
		n.peerSays("a", stamped(3, seekEvent(60)))
		n.mpvWasTold()
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		n.broadcasts()
		n.mpvIsAt(42.1)
		n.mpvSays(`{"event":"seek"}`, mpvAt(42), `{"event":"playback-restart"}`, mpvAt(42.1), mpvAt(42.2))
		if told := n.mpvWasTold(); !slices.Contains(told, "set_property pause true") {
			t.Fatalf("told mpv %q, want the party paused", told)
		}
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		n.mpvAnswers()
		got := n.broadcasts()
		if fmt.Sprint(said(got)) != "[pause=yes time-pos=42.200000 pause-reason=seek]" || *got[0].At != 42.2 {
			t.Fatalf("broadcast %+v, want the party paused at 42.2 for a seek", got)
		}
	})
}

// A peer's stop at 9.5 reaches this node while it plays on 9.45: mpv answers the stop, then
// reports one more playing position, 9.5, and only then the stop (mpv 0.40, e2e [R6c] under
// --jitter 14). The node took the stop at the answer, so that position read as the person's
// seek while stopped, and the party stopped for it, catching up. It is mpv playing on. A stop
// nobody chose (a leave's), as a follower of a person's stop sends no seek anyway.
func TestAPositionBeforeAStopIsReportedIsNoSeek(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(mpvAt(9))
		n.plays()
		n.mpvSays(mpvAt(9.45))
		n.peerSays("a", `{"stamp":9,"at":9.5,"free":true,"name":"pause","data":"yes"}`)
		if told := n.mpvWasTold(); len(told) == 0 || told[0] != "set_property pause true" {
			t.Fatalf("told mpv %q, want the stop", told)
		}
		n.mpvSays(mpvAt(9.5))
		n.mpvObeys()
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q, want nothing", said(got))
		}
		// stopped, a position is the person's seek again
		n.mpvSays(mpvAt(20))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=20.000000]" {
			t.Fatalf("broadcast %q, want the person's seek", got)
		}
	})
}
