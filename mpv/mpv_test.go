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
		ctx:       context.Background(),
		ready:     make(chan struct{}),
		outgoing:  outgoing,
		paused:    false,
		headingTo: -1,
		seen:      map[string]bool{},
		missed:    map[string]bool{},
		conn:      &connection{scanner: sc, closed: make(chan struct{})},
		inbox:     newMailbox[input](),
		replies:   map[int64]outcome{},
		waiting:   map[int64]bool{},
		// as New leaves it: the script said its name
		script: scriptName,
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
					scanned(sc)
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

// scriptAnswers answers New's first two commands as mpv and the script do, and says whether
// req was one: the fakes count only the rest.
func scriptAnswers(conn net.Conn, req Request) bool {
	switch {
	case req.Command[0] == "script-message-to" && req.Command[2] == "watchparty-check":
		fmt.Fprintf(conn, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
	case req.Command[0] == "script-message":
		fmt.Fprintf(conn, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
		fmt.Fprint(conn, `{"event":"client-message","args":["watchparty","hello","watchparty","yes","0.000000"]}`+"\n")
	default:
		return false
	}
	return true
}

// scriptAcks answers the script's set as mpv and the script do, at once, and gives it as the
// set it is to mpv: its seek ("set_property time-pos 30.000000"), or its pause; ok is false for
// any other request.
func scriptAcks(conn net.Conn, req Request) (said string, ok bool) {
	if req.Command[0] != "script-message-to" || len(req.Command) != 9 || req.Command[2] != "watchparty-set" {
		return "", false
	}
	paused, second, id := fmt.Sprint(req.Command[3]), fmt.Sprint(req.Command[4]), fmt.Sprint(req.Command[6])
	said = fmt.Sprintf("set_property pause %t", paused == "yes")
	if second != "-" {
		said = "set_property time-pos " + second
	}
	fmt.Fprintf(conn, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
	fmt.Fprintf(conn, `{"event":"client-message","args":["watchparty","ack",%q,%q,""]}`+"\n", id, paused)
	return said, true
}

// scanned ends a fake's read loop: the pipe closed, as every test ends it, or a line the fake
// couldn't read, which would leave the node waiting on a fake that stopped.
func scanned(sc *bufio.Scanner) {
	if err := sc.Err(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		panic(fmt.Sprintf("the fake stopped reading: %v", err))
	}
}

// fakeMpv answers every request on conn with success and then sends events; with
// closeAfter > 0 it hangs up after that many requests instead.
func fakeMpv(conn net.Conn, closeAfter int, events ...string) {
	sc := bufio.NewScanner(conn)
	for n := 1; sc.Scan(); n++ {
		req := Request{}
		json.Unmarshal(sc.Bytes(), &req)
		if scriptAnswers(conn, req) {
			n--
			continue
		}
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
	scanned(sc)
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
		New(c, func() {}, ours, make(chan []byte, 1), make(chan IncomingMessage), nil, 0, 0, nil)
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
		_, err := New(c, func() { cancelled.Store(true) }, ours, make(chan []byte, 1), make(chan IncomingMessage), nil, 0, 0, nil)
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
				if said, ok := scriptAcks(theirs, req); ok {
					told <- said
					continue
				}
				told <- strings.TrimSpace(fmt.Sprintln(req.Command...))
				fmt.Fprintf(theirs, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
			}
			scanned(sc)
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
			scanned(sc)
		}()
		c, cancel := context.WithCancel(context.Background())
		defer cancel()
		if _, err := New(c, func() {}, ours, make(chan []byte, 8), make(chan IncomingMessage), nil, 0, 0, nil); err == nil || err.Error() != "property not found" {
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
			for n := 1; n <= len(observed) && sc.Scan(); n++ { // every observe
				req := Request{}
				json.Unmarshal(sc.Bytes(), &req)
				if scriptAnswers(theirs, req) {
					n--
					continue
				}
				fmt.Fprintf(theirs, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
			}
			scanned(sc)
			theirs.Close()
		}()
		c, cancel := context.WithCancel(context.Background())
		defer cancel()
		start := time.Now()
		if _, err := New(c, func() {}, ours, make(chan []byte, 8), make(chan IncomingMessage), nil, 0, 0, nil); !errors.Is(err, ErrClosed) || time.Since(start) > time.Second {
			t.Fatalf("got %v after %s, want ErrClosed at once", err, time.Since(start))
		}
	})
}

// An mpv built without Lua can't run the script, and every rule goes through it: the party
// never starts, and says why.
func TestAnMpvWithoutTheScriptIsRefused(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		if _, err := start(t, setup{noScript: true}); !errors.Is(err, ErrNoScript) {
			t.Fatalf("got %v, want ErrNoScript", err)
		}
	})
}

// New returns only once the script has said its name, which the node needs to reach it:
// a user's script with the same file name would take the name the node expects.
func TestNewWaitsForTheScriptsName(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ours, theirs := net.Pipe()
		defer ours.Close()
		defer theirs.Close()
		go func() {
			sc := bufio.NewScanner(theirs)
			for sc.Scan() {
				req := Request{}
				json.Unmarshal(sc.Bytes(), &req)
				fmt.Fprintf(theirs, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
				if req.Command[0] == "observe_property_string" && req.Command[2] == timePos {
					fmt.Fprint(theirs, mpvAt(0))
				}
			}
			scanned(sc)
		}()
		c, cancel := context.WithCancel(context.Background())
		defer cancel()
		opened := make(chan error, 1)
		go func() {
			_, err := New(c, func() {}, ours, make(chan []byte, 8), make(chan IncomingMessage), nil, 0, 0, nil)
			opened <- err
		}()
		synctest.Wait()
		if len(opened) > 0 {
			t.Fatalf("New returned %v before the script said its name", <-opened)
		}
		fmt.Fprint(theirs, `{"event":"client-message","args":["watchparty","hello","watchparty2","yes","0.000000"]}`+"\n")
		synctest.Wait()
		if len(opened) == 0 {
			t.Fatal("New is still waiting once the script said its name")
		}
		if err := <-opened; err != nil {
			t.Fatal(err)
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
			for n := 1; n <= len(observed) && sc.Scan(); n++ { // every observe
				req := Request{}
				json.Unmarshal(sc.Bytes(), &req)
				if scriptAnswers(theirs, req) {
					n--
					continue
				}
				fmt.Fprintf(theirs, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
			}
			scanned(sc)
			fmt.Fprint(theirs, mpvAt(0))
			theirs.Close()
		}()
		cancelled := atomic.Bool{}
		c, cancel := context.WithCancel(context.Background())
		defer cancel()
		if _, err := New(c, func() { cancelled.Store(true) }, ours, make(chan []byte, 8), make(chan IncomingMessage), nil, 0, 0, nil); err != nil {
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
		if _, err := New(c, func() { cancelled.Store(true) }, ours, make(chan []byte, 1), make(chan IncomingMessage), nil, 0, 0, nil); err != nil {
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

func tagged(entry int, event string) string {
	return fmt.Sprintf(`{"entry":%d,%s`, entry, event[1:])
}

// stamped is a peer's message as it arrives from a party: with the stamp of the change it
// is part of. Without one it is as old as a node that has heard nothing.
func stamped(stamp int, event string) string {
	return fmt.Sprintf(`{"stamp":%d,%s`, stamp, event[1:])
}
