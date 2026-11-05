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
	"strings"
	"sync/atomic"
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
		ready:    make(chan struct{}),
		outgoing: outgoing,
		paused:   false,
		applied:  map[string]string{},
		seen:     map[string]bool{},
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
		case pausedAt:
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

// mpv fires one property-change per observed property the moment it is
// registered. A fresh node is paused, so an unswallowed initial time-pos would be
// broadcast as time-pos=0 and rewind the whole party.
func TestInitialPropertyChangeIsNotBroadcast(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	outgoing := make(chan []byte, 4)
	c := newTestClient(outgoing, nil)
	c.paused = true
	c.seen[timePos] = true

	// sync() marks the value before the IPC call; this is the echo coming back
	c.seeks = []float64{77}
	got := feed(t, c, outgoing, propertyChangeLine(timePos, "77.000000", 0))
	if len(got) != 0 {
		t.Fatalf("rebroadcast the event it had just applied: %+v", got)
	}

	// a seek's echo can land a frame off the applied value
	c.seeks = []float64{12}
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

// b applies 30 and then 40 before mpv's echo of 30 is back. That late echo must
// not go out as b's own seek, or the whole party jumps back to 30.
func TestLateEchoOfAnEarlierSeekIsNotRebroadcast(t *testing.T) {
	t.Parallel()
	outgoing := make(chan []byte, 4)
	c := newTestClient(outgoing, nil)
	c.paused = true
	c.seen[timePos] = true

	c.seeks = []float64{30, 40}
	got := feed(t, c, outgoing, propertyChangeLine(timePos, "30.000000", 0)+propertyChangeLine(timePos, "40.000000", 1))
	if len(got) != 0 {
		t.Fatalf("rebroadcast an echo: %+v", got)
	}

	// 40's echo means mpv is done with 30, so seeking back there is the user's own
	got = feed(t, c, outgoing, propertyChangeLine(timePos, "30.000000", 2))
	if len(got) != 1 {
		t.Fatalf("swallowed a genuine seek back to an old position: %+v", got)
	}
}

// --hr-seek=yes is pinned, so an echo is at most a frame off. A deliberate seek
// a fraction of a second away is still the user's own.
func TestSmallSeekIsNotAnEcho(t *testing.T) {
	t.Parallel()
	outgoing := make(chan []byte, 4)
	c := newTestClient(outgoing, nil)
	c.paused = true
	c.seen[timePos] = true

	c.seeks = []float64{12}
	got := feed(t, c, outgoing, propertyChangeLine(timePos, "12.300000", 0))
	if len(got) != 1 {
		t.Fatalf("swallowed a 0.3s seek as an echo: %+v", got)
	}
}

func TestAppliedPauseIsNotRebroadcast(t *testing.T) {
	t.Parallel()
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
	if len(got) != 2 || got[0].Name != pause || got[1].Name != pausedAt {
		t.Fatalf("stale marker swallowed a genuine pause: %+v", got)
	}
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
		New(c, func() {}, ours, make(chan []byte, 1), make(chan IncomingMessage), 0)
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
	ours, theirs := net.Pipe()
	defer ours.Close()
	go fakeMpv(theirs, 2)
	cancelled := atomic.Bool{}
	start := time.Now()
	c, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := New(c, func() { cancelled.Store(true) }, ours, make(chan []byte, 1), make(chan IncomingMessage), 0)
	if !errors.Is(err, ErrClosed) || time.Since(start) > time.Second {
		t.Fatalf("got %v after %s, want ErrClosed at once", err, time.Since(start))
	}
	time.Sleep(50 * time.Millisecond)
	if cancelled.Load() {
		t.Fatal("cancelled the party before the video was ready")
	}
}

// A peer must never wait on a file that is slow to load (a stream): every send
// lands while mpv is still loading, and only the last position and pause are
// applied once it has, position first.
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
		_, err := New(c, func() {}, ours, make(chan []byte, 8), incoming, 0)
		loaded <- err
	}()
	send := func(name, data string) {
		t.Helper()
		select {
		case incoming <- IncomingMessage{HostName: "a", Event: []byte(`{"name":"` + name + `","data":"` + data + `"}`)}:
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
	want := []string{"set_property time-pos 999", "show-text synced by a 5000",
		"set_property pause false", "show-text resumed by a 5000", "show-text mark 5000"}
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
	t.Parallel()
	_, send, outgoing, requests := liveClient(t)

	send(propertyChangeLine(timePos, "10.000000", 0))
	send(propertyChangeLine(timePos, "10.041000", 0))
	send(`{"event":"seek"}` + "\n")
	send(propertyChangeLine(timePos, "40.000000", 0))
	if got := next(t, requests); got != "set_property pause true" {
		t.Fatalf("sent %q, want the node to pause itself", got)
	}
	send(propertyChangeLine(timePos, "40.041000", 0))
	send(propertyChangeLine(pause, "yes", 0))

	if e := nextEvent(t, outgoing); e.Name != pause || e.Data != "yes" {
		t.Fatalf("first broadcast %+v, want the pause", e)
	}
	if e := nextEvent(t, outgoing); e.Name != timePos || e.Data != "40.041000" {
		t.Fatalf("second broadcast %+v, want where it paused", e)
	}
	if e := nextEvent(t, outgoing); e.Name != pauseReason || e.Data != "seek" {
		t.Fatalf("third broadcast %+v, want the reason", e)
	}
	if got := next(t, requests); got != "show-text paused for everyone: seek 5000" {
		t.Fatalf("sent %q, want the local notice", got)
	}

	// mpv's frame-landing second value is the position just sent, not a new seek
	send(propertyChangeLine(timePos, "40.042000", 0))
	send(propertyChangeLine(timePos, "50.000000", 0))
	if e := nextEvent(t, outgoing); e.Name != timePos || e.Data != "50.000000" {
		t.Fatalf("broadcast %+v, want only the next real seek", e)
	}
}

// Playback moves time-pos on every frame, and none of it may go out or pause anyone.
func TestPlaybackIsNotASeek(t *testing.T) {
	t.Parallel()
	_, send, outgoing, requests := liveClient(t)

	for i := range 48 {
		send(propertyChangeLine(timePos, fmt.Sprintf("%f", 10+float64(i)/24), 0))
	}
	send(propertyChangeLine(pause, "yes", 0))
	if e := nextEvent(t, outgoing); e.Name != pause {
		t.Fatalf("broadcast %+v during playback, want only the later pause", e)
	}
	select {
	case r := <-requests:
		t.Fatalf("sent %q during playback", r)
	default:
	}
}

// A node waiting on its cache falls behind everyone still playing. It pauses the
// party instead, and stays paused for a manual resume once the cache refills.
func TestBufferingPausesTheParty(t *testing.T) {
	t.Parallel()
	_, send, outgoing, requests := liveClient(t)

	send(propertyChangeLine(timePos, "20.000000", 0))
	send(propertyChangeLine(pausedForCache, "yes", 0))
	if got := next(t, requests); got != "set_property pause true" {
		t.Fatalf("sent %q, want the node to pause itself", got)
	}
	send(propertyChangeLine(pause, "yes", 0))
	for _, want := range []string{pause + "=yes", timePos + "=20.000000", pauseReason + "=buffering"} {
		if e := nextEvent(t, outgoing); e.Name+"="+e.Data != want {
			t.Fatalf("broadcast %s=%s, want %s", e.Name, e.Data, want)
		}
	}
	next(t, requests)

	// a resume while still stalled is not passed on, and pauses again
	send(propertyChangeLine(pause, "no", 0))
	if got := next(t, requests); got != "set_property pause true" {
		t.Fatalf("sent %q, want the node to pause itself again", got)
	}
	send(propertyChangeLine(pause, "yes", 0))
	for _, want := range []string{timePos + "=20.000000", pauseReason + "=still buffering"} {
		if e := nextEvent(t, outgoing); e.Name+"="+e.Data != want {
			t.Fatalf("broadcast %s=%s, want %s", e.Name, e.Data, want)
		}
	}
}

// A peer's resume reaching a node that is still stalled is refused: the node never
// plays, and tells the party it is paused again and where. Applying the resume and
// pausing at once doesn't work, because mpv folds the two into no event at all.
func TestRemoteResumeWhileBufferingIsRefused(t *testing.T) {
	t.Parallel()
	c, _, outgoing, requests := liveClient(t)
	c.paused, c.stalled, c.pos = true, true, 20

	c.pause(Event{Name: pause, Data: "no"}, "a")
	for _, want := range []string{pause + "=yes", timePos + "=20.000000", pauseReason + "=still buffering"} {
		if e := nextEvent(t, outgoing); e.Name+"="+e.Data != want {
			t.Fatalf("broadcast %s=%s, want %s", e.Name, e.Data, want)
		}
	}
	if got := next(t, requests); got != "show-text paused for everyone: still buffering 5000" {
		t.Fatalf("sent %q, want the resume refused", got)
	}
}

func peer(host, event string) IncomingMessage {
	return IncomingMessage{HostName: host, Event: []byte(event)}
}

// Once its file has loaded, a node says so, and from then on it takes the first
// position the party answers with.
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
	incoming <- peer("a", `{"event":"property-change","name":"start-pos","data":"40.000000"}`)
	if got := next(t, requests); got != "set_property time-pos 40.000000" {
		t.Fatalf("sent %q, want the party's position", got)
	}
	next(t, requests)

	// the first answer is the one; a later one comes from another node and is ignored
	incoming <- peer("b", `{"event":"property-change","name":"start-pos","data":"41.000000"}`)
	incoming <- peer("b", `{"name":"show-text","data":"mark"}`)
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q, want the second answer ignored", got)
	}
	select {
	case e := <-outgoing:
		t.Fatalf("broadcast %s after the first joined", e)
	default:
	}
}

