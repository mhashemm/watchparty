package mpv

import (
	"bufio"
	"cmp"
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
		conn:     &connection{scanner: sc, pending: map[int64]chan Response{}, closed: make(chan struct{})},
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
	conn := &connection{conn: nc, pending: map[int64]chan Response{}, closed: make(chan struct{})}
	if err := conn.do("observe_property_string", 1, pause); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v, want ErrClosed", err)
	}
}

// Someone can add to the playlist from inside mpv, so the entry it reports may be one we never named.
func TestAnEntryWeDidNotNameDoesNotPanic(t *testing.T) {
	t.Parallel()
	c := newTestClient(make(chan []byte, 1), nil)
	c.names = []string{"e1"}
	if got := c.entryName(5); got != "another video" {
		t.Fatalf("got %q", got)
	}
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
		// but this node's own seek is not an echo and must reach everyone else
		n.mpvSays(mpvAt(42))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=42.000000]" {
			t.Fatalf("broadcast %q, want the genuine seek", got)
		}
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
		n.mpvSays(mpvAt(30))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=30.000000]" {
			t.Fatalf("broadcast %q, want the genuine seek back to an old position", got)
		}
	})
}

// --hr-seek=yes is pinned, so an echo is at most a frame off. A deliberate seek
// a fraction of a second away is still the user's own.
func TestSmallSeekIsNotAnEcho(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("a", stamped(5, seekEvent(12)))
		n.mpvSays(mpvAt(12.3))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=12.300000]" {
			t.Fatalf("broadcast %q, want the 0.3s seek", got)
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
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=55.000000]" {
			t.Fatalf("broadcast %q, want only the seek made once stopped: not the echo, and not the seek lost as playback", got)
		}
		// the echo used up the marker, so pausing again later is a real event
		n.plays()
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes]" {
			t.Fatalf("broadcast %q, want the genuine pause", got)
		}
	})
}

