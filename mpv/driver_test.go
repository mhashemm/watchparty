package mpv

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
)

// node is one Client as the party and mpv see it: made by New over a pipe, with a fake mpv
// on the other end, and driven only through its edges. What mpv says and what peers send go
// in; what it broadcasts and what it tells mpv come out. A test never reads or sets a field
// of Client, so a refactor that keeps the behaviour keeps the tests. Use it inside
// synctest.Test: after each step everything has run as far as it can, so "nothing was
// broadcast" is a fact, not a timeout.
type node struct {
	t   *testing.T
	c   *Client
	in  chan IncomingMessage
	out chan []byte

	writes chan string
	mu     sync.Mutex
	// told: what it told mpv since the last mpvWasTold. sets: what mpv was set and hasn't
	// reported yet (mpvObeys). refuse: commands mpv answers with an error.
	told   []string
	sets   [][]any
	refuse map[string]string
	// late: mpv answers a set only after reporting it (mpvAnswersLate); held: those answers
	late bool
	held []string
	// quitOn: mpv hangs up on a command starting with this (mpvQuitsOn)
	quitOn string
	// where: what mpv answers when asked where it is (mpvIsAt); held: that answer, until
	// mpvSaysWhere, when holdsWhere
	where      string
	holdsWhere bool
	whereHeld  string
	// what mpv holds, from what it reported
	paused bool
	entry  int
}

// setup is how a node starts. The zero value opened the party, with one file, paused at 0.
type setup struct {
	names  []string          // a playlist of these
	start  int               // the entry it opens on
	at     float64           // where a continued party stopped
	joiner bool              // it didn't open the room: its stamp is unheard until a member answers
	first  []string          // what mpv says before the file's values, as while it still loads
	heard  []IncomingMessage // what the party says while mpv loads
	late   bool              // mpv answers sets late from the start (mpvAnswersLate)
	// opening: what mpv says once observed, instead of the file's values; it must report a
	// time-pos with data, which is how the node knows the file loaded
	opening []string
}

