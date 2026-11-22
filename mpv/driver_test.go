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

	lua "github.com/yuin/gopher-lua"
)

// node is one Client as the party and mpv see it: made by New over a pipe, with a fake mpv
// on the other end, and driven only through its edges. What mpv says and what peers send go
// in; what it broadcasts and what it tells mpv come out. A test never reads or sets a field
// of Client, so a refactor that keeps the behaviour keeps the tests. Use it inside
// synctest.Test: after each step everything has run as far as it can, so "nothing was
// broadcast" is a fact, not a timeout. The fake mpv runs the shipped watchparty.lua
// (script_test.go): it models only mpv, and the script decides what it says.
type node struct {
	t     *testing.T
	c     *Client
	in    chan IncomingMessage
	out   chan []byte
	typed chan string
	picks chan string

	writes chan string
	// batch is held while one mpvSays (or mpvObeys step) writes its lines, and while mpv takes
	// each request: what the node asks in answer to those lines, mpv reads after all of
	// them, as it had said them before reading it. Taken mid-batch, the node's pause let mpv
	// report a playing frame after it had paused, which mpv doesn't do (MPV.md).
	batch sync.Mutex
	mu    sync.Mutex
	// told: what it told mpv since the last mpvWasTold, the script's steps as the sets they
	// are. sets: what mpv was set and hasn't reported yet (mpvObeys). refuse: commands mpv
	// answers with an error.
	told   []string
	chats  []string
	sets   [][]any
	refuse map[string]string
	// late: mpv answers a set only after reporting it (mpvAnswersLate); held: those answers.
	// Its script's seeks are answered once mpv reports them (replies), and what the script
	// says from a set's ack on waits for that too (saying), as mpv's event queue holds it.
	late    bool
	held    []string
	replies []int
	saying  []string
	// quitOn: mpv hangs up on a command starting with this (mpvQuitsOn); quit: the script
	// sent one
	quitOn string
	quit   bool
	// the person's keys around the next set: first, a space just before it, which the node
	// hasn't read; seekTo and moveTo, a seek or a move just before it; moveDuring, a move once
	// it has begun, before its seek; undo, a space right after it. ranOutTo: mpv's own move
	// there at the file's end, just before it.
	first      bool
	seekTo     float64
	moveTo     int
	moveDuring int
	undo       bool
	ranOutTo   int
	// what mpv holds: paused, pos, and entry, its playlist-pos; opened, the entry of the last
	// start-file. seeking: a seek event the script hears once its second is known.
	paused  bool
	pos     float64
	entry   int
	opened  int
	seeking bool
	// script: watchparty.lua; listening: the node has connected, so it hears what the script
	// says; pending: what the script said in this step, written after mpv's line
	script    *script
	listening bool
	pending   []string
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
	n := &node{t: t, in: make(chan IncomingMessage, 64), out: make(chan []byte, 64), typed: make(chan string, 8), picks: make(chan string, 8), writes: make(chan string, 64), refuse: map[string]string{}, paused: true, entry: s.start, opened: s.start, late: s.late}
	if !s.noScript {
		n.script = newScript(n)
	}
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
	observes := len(observed)
	go n.mpv(theirs, observes, s)
	if !s.joiner {
		n.in <- IncomingMessage{Opened: true}
	}
	for _, m := range s.heard {
		n.in <- m
	}
	c, err := New(ctx, cancel, ours, Config{Outgoing: n.out, Incoming: n.in, Chat: n.typed, React: n.picks, Names: s.names, Start: s.start, At: s.at, Heard: func(m Said) {
		n.mu.Lock()
		defer n.mu.Unlock()
		if m.Ours {
			m.Who += " (ours)"
		}
		if m.React != "" {
			n.chats = append(n.chats, m.Who+" reacts "+m.React)
		} else {
			n.chats = append(n.chats, m.Who+": "+m.Text)
		}
	}})
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
// as mpv does on an observe. It reports nothing it is set until mpvObeys. A script message
// reaches the script, which mpv answers first.
func (n *node) mpv(conn net.Conn, observes int, s setup) {
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		n.batch.Lock()
		req := Request{}
		json.Unmarshal(sc.Bytes(), &req)
		name := fmt.Sprint(req.Command[0])
		said := strings.TrimSpace(fmt.Sprintln(req.Command...))
		reply := fmt.Sprintf(`{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
		if name == "osd-overlay" {
			// as mpv answers it: data is an object, not a string
			reply = fmt.Sprintf(`{"data":{},"request_id":%d,"error":"success"}`+"\n", req.RequestId)
		}
		arg := func(i int) string { return fmt.Sprint(req.Command[i]) }
		n.mu.Lock()
		n.listening = true
		if n.script != nil && (name == "script-message-to" && arg(1) == scriptName || name == "script-message") {
			n.pending = append(n.pending, reply)
			args := make([]string, 0, len(req.Command))
			for i := range req.Command {
				args = append(args, arg(i))
			}
			if name == "script-message-to" {
				args = args[1:]
			}
			if args[1] == "watchparty-set" {
				n.goes(args[2:])
			} else {
				n.script.message(args[1], args[2:]...)
			}
			lines, quit := n.pending, n.quit
			n.pending = nil
			n.mu.Unlock()
			if quit {
				n.batch.Unlock()
				conn.Close()
				return
			}
			for _, line := range lines {
				n.writes <- line
			}
			n.batch.Unlock()
			continue
		}
		if n.quitOn != "" && strings.HasPrefix(said, n.quitOn) {
			n.told = append(n.told, said)
			n.mu.Unlock()
			n.batch.Unlock()
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
		if name == "script-message-to" && s.noScript {
			reply = fmt.Sprintf(`{"request_id":%d,"error":"error running command"}`+"\n", req.RequestId)
		}
		taken := !refused
		if name != "observe_property_string" && !(name == "script-message-to" && arg(1) == scriptName) {
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
		if name == "observe_property_string" {
			if observes--; observes == 0 {
				n.opens(s)
			}
		}
		n.batch.Unlock()
	}
	scanned(sc)
}

// goes hands the script a set, with the person's keys around it.
func (n *node) goes(args []string) {
	if n.first {
		n.first, n.paused = false, !n.paused
		n.script.pauseMarked()
	}
	if n.seekTo != 0 {
		n.pos, n.seekTo = n.seekTo, 0
		n.script.event(seek, nil)
	}
	if n.moveTo != 0 {
		n.opensLocked(n.moveTo)
		n.moveTo = 0
	}
	if n.ranOutTo != 0 {
		n.script.event("end-file", map[string]lua.LValue{"reason": lua.LString("eof")})
		n.opensLocked(n.ranOutTo)
		n.ranOutTo = 0
	}
	n.script.message("watchparty-set", args...)
	if n.undo {
		// the person's key, right after the set
		n.undo, n.paused = false, !n.paused
		n.script.pauseMarked()
	}
}

// opensLocked is mpv opening entry, by a key: it starts at 0, and the script hears it.
func (n *node) opensLocked(entry int) {
	n.entry, n.opened, n.pos, n.seeking = entry, entry, 0, false
	n.script.event("start-file", map[string]lua.LValue{"playlist_entry_id": lua.LNumber(entry + 1)})
}

// get is a property as the script reads it (scriptMpv).
func (n *node) get(name string) (string, bool) {
	switch name {
	case pause:
		return holds(n.paused), true
	case timePos:
		return fmt.Sprintf("%f", n.pos), true
	case playlistPos:
		return strconv.Itoa(n.entry), true
	}
	return "", false
}

// set is the script setting a property: told as a set, taken unless mpv refuses it, and
// reported once mpvObeys. A set to what mpv holds changes nothing, and isn't told.
func (n *node) set(name, value string) {
	var v any = value
	switch name {
	case pause:
		v = value == "yes"
		if v == n.paused {
			return
		}
	case playlistPos:
		e, _ := strconv.Atoi(value)
		if e == n.entry {
			return
		}
		v = e
	}
	if !n.step(name, v) {
		return
	}
	switch name {
	case pause:
		n.paused = v.(bool)
	case playlistPos:
		n.entry = v.(int)
	}
}

// step is a step of the script told mpv: false if mpv refused it or hung up on it.
func (n *node) step(name string, v any) bool {
	said := strings.TrimSpace(fmt.Sprintln(setProperty, name, v))
	n.told = append(n.told, said)
	if n.quitOn != "" && strings.HasPrefix(said, n.quitOn) {
		n.quit = true
		return false
	}
	for prefix := range n.refuse {
		if strings.HasPrefix(said, prefix) {
			return false
		}
	}
	n.sets = append(n.sets, []any{name, v})
	return true
}

// seek is the script's async seek, told as a set of time-pos: answered at once, or late once
// mpv reports it, or refused. The person's key opening another file before it (moveDuring)
// leaves it no file to seek in.
func (n *node) seek(second string, reply int) {
	if n.moveDuring != 0 {
		n.opensLocked(n.moveDuring)
		n.moveDuring = 0
	}
	if !n.step(timePos, second) {
		if !n.quit {
			n.script.replied(reply, false)
		}
		return
	}
	if n.late {
		n.replies = append(n.replies, reply)
		return
	}
	n.script.replied(reply, true)
}

// say is what the script says, written after the line that made it; late, from an ack on, it
// waits until mpv has reported what the set did.
func (n *node) say(args []string) {
	if !n.listening {
		return
	}
	b, _ := json.Marshal(map[string]any{"event": "client-message", "args": args})
	line := string(b) + "\n"
	if n.late && (len(n.saying) > 0 || len(args) > 1 && args[1] == "ack") {
		n.saying = append(n.saying, line)
		return
	}
	n.pending = append(n.pending, line)
}

// opens is mpv's answer to the observes, for the file it opened.
func (n *node) opens(s setup) {
	for _, line := range s.first {
		n.writes <- line
	}
	if s.opening != nil {
		n.writeLines(s.opening...)
		return
	}
	if s.names != nil {
		n.writeLines(started(s.start), `{"event":"file-loaded"}`, propertyChangeLine(playlistPos, strconv.Itoa(s.start), 0))
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

// write is mpv saying lines as one batch: the node's requests wait until all are out.
func (n *node) write(lines ...string) {
	n.batch.Lock()
	defer n.batch.Unlock()
	n.writeLines(lines...)
}

// writeLines is mpv saying each line, and the script hearing the same event: a pause line is the
// person's key, which the node hears from the script alone; a time-pos while stopped with no
// seek event is the person's seek, as tests say it. The caller holds batch.
func (n *node) writeLines(lines ...string) {
	for _, line := range lines {
		if !strings.HasSuffix(line, "\n") {
			line += "\n"
		}
		e := Event{}
		json.Unmarshal([]byte(line), &e)
		var end struct {
			Reason string
			// the playlist entry a start-file opens, counted from 1
			Entry int `json:"playlist_entry_id"`
		}
		json.Unmarshal([]byte(line), &end)
		n.mu.Lock()
		if n.script == nil {
			n.mu.Unlock()
			n.writes <- line
			continue
		}
		if e.EventType == "seek" {
			// the replies to the script's seeks come before the seek events they cause, and
			// with them what the script said from the set's ack on
			n.answerSeeksLocked()
			n.pending, n.saying = append(n.pending, n.saying...), nil
		}
		if !(e.Name == pause && e.Data != "") {
			n.pending = append(n.pending, line)
		}
		switch {
		case e.EventType == "seek":
			n.seeking = true
		case e.EventType == "playback-restart":
			n.seekHeardLocked()
			n.script.event("playback-restart", nil)
		case e.EventType == "start-file":
			n.answerSeeksLocked()
			n.opensLocked(end.Entry - 1)
		case e.EventType == "end-file":
			n.script.event("end-file", map[string]lua.LValue{"reason": lua.LString(end.Reason)})
		case e.EventType == "file-loaded":
			n.script.event("file-loaded", nil)
		case e.Name == timePos && e.Data != "":
			pos, _ := strconv.ParseFloat(e.Data, 64)
			moved := pos != n.pos
			n.pos = pos
			if n.seeking || n.paused && moved {
				n.seeking = true
				n.seekHeardLocked()
			}
		case e.Name == pause && e.Data != "":
			n.paused = e.paused()
			n.script.pauseMarked()
		case e.Name == playlistPos && e.Data != "":
			entry, _ := strconv.Atoi(e.Data)
			if entry != n.entry {
				n.entry, n.pos = entry, 0
			}
			if entry >= 0 && entry != n.opened {
				// a test's report of a move with no start-file: mpv opened that entry
				n.opensLocked(entry)
			}
		}
		lines := n.pending
		n.pending = nil
		n.mu.Unlock()
		for _, l := range lines {
			n.writes <- l
		}
	}
}

// seekHeardLocked is the script hearing the seek event mpv wrote, once its second is known:
// it reads where the seek goes.
func (n *node) seekHeardLocked() {
	if n.seeking {
		n.seeking = false
		n.script.event(seek, nil)
	}
}

// answerSeeksLocked is mpv answering the script's seeks it holds.
func (n *node) answerSeeksLocked() {
	replies := n.replies
	n.replies = nil
	for _, r := range replies {
		n.script.replied(r, true)
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
		waiting := len(n.saying) > 0 || len(n.replies) > 0
		n.mu.Unlock()
		if len(sets) == 0 && len(held) == 0 && !waiting {
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
		n.answerSeeksLocked()
		lines := append(n.pending, n.saying...)
		n.saying, n.pending = nil, nil
		n.mu.Unlock()
		for _, l := range lines {
			n.writes <- l
		}
	}
}

// reports is what mpv says once it has taken a set: a pause's change mark, which the script
// hears; a seek with its events; a move opening the entry at 0, and its first frame ready.
func (n *node) reports(set []any) []string {
	value := fmt.Sprint(set[1])
	switch set[0] {
	case pause:
		n.mu.Lock()
		n.script.pauseMarked()
		lines := n.pending
		n.pending = nil
		n.mu.Unlock()
		return lines
	case timePos:
		pos, _ := strconv.ParseFloat(value, 64)
		return []string{`{"event":"seek"}`, mpvAt(pos), `{"event":"playback-restart"}`}
	case playlistPos:
		i, _ := strconv.Atoi(value)
		return []string{started(i), propertyChangeLine(playlistPos, value, 0), `{"event":"file-loaded"}`, mpvAt(0), `{"event":"playback-restart"}`}
	}
	return nil
}

// mpvAnswers is mpv answering every set it holds, reporting nothing, until none is left, and
// the script acking every set.
func (n *node) mpvAnswers() {
	n.t.Helper()
	for {
		synctest.Wait()
		n.mu.Lock()
		n.answerSeeksLocked()
		held := append(n.held, n.saying...)
		held = append(held, n.pending...)
		n.held, n.sets, n.saying, n.pending = nil, nil, nil, nil
		n.mu.Unlock()
		if len(held) == 0 {
			return
		}
		for _, reply := range held {
			n.writes <- reply
		}
	}
}

// personPressesFirst is the person's space running just before the next set, which the node
// hasn't read when it sends it: the set undoes it, and its ack says so.
func (n *node) personPressesFirst() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.first = true
}

// personSeeksFirst is the person's seek to second running just before the next set, which the
// node hasn't read when it sends it: the script says it, and the set's ack says it came first.
func (n *node) personSeeksFirst(second float64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.seekTo = second
}

// personMovesFirst is the person's key opening entry (not the first) just before the next set,
// which the node hasn't read when it sends it.
func (n *node) personMovesFirst(entry int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.moveTo = entry
}

// mpvRunsOutFirst is mpv reaching the file's end and opening entry by itself just before the
// next set, which the node hasn't read when it sends it.
func (n *node) mpvRunsOutFirst(entry int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ranOutTo = entry
}

// personMovesDuring is the person's key opening entry once the next set has begun, so mpv
// refuses the set's seek: there is no file to seek in.
func (n *node) personMovesDuring(entry int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.moveDuring = entry
}

// personUndoesThePause is the person's key undoing the pause the next set leaves, right after
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
// for a property it can't set while it quits or between files. The script's seek, refused,
// says so; a set it makes, refused, it doesn't read.
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

// personTypes is a chat message typed in this node's GUI.
func (n *node) personTypes(text string) {
	n.t.Helper()
	n.typed <- text
	synctest.Wait()
}

// personPicks is a reaction picked in this node's GUI.
func (n *node) personPicks(code string) {
	n.t.Helper()
	n.picks <- code
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

// heardChats is every chat message and reaction the node handed on since the last call, as
// "who: text" or "who reacts code".
func (n *node) heardChats() []string {
	n.t.Helper()
	synctest.Wait()
	n.mu.Lock()
	defer n.mu.Unlock()
	chats := n.chats
	n.chats = nil
	return chats
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