// A node already in the party pauses it for a joiner and answers with where it
// stopped.
func TestJoinPausesThePartyAndSendsThePosition(t *testing.T) {
	t.Parallel()
	c, send, outgoing, requests := liveClient(t)
	send(propertyChangeLine(timePos, "33.500000", 0))

	go c.apply(peer("j", `{"name":"joined"}`))
	if got := next(t, requests); got != "set_property pause true" {
		t.Fatalf("sent %q, want the party paused", got)
	}
	send(propertyChangeLine(pause, "yes", 0))
	got := map[string]bool{}
	for range 3 {
		e := nextEvent(t, outgoing)
		got[e.Name+"="+e.Data] = true
	}
	if !got[pause+"=yes"] || !got[pausedAt+"=33.500000"] || !got[startPos+"=33.500000"] {
		t.Fatalf("broadcast %v, want the pause and the position", got)
	}
	if got := next(t, requests); got != "show-text paused for j: joining 5000" {
		t.Fatalf("sent %q, want the local notice", got)
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
	if e := nextEvent(t, outgoing); e.Name != startPos {
		t.Fatalf("broadcast %s, want the position", e.Name)
	}
}

// A node still waiting for its own position has none to give, so it doesn't answer.
func TestJoiningNodeDoesNotAnswerAnotherJoiner(t *testing.T) {
	t.Parallel()
	c, _, outgoing, requests := liveClient(t)
	c.paused, c.joining = true, true

	c.apply(peer("j", `{"name":"joined"}`))
	c.apply(peer("j", `{"name":"show-text","data":"mark"}`))
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q before the mark", got)
	}
	select {
	case e := <-outgoing:
		t.Fatalf("answered another joiner with %s", e)
	default:
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
	got := map[string]bool{}
	for range 2 {
		e := nextEvent(t, outgoing)
		got[e.Name+"="+e.Data] = true
	}
	if !got[pause+"=yes"] || !got[pausedAt+"=33.500000"] {
		t.Fatalf("broadcast %v, want the pause and the position", got)
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

// Everyone stops a moment apart, so they settle on the earliest stop, or they would
// resume that far apart and stay so.
func TestPausesForALeaveSettleOnTheEarliest(t *testing.T) {
	t.Parallel()
	c, _, _, requests := liveClient(t)
	c.paused, c.settling, c.pos = true, true, 40

	go func() {
		c.apply(peer("a", `{"event":"property-change","name":"paused-at","data":"41.000000"}`))
		c.apply(peer("b", `{"event":"property-change","name":"paused-at","data":"39.000000"}`))
	}()
	if got := next(t, requests); got != "set_property time-pos 39.000000" {
		t.Fatalf("sent %q, want only the earlier stop followed", got)
	}
}

// Without a leave, a pause that isn't ours to follow stays ignored, earlier or not.
// Every member stops for a joiner the moment it hears, so they stop a little apart, and
// the joiner stops wherever the first answer said. All of them settle on the earliest.
func TestPausesForAJoinSettleOnTheEarliest(t *testing.T) {
	t.Parallel()
	earlier := `{"name":"paused-at","data":"39.000000"}`

	c, _, outgoing, requests := liveClient(t)
	c.paused, c.pos = true, 40
	go func() {
		c.apply(peer("j", `{"name":"joined"}`))
		c.apply(peer("a", earlier))
	}()
	next(t, requests)
	nextEvent(t, outgoing)
	if got := next(t, requests); got != "set_property time-pos 39.000000" {
		t.Fatalf("a member sent %q, want to follow the earlier stop", got)
	}

	c, send, _, requests := liveClient(t)
	c.paused, c.joining = true, true
	go c.apply(peer("a", `{"name":"start-pos","data":"40.000000"}`))
	for next(t, requests) != "set_property time-pos 40.000000" {
	}
	next(t, requests)
	send(propertyChangeLine(timePos, "40.000000", 0))
	for !func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.pos == 40 }() {
		time.Sleep(time.Millisecond)
	}
	go c.apply(peer("b", earlier))
	if got := next(t, requests); got != "set_property time-pos 39.000000" {
		t.Fatalf("a joiner sent %q, want to follow the earlier stop", got)
	}
}

// The earliest member's paused-at can beat the first answer, while the joiner isn't
// stopped yet, and its own answer then comes second, before the first seek has reported.
func TestAJoinerFollowsAnEarlierAnswer(t *testing.T) {
	t.Parallel()
	c, _, _, requests := liveClient(t)
	c.paused, c.joining = true, true
	go func() {
		c.apply(peer("a", `{"name":"start-pos","data":"40.000000"}`))
		c.apply(peer("b", `{"name":"start-pos","data":"39.000000"}`))
		c.apply(peer("c", `{"name":"start-pos","data":"40.000000"}`))
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

func TestEarlierStopIsIgnoredWithoutALeave(t *testing.T) {
	t.Parallel()
	c, _, _, requests := liveClient(t)
	c.paused, c.pos = true, 40

	go func() {
		c.apply(peer("b", `{"event":"property-change","name":"paused-at","data":"39.000000"}`))
		c.apply(peer("b", `{"name":"show-text","data":"mark"}`))
	}()
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q, want the earlier stop ignored", got)
	}
}

// A joiner already where the party is skips the seek, but that still was its answer.
func TestStartPosAlreadyThereStillCounts(t *testing.T) {
	t.Parallel()
	c, _, _, requests := liveClient(t)
	c.paused, c.joining, c.pos = true, true, 40

	c.apply(peer("a", `{"event":"property-change","name":"start-pos","data":"40.050000"}`))
	c.apply(peer("b", `{"event":"property-change","name":"start-pos","data":"55.000000"}`))
	c.apply(peer("b", `{"name":"show-text","data":"mark"}`))
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q, want neither answer to seek", got)
	}
}

// A pause carries where the pauser stopped. Over a slow link the others stop a
// round trip later; without this they resume that far apart and stay so.
func TestOwnPauseSendsWhereItStopped(t *testing.T) {
	t.Parallel()
	_, send, outgoing, _ := liveClient(t)
	send(propertyChangeLine(timePos, "61.250000", 0))
	send(propertyChangeLine(pause, "yes", 0))
	for _, want := range []string{pause + "=yes", pausedAt + "=61.250000"} {
		if e := nextEvent(t, outgoing); e.Name+"="+e.Data != want {
			t.Fatalf("broadcast %s=%s, want %s", e.Name, e.Data, want)
		}
	}
}

// A node follows the position of the peer whose pause it applied, and nobody
// else's: two peers pausing at once must not each jump to the other's spot.
func TestPausedAtFollowsOnlyWhoPausedUs(t *testing.T) {
	t.Parallel()
	c, _, _, requests := liveClient(t)
	c.pos = 60

	c.pause(Event{Name: pause, Data: "yes"}, "a")
	next(t, requests)
	next(t, requests)
	c.apply(peer("b", `{"event":"property-change","name":"paused-at","data":"59.000000"}`))
	c.apply(peer("a", `{"event":"property-change","name":"paused-at","data":"60.400000"}`))
	if got := next(t, requests); got != "set_property time-pos 60.400000" {
		t.Fatalf("sent %q, want only a's position followed", got)
	}
	c.apply(peer("a", `{"event":"property-change","name":"paused-at","data":"60.420000"}`))
	c.apply(peer("a", `{"name":"show-text","data":"mark"}`))
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q, want a position already within a frame skipped", got)
	}
}

func playlistClient(t *testing.T) (*Client, func(string), chan []byte, chan string) {
	t.Helper()
	c, send, outgoing, requests := liveClient(t)
	c.entries = 3
	c.seen[playlistPos] = true
	c.movedFrom = -1
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

	go c.apply(peer("a", move(0, 2)))
	for _, want := range []string{"set_property pause true", "show-text paused by a 5000", "set_property playlist-pos 2", "show-text video 3 of 3, by a 5000"} {
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
	c.paused, c.current, c.joining = true, 1, true

	go func() {
		c.apply(peer("a", `{"event":"property-change","name":"playlist-pos","data":"1"}`))
		c.apply(peer("a", `{"event":"property-change","name":"playlist-pos","data":"2"}`))
		c.apply(peer("b", `{"event":"property-change","name":"playlist-pos","data":"2"}`))
		c.apply(peer("b", `{"event":"property-change","name":"playlist-pos","data":"3"}`))
		c.apply(peer("b", `{"name":"show-text","data":"mark"}`))
	}()
	for _, want := range []string{"set_property playlist-pos 2", "show-text video 3 of 3, by a 5000", "show-text mark 5000"} {
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
		c.apply(peer("a", move(0, 1)))
		c.apply(peer("a", `{"event":"property-change","name":"time-pos","data":"40.000000"}`))
	}()
	for _, want := range []string{"set_property playlist-pos 1", "show-text video 2 of 3, by a 5000", "show-text synced by a 5000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("sent %q, want %q", got, want)
		}
	}
	send(`{"event":"file-loaded"}` + "\n")
	if got := next(t, requests); got != "set_property time-pos 40.000000" {
		t.Fatalf("sent %q, want the held seek", got)
	}
}

// The room's first node is still a joiner until someone resumes or seeks, so it has no
// position to give, but it does know which entry it opened.
func TestJoiningNodeStillNamesItsEntry(t *testing.T) {
	t.Parallel()
	c, _, outgoing, requests := playlistClient(t)
	c.paused, c.joining, c.current = true, true, 2

	c.apply(peer("j", `{"name":"joined"}`))
	c.apply(peer("j", `{"name":"show-text","data":"mark"}`))
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("sent %q before the mark", got)
	}
	if e := nextEvent(t, outgoing); e.Name != playlistPos || e.Data != "2" {
		t.Fatalf("broadcast %+v, want the entry", e)
	}
	select {
	case e := <-outgoing:
		t.Fatalf("answered with a position too: %s", e)
	default:
	}
}

