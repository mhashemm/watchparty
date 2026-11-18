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
	// undo: the person's key undoes the pause the next go leaves, right after it
	// (personUndoesThePause). acks: the gos whose acks the script says late, once mpv reports
	// what they did, with what mpv holds then
	undo bool
	// first: the person's space runs just before the next go, which the node hasn't read when
	// it sends it. said: the script's messages so far, with the pause each said; over: the gos
	// whose ack says they undid a change the node hadn't read (watchparty.lua)
	first  bool
	seekTo float64
	said   []scriptLine
	over   map[string]bool
	acks   []lateAck
	// the script's seeks: replied, those mpv queued; carrying, the id of the seek mpv runs;
	// seeking, a seek mpv began that the script hasn't said; pos, where mpv is
	replied  []string
	carrying string
	// labels: the id each seek mpvObeys reports carries, taken as mpv takes the seek: a go
	// sent after it, before its event is written, is a seek of its own (or folds into it)
	labels  []string
	seeking bool
	pos     float64
	// waiting: the go a move waits for, applied once its file loads; loading: a move is on its
	// way to heading (watchparty.lua)
	waiting *fakeGo
	loading bool
	heading int
	// what mpv holds: paused, as the script sets it; entry, from what it reported; started,
	// the file it opens
	paused         bool
	entry, started int
}

// lateAck is a go whose ack the script says late; seeks: the go seeks, so its ack comes before
// any seek event (the script's callback).
type lateAck struct {
	id    string
	seeks bool
}

// scriptLine is a script message as the script keeps it: the pause it said, and whether it said
// the person's seek.
type scriptLine struct {
	pause  string
	person bool
}

// fakeGo is a go the script holds until its file loads.
type fakeGo struct {
	paused, second, id string
	entry              int
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
	// noScript: mpv couldn't load the script, as an mpv built without Lua
	noScript bool
}

// newNode starts a node and returns once its file has loaded: what joining said, it still
// has to say (ready drops that).
func newNode(t *testing.T, s setup) *node {
	t.Helper()
	n, err := start(t, s)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return n
}

// start is newNode, giving New's error instead of failing on it.
func start(t *testing.T, s setup) (*node, error) {
	t.Helper()
	ours, theirs := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	n := &node{t: t, in: make(chan IncomingMessage, 64), out: make(chan []byte, 64), writes: make(chan string, 64), refuse: map[string]string{}, over: map[string]bool{}, paused: true, entry: s.start, started: s.start, late: s.late}
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
	observes := 4
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
		return nil, err
	}
	n.c = c
	synctest.Wait()
	return n, nil
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
		if name == "script-message-to" && len(req.Command) == 8 && req.Command[2] == "watchparty-go" && !s.noScript {
			// the script's go is mpv being set, as tests read it (watchparty.lua)
			arg := func(i int) string { return fmt.Sprint(req.Command[i]) }
			n.mu.Lock()
			var key []string
			if n.first {
				n.first, n.paused = false, !n.paused
				key = append(key, n.scriptLocked("pause", ""))
			}
			if n.seekTo != 0 {
				n.pos, n.seekTo = n.seekTo, 0
				key = append(key, n.scriptLocked(seek, ""))
			}
			// what mpv holds against what the node had read, undone by the go (overrules)
			if seen, err := strconv.Atoi(arg(7)); err == nil && seen >= 1 && seen <= len(n.said) {
				if now := holds(n.paused); now != n.said[seen-1].pause && now != arg(3) {
					n.over[arg(6)] = true
				}
				for _, s := range n.said[seen:] {
					if s.person {
						n.over[arg(6)] = true
					}
				}
			}
			lines, quit := n.goLocked(arg(3), arg(4), arg(5), arg(6))
			lines = append(key, lines...)
			n.mu.Unlock()
			if quit {
				conn.Close()
				return
			}
			n.writes <- reply
			for _, line := range lines {
				n.writes <- line
			}
			continue
		}
		n.mu.Lock()
		if n.quitOn != "" && strings.HasPrefix(said, n.quitOn) {
			n.told = append(n.told, said)
			n.mu.Unlock()
			conn.Close()
			return
		}
		refused := false
		for prefix, why := range n.refuse {
			if strings.HasPrefix(said, prefix) {
				refused = true
				reply = fmt.Sprintf(`{"request_id":%d,"error":%q}`+"\n", req.RequestId, why)
			}
		}
		// the script's answer to the node's first two commands, which no test reads (told)
		handshake := name == "script-message-to" && req.Command[2] == "watchparty-check" || name == "script-message" && req.Command[1] == "watchparty-hello"
		if name == "script-message-to" && s.noScript {
			reply = fmt.Sprintf(`{"request_id":%d,"error":"error running command"}`+"\n", req.RequestId)
		}
		taken := !refused
		if name != "observe_property_string" && !handshake {
			n.told = append(n.told, said)
			if name == setProperty && taken {
				n.sets = append(n.sets, req.Command[1:])
			}
		}
		late := n.late && name == setProperty && taken
		if late {
			n.held = append(n.held, reply)
		}
		n.mu.Unlock()
		if !late {
			n.writes <- reply
		}
		if name == "script-message" && handshake {
			n.writes <- n.scriptSays("hello", "watchparty")
		}
		if name == "observe_property_string" {
			if observes--; observes == 0 {
				n.opens(s)
			}
		}
	}
}

