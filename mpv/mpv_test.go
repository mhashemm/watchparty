package mpv

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	if len(got) != 1 {
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
		New(c, func() {}, ours, make(chan []byte, 1), make(chan IncomingMessage))
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
	_, err := New(c, func() { cancelled.Store(true) }, ours, make(chan []byte, 1), make(chan IncomingMessage))
	if !errors.Is(err, ErrClosed) || time.Since(start) > time.Second {
		t.Fatalf("got %v after %s, want ErrClosed at once", err, time.Since(start))
	}
	time.Sleep(50 * time.Millisecond)
	if cancelled.Load() {
		t.Fatal("cancelled the party before the movie was ready")
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
			} else if n > 3 {
				sent <- strings.TrimSpace(fmt.Sprintln(req.Command...))
			}
		}
	}()

	incoming := make(chan IncomingMessage)
	loaded := make(chan error, 1)
	go func() {
		_, err := New(c, func() {}, ours, make(chan []byte, 8), incoming)
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
	for range 2 {
		e := nextEvent(t, outgoing)
		got[e.Name+"="+e.Data] = true
	}
	if !got[pause+"=yes"] || !got[startPos+"=33.500000"] {
		t.Fatalf("broadcast %v, want the pause and the position", got)
	}
	if got := next(t, requests); got != "show-text paused for j: joining 5000" {
		t.Fatalf("sent %q, want the local notice", got)
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