// A joiner opens on the first entry, so it is told which one before where in it.
func TestJoinAnswerNamesTheEntryFirst(t *testing.T) {
	t.Parallel()
	c, _, outgoing, _ := playlistClient(t)
	c.paused, c.current, c.pos = true, 2, 33

	go c.apply(peer("j", `{"name":"joined"}`))
	if e := nextEvent(t, outgoing); e.Name != playlistPos || e.Data != "2" {
		t.Fatalf("broadcast %+v, want the entry", e)
	}
	if e := nextEvent(t, outgoing); e.Name != startPos || e.Data != "33.000000" {
		t.Fatalf("broadcast %+v, want the position", e)
	}
}

// Everyone hears the answer to a joiner, and it can arrive after someone has moved on.
// Only the joiner may act on it: followed by the rest, an answer sent just before two
// people crossed sends each to the other's entry, and they stay apart.
func TestAnswerToAJoinerMovesOnlyTheJoiner(t *testing.T) {
	t.Parallel()
	answer := `{"event":"property-change","name":"playlist-pos","data":"2"}`
	mark := `{"name":"show-text","data":"mark"}`

	c, _, _, requests := playlistClient(t)
	c.paused, c.current = true, 1
	go func() {
		c.apply(peer("a", answer))
		c.apply(peer("a", mark))
	}()
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("a member followed it: %q", got)
	}

	c, send, outgoing, requests := playlistClient(t)
	c.paused, c.current, c.joining = true, 1, true
	send(propertyChangeLine(playlistPos, "0", 0))
	next(t, outgoing)
	go func() {
		c.apply(peer("a", answer))
		c.apply(peer("a", mark))
	}()
	if got := next(t, requests); got != "show-text mark 5000" {
		t.Fatalf("a joiner who has moved by hand followed it: %q", got)
	}

	c, _, _, requests = playlistClient(t)
	c.paused, c.current, c.joining = true, 1, true
	go c.apply(peer("a", answer))
	if got := next(t, requests); got != "set_property playlist-pos 2" {
		t.Fatalf("a joiner sent %q, want to follow it", got)
	}
}