// goLocked is the script taking a go (watchparty.lua): another entry opens stopped, and the rest
// waits for its file; otherwise stopped, on the second, then playing if asked, and acked. It
// gives what the script says at once.
func (n *node) goLocked(paused, second, entry, id string) (lines []string, quit bool) {
	if n.waiting != nil {
		lines = append(lines, n.ackLocked(n.waiting.id, false)...)
		n.waiting = nil
	}
	if entry == "-" && n.loading {
		entry = strconv.Itoa(n.heading)
	}
	on := n.started
	if n.loading {
		on = n.heading
	}
	if e, err := strconv.Atoi(entry); err == nil && (n.loading || on != e) {
		if on != e {
			steps := [][]any{{playlistPos, e}}
			if !n.paused {
				steps = [][]any{{pause, true}, {playlistPos, e}}
			}
			if refused, quit := n.stepsLocked(steps); quit || refused {
				return n.refusedLocked(id, refused), quit
			}
			n.loading, n.heading = true, e
		}
		n.paused, n.waiting = true, &fakeGo{paused: paused, second: second, id: id, entry: e}
		return lines, false
	}
	more, quit := n.applyLocked(paused, second, id)
	return append(lines, more...), quit
}

// applyLocked is the script's go on the file mpv is on.
func (n *node) applyLocked(paused, second, id string) (lines []string, quit bool) {
	var steps [][]any
	if second == "-" {
		if holds(n.paused) != paused {
			steps = append(steps, []any{pause, paused == "yes"})
		}
	} else {
		if !n.paused {
			steps = append(steps, []any{pause, true})
		}
		steps = append(steps, []any{timePos, second})
		if paused == "no" {
			steps = append(steps, []any{pause, false})
		}
	}
	refused, quit := n.stepsLocked(steps)
	if quit || refused {
		return n.refusedLocked(id, refused), quit
	}
	if second != "-" {
		n.replied = append(n.replied, id)
	}
	n.paused = paused == "yes"
	lines = n.ackLocked(id, second != "-")
	if n.undo {
		// the person's key, right after the go: the script says its change mark
		n.undo, n.paused = false, !n.paused
		lines = append(lines, n.scriptLocked("pause", ""))
	}
	return lines, false
}