// Two do() calls in flight must each get their own reply, even when mpv answers
// them out of order. A shared response channel let one steal the other's reply
// and leave it to time out.
func TestConcurrentRequestsGetTheirOwnResponse(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// liveClient runs watch() against a fake mpv on the other end of a pipe. The fake
// answers every request with success and reports each command on requests; send
// writes one of mpv's events.
func liveClient(t *testing.T) (c *Client, send func(string), outgoing chan []byte, requests chan string) {
	t.Helper()
	ours, theirs := net.Pipe()
	t.Cleanup(func() { ours.Close(); theirs.Close() })
	outgoing = make(chan []byte, 16)
	requests = make(chan string, 16)
	c = newTestClient(outgoing, bufio.NewScanner(ours))
	c.conn.conn = ours
	c.seen[pause], c.seen[timePos] = true, true
	writes := make(chan string, 64)
	go func() {
		for line := range writes {
			fmt.Fprint(theirs, line)
		}
	}()
	go func() {
		sc := bufio.NewScanner(theirs)
		for sc.Scan() {
			req := Request{}
			json.Unmarshal(sc.Bytes(), &req)
			requests <- strings.TrimSpace(fmt.Sprintln(req.Command...))
			writes <- fmt.Sprintf(`{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
		}
	}()
	go c.watch()
	return c, func(line string) { writes <- line }, outgoing, requests
}

func next[T any](t *testing.T, ch chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(time.Second):
		t.Fatal("nothing arrived")
		panic("unreachable")
	}
}

func nextEvent(t *testing.T, outgoing chan []byte) Event {
	t.Helper()
	e := Event{}
	json.Unmarshal(next(t, outgoing), &e)
	return e
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
	t.Parallel()
	c, _, outgoing, requests := liveClient(t)
	c.paused = true
	incoming := make(chan IncomingMessage)
	close(c.ready)
	go c.ProcessIncomingEvents(incoming)

	if e := nextEvent(t, outgoing); e.Name != joined {
		t.Fatalf("broadcast %+v, want joined", e)
	}
	incoming <- peer("a", `{"stamp":1,"event":"property-change","name":"time-pos","data":"40.000000"}`)
	if got := next(t, requests); got != "set_property time-pos 40.000000" {
		t.Fatalf("sent %q, want the party's position", got)
	}
	next(t, requests)

	// every member answers, and the rest say the same
	incoming <- peer("b", `{"stamp":1,"event":"property-change","name":"time-pos","data":"40.000000"}`)
	incoming <- peer("b", `{"name":"show-text","data":"mark"}`)
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q, want nothing for the same answer again", got)
	}
	select {
	case e := <-outgoing:
		t.Fatalf("broadcast %s after joined", e)
	default:
	}
}

// What the party says while this node loads is applied once it has, before it says joined:
// the newest, which says the entry and where on it.
func TestWhatIsHeardWhileLoadingIsAppliedBeforeJoining(t *testing.T) {
	t.Parallel()
	c, _, outgoing, requests := playlistClient(t)
	c.paused, c.pos = true, 30
	incoming := make(chan IncomingMessage)
	go c.ProcessIncomingEvents(incoming)
	incoming <- peer("a", move(1, 2))
	incoming <- peer("a", stamped(2, tagged(2, `{"event":"property-change","name":"time-pos","data":"50.000000"}`)))
	close(c.ready)

	for _, want := range []string{"set_property playlist-pos 2", "show-text from e1 to e3, by a 5000", "show-text synced by a 5000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("sent %q, want %q: the party's entry and second", got, want)
		}
	}
	if e := nextEvent(t, outgoing); e.Name != joined {
		t.Fatalf("broadcast %+v, want joined", e)
	}
	c.mu.Lock()
	held := c.heldSeek
	c.mu.Unlock()
	if held != "50.000000" {
		t.Fatalf("holds %q, want the seek waiting for the file", held)
	}
}

// A node already in the party stops for a joiner, and its own pause is its answer: it
// says where, with a stamp newer than anything it could say before mpv had stopped.
func TestJoinPausesThePartyAndSaysWhere(t *testing.T) {
	t.Parallel()
	c, send, outgoing, requests := liveClient(t)
	send(propertyChangeLine(timePos, "33.500000", 0))

	go c.apply(peer("j", `{"name":"joined"}`))
	// why first: the GUI's feed takes the pause that follows for this one
	for _, want := range []string{"show-text paused for j: joining 5000", "set_property pause true"} {
		if got := next(t, requests); got != want {
			t.Fatalf("sent %q, want %q", got, want)
		}
	}
	send(propertyChangeLine(pause, "yes", 0))
	if e := nextEvent(t, outgoing); e.Name != pause || e.Data != "yes" || e.At == nil || *e.At != 33.5 || e.Stamp != 1 {
		t.Fatalf("broadcast %+v, want its own pause, where it stopped", e)
	}
	select {
	case e := <-outgoing:
		t.Fatalf("also answered with %s, which could carry the stamp from before the pause", e)
	default:
	}
}

// A node already paused, often by another node's pause for the same joiner, still
// says why the party is waiting.
func TestJoinWhilePausedStillShowsWhy(t *testing.T) {
	t.Parallel()
	c, _, outgoing, requests := liveClient(t)
	c.paused = true

	go c.apply(peer("j", `{"name":"joined"}`))
	if got := next(t, requests); got != "show-text paused for j: joining 5000" {
		t.Fatalf("sent %q, want the notice and no pause", got)
	}
	if e := nextEvent(t, outgoing); e.Name != timePos {
		t.Fatalf("broadcast %s, want the position", e.Name)
	}
}

// Everyone gets the notice at their own moment, so everyone pauses themselves, and any
// one of those pauses is enough for a node that missed the notice.
func TestMemberLeavingPausesTheParty(t *testing.T) {
	t.Parallel()
	c, send, outgoing, requests := liveClient(t)
	send(propertyChangeLine(timePos, "33.500000", 0))

	go c.apply(peer("x", `{"name":"show-text","data":"x has left"}`))
	if got := next(t, requests); got != "show-text x has left 5000" {
		t.Fatalf("sent %q, want the notice", got)
	}
	if got := next(t, requests); got != "show-text paused for x: left 5000" {
		t.Fatalf("sent %q, want the reason", got)
	}
	if got := next(t, requests); got != "set_property pause true" {
		t.Fatalf("sent %q, want the party paused", got)
	}
	send(propertyChangeLine(pause, "yes", 0))
	if e := nextEvent(t, outgoing); e.Name != pause || e.Data != "yes" || e.At == nil || *e.At != 33.5 {
		t.Fatalf("broadcast %+v, want the pause, where it stopped", e)
	}
}

func TestMemberLeavingChangesNothingWhileAlreadyPaused(t *testing.T) {
	t.Parallel()
	c, _, outgoing, requests := liveClient(t)
	c.paused = true

	c.apply(peer("x", `{"name":"show-text","data":"x has left"}`))
	c.apply(peer("x", `{"name":"show-text","data":"mark"}`))
	if got := next(t, requests); got != "show-text x has left 5000" {
		t.Fatalf("sent %q, want the notice", got)
	}
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q, want no pause", got)
	}
	select {
	case e := <-outgoing:
		t.Fatalf("broadcast %s", e)
	default:
	}
}

// The video ending is how a party normally finishes. Each player reaches the end at
// its own moment, and whoever gets there first leaves; a pause would hold the rest a
// moment before the end, with nothing telling them why.
func TestMemberLeavingNearTheEndDoesNotPause(t *testing.T) {
	t.Parallel()
	c, send, _, requests := liveClient(t)
	send(propertyChangeLine(duration, "100.000000", 0))
	send(propertyChangeLine(timePos, "96.000000", 0))
	for !func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.pos == 96 }() {
		time.Sleep(time.Millisecond)
	}

	go func() {
		c.apply(peer("x", `{"name":"show-text","data":"x has left"}`))
		c.apply(peer("x", `{"name":"show-text","data":"mark"}`))
	}()
	next(t, requests)
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q, want no pause", got)
	}
}

// Only the notice about its own sender counts, which is the shape both transports send.
func TestOnlyALeaveNoticePauses(t *testing.T) {
	t.Parallel()
	c, _, _, requests := liveClient(t)

	go func() {
		c.apply(peer("x", `{"name":"show-text","data":"y has left"}`))
		c.apply(peer("x", `{"name":"show-text","data":"x has joined"}`))
		c.apply(peer("x", `{"name":"show-text","data":"mark"}`))
	}()
	next(t, requests)
	next(t, requests)
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q, want no pause", got)
	}
}

// Everyone stops a moment apart for a leave or a join, each with the same stamp, so they
// settle on the earliest stop, or they would resume that far apart and stay so. A joiner
// takes the first it hears, then the earliest too.
func TestStopsAMomentApartSettleOnTheEarliest(t *testing.T) {
	t.Parallel()
	stop := func(at float64) string {
		return fmt.Sprintf(`{"stamp":2,"at":%f,"name":"pause","data":"yes"}`, at)
	}
	c, _, _, requests := liveClient(t)
	c.paused, c.pos, c.stamp = true, 40, 2
	go func() {
		c.apply(peer("a", stop(41)))
		c.apply(peer("b", stop(39)))
	}()
	if got := next(t, requests); got != "set_property time-pos 39.000000" {
		t.Fatalf("a member sent %q, want only the earlier stop followed", got)
	}

	c, _, _, requests = liveClient(t)
	c.paused, c.stamp = true, unheard
	go func() {
		c.apply(peer("a", stop(40)))
		c.apply(peer("b", stop(39)))
	}()
	for _, want := range []string{"set_property time-pos 40.000000", "set_property time-pos 39.000000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("a joiner sent %q, want %q", got, want)
		}
	}
}

// Two people start at once on different entries, and each answers the other. Neither
// knows anything yet, so their stamps tie and the higher entry wins, as it does for two
// moves: each following the other would swap them, for good. A party's answer has a
// stamp, and that is followed, lower or not.
func TestTwoJoinersSettleOnTheHigherEntry(t *testing.T) {
	t.Parallel()
	answer := func(i int) string { return move(unheard, i) }
	mark := `{"name":"show-text","data":"mark"}`

	c, _, outgoing, requests := playlistClient(t)
	c.paused, c.stamp, c.current = true, unheard, 2
	go func() {
		c.apply(peer("a", `{"name":"joined"}`))
		c.apply(peer("a", answer(0)))
		c.apply(peer("a", mark))
	}()
	nextEvent(t, outgoing)
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("the higher one sent %q, want to stay", got)
	}

	c, _, outgoing, requests = playlistClient(t)
	c.paused, c.stamp, c.current = true, unheard, 0
	go func() {
		c.apply(peer("a", `{"name":"joined"}`))
		c.apply(peer("a", answer(2)))
	}()
	nextEvent(t, outgoing)
	if got := next(t, requests); got != "set_property playlist-pos 2" {
		t.Fatalf("the lower one sent %q, want to follow", got)
	}

	c, _, _, requests = playlistClient(t)
	c.paused, c.stamp, c.current = true, unheard, 2
	go c.apply(peer("a", move(1, 0)))
	if got := next(t, requests); got != "set_property playlist-pos 0" {
		t.Fatalf("sent %q, want to follow the party", got)
	}
}

// Members answer a joiner with where each stopped, a moment apart and with one stamp. The
// joiner takes the first, then an earlier one, and not a later one after that.
func TestAJoinerFollowsAnEarlierAnswer(t *testing.T) {
	t.Parallel()
	answer := func(at float64) string {
		return fmt.Sprintf(`{"stamp":1,"name":"time-pos","data":"%f"}`, at)
	}
	c, _, _, requests := liveClient(t)
	c.paused, c.stamp = true, unheard
	go func() {
		c.apply(peer("a", answer(40)))
		c.apply(peer("b", answer(39)))
		c.apply(peer("c", answer(40)))
		c.apply(peer("c", `{"name":"show-text","data":"mark"}`))
	}()
	var seeks []string
	for {
		got := next(t, requests)
		if got == "show-text mark 5000" {
			break
		}
		if strings.HasPrefix(got, "set_property time-pos") {
			seeks = append(seeks, got)
		}
	}
	if want := []string{"set_property time-pos 40.000000", "set_property time-pos 39.000000"}; !slices.Equal(seeks, want) {
		t.Fatalf("sent %q, want %q", seeks, want)
	}
}

// A joiner already where the party is takes its stamp and seeks nowhere.
func TestAnAnswerWhereItAlreadyIsSeeksNowhere(t *testing.T) {
	t.Parallel()
	c, _, _, requests := liveClient(t)
	c.paused, c.stamp, c.pos = true, unheard, 40

	c.apply(peer("a", `{"stamp":1,"event":"property-change","name":"time-pos","data":"40.050000"}`))
	c.apply(peer("b", `{"stamp":1,"event":"property-change","name":"time-pos","data":"55.000000"}`))
	c.apply(peer("b", `{"name":"show-text","data":"mark"}`))
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q, want neither answer to seek: it is there, and the later one loses", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stamp != 1 {
		t.Fatalf("its stamp is %d, want the party's", c.stamp)
	}
}

// A pause says where the pauser stopped. Over a slow link the others stop a round trip
// later; without this they resume that far apart and stay so.
func TestOwnPauseSaysWhereItStopped(t *testing.T) {
	t.Parallel()
	_, send, outgoing, _ := liveClient(t)
	send(propertyChangeLine(timePos, "61.250000", 0))
	send(propertyChangeLine(pause, "yes", 0))
	if e := nextEvent(t, outgoing); e.Name != pause || e.Data != "yes" || e.At == nil || *e.At != 61.25 {
		t.Fatalf("broadcast %+v, want the pause, where it stopped", e)
	}
}

// A node that loses the relay plays on, as the party most likely does, and says so. Back
// in the room it says, quietly, the state it holds; the party stops only for a state that
// isn't its own (take). A joiner that was never answered has none, and joins again.
func TestLosingTheRelayPlaysOnAndComingBackSaysWhereItIs(t *testing.T) {
	t.Parallel()
	c, _, outgoing, requests := liveClient(t)
	c.pos, c.stamp = 10, 4

	c.apply(IncomingMessage{Lost: true})
	c.apply(peer("a", `{"name":"show-text","data":"mark"}`))
	for _, want := range []string{"show-text lost the relay, reconnecting 5000", "show-text mark 5000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("sent %q, want %q", got, want)
		}
	}

	c.apply(IncomingMessage{Back: true})
	e := nextEvent(t, outgoing)
	if !e.Check || e.Name != pause || e.Stamp != 4 || e.Paused == nil || *e.Paused || e.At == nil || *e.At != 10 {
		t.Fatalf("broadcast %+v, want its state, playing from 10 at stamp 4, as a check", e)
	}

	c.mu.Lock()
	c.stamp = unheard
	c.mu.Unlock()
	c.apply(IncomingMessage{Back: true})
	if e := nextEvent(t, outgoing); e.Name != joined {
		t.Fatalf("broadcast %+v, want a joiner to join again", e)
	}
}

// The last seconds are played out, as when a member leaves: a state that stops the party
// would hold everyone else just short of the end.
func TestComingBackNearTheEndSaysNothing(t *testing.T) {
	t.Parallel()
	c, _, outgoing, requests := liveClient(t)
	c.pos, c.duration, c.stamp = 117, 120, 4
	c.apply(IncomingMessage{Lost: true})
	c.apply(IncomingMessage{Back: true})
	c.apply(peer("a", `{"name":"show-text","data":"mark"}`))
	for got := next(t, requests); got != "show-text mark 5000"; got = next(t, requests) {
	}
	if len(outgoing) != 0 {
		t.Fatalf("broadcast %s, want nothing this close to the end", <-outgoing)
	}

	// stopped by hand there, it says where like anywhere else
	c.mu.Lock()
	c.paused = true
	c.mu.Unlock()
	c.apply(IncomingMessage{Back: true})
	if e := nextEvent(t, outgoing); !e.Check {
		t.Fatalf("broadcast %+v, want its state", e)
	}
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
		{"the party's own, playing", false, check(4, false, 10), nil},
		{"the party's own, stopped", true, check(4, true, 30), nil},
		{"older, while the party moved on", false, check(3, false, 10), []string{"show-text paused for c: catching up 5000", "set_property pause true"}},
		{"a newer pause", false, check(5, true, 25), []string{"set_property pause true", "show-text paused by c 5000", "set_property time-pos 25.000000"}},
		{"a newer resume, the party stopped", true, check(5, false, 30), []string{"show-text paused: out of sync with c 5000"}},
		{"a newer resume, the party playing", false, check(5, false, 30), []string{"show-text paused: out of sync with c 5000", "set_property pause true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, _, _, requests := liveClient(t)
			c.paused, c.pos, c.stamp = tc.paused, 30, 4
			c.apply(peer("c", tc.event))
			c.apply(peer("a", `{"name":"show-text","data":"mark"}`))
			var got []string
			for r := next(t, requests); r != "show-text mark 5000"; r = next(t, requests) {
				got = append(got, r)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("sent %q, want %q", got, tc.want)
			}
		})
	}
}

func playlistClient(t *testing.T) (*Client, func(string), chan []byte, chan string) {
	t.Helper()
	c, send, outgoing, requests := liveClient(t)
	c.names = []string{"e1", "e2", "e3"}
	c.seen[playlistPos] = true
	return c, send, outgoing, requests
}

// Moving on while playing, at the end of an entry or by hand, stops the party on the
// next one. Peers hear which entry first, so the pause and position land on it.
func TestMovingToAnEntryWhilePlayingPausesTheParty(t *testing.T) {
	t.Parallel()
	_, send, outgoing, requests := playlistClient(t)
	send(propertyChangeLine(timePos, "900.000000", 0))

	send(propertyChangeLine(playlistPos, "1", 0))
	if e := nextEvent(t, outgoing); e.Name != playlistPos || e.Data != "1" {
		t.Fatalf("broadcast %+v, want the entry", e)
	}
	if got := next(t, requests); got != "set_property pause true" {
		t.Fatalf("sent %q, want the party paused", got)
	}
	send(propertyChangeLine(pause, "yes", 0))
	if e := nextEvent(t, outgoing); e.Name != pause {
		t.Fatalf("broadcast %+v, want the pause", e)
	}
	if e := nextEvent(t, outgoing); e.Name != timePos || e.Data != "0.000000" {
		t.Fatalf("broadcast %+v, want the new entry's start, not the old position", e)
	}
	if e := nextEvent(t, outgoing); e.Name != pauseReason || e.Data != "video changed" {
		t.Fatalf("broadcast %+v, want the reason", e)
	}
}

// A new file reports time-pos 0 while paused. Sent on, it would seek a peer still
// playing the end of the last entry.
func TestMovingWhilePausedSendsOnlyTheEntry(t *testing.T) {
	t.Parallel()
	c, send, outgoing, _ := playlistClient(t)
	c.paused = true

	send(propertyChangeLine(playlistPos, "2", 0))
	if e := nextEvent(t, outgoing); e.Name != playlistPos || e.Data != "2" {
		t.Fatalf("broadcast %+v, want the entry", e)
	}
	send(propertyChangeLine(timePos, "0.000000", 0))
	send(propertyChangeLine(pause, "no", 0))
	if e := nextEvent(t, outgoing); e.Name != pause {
		t.Fatalf("broadcast %+v, want nothing before the resume", e)
	}
}

// mpv reports where it starts as soon as it is observed, and -1 once the list runs out.
// Neither is someone moving.
func TestPlaylistPositionThatIsNotAMoveStaysLocal(t *testing.T) {
	t.Parallel()
	c, send, outgoing, _ := playlistClient(t)
	c.paused = true
	delete(c.seen, playlistPos)

	send(propertyChangeLine(playlistPos, "2", 0))
	send(propertyChangeLine(playlistPos, "-1", 0))
	send(propertyChangeLine(playlistPos, "0", 0))
	if e := nextEvent(t, outgoing); e.Data != "0" {
		t.Fatalf("broadcast %+v, want only the move to the first", e)
	}
}

// mpv can report the entry it starts on, from --playlist-start, before the file is
// loaded. That isn't a move, and the file's first time-pos must still count as loaded.
func TestStartingEntryBeforeTheFileLoadsIsNotAMove(t *testing.T) {
	t.Parallel()
	c, send, outgoing, _ := playlistClient(t)
	c.paused = true
	delete(c.seen, timePos)

	send(propertyChangeLine(playlistPos, "1", 0))
	send(propertyChangeLine(timePos, "0.000000", 0))
	select {
	case <-c.ready:
	case <-time.After(time.Second):
		t.Fatal("the party never learned the file loaded")
	}
	send(propertyChangeLine(pause, "no", 0))
	if e := nextEvent(t, outgoing); e.Name != pause {
		t.Fatalf("broadcast %+v, want nothing before the resume", e)
	}
	if c.current != 1 {
		t.Fatalf("on entry %d, want 1", c.current)
	}
}

// A peer's move opens the next file stopped, and mpv's report of it isn't sent back.
func TestFollowingAnEntryStopsFirstAndStaysQuiet(t *testing.T) {
	t.Parallel()
	c, send, outgoing, requests := playlistClient(t)

	go c.apply(peer("a", move(1, 2)))
	for _, want := range []string{"set_property pause true", "show-text paused by a 5000", "set_property playlist-pos 2", "show-text from e1 to e3, by a 5000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("sent %q, want %q", got, want)
		}
	}
	send(propertyChangeLine(pause, "yes", 0))
	send(propertyChangeLine(playlistPos, "2", 0))
	send(propertyChangeLine(timePos, "0.000000", 0))
	send(propertyChangeLine(pause, "no", 0))
	if e := nextEvent(t, outgoing); e.Name != pause || e.Data != "no" {
		t.Fatalf("broadcast %+v, want nothing before the resume", e)
	}
}

// Every member answers a joiner, and two nodes can reach the end together.
func TestEntryAlreadyHeadedThereIsNotRepeated(t *testing.T) {
	t.Parallel()
	c, _, _, requests := playlistClient(t)
	c.paused, c.current = true, 1

	go func() {
		c.apply(peer("a", `{"event":"property-change","name":"playlist-pos","data":"1"}`))
		c.apply(peer("a", `{"event":"property-change","name":"playlist-pos","data":"2"}`))
		c.apply(peer("b", `{"event":"property-change","name":"playlist-pos","data":"2"}`))
		c.apply(peer("b", `{"event":"property-change","name":"playlist-pos","data":"3"}`))
		c.apply(peer("b", `{"name":"show-text","data":"mark"}`))
	}()
	for _, want := range []string{"set_property playlist-pos 2", "show-text from e2 to e3, by a 5000", "show-text mark 5000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("sent %q, want %q", got, want)
		}
	}
}

// A seek right after a move would land on the old file, which then ends and loses it.
// A joiner hits this every time: it is told the entry, then the position.
func TestSeekAfterFollowingAnEntryWaitsForTheFile(t *testing.T) {
	t.Parallel()
	c, send, _, requests := playlistClient(t)
	c.paused = true

	go func() {
		c.apply(peer("a", move(1, 1)))
		c.apply(peer("a", stamped(2, tagged(1, `{"event":"property-change","name":"time-pos","data":"40.000000"}`))))
	}()
	for _, want := range []string{"set_property playlist-pos 1", "show-text from e1 to e2, by a 5000", "show-text synced by a 5000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("sent %q, want %q", got, want)
		}
	}
	send(`{"event":"start-file","playlist_entry_id":2}` + "\n")
	send(`{"event":"property-change","name":"playlist-pos","data":"1"}` + "\n")
	send(`{"event":"file-loaded"}` + "\n")
	if got := next(t, requests); got != "set_property time-pos 40.000000" {
		t.Fatalf("sent %q, want the held seek", got)
	}
}

// A node that hasn't heard the party answers too, with what it holds: its stamp is below
// any the party has, so only a fellow joiner can take it.
func TestJoiningNodeAnswersWithAnUnheardStamp(t *testing.T) {
	t.Parallel()
	c, _, outgoing, _ := playlistClient(t)
	c.paused, c.stamp, c.current, c.pos = true, unheard, 2, 7

	go c.apply(peer("j", `{"name":"joined"}`))
	if e := nextEvent(t, outgoing); e.Name != timePos || e.Stamp != unheard || e.Entry == nil || *e.Entry != 2 || *e.At != 7 {
		t.Fatalf("broadcast %+v, want its state, unheard", e)
	}
}

// An answer is one message: the entry, and where on it.
func TestJoinAnswerSaysTheEntryAndWhereOnIt(t *testing.T) {
	t.Parallel()
	c, _, outgoing, _ := playlistClient(t)
	c.paused, c.current, c.pos = true, 2, 33

	go c.apply(peer("j", `{"name":"joined"}`))
	if e := nextEvent(t, outgoing); e.Name != timePos || e.Data != "33.000000" || e.Entry == nil || *e.Entry != 2 {
		t.Fatalf("broadcast %+v, want the entry and the position", e)
	}
}

// Everyone hears the answer to a joiner, and it can arrive after someone has moved on. It
// carries the stamp its sender had, so one sent just before two people crossed is older
// than both moves and sends nobody to the other's entry. One from someone who knows of a
// later move is followed by anyone, joiner or not.
func TestAnAnswerIsFollowedOnlyIfItIsNewer(t *testing.T) {
	t.Parallel()
	mark := `{"name":"show-text","data":"mark"}`

	c, _, _, requests := playlistClient(t)
	c.paused, c.current, c.stamp = true, 1, 3
	go func() {
		c.apply(peer("a", move(2, 2)))
		c.apply(peer("a", mark))
	}()
	for _, want := range []string{"show-text paused for a: catching up 5000", "show-text mark 5000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("a member sent %q for an older answer, want %q", got, want)
		}
	}

	c, send, outgoing, requests := playlistClient(t)
	c.paused, c.current, c.stamp = true, 1, unheard
	send(propertyChangeLine(playlistPos, "0", 0))
	next(t, outgoing)
	go func() {
		c.apply(peer("a", move(unheard, 2)))
		c.apply(peer("a", mark))
	}()
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("a joiner who has moved by hand followed a fellow joiner: %q", got)
	}

	c, _, _, requests = playlistClient(t)
	c.paused, c.current, c.stamp = true, 1, 3
	go c.apply(peer("a", move(4, 0)))
	if got := next(t, requests); got != "set_property playlist-pos 0" {
		t.Fatalf("a member sent %q, want to follow a newer answer", got)
	}
	next(t, requests)
	c.mu.Lock()
	stamp := c.stamp
	c.mu.Unlock()
	if stamp != 4 {
		t.Fatalf("its stamp is %d, want the one it followed", stamp)
	}
}

// A newer stamp for the entry this node is already on moves nothing, and is still taken:
// its next move must beat it.
func TestANewerStampForTheSameEntryIsTaken(t *testing.T) {
	t.Parallel()
	c, send, outgoing, requests := playlistClient(t)
	c.paused, c.current, c.stamp = true, 1, 1
	c.apply(peer("a", move(5, 1)))
	c.apply(peer("a", `{"name":"show-text","data":"mark"}`))
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q, want nothing for the entry it is on", got)
	}
	send(propertyChangeLine(playlistPos, "2", 0))
	if e := nextEvent(t, outgoing); e.Stamp != 6 {
		t.Fatalf("moved with stamp %d, want one past the highest it heard", e.Stamp)
	}
}

// A joiner knows nothing, whatever it knew before its connection dropped, and whoever
// opened the party is the party from the start.
func TestAJoinForgetsTheStampAndOpeningTakesOne(t *testing.T) {
	t.Parallel()
	c, _, outgoing, _ := playlistClient(t)
	stamp := func() int {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.stamp
	}
	c.stamp = 7
	go c.announce()
	nextEvent(t, outgoing)
	joiner := stamp()
	c.apply(IncomingMessage{Opened: true})
	opener := stamp()
	c.mu.Lock()
	c.stamp = 7
	c.mu.Unlock()
	c.apply(IncomingMessage{Opened: true})
	if again := stamp(); joiner != unheard || opener != 1 || again != 7 {
		t.Fatalf("stamps %d, %d and %d, want unheard for a joiner, 1 for an opener, and 7 kept by one who reopens", joiner, opener, again)
	}
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
	c, send, outgoing, _ := playlistClient(t)
	c.paused = true

	send(propertyChangeLine(playlistPos, "2", 0))
	e := nextEvent(t, outgoing)
	if e.Name != playlistPos || e.Stamp != 1 || e.Entry == nil || *e.Entry != 2 {
		t.Fatalf("broadcast %+v, want a move to 2 with a new stamp", e)
	}
	send(propertyChangeLine(timePos, "0.000000", 0))
	send(propertyChangeLine(timePos, "9.000000", 0))
	if e := nextEvent(t, outgoing); e.Name != timePos || e.Stamp != 2 || e.At == nil || *e.At != 9 || e.Entry == nil || *e.Entry != 2 {
		t.Fatalf("broadcast %+v, want a seek on entry 2, at 9, with the next stamp", e)
	}

	single, send, outgoing, _ := liveClient(t)
	single.paused = true
	send(propertyChangeLine(timePos, "9.000000", 0))
	if msg := string(next(t, outgoing)); strings.Contains(msg, "entry") || !strings.Contains(msg, `"stamp":1`) {
		t.Fatalf("a single file sent %s, want a stamp and no entry", msg)
	}
}

// A seek or a resume made at the same moment as the move that brought this node here
// carries the move's stamp and a lower entry: it belongs to a file the party has left. A
// pause still stops us: stopping is always safe.
func TestMessagesFromAnEntryThePartyLeftAreDropped(t *testing.T) {
	t.Parallel()
	c, _, _, requests := playlistClient(t)
	c.paused, c.current, c.stamp = true, 1, 2

	go func() {
		c.apply(peer("a", stamped(2, tagged(0, `{"event":"property-change","name":"time-pos","data":"40.000000"}`))))
		c.apply(peer("a", stamped(2, tagged(0, `{"name":"pause","data":"no"}`))))
		c.apply(peer("a", stamped(3, tagged(1, `{"event":"property-change","name":"time-pos","data":"41.000000"}`))))
	}()
	if got := next(t, requests); got != "set_property time-pos 41.000000" {
		t.Fatalf("sent %q, want only the seek on our entry", got)
	}
}

// A newer pause or seek from another entry means the party is there: this node missed the
// move. It goes there first, then to the second it was told, once the file has loaded.
func TestANewerSeekFromAnotherEntryBringsThisNodeThere(t *testing.T) {
	t.Parallel()
	c, send, _, requests := playlistClient(t)
	c.paused, c.stamp = true, 1

	go c.apply(peer("a", `{"stamp":3,"entry":2,"event":"property-change","name":"time-pos","data":"40.000000"}`))
	for _, want := range []string{"set_property playlist-pos 2", "show-text from e1 to e3, by a 5000", "show-text synced by a 5000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("sent %q, want %q", got, want)
		}
	}
	send(`{"event":"start-file","playlist_entry_id":3}` + "\n")
	send(`{"event":"property-change","name":"playlist-pos","data":"2"}` + "\n")
	send(`{"event":"file-loaded"}` + "\n")
	if got := next(t, requests); got != "set_property time-pos 40.000000" {
		t.Fatalf("sent %q, want the second it was told, on the new file", got)
	}
}

// It loses to the higher entry, and this node, playing, answers by stopping where it is.
func TestAPauseFromAnotherEntryStillStops(t *testing.T) {
	t.Parallel()
	c, _, _, requests := playlistClient(t)
	c.current, c.pos = 1, 5

	go func() {
		c.apply(peer("a", `{"at":30,"entry":0,"name":"pause","data":"yes"}`))
		c.apply(peer("a", `{"name":"show-text","data":"mark"}`))
	}()
	for _, want := range []string{"set_property pause true", "show-text mark 5000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("sent %q, want %q: the pause, and no seek", got, want)
		}
	}
}

// While a move is on its way, the entry it is heading to is the one that counts.
func TestMessagesAreCheckedAgainstWhereWeAreHeading(t *testing.T) {
	t.Parallel()
	c, send, _, requests := playlistClient(t)
	c.paused = true

	go func() {
		c.apply(peer("a", move(1, 1)))
		c.apply(peer("a", stamped(1, tagged(0, `{"event":"property-change","name":"time-pos","data":"40.000000"}`))))
		c.apply(peer("a", stamped(2, tagged(1, `{"event":"property-change","name":"time-pos","data":"41.000000"}`))))
	}()
	for _, want := range []string{"set_property playlist-pos 1", "show-text from e1 to e2, by a 5000", "show-text synced by a 5000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("sent %q, want %q", got, want)
		}
	}
	send(`{"event":"start-file","playlist_entry_id":2}` + "\n")
	send(`{"event":"property-change","name":"playlist-pos","data":"1"}` + "\n")
	send(`{"event":"file-loaded"}` + "\n")
	if got := next(t, requests); got != "set_property time-pos 41.000000" {
		t.Fatalf("sent %q, want the seek on the new entry", got)
	}
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
	const left = 1
	for name, c := range map[string]struct{ mine, theirs int }{
		"we went higher": {2, 0},
		"we went lower":  {0, 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client, send, outgoing, requests := playlistClient(t)
			client.paused, client.current = true, left

			send(propertyChangeLine(playlistPos, fmt.Sprint(c.mine), 0))
			next(t, outgoing)
			go func() {
				client.apply(peer("a", move(1, c.theirs)))
				client.apply(peer("a", `{"name":"show-text","data":"mark"}`))
			}()
			got := next(t, requests)
			if c.mine > c.theirs && got != "show-text mark 5000" {
				t.Fatalf("sent %q, want to keep the higher one", got)
			}
			if c.mine < c.theirs && got != fmt.Sprintf("set_property playlist-pos %d", c.theirs) {
				t.Fatalf("sent %q, want to follow the higher one", got)
			}
		})
	}
}

// A third member hears both moves, in either order, and must land in the same place.
func TestAThirdMemberSettlesOnTheHigherInEitherOrder(t *testing.T) {
	t.Parallel()
	for name, moves := range map[string][2]int{"higher first": {2, 0}, "lower first": {0, 2}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, _, _, requests := playlistClient(t)
			c.paused, c.current = true, 1

			go func() {
				c.apply(peer("a", move(1, moves[0])))
				c.apply(peer("b", move(1, moves[1])))
				c.apply(peer("b", `{"name":"show-text","data":"mark"}`))
			}()
			last := ""
			for got := next(t, requests); got != "show-text mark 5000"; got = next(t, requests) {
				if strings.HasPrefix(got, "set_property playlist-pos") {
					last = got
				}
			}
			if last != "set_property playlist-pos 2" {
				t.Fatalf("settled with %q, want the higher one", last)
			}
		})
	}
}

// Someone behind everyone else hears a tie too, and what it compares is the stamps, not
// where it happens to be.
func TestATieIsSeenFromAnyEntry(t *testing.T) {
	t.Parallel()
	c, _, _, requests := playlistClient(t)
	c.paused = true

	go func() {
		c.apply(peer("a", move(1, 2)))
		c.apply(peer("b", move(1, 1)))
		c.apply(peer("b", `{"name":"show-text","data":"mark"}`))
	}()
	for _, want := range []string{"set_property playlist-pos 2", "show-text from e1 to e3, by a 5000", "show-text mark 5000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("sent %q, want %q", got, want)
		}
	}
}

// The higher entry only settles a tie. A newer move is followed, lower or not, so someone
// who missed a message catches up on the next one instead of staying apart.
func TestALowerMoveThatIsNotATieIsFollowed(t *testing.T) {
	t.Parallel()
	c, _, _, requests := playlistClient(t)
	c.paused, c.current, c.stamp = true, 2, 1

	go c.apply(peer("a", move(2, 0)))
	if got := next(t, requests); got != "set_property playlist-pos 0" {
		t.Fatalf("sent %q, want it followed", got)
	}
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
	started := `{"event":"start-file","playlist_entry_id":3}` + "\n"
	loaded := `{"event":"file-loaded"}` + "\n"
	reported := propertyChangeLine(playlistPos, "2", 0)
	for _, tc := range []struct{ name, before, pausing, after, seeking string }{
		{"reported, then loaded", "", started + reported + loaded, "", ""},
		{"loaded, then reported", "", started + loaded + reported, "", ""},
		{"loaded before the move came", started + loaded, reported, "", ""},
		{"loaded with a seek held, then reported", "", "", started + loaded, reported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ours, theirs := net.Pipe()
			t.Cleanup(func() { ours.Close(); theirs.Close() })
			outgoing := make(chan []byte, 16)
			c := newTestClient(outgoing, bufio.NewScanner(ours))
			c.conn.conn = ours
			c.names = []string{"e1", "e2", "e3"}
			c.current = 1
			c.seen[pause], c.seen[timePos], c.seen[playlistPos] = true, true, true
			sent := make(chan string, 16)
			go func() {
				sc := bufio.NewScanner(theirs)
				for sc.Scan() {
					req := Request{}
					json.Unmarshal(sc.Bytes(), &req)
					command := strings.TrimSpace(fmt.Sprintln(req.Command...))
					sent <- command
					switch command {
					case "show-text before 5000":
						fmt.Fprint(theirs, tc.before)
					case "set_property pause true":
						fmt.Fprint(theirs, tc.pausing)
					case "set_property time-pos 5.000000":
						// the person resumes once the seek has landed: what goes out first
						fmt.Fprint(theirs, tc.seeking, `{"event":"seek"}`+"\n", propertyChangeLine(timePos, "5.000000", 0),
							`{"event":"playback-restart"}`+"\n", propertyChangeLine(pause, "no", 0))
					}
					fmt.Fprintf(theirs, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
				}
			}()
			go c.watch()
			// what mpv sends before replying is handled before the reply
			c.showText("before")

			c.apply(peer("a", move(1, 2)))
			c.apply(peer("a", stamped(2, tagged(2, `{"event":"property-change","name":"time-pos","data":"5.000000"}`))))
			fmt.Fprint(theirs, tc.after)
			// setting the entry mpv reached by itself is a no-op (MPV.md), and loading, set
			// before mpv was told, was cleared by that file's file-loaded
			for got := next(t, sent); got != "set_property time-pos 5.000000"; got = next(t, sent) {
			}
			if got := next(t, outgoing); !strings.Contains(string(got), `"name":"pause","data":"no"`) {
				t.Fatalf("went out before the person's resume: %s", got)
			}
		})
	}
}

// mpv's first report of where it started can land after a peer already sent this node to
// another video. That report isn't the echo of the move, so it mustn't use up the marker,
// or the real echo goes out as this node moving the party.
func TestAnEarlierPositionDoesNotUseUpTheMarker(t *testing.T) {
	t.Parallel()
	c, send, outgoing, requests := playlistClient(t)
	c.paused = true
	delete(c.seen, playlistPos)

	go c.apply(peer("a", move(1, 1)))
	if got := next(t, requests); got != "set_property playlist-pos 1" {
		t.Fatalf("sent %q", got)
	}
	send(propertyChangeLine(playlistPos, "0", 0))
	send(propertyChangeLine(playlistPos, "1", 0))
	send(propertyChangeLine(pause, "no", 0))
	if e := nextEvent(t, outgoing); e.Name != pause {
		t.Fatalf("broadcast %+v, want nothing before the resume", e)
	}
}

// Where a node stopped is where a continued party starts, except right at an entry's
// end (mpv's keep-open=always stays there): then it is the next entry, from its start.
func TestPositionToContinueFrom(t *testing.T) {
	t.Parallel()
	three := []string{"e1", "e2", "e3"}
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
		{three, 1, 50, 100, 1, 50},
		{three, 1, 97, 100, 2, 0},
		{three, 2, 97, 100, 2, 0},
	} {
		entry, at := (&Client{names: c.names, current: c.current, pos: c.pos, duration: c.dur}).Position()
		if entry != c.entry || at != c.at {
			t.Errorf("%d entries, on %d at %v of %v: entry %d at %v, want %d at %v", len(c.names), c.current, c.pos, c.dur, entry, at, c.entry, c.at)
		}
	}
}

// Only the end of the last entry, or of the only file, finishes the party: any other
// entry has a next one to continue from.
func TestFinishedOnlyAtTheEndOfTheLastEntry(t *testing.T) {
	t.Parallel()
	three := []string{"e1", "e2", "e3"}
	for _, c := range []struct {
		names    []string
		current  int
		pos      float64
		finished bool
	}{{nil, 0, 97, true}, {nil, 0, 50, false}, {three, 2, 97, true}, {three, 1, 97, false}, {three, 2, 50, false}} {
		if got := (&Client{names: c.names, current: c.current, pos: c.pos, duration: 100}).Finished(); got != c.finished {
			t.Errorf("%d entries, on %d at %v: %v, want %v", len(c.names), c.current, c.pos, got, c.finished)
		}
	}
}

// A node that opens on a later entry can hear the party name that entry before mpv has
// reported where it is. Taking itself for entry 0, it set the entry it was already on,
// which mpv answers with nothing, and waited for that load forever.
func TestJoinerKnowsItsStartingEntryBeforeMpvSaysSo(t *testing.T) {
	t.Parallel()
	c, cancel := context.WithCancel(context.Background())
	defer cancel()
	ours, theirs := net.Pipe()
	t.Cleanup(func() { ours.Close(); theirs.Close() })
	sent := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(theirs)
		for sc.Scan() {
			req := Request{}
			json.Unmarshal(sc.Bytes(), &req)
			fmt.Fprintf(theirs, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
			if req.Command[0] != "observe_property_string" {
				sent <- strings.TrimSpace(fmt.Sprintln(req.Command...))
			} else if req.Command[2] == timePos {
				fmt.Fprint(theirs, `{"event":"property-change","id":3,"name":"time-pos","data":"0.000000"}`+"\n")
			}
		}
	}()
	incoming := make(chan IncomingMessage, 2)
	incoming <- peer("a", `{"event":"property-change","name":"playlist-pos","data":"1"}`)
	incoming <- IncomingMessage{HostName: "a", From: "a", Event: []byte(`{"name":"show-text","data":"mark"}`)}
	if _, err := New(c, func() {}, ours, make(chan []byte, 8), incoming, []string{"e1", "e2", "e3"}, 1, 0); err != nil {
		t.Fatal(err)
	}
	if got := next(t, sent); got != "show-text mark 5000" {
		t.Fatalf("sent %q before the mark, want nothing: it is already on that entry", got)
	}
}

// A continued party seeks the file it continues on once it is open, and only it. Then
// whatever the party sent while it loaded is applied, so the party's position wins.
// The seek is its own echo, so nobody else is moved by it.
func TestContinuingSeeksBeforeThePartyIsHeard(t *testing.T) {
	t.Parallel()
	c, cancel := context.WithCancel(context.Background())
	defer cancel()
	ours, theirs := net.Pipe()
	t.Cleanup(func() { ours.Close(); theirs.Close() })
	sent := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(theirs)
		for sc.Scan() {
			req := Request{}
			json.Unmarshal(sc.Bytes(), &req)
			fmt.Fprintf(theirs, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
			switch {
			case req.Command[0] != "observe_property_string":
				sent <- strings.TrimSpace(fmt.Sprintln(req.Command...))
				if req.Command[1] == timePos {
					fmt.Fprintf(theirs, `{"event":"property-change","id":3,"name":"time-pos","data":"%s"}`+"\n", req.Command[2])
				}
			case req.Command[2] == timePos:
				fmt.Fprint(theirs, `{"event":"property-change","id":3,"name":"time-pos","data":"0.000000"}`+"\n")
			}
		}
	}()
	incoming := make(chan IncomingMessage, 2)
	incoming <- peer("a", stamped(1, `{"event":"property-change","name":"time-pos","data":"55.000000"}`))
	incoming <- peer("a", `{"name":"show-text","data":"mark"}`)
	outgoing := make(chan []byte, 16)
	if _, err := New(c, func() {}, ours, outgoing, incoming, nil, 0, 40); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"set_property time-pos 40.000000", "set_property time-pos 55.000000", "show-text synced by a 5000", "show-text mark 5000"} {
		if got := next(t, sent); got != want {
			t.Fatalf("sent %q, want %q", got, want)
		}
	}
	for len(outgoing) > 0 {
		if e := <-outgoing; strings.Contains(string(e), "time-pos") {
			t.Fatalf("broadcast %s: continuing moved the party", e)
		}
	}
}

// A pause says where its sender stopped, and a seek that it was stopped: in a mesh a send
// can fail, and either one alone must leave the node that got it stopped on that second.
func TestAPauseSaysWhereAndASeekSaysStopped(t *testing.T) {
	t.Parallel()
	c, _, _, requests := liveClient(t)
	c.pos = 30
	go c.apply(peer("a", `{"stamp":1,"at":40,"name":"pause","data":"yes"}`))
	for _, want := range []string{"set_property pause true", "show-text paused by a 5000", "set_property time-pos 40.000000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("a pause alone sent %q, want %q", got, want)
		}
	}

	c, _, _, requests = liveClient(t)
	c.pos = 30
	go c.apply(peer("a", `{"stamp":1,"name":"time-pos","data":"40.000000"}`))
	for _, want := range []string{"set_property pause true", "show-text paused by a 5000", "set_property time-pos 40.000000", "show-text synced by a 5000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("a seek alone sent %q, want %q", got, want)
		}
	}
}

// A resume says where from, so a node that missed the seek before it starts there too.
func TestAResumeSaysWhereFrom(t *testing.T) {
	t.Parallel()
	c, _, _, requests := liveClient(t)
	c.paused, c.pos = true, 30
	go c.apply(peer("a", `{"stamp":1,"at":40,"name":"pause","data":"no"}`))
	for _, want := range []string{"set_property time-pos 40.000000", "set_property pause false"} {
		if got := next(t, requests); got != want {
			t.Fatalf("sent %q, want %q", got, want)
		}
	}
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
			t.Parallel()
			c, _, outgoing, requests := liveClient(t)
			c.paused, c.pos, c.stamp = true, 30, 3
			go c.apply(peer("b", old))
			if got := next(t, requests); got != "show-text paused for b: catching up 5000" {
				t.Fatalf("sent %q, want only why", got)
			}
			if e := nextEvent(t, outgoing); e.Name != timePos || e.Data != "30.000000" || e.Stamp != 3 {
				t.Fatalf("answered %+v, want where the party is, with its stamp", e)
			}
		})
	}

	c, send, outgoing, requests := liveClient(t)
	c.pos, c.stamp = 30, 3
	go c.apply(peer("b", `{"stamp":2,"at":10,"name":"pause","data":"yes"}`))
	// why first, so the GUI's feed doesn't take the pause for the person's own
	for _, want := range []string{"show-text paused for b: catching up 5000", "set_property pause true"} {
		if got := next(t, requests); got != want {
			t.Fatalf("a playing party sent %q, want %q", got, want)
		}
	}
	send(propertyChangeLine(pause, "yes", 0))
	// its own pause is the answer: newer, and where it stopped
	if e := nextEvent(t, outgoing); e.Name != pause || e.Stamp != 4 || e.At == nil || *e.At != 30 {
		t.Fatalf("broadcast %+v, want its own pause, newer, where it stopped", e)
	}
}

// Two changes with the same stamp were made at the same moment, and everyone must pick
// the same one: stopped over playing, then the earlier second. The one that loses is
// told, quietly, since its sender may never have heard the winner.
func TestChangesMadeAtOnceGoToTheSaferOne(t *testing.T) {
	t.Parallel()
	mark := `{"name":"show-text","data":"mark"}`
	for name, c := range map[string]struct{ event, does string }{
		"a resume loses to a seek":    {`{"stamp":1,"at":30,"name":"pause","data":"no"}`, ""},
		"a later seek loses":          {`{"stamp":1,"name":"time-pos","data":"50.000000"}`, ""},
		"a later stop loses":          {`{"stamp":1,"at":50,"name":"pause","data":"yes"}`, ""},
		"an earlier seek wins":        {`{"stamp":1,"name":"time-pos","data":"20.000000"}`, "set_property time-pos 20.000000"},
		"an earlier stop wins":        {`{"stamp":1,"at":20,"name":"pause","data":"yes"}`, "set_property time-pos 20.000000"},
		"the same second is the same": {`{"stamp":1,"name":"time-pos","data":"40.000000"}`, "show-text mark 5000"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client, _, outgoing, requests := liveClient(t)
			client.paused, client.pos, client.stamp = true, 40, 1
			go func() {
				client.apply(peer("b", c.event))
				client.apply(peer("b", mark))
			}()
			want := cmp.Or(c.does, "show-text mark 5000")
			if got := next(t, requests); got != want {
				t.Fatalf("sent %q, want %q", got, want)
			}
			if c.does != "" {
				return
			}
			if e := nextEvent(t, outgoing); e.Name != timePos || e.Data != "40.000000" || e.Stamp != 1 {
				t.Fatalf("answered %+v, want where this node is", e)
			}
		})
	}

	c, _, _, requests := liveClient(t)
	c.pos, c.stamp = 40, 1
	go c.apply(peer("b", `{"stamp":1,"name":"time-pos","data":"50.000000"}`))
	for _, want := range []string{"set_property pause true", "show-text paused by b 5000", "set_property time-pos 50.000000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("a node playing sent %q, want %q: a seek made as it resumed stops it", got, want)
		}
	}
}

// A joiner takes the party's stamp with its position. Left unheard, the first thing it did
// would be older than what the party holds, and nobody would follow it.
func TestAJoinerTakesThePartysStampWithItsPosition(t *testing.T) {
	t.Parallel()
	c, send, outgoing, requests := liveClient(t)
	c.paused, c.stamp = true, unheard
	go c.apply(peer("a", `{"stamp":5,"event":"property-change","name":"time-pos","data":"40.000000"}`))
	if got := next(t, requests); got != "set_property time-pos 40.000000" {
		t.Fatalf("sent %q, want the party's position", got)
	}
	next(t, requests)
	send(propertyChangeLine(pause, "no", 0))
	if e := nextEvent(t, outgoing); e.Name != pause || e.Stamp != 6 {
		t.Fatalf("resumed with %+v, want a stamp newer than the party's", e)
	}
}

// Where this node is, for a tie, is where it is heading: not any second it has been sent
// to since the last resume, which is kept only to know mpv's echo of that seek.
func TestATieIsWeighedAgainstWhereThisNodeIsHeading(t *testing.T) {
	t.Parallel()
	c, _, _, requests := liveClient(t)
	c.paused = true
	go func() {
		c.apply(peer("a", `{"stamp":1,"name":"time-pos","data":"10.000000"}`))
		c.apply(peer("a", `{"stamp":2,"name":"time-pos","data":"20.000000"}`))
		c.apply(peer("b", `{"stamp":2,"at":10,"name":"pause","data":"yes"}`))
	}()
	for _, want := range []string{"set_property time-pos 10.000000", "show-text synced by a 5000", "set_property time-pos 20.000000", "show-text synced by a 5000", "set_property time-pos 10.000000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("sent %q, want %q: the earlier of two stops made at once, though this node was there before", got, want)
		}
	}
}

// An entry a peer answers with says where on it, so a node that takes the party's stamp
// from it takes the party's second too. A node already on that entry does as well.
func TestAnEntryAnswerSaysWhereOnIt(t *testing.T) {
	t.Parallel()
	c, _, _, requests := playlistClient(t)
	c.paused, c.current, c.pos, c.stamp = true, 1, 10, 1
	go c.apply(peer("a", `{"stamp":4,"at":30,"event":"property-change","name":"playlist-pos","data":"1"}`))
	if got := next(t, requests); got != "set_property time-pos 30.000000" {
		t.Fatalf("sent %q, want the party's second on the entry it is already on", got)
	}
}

// A node that answers while a move it followed is still loading names the entry it is
// heading to, and the start of that file, or the seek waiting for it: not the second it is
// on in the file it is leaving.
func TestAnAnswerWhileAMoveLoadsIsAboutTheNewEntry(t *testing.T) {
	t.Parallel()
	c, _, outgoing, requests := playlistClient(t)
	c.paused, c.pos, c.stamp = true, 30, 1
	c.apply(peer("a", move(2, 1)))
	next(t, requests)
	next(t, requests)
	go c.apply(peer("j", `{"name":"joined"}`))
	if e := nextEvent(t, outgoing); e.Name != timePos || e.Data != "0.000000" || e.Entry == nil || *e.Entry != 1 || e.Stamp != 2 {
		t.Fatalf("answered %+v, want entry 1 at its start, with the move's stamp", e)
	}
}

// mpv says playback-restart when a file is ready, and on a slow stream that can come after
// this node was already sent somewhere. It doesn't mean the seek has landed: until mpv
// reports that second, this node is still heading to it. Taken for where mpv last was, the
// start, a joiner lost a tie to the next member's stop and told the party it was at 0, and
// the party went there.
func TestASeekIsStillOnItsWayAfterAStrayPlaybackRestart(t *testing.T) {
	t.Parallel()
	c, _, outgoing, requests := liveClient(t)
	c.paused, c.stamp = true, unheard
	go func() {
		c.apply(peer("a", `{"stamp":8,"at":53.1,"name":"pause","data":"yes"}`))
		c.handleEvent(Event{EventType: playbackRestart}, nil)
		c.apply(peer("b", `{"stamp":8,"at":53.3,"name":"pause","data":"yes"}`))
		c.apply(peer("b", `{"name":"show-text","data":"mark"}`))
	}()
	if got := next(t, requests); got != "set_property time-pos 53.100000" {
		t.Fatalf("sent %q, want the party's second", got)
	}
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q, want nothing more", got)
	}
	if e := nextEvent(t, outgoing); e.Name != timePos || e.Data != "53.100000" {
		t.Fatalf("answered %+v, want the earlier of the two stops, where it is heading", e)
	}
}

// What is kept while loading is the newest by its stamp, not the last to arrive: members'
// messages come in any order, and an older one applied after a newer one would be answered
// as if its sender had fallen behind, by a node that hasn't even joined.
func TestTheNewestHeardWhileLoadingIsByStampNotArrival(t *testing.T) {
	t.Parallel()
	c, _, outgoing, requests := liveClient(t)
	c.paused, c.stamp = true, unheard
	incoming := make(chan IncomingMessage)
	go c.ProcessIncomingEvents(incoming)
	incoming <- peer("a", `{"stamp":5,"at":30,"name":"pause","data":"yes"}`)
	incoming <- peer("b", `{"stamp":3,"event":"property-change","name":"time-pos","data":"10.000000"}`)
	close(c.ready)
	if got := next(t, requests); got != "set_property time-pos 30.000000" {
		t.Fatalf("sent %q, want only the newer of the two", got)
	}
	if e := nextEvent(t, outgoing); e.Name != joined {
		t.Fatalf("broadcast %+v, want joined and nothing before it", e)
	}
}

// What a joiner does before it has heard the party is not a member falling behind: its
// join is being answered, so nobody stops again or says so.
func TestAJoinersOwnChangeIsNotCaughtUp(t *testing.T) {
	t.Parallel()
	c, _, outgoing, requests := liveClient(t)
	c.paused, c.pos, c.stamp = true, 30, 3
	c.apply(peer("j", fmt.Sprintf(`{"stamp":%d,"event":"property-change","name":"time-pos","data":"10.000000"}`, unheard+1)))
	c.apply(peer("j", fmt.Sprintf(`{"stamp":%d,"at":10,"name":"pause","data":"no"}`, unheard+2)))
	c.apply(peer("j", `{"name":"show-text","data":"mark"}`))
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q, want nothing before the mark", got)
	}
	select {
	case e := <-outgoing:
		t.Fatalf("answered %s", e)
	default:
	}
}

// Its user's own change while a move it followed loads is about the entry it is heading
// to. Tagged with the file mpv is still on, it carried the party's newest stamp on the
// entry the party left, and dragged everyone back there.
func TestOwnChangeWhileAMoveLoadsIsAboutTheNewEntry(t *testing.T) {
	t.Parallel()
	c, send, outgoing, requests := playlistClient(t)
	c.paused, c.pos, c.stamp = true, 30, 1
	c.apply(peer("a", move(2, 1)))
	next(t, requests)
	next(t, requests)
	send(propertyChangeLine(pause, "no", 0))
	if e := nextEvent(t, outgoing); e.Name != pause || e.Entry == nil || *e.Entry != 1 || e.At == nil || *e.At != 0 || e.Stamp != 3 {
		t.Fatalf("resumed with %+v, want entry 1 from its start, newer than the move", e)
	}
}

// A newer resume from another entry means this node missed the move. It goes there
// stopped, as the file has to load, and stops the party instead of playing that far behind.
func TestANewerResumeFromAnotherEntryStopsTheParty(t *testing.T) {
	t.Parallel()
	c, _, outgoing, requests := playlistClient(t)
	c.pos, c.stamp = 5, 1
	go c.apply(peer("a", `{"stamp":2,"at":10,"entry":2,"name":"pause","data":"no"}`))
	for _, want := range []string{"set_property pause true", "show-text paused by a 5000", "set_property playlist-pos 2", "show-text from e1 to e3, by a 5000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("sent %q, want %q", got, want)
		}
	}
	if e := nextEvent(t, outgoing); e.Name != timePos || e.Data != "10.000000" || e.Stamp != 2 || *e.Entry != 2 {
		t.Fatalf("answered %+v, want where it holds the party is, stopped", e)
	}
}

// Every node sends its whole state, so a message missing part of it is refused rather than
// filled in from this node's own: each of these is newer, and would move it.
func TestAMessageWithoutItsStateIsRefused(t *testing.T) {
	t.Parallel()
	c, _, _, requests := playlistClient(t)
	c.paused, c.pos, c.stamp = true, 40, 1
	go func() {
		// raw, not through peer, which fills in what a node always sends
		for _, e := range []string{
			`{"stamp":2,"at":10,"entry":0,"name":"time-pos","data":"10.000000"}`,
			`{"stamp":2,"paused":true,"entry":0,"name":"pause","data":"yes"}`,
			`{"stamp":2,"paused":true,"at":10,"name":"time-pos","data":"10.000000"}`,
			`{"stamp":2,"paused":true,"at":0,"entry":3,"name":"playlist-pos","data":"3"}`,
			`{"stamp":2,"paused":true,"at":1e999,"entry":0,"name":"time-pos","data":"10.000000"}`,
		} {
			c.apply(IncomingMessage{HostName: "a", From: "a", Event: []byte(e)})
		}
		c.apply(peer("a", `{"name":"show-text","data":"mark"}`))
	}()
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q, want nothing for a message without its state", got)
	}
}

// A move while playing stops the party, so it says stopped before mpv has: whoever takes it
// opens the file stopped instead of answering a node it thinks plays on.
func TestAMoveWhilePlayingSaysStopped(t *testing.T) {
	t.Parallel()
	c, send, outgoing, _ := playlistClient(t)
	send(propertyChangeLine(playlistPos, "2", 0))
	if e := nextEvent(t, outgoing); e.Name != playlistPos || e.Paused == nil || !*e.Paused {
		t.Fatalf("broadcast %+v, want the move, stopped", e)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reason != "video changed" {
		t.Fatalf("stopping for %q, want the party stopped for the move", c.reason)
	}
}

// A joiner that pressed play before it was answered is not out of sync with the party: it
// isn't in it yet, and stopping the party with its own second would drag the party there.
func TestAJoinerPlayingIsNotOutOfSync(t *testing.T) {
	t.Parallel()
	c, _, _, requests := liveClient(t)
	c.pos, c.stamp, c.missed["a"] = 40, unheard, true
	go func() {
		c.apply(peer("a", `{"stamp":2,"at":30,"name":"pause","data":"no"}`))
		c.apply(peer("a", `{"name":"show-text","data":"mark"}`))
	}()
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q, want nothing until its join is answered", got)
	}
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
		"newer":                     {`{"stamp":2,"at":30,"name":"pause","data":"no"}`, false},
		"newer, from where it is":   {`{"stamp":2,"at":40,"name":"pause","data":"no"}`, false},
		"a tie, from one it missed": {`{"stamp":1,"at":30,"name":"pause","data":"no"}`, true},
		"a tie, from ahead":         {`{"stamp":1,"at":50,"name":"pause","data":"no"}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n, send, outgoing, requests := liveClient(t)
			n.pos, n.stamp = 40, 1
			go func() {
				if c.missed {
					n.apply(IncomingMessage{HostName: "a", From: "a", Missed: true})
				}
				n.apply(peer("a", c.resume))
			}()
			for _, want := range []string{"show-text paused: out of sync with a 5000", "set_property pause true"} {
				if got := next(t, requests); got != want {
					t.Fatalf("sent %q, want %q", got, want)
				}
			}
			send(propertyChangeLine(pause, "yes", 0))
			var r Event
			json.Unmarshal([]byte(c.resume), &r)
			if e := nextEvent(t, outgoing); e.Name != pause || *e.At != 40 || e.Stamp != r.Stamp+1 {
				t.Fatalf("broadcast %+v, want its own pause where it is, newer than the resume", e)
			}
		})
	}
}

// A tie with the resume this node plays, from a member that heard it, is the same resume
// made at once: whatever second it names, that was a moment ago on another machine.
func TestATieFromAMemberThatMissedNothingMovesNothing(t *testing.T) {
	t.Parallel()
	c, _, _, requests := liveClient(t)
	c.pos, c.stamp = 40, 1
	go func() {
		c.apply(peer("a", `{"stamp":1,"at":30,"name":"pause","data":"no"}`))
		c.apply(peer("a", `{"name":"show-text","data":"mark"}`))
	}()
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q, want nothing for the same resume", got)
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
		"sent again": {`{"stamp":2,"at":30,"again":true,"name":"pause","data":"yes"}`, "show-text mark 5000"},
		"not":        {`{"stamp":2,"at":30,"name":"pause","data":"yes"}`, "show-text paused for a: catching up 5000"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n, _, _, requests := liveClient(t)
			n.paused, n.pos, n.stamp = true, 40, 3
			go func() {
				n.apply(peer("a", c.event))
				n.apply(peer("a", `{"name":"show-text","data":"mark"}`))
			}()
			if got := next(t, requests); got != c.want {
				t.Fatalf("sent %q, want %q", got, c.want)
			}
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
		stamp  int
		by     string
		want   string
	}{
		"stopped":       {true, 3, "", "yes"},
		"playing":       {false, 3, "", "no"},
		"taken from b":  {false, 3, "b", "no"},
		"joining":       {true, unheard, "", ""},
		"taken from it": {false, 3, "a", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n, _, outgoing, requests := liveClient(t)
			n.paused, n.pos, n.stamp, n.by = c.paused, 40, c.stamp, c.by
			replies := make(chan []byte, 2)
			go func() {
				n.apply(IncomingMessage{HostName: "a", From: "a", Missed: true})
				n.apply(IncomingMessage{HostName: "a", From: "a", Resend: true, Reply: replies})
				n.apply(IncomingMessage{HostName: "a", From: "a", Resend: true, Reply: replies})
				n.apply(peer("a", `{"name":"show-text","data":"mark"}`))
			}()
			if got := next(t, requests); got != "show-text mark 5000" {
				t.Fatalf("sent %q to mpv, want nothing", got)
			}
			for range 2 {
				b := next(t, replies)
				if c.want == "" {
					if b != nil {
						t.Fatalf("handed back %s, want nothing to send", b)
					}
					continue
				}
				e := Event{}
				json.Unmarshal(b, &e)
				if e.Name != pause || !e.Again || e.Data != c.want || e.Stamp != c.stamp || *e.Paused != c.paused || *e.At != 40 {
					t.Fatalf("handed back %+v, want its state again", e)
				}
			}
			if len(outgoing) > 0 {
				t.Fatalf("broadcast %s, want it sent to a alone", <-outgoing)
			}
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
		by    string
		stops bool
	}{"taken from a": {"a", true}, "taken from c": {"c", false}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n, _, _, requests := liveClient(t)
			n.pos, n.stamp, n.by = 30, 2, c.by
			go func() {
				n.apply(peer("a", `{"stamp":2,"at":40,"again":true,"name":"pause","data":"no"}`))
				n.apply(peer("a", `{"name":"show-text","data":"mark"}`))
			}()
			want := "show-text mark 5000"
			if c.stops {
				want = "show-text paused: out of sync with a 5000"
			}
			if got := next(t, requests); got != want {
				t.Fatalf("sent %q, want %q", got, want)
			}
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
		t.Parallel()
		n, _, _, requests := liveClient(t)
		n.pos, n.stamp = 40, 1
		go func() {
			n.apply(IncomingMessage{HostName: "laptop", From: "l1", Missed: true})
			n.apply(laptop("l2", `{"stamp":1,"at":30,"name":"pause","data":"no"}`))
			n.apply(laptop("l1", `{"stamp":1,"at":30,"name":"pause","data":"no"}`))
		}()
		if got := next(t, requests); got != "show-text paused: out of sync with laptop 5000" {
			t.Fatalf("sent %q, want the party stopped for l1, and only for it", got)
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		if !n.missed["l1"] || n.missed["l2"] || len(n.missed) != 1 {
			t.Fatalf("missed %v, want l1 alone", n.missed)
		}
	})
	t.Run("synced by", func(t *testing.T) {
		t.Parallel()
		n, _, _, requests := liveClient(t)
		n.paused, n.pos, n.stamp = true, 40, 1
		go func() {
			n.apply(laptop("l1", `{"stamp":2,"at":40,"name":"pause","data":"yes"}`))
			n.apply(laptop("l2", `{"stamp":2,"at":40,"name":"time-pos","data":"40.000000"}`))
			n.apply(laptop("l1", `{"stamp":2,"at":40,"name":"time-pos","data":"40.000000"}`))
		}()
		if got := next(t, requests); got != "show-text synced by laptop 5000" {
			t.Fatalf("sent %q, want one synced by, for l1", got)
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		if n.by != "l1" {
			t.Fatalf("took the state from %q, want l1", n.by)
		}
	})
}

// A member that missed something stays missed until it sends something newer than this
// node holds, which it heard since or made, or joins again. Not when a send reaches it: a
// tie it sent before can still be on its way. Something older says nothing of it.
func TestAMemberThatMissedSomethingIsForgottenOnceItSendsNews(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		via    IncomingMessage
		missed bool
	}{
		"newer":    {peer("a", `{"stamp":2,"at":30,"name":"pause","data":"yes"}`), false},
		"rejoined": {peer("a", `{"name":"joined"}`), false},
		"a tie":    {peer("a", `{"stamp":1,"at":40,"name":"pause","data":"yes"}`), true},
		"older":    {peer("a", `{"stamp":0,"at":40,"name":"pause","data":"yes"}`), true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n, _, _, requests := liveClient(t)
			n.paused, n.pos, n.stamp, n.missed["a"] = true, 40, 1, true
			go func() {
				n.apply(c.via)
				n.apply(peer("a", `{"name":"show-text","data":"mark"}`))
			}()
			for next(t, requests) != "show-text mark 5000" {
			}
			n.mu.Lock()
			defer n.mu.Unlock()
			if n.missed["a"] != c.missed {
				t.Fatalf("a missed: %v, want %v", n.missed["a"], c.missed)
			}
		})
	}
}

// mpv refuses a set while it quits or between files. commit marked every step before mpv
// was told, so whatever mpv never did is let go of: a pause marker would swallow the
// user's next pause, loading would hold every later seek, a seek marker would say this
// node is on a second it never reached.
func TestARefusedStepLeavesNoMarker(t *testing.T) {
	t.Parallel()
	refusing := func(paused, gone bool) (*Client, chan []byte) {
		ours, theirs := net.Pipe()
		t.Cleanup(func() { ours.Close(); theirs.Close() })
		outgoing := make(chan []byte, 16)
		c := newTestClient(outgoing, bufio.NewScanner(ours))
		c.conn.conn = ours
		c.names, c.pos, c.stamp, c.paused = []string{"e1", "e2", "e3"}, 5, 1, paused
		// as New does: a closed socket ends watch, and that is how do learns mpv is gone
		go func() { c.watch(); close(c.conn.closed) }()
		go func() {
			sc := bufio.NewScanner(theirs)
			for sc.Scan() {
				if gone {
					theirs.Close()
					return
				}
				req := Request{}
				json.Unmarshal(sc.Bytes(), &req)
				fmt.Fprintf(theirs, `{"request_id":%d,"error":"property unavailable"}`+"\n", req.RequestId)
			}
		}()
		return c, outgoing
	}
	for _, tc := range []struct {
		name         string
		paused, gone bool
	}{
		{"the pause before a move", false, false},
		{"a seek", true, false},
		{"anything, with mpv gone", false, true},
	} {
		c, outgoing := refusing(tc.paused, tc.gone)
		at := map[bool]string{false: `"entry":2`, true: `"entry":0`}[tc.paused]
		c.apply(peer("a", `{"stamp":2,"paused":true,"at":30,`+at+`,"name":"time-pos","data":"30.000000"}`))
		c.mu.Lock()
		if len(c.applied) > 0 || c.loading || c.heldSeek != "" || len(c.following) > 0 || c.landing || c.peerSeek || c.target() != 5 {
			t.Errorf("%s refused left applied %v, loading %v, held %q, following %v, landing %v, peerSeek %v, at %v", tc.name, c.applied, c.loading, c.heldSeek, c.following, c.landing, c.peerSeek, c.target())
		}
		stamp := c.stamp
		c.mu.Unlock()
		// it holds the party's stamp where mpv isn't, so it asks the party where it is
		var asked bool
		select {
		case b := <-outgoing:
			asked = strings.Contains(string(b), `"name":"joined"`)
		default:
		}
		if want := !tc.gone; asked != want || want && stamp != unheard {
			t.Errorf("%s refused: asked the party %v with stamp %d, want %v", tc.name, asked, stamp, want)
		}
	}
}

// Two players reach the end of a file together, and each stops the party for it. One
// adopts the other's pause, then sends its own reason with the next stamp; the other's
// position, sent at the same moment, arrives after it. That message is older, but it
// says the state this node holds: its sender missed nothing, so the party isn't stopped.
func TestAnOlderMessageThatAgreesIsNotCaughtUp(t *testing.T) {
	t.Parallel()
	c, _, _, requests := playlistClient(t)
	c.paused, c.current, c.pos, c.stamp = true, 2, 0, 11
	go func() {
		c.apply(peer("a", `{"stamp":10,"at":0,"entry":2,"name":"time-pos","data":"0.000000"}`))
		c.apply(peer("a", `{"stamp":10,"at":0,"entry":2,"name":"pause","data":"yes"}`))
		c.apply(peer("a", `{"name":"show-text","data":"mark"}`))
	}()
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q, want nothing for an older message that says what this node holds", got)
	}
}