func tagged(entry int, event string) string {
	return fmt.Sprintf(`{"entry":%d,%s`, entry, event[1:])
}

// Everything a playlist node sends says which entry it belongs to, and a move also
// says which one it left. A single file's messages stay exactly as they were.
func TestMessagesSayWhichEntryTheyBelongTo(t *testing.T) {
	t.Parallel()
	c, send, outgoing, _ := playlistClient(t)
	c.paused = true

	send(propertyChangeLine(playlistPos, "2", 0))
	e := nextEvent(t, outgoing)
	if e.Name != playlistPos || e.From == nil || *e.From != 0 || e.Entry == nil || *e.Entry != 2 {
		t.Fatalf("broadcast %+v, want a move from 0 to 2", e)
	}
	send(propertyChangeLine(timePos, "0.000000", 0))
	send(propertyChangeLine(timePos, "9.000000", 0))
	if e := nextEvent(t, outgoing); e.Name != timePos || e.Entry == nil || *e.Entry != 2 {
		t.Fatalf("broadcast %+v, want a seek on entry 2", e)
	}

	single, send, outgoing, _ := liveClient(t)
	single.paused = true
	send(propertyChangeLine(timePos, "9.000000", 0))
	if msg := string(next(t, outgoing)); strings.Contains(msg, "entry") {
		t.Fatalf("a single file sent %s", msg)
	}
}