// stepsLocked tells mpv the sets of a go, in order, as told and as sets mpvObeys reports;
// refused: mpv refused one, and the rest weren't made; quit: mpv hung up on one.
func (n *node) stepsLocked(steps [][]any) (refused, quit bool) {
	for _, set := range steps {
		said := strings.TrimSpace(fmt.Sprintln(setProperty, set[0], set[1]))
		n.told = append(n.told, said)
		if n.quitOn != "" && strings.HasPrefix(said, n.quitOn) {
			return false, true
		}
		for prefix := range n.refuse {
			if strings.HasPrefix(said, prefix) {
				return true, false
			}
		}
		n.sets = append(n.sets, set)
		if set[0] == pause {
			n.paused = set[1] == true
		}
	}
	return false, false
}

// refusedLocked is the script saying mpv refused the go, with what mpv holds.
func (n *node) refusedLocked(id string, refused bool) []string {
	if !refused {
		return nil
	}
	return []string{n.scriptLocked("refused", id)}
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
		var end struct{ Reason string }
		json.Unmarshal([]byte(line), &end)
		n.mu.Lock()
		// what the script says after the line, as it hears the same event
		var after []string
		switch {
		case e.EventType == "seek":
			// the script's ack of a go that seeks comes before any seek event (watchparty.lua)
			// of its seek: of a seek reported, the acks up to its own, none sent after it
			if len(n.labels) > 0 {
				label := n.labels[0]
				n.labels = n.labels[1:]
				if i := slices.IndexFunc(n.acks, func(a lateAck) bool { return a.id == label }); i >= 0 {
					for _, a := range n.acks[:i+1] {
						n.writes <- n.scriptLocked("ack", a.id)
					}
					n.acks = n.acks[i+1:]
				}
				n.seeking, n.carrying = true, label
				break
			}
			for len(n.acks) > 0 && n.acks[0].seeks {
				n.writes <- n.scriptLocked("ack", n.acks[0].id)
				n.acks = n.acks[1:]
			}
			// the script says where it goes at the seek event: here, once the second is known
			n.seeking, n.carrying = true, ""
			if len(n.replied) > 0 {
				n.carrying, n.replied = n.replied[len(n.replied)-1], nil
			}
		case e.EventType == "playback-restart" && n.seeking:
			after = []string{n.scriptLocked(seek, n.carrying)}
			n.seeking = false
		case e.EventType == "start-file":
			n.replied, n.carrying, n.seeking, n.pos, n.labels = nil, "", false, 0, nil
			n.started = e.EntryID - 1
		case e.EventType == "end-file" && end.Reason == "error" && n.loading:
			// a file mpv couldn't open: the go waiting for it, acked alone (watchparty.lua)
			n.loading = false
			if w := n.waiting; w != nil {
				n.waiting = nil
				after = n.ackLocked(w.id, false)
			}
		case e.EventType == "file-loaded" && n.waiting == nil:
			n.loading = false
		case e.EventType == "file-loaded":
			// the go the move waited for, or its ack alone on another file
			n.loading = false
			w := n.waiting
			n.waiting = nil
			if n.started == w.entry {
				after, _ = n.applyLocked(w.paused, w.second, w.id)
			} else {
				after = n.ackLocked(w.id, false)
			}
		case e.Name == timePos && e.Data != "":
			pos, _ := strconv.ParseFloat(e.Data, 64)
			// stopped, another second with no seek begun is the person's seek, as tests say it
			if n.seeking || n.paused && pos != n.pos {
				n.pos = pos
				after = []string{n.scriptLocked(seek, n.carrying)}
				n.seeking, n.carrying = false, ""
			}
			n.pos = pos
		case e.Name == pause && e.Data != "":
			// mpv's pause reaches the node from the script
			n.paused = e.paused()
			line = n.scriptLocked("pause", "")
		case e.Name == playlistPos && e.Data != "":
			if entry, _ := strconv.Atoi(e.Data); entry != n.entry {
				// another file: the script reads its second
				n.entry, n.pos = entry, 0
			}
		}
		n.mu.Unlock()
		n.writes <- line
		for _, a := range after {
			n.writes <- a
		}
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
		pending := len(n.acks) > 0
		n.mu.Unlock()
		if len(sets) == 0 && len(held) == 0 && !pending {
			return
		}
		// seeks mpv queued before it ran one share its one slot: only the last lands (ORDER.md)
		last := -1
		for i, set := range sets {
			if set[0] == timePos {
				last = i
			}
		}
		for i, set := range sets {
			if set[0] != timePos || i == last {
				n.write(n.reports(set)...)
			}
		}
		n.write(then...)
		then = nil
		for _, reply := range held {
			n.writes <- reply
		}
		n.mu.Lock()
		acks := n.lateAcksLocked()
		n.mu.Unlock()
		for _, ack := range acks {
			n.writes <- ack
		}
	}
}