// newNode starts a node and returns once its file has loaded: what joining said, it still
// has to say (ready drops that).
func newNode(t *testing.T, s setup) *node {
	t.Helper()
	ours, theirs := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	n := &node{t: t, in: make(chan IncomingMessage, 64), out: make(chan []byte, 64), writes: make(chan string, 64), refuse: map[string]string{}, paused: true, entry: s.start, late: s.late}
	t.Cleanup(func() { cancel(); ours.Close(); theirs.Close() })
	go func() {
		for {
			select {
			case line := <-n.writes:
				if _, err := fmt.Fprint(theirs, line); err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	observes := 5
	if s.names != nil {
		observes++
	}
	go n.mpv(theirs, observes, s)
	if !s.joiner {
		n.in <- IncomingMessage{Opened: true}
	}
	for _, m := range s.heard {
		n.in <- m
	}
	c, err := New(ctx, cancel, ours, n.out, n.in, s.names, s.start, s.at)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	n.c = c
	synctest.Wait()
	return n
}

// ready is a node that has joined and been answered: nothing from its start is left to read.
func ready(t *testing.T, s setup) *node {
	t.Helper()
	n := newNode(t, s)
	n.broadcasts()
	n.mpvWasTold()
	return n
}

// mpv answers every request, and once New has observed everything, reports what it holds
// as mpv does on an observe. It reports nothing it is set until mpvObeys.
func (n *node) mpv(conn net.Conn, observes int, s setup) {
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		req := Request{}
		json.Unmarshal(sc.Bytes(), &req)
		name := fmt.Sprint(req.Command[0])
		said := strings.TrimSpace(fmt.Sprintln(req.Command...))
		reply := fmt.Sprintf(`{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
		n.mu.Lock()
		if n.quitOn != "" && strings.HasPrefix(said, n.quitOn) {
			n.told = append(n.told, said)
			n.mu.Unlock()
			conn.Close()
			return
		}
		for prefix, why := range n.refuse {
			if strings.HasPrefix(said, prefix) {
				reply = fmt.Sprintf(`{"request_id":%d,"error":%q}`+"\n", req.RequestId, why)
			}
		}
		if said == "get_property_string time-pos" && n.where != "" {
			reply = fmt.Sprintf(`{"data":%q,"request_id":%d,"error":"success"}`+"\n", n.where, req.RequestId)
		}
		taken := strings.Contains(reply, `"error":"success"`)
		if name != "observe_property_string" {
			n.told = append(n.told, said)
			if name == setProperty && taken {
				n.sets = append(n.sets, req.Command[1:])
			}
		}
		late := n.late && name == setProperty && taken
		if late {
			n.held = append(n.held, reply)
		}
		if said == "get_property_string time-pos" && n.holdsWhere {
			n.whereHeld, late = reply, true
		}
		n.mu.Unlock()
		if !late {
			n.writes <- reply
		}
		if name == "observe_property_string" {
			if observes--; observes == 0 {
				n.opens(s)
			}
		}
	}
}

// opens is mpv's answer to the observes, for the file it opened.
func (n *node) opens(s setup) {
	for _, line := range s.first {
		n.writes <- line
	}
	if s.opening != nil {
		for _, line := range s.opening {
			n.writes <- line + "\n"
		}
		return
	}
	if s.names != nil {
		n.writes <- started(s.start)
		n.writes <- `{"event":"file-loaded"}` + "\n"
		n.writes <- propertyChangeLine(playlistPos, strconv.Itoa(s.start), 0)
	}
	n.writes <- propertyChangeLine(filename, "video.mkv", 0)
	n.writes <- propertyChangeLine(pause, "yes", 0)
	n.writes <- propertyChangeLine(pausedForCache, "no", 0)
	n.writes <- propertyChangeLine(duration, "120.000000", 0)
	n.writes <- propertyChangeLine(timePos, "0.000000", 0)
}

// mpvSays is mpv sending these lines, in order.
func (n *node) mpvSays(lines ...string) {
	n.t.Helper()
	n.write(lines...)
	synctest.Wait()
}

func (n *node) write(lines ...string) {
	for _, line := range lines {
		if !strings.HasSuffix(line, "\n") {
			line += "\n"
		}
		e := Event{}
		json.Unmarshal([]byte(line), &e)
		n.mu.Lock()
		switch {
		case e.Name == pause && e.Data != "":
			n.paused = e.paused()
		case e.Name == playlistPos && e.Data != "":
			n.entry, _ = strconv.Atoi(e.Data)
		}
		n.mu.Unlock()
		n.writes <- line
	}
}

// mpvObeys is mpv reporting every set it took since the last time, as it does: a no-op
// set says nothing, a seek is reported with seek and playback-restart around it, and a
// move opens the entry at 0. Answering late, it reports first, then says then, then
// answers, until nothing more is set.
func (n *node) mpvObeys(then ...string) {
	n.t.Helper()
	for {
		synctest.Wait()
		n.mu.Lock()
		sets, held := n.sets, n.held
		n.sets, n.held = nil, nil
		n.mu.Unlock()
		if len(sets) == 0 && len(held) == 0 {
			return
		}
		for _, set := range sets {
			n.write(n.reports(set)...)
		}
		n.write(then...)
		then = nil
		for _, reply := range held {
			n.writes <- reply
		}
	}
}

// reports is what mpv says once it has taken a set.
func (n *node) reports(set []any) []string {
	value := fmt.Sprint(set[1])
	n.mu.Lock()
	defer n.mu.Unlock()
	switch set[0] {
	case pause:
		if paused := set[1] == true; paused != n.paused {
			return []string{propertyChangeLine(pause, map[bool]string{true: "yes", false: "no"}[paused], 0)}
		}
	case timePos:
		pos, _ := strconv.ParseFloat(value, 64)
		return []string{`{"event":"seek"}`, mpvAt(pos), `{"event":"playback-restart"}`}
	case playlistPos:
		if i, _ := strconv.Atoi(value); i != n.entry {
			return []string{started(i), propertyChangeLine(playlistPos, value, 0), `{"event":"file-loaded"}`, mpvAt(0)}
		}
	}
	return nil
}

// mpvAnswers is mpv answering every set it holds, reporting nothing, until none is left.
func (n *node) mpvAnswers() {
	n.t.Helper()
	for {
		synctest.Wait()
		n.mu.Lock()
		held := n.held
		n.held, n.sets = nil, nil
		n.mu.Unlock()
		if len(held) == 0 {
			return
		}
		for _, reply := range held {
			n.writes <- reply
		}
	}
}

// mpvAnswersLate makes mpv answer each set only after reporting it, in mpvObeys: what the
// node does with mpv's report before it has heard back from its own request.
func (n *node) mpvAnswersLate() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.late = true
}

// mpvRefuses makes mpv answer every command starting with prefix with an error, as it does
// for a property it can't set while it quits or between files.
func (n *node) mpvRefuses(prefix, why string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.refuse[prefix] = why
}

// mpvIsAt makes mpv answer get_property_string time-pos with pos, as it does with a seek's
// second at once, before it reports it.
func (n *node) mpvIsAt(pos float64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.where = fmt.Sprintf("%f", pos)
}

// mpvHoldsWhere makes mpv hold its answer to where it is until mpvSaysWhere, so what the
// test says first comes before it.
func (n *node) mpvHoldsWhere() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.holdsWhere = true
}

// mpvSaysWhere sends the answer mpv held.
func (n *node) mpvSaysWhere() {
	n.t.Helper()
	synctest.Wait()
	n.mu.Lock()
	reply := n.whereHeld
	n.whereHeld = ""
	n.mu.Unlock()
	if reply == "" {
		n.t.Fatal("mpv was not asked where it is")
	}
	n.mpvSays(reply)
}

// mpvTakesAgain lifts every refusal.
func (n *node) mpvTakesAgain() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.refuse = map[string]string{}
}

// mpvQuitsOn makes mpv hang up when it is sent a command starting with prefix.
func (n *node) mpvQuitsOn(prefix string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.quitOn = prefix
}

// peerSays is a member's message arriving, whole state filled in as peer() does.
func (n *node) peerSays(host, event string) {
	n.t.Helper()
	n.transportSays(peer(host, event))
}

// transportSays is anything the transport hands the node: a message, or Lost, Back,
// Missed and Resend.
func (n *node) transportSays(m IncomingMessage) {
	n.t.Helper()
	n.in <- m
	synctest.Wait()
}

// broadcasts is what it sent the party since the last call.
func (n *node) broadcasts() []Event {
	n.t.Helper()
	synctest.Wait()
	got := []Event{}
	for len(n.out) > 0 {
		e := Event{}
		if err := json.Unmarshal(<-n.out, &e); err != nil {
			n.t.Fatalf("broadcast something that isn't JSON: %v", err)
		}
		got = append(got, e)
	}
	return got
}

// mpvWasTold is every command it sent mpv since the last call, as "set_property pause true".
func (n *node) mpvWasTold() []string {
	n.t.Helper()
	synctest.Wait()
	n.mu.Lock()
	defer n.mu.Unlock()
	told := n.told
	n.told = nil
	return told
}

// seeksIn is the seeks among what it told mpv.
func seeksIn(told []string) []string {
	out := []string{}
	for _, s := range told {
		if strings.HasPrefix(s, "set_property time-pos") {
			out = append(out, s)
		}
	}
	return out
}

// said is its broadcasts as name=data, the shortest way to compare them.
func said(events []Event) []string {
	out := []string{}
	for _, e := range events {
		out = append(out, e.Name+"="+e.Data)
	}
	return out
}

// keepsTheSeek is a follower keeping the person's seek on its screen: nothing goes out, and the
// person is told who holds the seek, which also says the seek wasn't taken for an echo.
func (n *node) keepsTheSeek(leader string) {
	n.t.Helper()
	if got := n.broadcasts(); len(got) != 0 {
		n.t.Fatalf("broadcast %q, want the follower's seek kept on its screen", said(got))
	}
	want := fmt.Sprintf("show-text only %s can seek until resumed 5000", leader)
	if told := n.mpvWasTold(); !slices.Contains(told, want) {
		n.t.Fatalf("told mpv %q, want %q", told, want)
	}
}

// plays is the person at this node resuming, and mpv doing it.
func (n *node) plays() {
	n.t.Helper()
	n.mpvSays(propertyChangeLine(pause, "no", 0))
	n.broadcasts()
}

// takes is the node taking a member's state, stopped or playing from at, with its stamp,
// and mpv doing it. Nothing it said or told mpv is left to read.
func (n *node) takes(stamp int, paused bool, at float64) {
	n.t.Helper()
	n.takesFrom("z", stamp, paused, at)
}

// takesFrom is takes, from that member.
func (n *node) takesFrom(host string, stamp int, paused bool, at float64) {
	n.t.Helper()
	n.mu.Lock()
	entry := n.entry
	n.mu.Unlock()
	n.peerSays(host, fmt.Sprintf(`{"stamp":%d,"at":%f,"entry":%d,"event":"property-change","name":"pause","data":"%s"}`, stamp, at, entry, map[bool]string{true: "yes", false: "no"}[paused]))
	n.mpvObeys()
	n.broadcasts()
	n.mpvWasTold()
}

// nodeTest runs f in a bubble of its own.
func nodeTest(t *testing.T, f func(t *testing.T)) {
	t.Parallel()
	synctest.Test(t, f)
}

// seekEvent is a member's seek, as it arrives.
func seekEvent(pos float64) string {
	return fmt.Sprintf(`{"event":"property-change","name":"time-pos","data":"%f"}`, pos)
}

// mpvAt is mpv reporting where it is.
func mpvAt(pos float64) string {
	return propertyChangeLine(timePos, fmt.Sprintf("%f", pos), 0)
}

// threeVideos is a playlist's names, as everyone's list is the same.
var threeVideos = []string{"e1", "e2", "e3"}