// A seek sent just before its sender moved, or just before we did, belongs to a file
// we're not playing. So does a resume. A pause still stops us: stopping is always safe.
func TestMessagesFromAnotherEntryAreDropped(t *testing.T) {
	t.Parallel()
	c, _, _, requests := playlistClient(t)
	c.paused, c.current = true, 1

	go func() {
		c.apply(peer("a", tagged(0, `{"event":"property-change","name":"time-pos","data":"40.000000"}`)))
		c.apply(peer("a", tagged(0, `{"event":"property-change","name":"start-pos","data":"40.000000"}`)))
		c.apply(peer("a", tagged(0, `{"name":"pause","data":"no"}`)))
		c.apply(peer("a", tagged(1, `{"event":"property-change","name":"time-pos","data":"41.000000"}`)))
	}()
	if got := next(t, requests); got != "set_property time-pos 41.000000" {
		t.Fatalf("sent %q, want only the seek on our entry", got)
	}
}

func TestAPauseFromAnotherEntryStillStops(t *testing.T) {
	t.Parallel()
	c, _, _, requests := playlistClient(t)
	c.current = 1

	go c.apply(peer("a", tagged(0, `{"name":"pause","data":"yes"}`)))
	if got := next(t, requests); got != "set_property pause true" {
		t.Fatalf("sent %q, want the pause", got)
	}
}