// reports is what mpv says once it has taken a set: the script's change mark of a pause, a
// seek with its events (the ack held before it, as the script's callback comes first), a
// move opening the entry at 0.
func (n *node) reports(set []any) []string {
	value := fmt.Sprint(set[1])
	n.mu.Lock()
	defer n.mu.Unlock()
	switch set[0] {
	case pause:
		return []string{n.scriptLocked("pause", "")}
	case timePos:
		pos, _ := strconv.ParseFloat(value, 64)
		label := ""
		if len(n.replied) > 0 {
			label, n.replied = n.replied[len(n.replied)-1], nil
		}
		n.labels = append(n.labels, label)
		return append(n.lateAcksLocked(), `{"event":"seek"}`, mpvAt(pos), `{"event":"playback-restart"}`)
	case playlistPos:
		if i, _ := strconv.Atoi(value); i != n.entry {
			return []string{started(i), propertyChangeLine(playlistPos, value, 0), `{"event":"file-loaded"}`, mpvAt(0)}
		}
	}
	return nil
}

// mpvAnswers is mpv answering every set it holds, reporting nothing, until none is left, and
// the script acking every go.
func (n *node) mpvAnswers() {
	n.t.Helper()
	for {
		synctest.Wait()
		n.mu.Lock()
		held := append(n.held, n.lateAcksLocked()...)
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

// ackLocked is the script's ack of a go, or nothing yet while mpv answers late: it comes once
// mpv reports what the go did (mpvObeys).
func (n *node) ackLocked(id string, seeks bool) []string {
	if n.late {
		n.acks = append(n.acks, lateAck{id, seeks})
		return nil
	}
	return []string{n.scriptLocked("ack", id)}
}

// lateAcksLocked is every ack held, said now.
func (n *node) lateAcksLocked() []string {
	var out []string
	for _, a := range n.acks {
		out = append(out, n.scriptLocked("ack", a.id))
	}
	n.acks = nil
	return out
}

// scriptLocked is the script saying what mpv holds, as watchparty.lua does.
func (n *node) scriptLocked(kind, id string) string {
	n.said = append(n.said, scriptLine{holds(n.paused), kind == seek && id == ""})
	over := ""
	if kind == "ack" && n.over[id] {
		over = "over"
		delete(n.over, id)
	}
	return fmt.Sprintf(`{"event":"client-message","args":["watchparty",%q,%q,%q,"%f","%d",%q]}`+"\n", kind, id, holds(n.paused), n.pos, len(n.said), over)
}

// scriptSays is scriptLocked, for a caller that doesn't hold the lock.
func (n *node) scriptSays(kind, id string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.scriptLocked(kind, id)
}

// personPressesFirst is the person's space running just before the next go, which the node
// hasn't read when it sends it: the go undoes it, and its ack says so.
func (n *node) personPressesFirst() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.first = true
}

// personSeeksFirst is the person's seek to second running just before the next go, which the
// node hasn't read when it sends it: the script says it, and the go's ack says it came first.
func (n *node) personSeeksFirst(second float64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.seekTo = second
}

// personUndoesThePause is the person's key undoing the pause the next go leaves, right after
// its ack: the script says the change mark.
func (n *node) personUndoesThePause() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.undo = true
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