// While a move is on its way, the entry it is heading to is the one that counts.
func TestMessagesAreCheckedAgainstWhereWeAreHeading(t *testing.T) {
	t.Parallel()
	c, send, _, requests := playlistClient(t)
	c.paused = true

	go func() {
		c.apply(peer("a", move(0, 1)))
		c.apply(peer("a", tagged(0, `{"event":"property-change","name":"time-pos","data":"40.000000"}`)))
		c.apply(peer("a", tagged(1, `{"event":"property-change","name":"time-pos","data":"41.000000"}`)))
	}()
	for _, want := range []string{"set_property playlist-pos 1", "show-text video 2 of 3, by a 5000", "show-text synced by a 5000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("sent %q, want %q", got, want)
		}
	}
	send(`{"event":"file-loaded"}` + "\n")
	if got := next(t, requests); got != "set_property time-pos 41.000000" {
		t.Fatalf("sent %q, want the seek on the new entry", got)
	}
}

func move(from, to int) string {
	return fmt.Sprintf(`{"from":%d,"event":"property-change","name":"playlist-pos","data":"%d"}`, from, to)
}

// Two people leave entry 5 at once, one forward and one back. Without a rule each
// follows the other and they swap. Everyone applies the same rule, so they end up
// together on the higher one.
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
				client.apply(peer("a", move(left, c.theirs)))
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

// Someone behind everyone else hears a tie too, and what it compares is where the moves
// left from, not where it happens to be.
func TestATieIsSeenFromAnyEntry(t *testing.T) {
	t.Parallel()
	c, _, _, requests := playlistClient(t)
	c.paused = true

	go func() {
		c.apply(peer("a", move(1, 2)))
		c.apply(peer("b", move(1, 1)))
		c.apply(peer("b", `{"name":"show-text","data":"mark"}`))
	}()
	for _, want := range []string{"set_property playlist-pos 2", "show-text video 3 of 3, by a 5000", "show-text mark 5000"} {
		if got := next(t, requests); got != want {
			t.Fatalf("sent %q, want %q", got, want)
		}
	}
}

// The rule only settles a tie. Any other move is followed, lower or not, so someone who
// missed a message catches up on the next one instead of staying apart.
func TestALowerMoveThatIsNotATieIsFollowed(t *testing.T) {
	t.Parallel()
	c, _, _, requests := playlistClient(t)
	c.paused, c.current, c.movedFrom = true, 2, 1

	go c.apply(peer("a", move(2, 0)))
	if got := next(t, requests); got != "set_property playlist-pos 0" {
		t.Fatalf("sent %q, want it followed", got)
	}
}

// Two players reach the end of an entry together. One decides to follow the other, and
// its own move arrives while it is pausing. The set that follows would change nothing, so
// mpv would never answer it with an event, and it would wait for a load that has passed,
// holding every later seek forever.
func TestOwnMoveWhileFollowingDoesNotHoldLaterSeeks(t *testing.T) {
	t.Parallel()
	ours, theirs := net.Pipe()
	t.Cleanup(func() { ours.Close(); theirs.Close() })
	c := newTestClient(make(chan []byte, 16), bufio.NewScanner(ours))
	c.conn.conn = ours
	c.entries, c.movedFrom = 3, -1
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
			if command == "set_property pause true" {
				fmt.Fprint(theirs, propertyChangeLine(playlistPos, "2", 0), `{"event":"file-loaded"}`+"\n")
			}
			fmt.Fprintf(theirs, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
		}
	}()
	go c.watch()

	c.apply(peer("a", move(1, 2)))
	c.apply(peer("a", tagged(2, `{"event":"property-change","name":"time-pos","data":"5.000000"}`)))
	for got := next(t, sent); got != "set_property time-pos 5.000000"; got = next(t, sent) {
		if got == "set_property playlist-pos 2" {
			t.Fatal("set the entry it was already on")
		}
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

	go c.apply(peer("a", move(0, 1)))
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
