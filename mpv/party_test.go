package mpv

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	lua "github.com/yuin/gopher-lua"
)

// A whole party in one bubble: real Clients, a small model of mpv under each, and a
// network that a seed reorders. After each step everything has run as far as it can, so a
// failing seed fails again. Each sender's messages stay in order, as on a connection; what
// varies is who hears what first, when mpv reports a seek, and in a second pass when mpv
// answers. Checks are what the protocol guarantees, for every order: that is what e2e can
// only sample.
type simNode struct {
	name   string
	c      *Client
	out    chan []byte
	in     chan IncomingMessage
	writes chan string

	mu      sync.Mutex
	paused  bool
	pos     float64
	speed   string
	entry   int
	notices []string
	// seeks: queued in mpv's one slot, the node's (the script's) and the person's alike
	seeks []string
	moves []int
	// reports: what mpv holds back until its event queue is empty (client.h), so a move's
	// report can come after its file-loaded
	reports []string
	// late: mpv answers a request when the seed picks (answers), with events before it;
	// answer is the one it holds, and it reads the next request only once that is out. The
	// script's set is answered at once, and the script runs it when the seed picks (act)
	late     bool
	answer   string
	act      func()
	answered chan struct{}
	// script: watchparty.lua, which hears mpv's events as mpv has them; listening: the node
	// hears what it says, which it doesn't before the sim runs (run)
	script    *script
	listening bool
}

type sim struct {
	t     *testing.T
	rng   *rand.Rand
	nodes []*simNode
	links map[[2]string][]IncomingMessage
	down  map[string]bool
	lost  map[[2]string]func(Event) bool
	// missed: links a send failed on, until one gets through; told: what each node's
	// transport tells it about them, in order
	missed map[[2]string]bool
	told   map[*simNode][]IncomingMessage
	// held: links whose messages wait, as for a process that stopped; replies: states a
	// node handed its transport to send again, to one member
	held    map[[2]string]bool
	replies []reply
	// late: how many of a link's messages were sent while it was held, whose sender gave up
	// on them: delivered, they end no chase
	late   map[[2]string]int
	ticks  int
	script []func()
	// newest: the highest stamp each node has sent
	newest map[string]int
}

const simEntries = 3

func newSim(t *testing.T, seed uint64, entries int, names ...string) *sim {
	s := &sim{newest: map[string]int{}, t: t, rng: rand.New(rand.NewPCG(seed, 0)), links: map[[2]string][]IncomingMessage{}, down: map[string]bool{}, lost: map[[2]string]func(Event) bool{}, missed: map[[2]string]bool{}, told: map[*simNode][]IncomingMessage{}, held: map[[2]string]bool{}, late: map[[2]string]int{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, name := range names {
		ours, theirs := net.Pipe()
		n := &simNode{name: name, out: make(chan []byte, 1024), in: make(chan IncomingMessage, 1024), writes: make(chan string, 1024), paused: true, speed: seconds(1),
			late: strings.HasSuffix(t.Name(), "/"+lateRun), answered: make(chan struct{})}
		n.script = newScript(n)
		n.c = newTestClient(n.out, bufio.NewScanner(ours))
		n.c.ctx, n.c.conn.conn, n.c.paused, n.c.names = ctx, ours, true, slices.Repeat([]string{"e"}, entries)
		n.c.seen[pause], n.c.seen[timePos], n.c.seen[playlistPos] = true, true, true
		t.Cleanup(func() { ours.Close(); theirs.Close(); close(n.c.conn.closed) })
		go n.model(ctx, theirs)
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
		n.c.incoming = n.in
		go n.c.watch()
		s.nodes = append(s.nodes, n)
	}
	return s
}

// model answers requests the way mpv does (MPV.md, ORDER.md), and hands a script message to
// the script (watchparty.lua). A seek is queued and lands later, ahead of a pause change right
// behind it; seeks fold into the last, which reports no second if mpv was already on it. A set
// of playlist-pos replies first and reports later, so the next set can overtake the report,
// and two close together can report only the last (MPV.md). Late, events can come before an
// answer, as MPV.md allows; never a later request's, as mpv's IPC thread runs one request at a
// time and answers it before it reads the next.
func (n *simNode) model(ctx context.Context, conn net.Conn) {
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		req := Request{}
		json.Unmarshal(sc.Bytes(), &req)
		name := fmt.Sprint(req.Command[0])
		reply := fmt.Sprintf(`{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
		replied := false
		n.mu.Lock()
		switch {
		case name == showText:
			n.notices = append(n.notices, fmt.Sprint(req.Command[1]))
		case name == "script-message-to":
			// mpv answers once the script has the message; the script acts on its own
			n.writes <- reply
			replied = true
			args := make([]string, len(req.Command)-3)
			for i := range args {
				args[i] = fmt.Sprint(req.Command[i+3])
			}
			message := fmt.Sprint(req.Command[2])
			act := func() { n.script.message(message, args...) }
			if n.late {
				n.act = act
			} else {
				act()
			}
		case name == setProperty && req.Command[1] == playlistPos:
			i, _ := strconv.Atoi(fmt.Sprint(req.Command[2]))
			n.request(i)
		}
		if n.late {
			if !replied {
				n.answer = reply
			}
			n.mu.Unlock()
			if replied && n.act == nil {
				continue
			}
			select {
			case <-n.answered:
			case <-ctx.Done():
				return
			}
			continue
		}
		n.mu.Unlock()
		if replied {
			continue
		}
		select {
		case n.writes <- reply:
		case <-ctx.Done():
			return
		}
	}
	scanned(sc)
}

// answers is mpv sending the answer it held, or the script running what it was told; then
// mpv reads the next request.
func (n *simNode) answers() {
	n.mu.Lock()
	if n.act != nil {
		n.act()
		n.act = nil
	} else {
		n.writes <- n.answer
		n.answer = ""
	}
	n.mu.Unlock()
	n.answered <- struct{}{}
}

// get is a property as the script reads it (scriptMpv): playlist-pos is the entry mpv was last
// set to or a key took it to, which it opens next.
func (n *simNode) get(name string) (string, bool) {
	switch name {
	case pause:
		return holds(n.paused), true
	case timePos:
		return fmt.Sprintf("%f", n.pos), true
	case playlistPos:
		return strconv.Itoa(n.heading()), true
	case playSpeed:
		return n.speed, true
	}
	return "", false
}

// set is the script setting a property, as mpv takes it: a pause at once, a move in the
// queue of moves. Its pause's change mark reaches the script once what it hears now is done,
// or, late, at once.
func (n *simNode) set(name, value string) {
	switch name {
	case pause:
		n.setPause(value == "yes", !n.late)
	case playlistPos:
		i, _ := strconv.Atoi(value)
		if i != n.heading() {
			n.request(i)
		}
	case playSpeed:
		n.setSpeed(value, !n.late)
	}
}

// setSpeed is the person's key or the script's set: the script hears its change mark, at once
// or (later) once it is idle.
func (n *simNode) setSpeed(speed string, later bool) {
	if n.speed == speed {
		return
	}
	n.speed = speed
	if later {
		n.script.whenIdle(func() { n.script.changeMarked(playSpeed) })
		return
	}
	n.script.changeMarked(playSpeed)
}

// seek is the script's async seek: queued in mpv's slot, and answered at once.
func (n *simNode) seek(second string, reply int) {
	n.seeks = append(n.seeks, second)
	n.script.replied(reply, true)
}

// say is what the script says, after what mpv holds back.
func (n *simNode) say(args []string) {
	if !n.listening {
		return
	}
	b, _ := json.Marshal(map[string]any{"event": "client-message", "args": args})
	n.emit(string(b) + "\n")
}

// heading is the entry mpv opens next: the last it was set or moved to, else the one it is on.
func (n *simNode) heading() int {
	if len(n.moves) > 0 {
		return n.moves[len(n.moves)-1]
	}
	return n.entry
}

func (n *simNode) emit(line string) {
	n.landReportsLocked()
	n.writes <- line
}

func (n *simNode) landReports() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.landReportsLocked()
}

func (n *simNode) landReportsLocked() {
	for _, r := range n.reports {
		n.writes <- r
	}
	n.reports = nil
}

// setPause is the person's key or the script's set: a change lands the seek queued first, and
// the script hears its change mark, at once or (later) once it is idle.
func (n *simNode) setPause(paused, later bool) {
	if n.paused == paused {
		return
	}
	n.landSeekLocked()
	n.paused = paused
	if later {
		n.script.whenIdle(n.script.pauseMarked)
		return
	}
	n.script.pauseMarked()
}

// fileLoaded is mpv's file-loaded, with reports mpv still holds back after it, and the first
// frame ready.
func (n *simNode) fileLoaded(reports ...string) {
	n.emit(`{"event":"file-loaded"}` + "\n")
	n.script.event("file-loaded", nil)
	n.reports = append(n.reports, reports...)
	n.emit(`{"event":"playback-restart"}` + "\n")
	n.script.event("playback-restart", nil)
}

// queueSeek is the person's seek, queued in mpv's slot.
func (n *simNode) queueSeek(pos float64) {
	n.seeks = append(n.seeks, fmt.Sprintf("%f", pos))
}

func (n *simNode) landSeek() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.landSeekLocked()
}

// landSeekLocked is mpv running the seek in its slot: the script hears where it goes at its
// seek event, with the node's id if it carries the node's (watchparty.lua).
func (n *simNode) landSeekLocked() {
	if len(n.seeks) == 0 {
		return
	}
	last := n.seeks[len(n.seeks)-1]
	n.seeks = nil
	was := fmt.Sprintf("%f", n.pos)
	n.pos, _ = strconv.ParseFloat(last, 64)
	n.emit(`{"event":"seek"}` + "\n")
	n.script.event(seek, nil)
	if last != was {
		n.emit(propertyChangeLine(timePos, last, 0))
	}
	n.emit(`{"event":"playback-restart"}` + "\n")
	n.script.event("playback-restart", nil)
}

// request is a set or a key press: mpv takes them in the order they came.
func (n *simNode) request(i int) {
	if len(n.moves) > 0 || i != n.entry {
		n.moves = append(n.moves, i)
	}
}

func (n *simNode) landMove() { n.landMoveReporting(false) }

// landMoveLate is mpv's queue still holding file-loaded when it would report the entry.
func (n *simNode) landMoveLate() { n.landMoveReporting(true) }

func (n *simNode) landMoveReporting(late bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	i := n.moves[0]
	n.moves = n.moves[1:]
	n.move(i, late)
}

// foldMoves is mpv handling them too close together to report any but the last. Back
// where it started, it reports nothing at all and still loads the file.
func (n *simNode) foldMoves() {
	n.mu.Lock()
	defer n.mu.Unlock()
	i := n.moves[len(n.moves)-1]
	n.moves = nil
	if i == n.entry {
		n.starts(i)
		n.fileLoaded()
		return
	}
	n.move(i, false)
}

func started(i int) string {
	return fmt.Sprintf(`{"event":"start-file","playlist_entry_id":%d}`+"\n", i+1)
}

// starts is mpv's start-file for entry i, which the script hears.
func (n *simNode) starts(i int) {
	n.emit(started(i))
	n.script.event("start-file", map[string]lua.LValue{"playlist_entry_id": lua.LNumber(i + 1)})
}

// move drops a seek that hasn't landed: mpv reports one sent just before a switch ahead
// of it or not at all, never on the new file, which always opens at 0 (MPV.md).
func (n *simNode) move(i int, late bool) {
	if i == n.entry {
		return
	}
	n.entry, n.pos, n.seeks = i, 0, nil
	n.starts(i)
	reports := []string{propertyChangeLine(playlistPos, strconv.Itoa(i), 0), propertyChangeLine(timePos, "0.000000", 0)}
	if late {
		n.fileLoaded(reports...)
		return
	}
	n.emit(reports[0])
	n.fileLoaded()
	n.emit(reports[1])
}

// run goes until nothing is left to happen, picking at random among what could happen next.
func (s *sim) run() {
	for _, n := range s.nodes {
		n.mu.Lock()
		if !n.listening {
			// mpv opened its file before the party: the script heard it, the node didn't
			n.script.event("start-file", map[string]lua.LValue{"playlist_entry_id": lua.LNumber(n.entry + 1)})
			n.listening = true
		}
		n.mu.Unlock()
	}
	for {
		synctest.Wait()
		for i := 0; i < len(s.replies); i++ {
			r := s.replies[i]
			select {
			case b := <-r.ch:
				if b != nil && !s.down[r.k[0]] && !s.down[r.k[1]] && !s.loses(r.k, b) {
					s.links[r.k] = append(s.links[r.k], IncomingMessage{HostName: r.k[0], From: r.k[0], Event: b})
				}
				s.replies = slices.Delete(s.replies, i, i+1)
				i--
			default:
			}
		}
		for _, from := range s.nodes {
			for len(from.out) > 0 {
				b := <-from.out
				// a node never says a state older than one it said: what it sends after a change
				// of its own says that change. A join starts it over from unheard.
				e := struct {
					Stamp *int
					Name  string
				}{}
				json.Unmarshal(b, &e)
				if e.Name == joined {
					delete(s.newest, from.name)
				}
				newest, ok := s.newest[from.name]
				if e.Stamp != nil && ok && *e.Stamp < newest {
					s.t.Errorf("%s sent stamp %d after %d: %s", from.name, *e.Stamp, newest, b)
				}
				if e.Stamp != nil && (!ok || *e.Stamp > newest) {
					s.newest[from.name] = *e.Stamp
				}
				for _, to := range s.nodes {
					// a node without its relay connection sends and hears nothing
					k := [2]string{from.name, to.name}
					if to == from || s.down[from.name] || s.down[to.name] {
						continue
					}
					if !s.loses(k, b) {
						s.links[k] = append(s.links[k], IncomingMessage{HostName: from.name, From: from.name, Event: b})
						if s.held[k] {
							s.late[k]++
						}
					} else if !s.missed[k] {
						s.missed[k] = true
						s.tell(from, IncomingMessage{HostName: to.name, From: to.name, Missed: true})
					}
				}
			}
		}
		var next []func()
		for _, from := range s.nodes {
			for _, to := range s.nodes {
				if k := [2]string{from.name, to.name}; len(s.links[k]) > 0 && !s.held[k] {
					next = append(next, func() {
						to.in <- s.links[k][0]
						s.links[k] = s.links[k][1:]
						// a send got through: the transport's chase ends
						if s.late[k] > 0 {
							s.late[k]--
						} else {
							delete(s.missed, k)
						}
					})
				}
			}
			if len(s.told[from]) > 0 {
				next = append(next, func() {
					from.in <- s.told[from][0]
					s.told[from] = s.told[from][1:]
				})
			}
			from.mu.Lock()
			if len(from.seeks) > 0 {
				next = append(next, from.landSeek)
			}
			if len(from.moves) > 0 {
				next = append(next, from.landMove, from.landMoveLate)
			}
			if len(from.reports) > 0 {
				next = append(next, from.landReports)
			}
			if from.answer != "" || from.act != nil {
				next = append(next, from.answers)
			}
			if len(from.moves) > 1 {
				next = append(next, from.foldMoves)
			}
			from.mu.Unlock()
		}
		if s.ticks > 0 {
			next = append(next, func() {
				s.ticks--
				for _, n := range s.nodes {
					n.mu.Lock()
					if !n.paused {
						n.pos += 0.1
						n.emit(propertyChangeLine(timePos, fmt.Sprintf("%f", n.pos), 0))
					}
					n.mu.Unlock()
				}
			})
		}
		if len(s.script) > 0 {
			next = append(next, func() {
				s.script[0]()
				s.script = s.script[1:]
			})
		}
		if len(next) == 0 {
			return
		}
		next[s.rng.IntN(len(next))]()
	}
}

// loses is a send that failed, as one can in a mesh: the rest of what that node sends
// still arrives.
func (s *sim) loses(k [2]string, b []byte) bool {
	e := Event{}
	json.Unmarshal(b, &e)
	return s.lost[k] != nil && s.lost[k](e)
}

func (s *sim) tell(n *simNode, m IncomingMessage) {
	s.told[n] = append(s.told[n], m)
}

type reply struct {
	k  [2]string
	ch chan []byte
}

// chase is the transport asking every node for its state again, for each member it
// missed, as it does every few seconds until a send gets through, and sending it to that
// member alone.
func (s *sim) chase() {
	for k := range s.missed {
		for _, n := range s.nodes {
			if n.name == k[0] {
				ch := make(chan []byte, 1)
				s.replies = append(s.replies, reply{k, ch})
				s.tell(n, IncomingMessage{HostName: k[1], From: k[1], Resend: true, Reply: ch})
			}
		}
	}
}

// drop loses every message named name from one node to another from now on.
func (s *sim) drop(from, to *simNode, name string) {
	s.lost[[2]string{from.name, to.name}] = func(e Event) bool { return e.Name == name }
}

// seeks is the person at n seeking by hand.
func seeks(n *simNode, pos float64) func() {
	return func() {
		n.mu.Lock()
		n.queueSeek(pos)
		n.mu.Unlock()
		n.landSeek()
	}
}

// moves is the person at n moving to another entry by hand, and mpv doing it at once.
func moves(n *simNode, i int) func() {
	return func() {
		n.mu.Lock()
		n.request(i)
		n.mu.Unlock()
		n.landMove()
	}
}

// speeds is the person at n changing mpv's speed.
func speeds(n *simNode, speed float64) func() {
	return func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.setSpeed(seconds(speed), false)
	}
}

// sameSpeed is every node holding one party speed, and its mpv playing at it.
func (s *sim) sameSpeed(seed uint64) {
	s.t.Helper()
	want := s.nodes[0].c.speed
	for _, n := range s.nodes {
		n.mu.Lock()
		if n.c.speed != want || n.speed != seconds(want) {
			s.t.Errorf("seed %d: %s holds %g, its mpv %s; %s holds %g", seed, n.name, n.c.speed, n.speed, s.nodes[0].name, want)
		}
		n.mu.Unlock()
	}
}

func sets(n *simNode, paused bool) func() {
	return func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.setPause(paused, false)
	}
}

// inStep is everyone stopped on the same second, or everyone playing from it: then each
// started when the resume reached it, at most lag seconds after the first. Each stops within
// timePosTolerance of the party's second, so two can be twice that apart (SIM_SEED=210240).
func (s *sim) inStep(seed uint64, lag float64) {
	s.t.Helper()
	apart := 2*timePosTolerance + 0.05
	if !s.nodes[0].paused {
		apart += lag
	}
	first := s.nodes[0].second()
	for _, n := range s.nodes {
		if d := n.second() - first; n.paused != s.nodes[0].paused || d > apart || d < -apart {
			s.t.Errorf("seed %d: %s is at %.2f (paused %v), %s at %.2f (paused %v)", seed, n.name, n.second(), n.paused, s.nodes[0].name, first, s.nodes[0].paused)
		}
	}
}

// playingOn is everyone still playing, on the same second, and told nothing that stops them.
func (s *sim) playingOn(seed uint64) {
	s.t.Helper()
	for _, n := range s.nodes {
		if n.paused {
			s.t.Errorf("seed %d: %s stopped: %q", seed, n.name, n.notices)
		}
		s.toldNothing(seed, n)
	}
	s.inStep(seed, 0)
}

// toldNothing is a node that never said the party stopped for someone.
func (s *sim) toldNothing(seed uint64, n *simNode) {
	s.t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, text := range n.notices {
		if strings.HasPrefix(text, "paused") {
			s.t.Errorf("seed %d: %s said %q", seed, n.name, text)
		}
	}
}

func (n *simNode) said(text string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Contains(n.notices, text)
}

// play starts these nodes at pos as if someone had opened the party and resumed long ago.
func play(pos float64, nodes ...*simNode) {
	for _, n := range nodes {
		n.paused, n.pos = false, pos
		n.c.paused, n.c.pos, n.c.reported, n.c.stamp = false, pos, pos, 1
		n.c.snapshot()
	}
}

func (s *sim) together(seed uint64) {
	s.t.Helper()
	for _, n := range s.nodes {
		if !n.paused {
			s.t.Errorf("seed %d: %s is playing", seed, n.name)
		}
		if d := n.second() - s.nodes[0].second(); d > 0.15 || d < -0.15 {
			s.t.Errorf("seed %d: %s is at %.2f, %s at %.2f", seed, n.name, n.second(), s.nodes[0].name, s.nodes[0].second())
		}
	}
}

// second is where n is: playing, its mpv's second; stopped, the party's second it holds
// (Position), as a follower's screen stays wherever the person took it until the party resumes.
func (n *simNode) second() float64 {
	if !n.paused {
		return n.pos
	}
	_, at := n.c.Position()
	return at
}

func forSeeds(t *testing.T, f func(t *testing.T, seed uint64)) {
	forManySeeds(t, 200, f)
}

// forManySeeds runs seeds 0 to seeds-1, which keep the orders that once failed, or from
// SIM_SEED on when it is set: CI sets a new one each run, so every run tries orders no run
// tried before. A seed always picks the same orders, so -count adds nothing: -short skips
// the sweep, to stress the other tests. Then the same seeds again with mpv answering when
// the seed picks (lateRun); a pass of its own, so the first keeps the orders seeds were
// named for.
func forManySeeds(t *testing.T, seeds uint64, f func(t *testing.T, seed uint64)) {
	t.Parallel()
	if testing.Short() {
		t.Skip("a seed sweep, skipped by -short")
	}
	base := uint64(0)
	if v := os.Getenv("SIM_SEED"); v != "" {
		var err error
		if base, err = strconv.ParseUint(v, 10, 64); err != nil {
			t.Fatalf("SIM_SEED=%q: %v", v, err)
		}
	}
	sweep := func(t *testing.T) {
		run := "^" + strings.ReplaceAll(regexp.QuoteMeta(t.Name()), "/", "$/^") + "$"
		for seed := base; seed < base+seeds; seed++ {
			// a failure inside the bubble ends this test too, so it says how to replay from there
			synctest.Test(t, func(t *testing.T) {
				t.Cleanup(func() {
					if t.Failed() {
						t.Logf("replay: SIM_SEED=%d go test -race -run '%s' ./mpv", seed, run)
					}
				})
				f(t, seed)
			})
		}
	}
	sweep(t)
	t.Run(lateRun, sweep)
}

const lateRun = "mpv_answers_late"

// Members stop a moment apart and hear each other in any order; seeds 391 and 474 were
// the first to strand one 0.2s away, so these run 1000.
func TestPartySettlesAfterAJoin(t *testing.T) {
	forManySeeds(t, 1000, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, 0, "a", "b", "c", "j")
		a, b, c, j := s.nodes[0], s.nodes[1], s.nodes[2], s.nodes[3]
		play(30, a, b, c)
		joins(j)
		s.ticks = 20
		j.out <- []byte(`{"name":"joined"}`)
		s.run()
		s.together(seed)
		for _, n := range []*simNode{a, b, c} {
			if !n.said("paused for j: joining") {
				t.Errorf("seed %d: %s never said why: %q", seed, n.name, n.notices)
			}
		}
	})
}

func TestPartySettlesAfterALeave(t *testing.T) {
	forManySeeds(t, 1000, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, 0, "a", "b", "c")
		play(30, s.nodes...)
		s.ticks = 20
		for _, n := range s.nodes {
			s.script = append(s.script, func() { n.in <- peer("x", `{"name":"show-text","data":"x has left"}`) })
		}
		s.run()
		s.together(seed)
		if !slices.ContainsFunc(s.nodes, func(n *simNode) bool { return n.said("paused for x: left") }) {
			t.Errorf("seed %d: nobody said why", seed)
		}
	})
}

// lose and back are a node's relay connection dropping and returning, as its relay
// client tells it.
func (s *sim) lose(n *simNode) func() {
	return func() {
		s.down[n.name] = true
		n.in <- IncomingMessage{Lost: true}
	}
}

func (s *sim) back(n *simNode) func() {
	return func() {
		s.down[n.name] = false
		n.in <- IncomingMessage{Back: true}
	}
}

// One member's connection drops while the party plays, and everyone plays on. The party
// moves and resumes meanwhile, which the member never hears: back, the state it says is
// older and not the party's, so the party stops, on whichever entry and second it is by
// then.
func TestPartySettlesAfterAMemberLosesTheRelay(t *testing.T) {
	forSeeds(t, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, simEntries, "a", "b", "c")
		a, b, c := s.nodes[0], s.nodes[1], s.nodes[2]
		play(30, s.nodes...)
		s.ticks = 20
		s.script = []func(){s.lose(c), func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.request(1)
		}, func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.setPause(false, false)
		}}
		s.run()
		// only once the move and the resume have reached everyone still there. c ends on the
		// party's entry either way, but not always on its second: a member that hasn't heard
		// of the move answers with a position for the old entry, and one that answers before
		// the resume reaches it is started again by it and stops later than the rest. The
		// relay's order prevents both; this network, which delivers each pair's messages
		// independently, doesn't.
		s.ticks = 20
		s.script = []func(){s.back(c)}
		s.run()
		s.together(seed)
		if !c.said(lostRelay) {
			t.Errorf("seed %d: c never said it lost the relay: %q", seed, c.notices)
		}
		if c.entry != a.entry || b.entry != a.entry {
			t.Errorf("seed %d: on entries %d, %d and %d", seed, a.entry, b.entry, c.entry)
		}
	})
}

// The relay itself goes away: everyone plays on, the key holder opens the room again, and
// the others come back to it. Nobody did anything meanwhile, so nobody stops.
func TestNobodyStopsWhenTheRelayRestarts(t *testing.T) {
	forSeeds(t, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, 0, "o", "b", "c")
		o, b, c := s.nodes[0], s.nodes[1], s.nodes[2]
		play(30, s.nodes...)
		s.ticks = 20
		s.script = []func(){s.lose(o), s.lose(b), s.lose(c), func() {
			s.down["o"] = false
			o.in <- IncomingMessage{Opened: true}
		}, s.back(b), s.back(c)}
		s.run()
		s.playingOn(seed)
		for _, n := range s.nodes {
			if !n.said(lostRelay) {
				t.Errorf("seed %d: %s never said it lost the relay: %q", seed, n.name, n.notices)
			}
		}
	})
}

// A member loses the relay and comes back while nothing changed, playing or stopped: its
// state is the party's, so nobody stops, and nobody is told anything.
func TestAMemberBackFromLosingTheRelayStopsNobody(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprint("paused ", paused), func(t *testing.T) {
			forManySeeds(t, 50, func(t *testing.T, seed uint64) {
				s := newSim(t, seed, simEntries, "a", "b", "c")
				if paused {
					stopped(30, s.nodes...)
				} else {
					play(30, s.nodes...)
				}
				s.ticks = 20
				s.script = []func(){s.lose(s.nodes[2]), s.back(s.nodes[2])}
				s.run()
				s.inStep(seed, 0)
				for _, n := range s.nodes {
					if n.paused != paused {
						t.Errorf("seed %d: %s paused %v, want %v: %q", seed, n.name, n.paused, paused, n.notices)
					}
					s.toldNothing(seed, n)
				}
			})
		})
	}
}

// The party stops and plays again while a member, which lost the relay, plays on. Both
// play, but that member is ahead by however long the party stood still: back, the party
// stops for it.
func TestAMemberThatMissedAStopWhileItLostTheRelayEndsInStep(t *testing.T) {
	forManySeeds(t, 50, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, 0, "a", "b", "c")
		a, c := s.nodes[0], s.nodes[2]
		play(30, s.nodes...)
		s.ticks = 20
		s.script = []func(){s.lose(c), sets(a, true), func() { s.ticks = 20 }, sets(a, false)}
		s.run()
		s.ticks = 20
		s.script = []func(){s.back(c)}
		s.run()
		s.together(seed)
	})
}

// A member does something while it has lost the relay, and nobody hears it. Back, the
// party takes a pause or a seek, since a stopped state is where it says; a resume it
// can't take, as nobody knows how far the member has played since: the party stops.
func TestWhatAMemberDidWhileItLostTheRelayEndsInStep(t *testing.T) {
	for name, tc := range map[string]struct {
		playing bool
		does    func(*simNode) func()
		at      float64
	}{
		"pauses":  {true, func(n *simNode) func() { return sets(n, true) }, -1},
		"seeks":   {false, func(n *simNode) func() { return seeks(n, 50) }, 50},
		"resumes": {false, func(n *simNode) func() { return sets(n, false) }, -1},
	} {
		t.Run(name, func(t *testing.T) {
			forManySeeds(t, 50, func(t *testing.T, seed uint64) {
				s := newSim(t, seed, 0, "a", "b", "c")
				c := s.nodes[2]
				if tc.playing {
					play(30, s.nodes...)
				} else {
					stopped(30, s.nodes...)
				}
				s.ticks = 20
				s.script = []func(){s.lose(c), tc.does(c), func() { s.ticks = 20 }, s.back(c)}
				s.run()
				s.together(seed)
				if tc.at >= 0 && c.pos != tc.at {
					t.Errorf("seed %d: the party stopped at %.2f, want where c went, %.2f", seed, c.pos, tc.at)
				}
			})
		})
	}
}

func TestTwoJoinersEndOnOneEntry(t *testing.T) {
	forSeeds(t, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, simEntries, "a", "b")
		a, b := s.nodes[0], s.nodes[1]
		a.entry, a.c.current = 0, 0
		b.entry, b.c.current = 2, 2
		joins(a)
		joins(b)
		a.out <- []byte(`{"name":"joined"}`)
		b.out <- []byte(`{"name":"joined"}`)
		s.run()
		if a.entry != b.entry {
			t.Errorf("seed %d: on entries %d and %d", seed, a.entry, b.entry)
		}
	})
}

func TestMovesCrossingAJoinEndOnOneEntry(t *testing.T) {
	forSeeds(t, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, simEntries, "a", "b")
		a, b := s.nodes[0], s.nodes[1]
		for _, n := range s.nodes {
			n.entry, n.c.current = 1, 1
			joins(n)
			n.out <- []byte(`{"name":"joined"}`)
		}
		hand := func(n *simNode, i int) func() {
			return func() {
				n.mu.Lock()
				defer n.mu.Unlock()
				n.request(i)
			}
		}
		s.script = []func(){hand(a, 0), hand(b, 2)}
		s.rng.Shuffle(2, func(i, j int) { s.script[i], s.script[j] = s.script[j], s.script[i] })
		s.run()
		if a.entry != b.entry {
			t.Errorf("seed %d: on entries %d and %d", seed, a.entry, b.entry)
		}
	})
}

// A member moves to another entry just as someone joins. Members answer the joiner in any
// order with the move, so the joiner can hear the move and then an answer sent before its
// sender had: it must not go to the entry the party just left, whichever entry it opened
// on. Sent there and on again while the first file loads, it must not take the position
// it was given for the first to the second. Each of those failed within the first 20
// orderings, about one in twenty; 600 is 200 for each entry it can open on.
func TestAJoinCrossingAMoveEndsOnThePartysEntry(t *testing.T) {
	forManySeeds(t, 600, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, simEntries, "a", "b", "j")
		a, b, j := s.nodes[0], s.nodes[1], s.nodes[2]
		play(30, a, b)
		joins(j)
		j.entry, j.c.current = int(seed%simEntries), int(seed%simEntries)
		s.ticks = 20
		s.script = []func(){func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.request(1)
		}, func() { j.out <- []byte(`{"name":"joined"}`) }}
		s.rng.Shuffle(2, func(i, j int) { s.script[i], s.script[j] = s.script[j], s.script[i] })
		s.run()
		if a.entry != 1 || b.entry != 1 || j.entry != 1 {
			t.Errorf("seed %d: on entries %d, %d and %d, want 1: j heard %q", seed, a.entry, b.entry, j.entry, j.notices)
		}
		s.together(seed)
	})
}

// A member moves and the joiner never hears it: it was not there yet. The other member
// can answer the joiner before it has heard of the move itself, so the joiner is told the
// old entry by one and the new one by the other, in either order. On this network each
// pair's messages travel independently, as in a mesh; a relay keeps each member's order.
func TestAJoinerThatMissedAMoveEndsOnThePartysEntry(t *testing.T) {
	forManySeeds(t, 100, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, simEntries, "a", "b", "j")
		a, b, j := s.nodes[0], s.nodes[1], s.nodes[2]
		play(30, a, b)
		joins(j)
		j.entry, j.c.current = int(seed%simEntries), int(seed%simEntries)
		s.down["j"] = true
		s.ticks = 20
		s.script = []func(){func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.request(1)
		}, func() {
			s.down["j"] = false
			j.out <- []byte(`{"name":"joined"}`)
		}}
		s.run()
		if a.entry != 1 || b.entry != 1 || j.entry != 1 {
			t.Errorf("seed %d: on entries %d, %d and %d, want 1: j heard %q", seed, a.entry, b.entry, j.entry, j.notices)
		}
		s.together(seed)
	})
}

func TestQuickMovesEndOnOneEntryAndLeaveNoMarker(t *testing.T) {
	for _, moves := range [][]int{{1, 2}, {1, 0}, {1, 2, 0}} {
		t.Run(fmt.Sprint(moves), func(t *testing.T) {
			forSeeds(t, func(t *testing.T, seed uint64) {
				s := newSim(t, seed, simEntries, "a", "b")
				a, b := s.nodes[0], s.nodes[1]
				for _, i := range moves {
					s.script = append(s.script, func() {
						a.mu.Lock()
						defer a.mu.Unlock()
						a.request(i)
					})
				}
				s.run()
				want := moves[len(moves)-1]
				if a.entry != want || b.entry != want {
					t.Errorf("seed %d: on entries %d and %d, want %d", seed, a.entry, b.entry, want)
				}
				// a marker left behind would swallow this as an echo, and nobody would follow.
				// b follows a's moves, so its own stays on its screen until the party plays.
				s.script = []func(){sets(a, false)}
				s.run()
				to := (want + 1) % simEntries
				s.script = []func(){func() {
					b.mu.Lock()
					defer b.mu.Unlock()
					b.request(to)
				}}
				s.run()
				if a.entry != to {
					t.Errorf("seed %d: a stayed on %d when b moved to %d", seed, a.entry, to)
				}
			})
		})
	}
}

func TestSeekAndResumeAtOnceDoesNotPauseAnyone(t *testing.T) {
	forSeeds(t, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, 0, "a", "b", "c")
		a := s.nodes[0]
		s.ticks = 5
		s.script = []func(){func() {
			a.mu.Lock()
			a.queueSeek(40)
			a.mu.Unlock()
			a.landSeek()
			a.mu.Lock()
			defer a.mu.Unlock()
			a.setPause(false, false)
		}}
		s.run()
		for _, n := range s.nodes {
			if slices.ContainsFunc(n.notices, func(text string) bool { return strings.HasSuffix(text, ": seek") }) {
				t.Errorf("seed %d: %s took it for a seek while playing: %q", seed, n.name, n.notices)
			}
		}
	})
}

// A party continued later: the key holder opens the room where it stopped, and members
// who stopped elsewhere, on other entries, all end up where the key holder is. The key
// holder says joined too, before the relay tells it it opened the room. Without its stamp
// for opening, or with an older entry followed, seed 0 fails.
func TestJoinersTakeTheOpenersSpot(t *testing.T) {
	forManySeeds(t, 600, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, simEntries, "o", "g", "h")
		o, g, h := s.nodes[0], s.nodes[1], s.nodes[2]
		for n, at := range map[*simNode][2]float64{o: {1, 40}, g: {2, 10}, h: {0, 5}} {
			n.entry, n.c.current = int(at[0]), int(at[0])
			joins(n)
			n.pos, n.c.pos = at[1], at[1]
		}
		o.in <- IncomingMessage{Opened: true}
		synctest.Wait()
		o.out <- []byte(`{"name":"joined"}`)
		g.out <- []byte(`{"name":"joined"}`)
		h.out <- []byte(`{"name":"joined"}`)
		s.run()
		for _, n := range []*simNode{g, h} {
			if n.entry != o.entry || n.pos != o.pos {
				t.Errorf("seed %d: %s is on %d at %.1f, the key holder on %d at %.1f: %q", seed, n.name, n.entry, n.pos, o.entry, o.pos, n.notices)
			}
		}
		if o.entry != 1 || o.pos != 40 {
			t.Errorf("seed %d: the key holder moved to %d at %.1f: %q", seed, o.entry, o.pos, o.notices)
		}
	})
}

// joins makes n a node that hasn't heard the party yet, as announce does.
func joins(n *simNode) {
	n.c.stamp = unheard
}

// stopped starts these nodes paused together at pos, in a party someone opened.
func stopped(pos float64, nodes ...*simNode) {
	for _, n := range nodes {
		n.pos, n.c.pos, n.c.reported, n.c.stamp = pos, pos, pos, 1
		n.c.snapshot()
	}
}

// Two people seek at the same moment while the party is stopped. Each applying the
// other's would swap them, and a resume says nothing of where, so they would play apart.
func TestTwoSeeksAtOnceEndOnOneSecond(t *testing.T) {
	forManySeeds(t, 100, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, 0, "a", "b", "c")
		a, b := s.nodes[0], s.nodes[1]
		stopped(30, s.nodes...)
		s.script = []func(){seeks(a, 40), seeks(b, 50)}
		s.run()
		s.together(seed)
	})
}

// One resumes as another seeks. The one playing dropped the seek and the other took the
// resume, so they played apart; stopping is the safe way out, where the seek went.
func TestAResumeCrossingASeekLeavesNobodyApart(t *testing.T) {
	forManySeeds(t, 100, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, 0, "a", "b", "c")
		a, b := s.nodes[0], s.nodes[1]
		stopped(30, s.nodes...)
		s.ticks = 20
		s.script = []func(){sets(a, false), seeks(b, 50)}
		s.rng.Shuffle(2, func(i, j int) { s.script[i], s.script[j] = s.script[j], s.script[i] })
		s.run()
		s.inStep(seed, 2)
	})
}

// A seek while playing goes out as a pause, then where. A send can fail in a mesh, and
// either one alone must still stop the node that got it on the right second.
func TestEitherHalfOfASeekWhilePlayingIsEnough(t *testing.T) {
	for _, name := range []string{pause, timePos} {
		t.Run(name, func(t *testing.T) {
			forManySeeds(t, 50, func(t *testing.T, seed uint64) {
				s := newSim(t, seed, 0, "a", "b", "c")
				a, b := s.nodes[0], s.nodes[1]
				play(30, s.nodes...)
				s.drop(a, b, name)
				s.script = []func(){seeks(a, 40)}
				s.run()
				s.together(seed)
			})
		})
	}
}

// A node that missed something is behind without knowing it, and acts. Whatever it does,
// the party ends stopped with that node in step, and says why when what it sent was older
// than what the party holds. Seed 22 is the party settling on where the node that was
// behind had stopped.
func TestANodeThatFellBehindIsCaughtUp(t *testing.T) {
	for name, c := range map[string]struct {
		playing bool
		missed  []func(*simNode) func()
		does    func(*simNode) func()
		told    bool
	}{
		"it resumes a party that sought":             {false, []func(*simNode) func(){func(a *simNode) func() { return seeks(a, 40) }}, func(b *simNode) func() { return sets(b, false) }, false},
		"it seeks later than the party did":          {false, []func(*simNode) func(){func(a *simNode) func() { return seeks(a, 40) }}, func(b *simNode) func() { return seeks(b, 45) }, false},
		"it resumes a party that sought twice":       {false, []func(*simNode) func(){func(a *simNode) func() { return seeks(a, 40) }, func(a *simNode) func() { return seeks(a, 42) }}, func(b *simNode) func() { return sets(b, false) }, true},
		"it pauses a party that stopped and went on": {true, []func(*simNode) func(){func(a *simNode) func() { return sets(a, true) }, func(a *simNode) func() { return sets(a, false) }}, func(b *simNode) func() { return sets(b, true) }, true},
	} {
		t.Run(name, func(t *testing.T) {
			forManySeeds(t, 50, func(t *testing.T, seed uint64) {
				s := newSim(t, seed, 0, "a", "b", "c")
				a, b := s.nodes[0], s.nodes[1]
				stopped(30, s.nodes...)
				if c.playing {
					play(30, s.nodes...)
				}
				s.lost[[2]string{"a", "b"}] = func(Event) bool { return true }
				for _, m := range c.missed {
					s.script = append(s.script, m(a))
				}
				s.run()
				delete(s.lost, [2]string{"a", "b"})
				s.ticks = 20
				s.script = []func(){c.does(b)}
				s.run()
				s.together(seed)
				// whoever stops the party for it says why; one that already holds that stop has
				// nothing to fix
				if c.told && !a.said("paused for b: catching up") && !s.nodes[2].said("paused for b: catching up") {
					t.Errorf("seed %d: nobody said why: %q, %q", seed, a.notices, s.nodes[2].notices)
				}
			})
		})
	}
}

// A member misses a resume, or a pause, or both, and its transport sends the party's state
// again once the link is back, before or after that member acts. Stopped, it takes the
// resume it missed and plays in step, one trip late like anyone. Playing through a pause
// and a resume, it is ahead, which no stamp shows but the newer resume reaching it while it
// plays: it stops the party, saying so. A tie, its own resume from where it stopped, is
// told by the member that missed it.
func TestAMemberThatMissedAChangeIsChasedIntoStep(t *testing.T) {
	resume := func(a *simNode) func() { return sets(a, false) }
	pause := func(a *simNode) func() { return sets(a, true) }
	for name, c := range map[string]struct {
		playing bool
		misses  []func(*simNode) func()
		does    func(*simNode) func()
		says    string
	}{
		"a resume, alone":                      {false, []func(*simNode) func(){resume}, nil, ""},
		"a resume, then it resumes":            {false, []func(*simNode) func(){resume}, func(b *simNode) func() { return sets(b, false) }, ""},
		"a resume, then it seeks":              {false, []func(*simNode) func(){resume}, func(b *simNode) func() { return seeks(b, 20) }, ""},
		"a pause, alone":                       {true, []func(*simNode) func(){pause}, nil, ""},
		"a pause, then it pauses":              {true, []func(*simNode) func(){pause}, func(b *simNode) func() { return sets(b, true) }, ""},
		"a pause and a resume, alone":          {true, []func(*simNode) func(){pause, resume}, nil, "b said paused: out of sync with a"},
		"a pause and a resume, then it pauses": {true, []func(*simNode) func(){pause, resume}, func(b *simNode) func() { return sets(b, true) }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			forManySeeds(t, 50, func(t *testing.T, seed uint64) {
				s := newSim(t, seed, 0, "a", "b", "c")
				a, b := s.nodes[0], s.nodes[1]
				stopped(30, s.nodes...)
				if c.playing {
					play(30, s.nodes...)
				}
				s.lost[[2]string{"a", "b"}] = func(Event) bool { return true }
				// further apart than a message's trip, before the link is back
				s.script = nil
				for _, m := range c.misses {
					s.script = append(s.script, m(a), func() { s.ticks = 40 })
				}
				s.run()
				delete(s.lost, [2]string{"a", "b"})
				s.script = []func(){s.chase}
				if c.does != nil && seed%2 == 0 {
					s.script = []func(){s.chase, c.does(b)}
				} else if c.does != nil {
					s.script = []func(){c.does(b), s.chase}
				}
				s.run()
				s.inStep(seed, 2)
				if c.says != "" && !strings.Contains(s.outcome(2), c.says) {
					t.Errorf("seed %d: ended %s, want %s", seed, s.outcome(2), c.says)
				}
			})
		})
	}
}

// outcome is how a party ended: stopped or playing, in step or not, and every "catching
// up" or "out of sync" said, by whom.
func (s *sim) outcome(lag float64) string {
	apart := 0.15
	if !s.nodes[0].paused {
		apart += lag
	}
	out := "in step"
	for _, n := range s.nodes {
		if d := n.pos - s.nodes[0].pos; n.paused != s.nodes[0].paused || d > apart || d < -apart {
			out = "APART"
		}
	}
	if s.nodes[0].paused {
		out = "stopped " + out
	} else {
		out = "playing " + out
	}
	for _, n := range s.nodes {
		for _, text := range n.notices {
			if strings.Contains(text, "catching up") || strings.Contains(text, "out of sync") {
				out += "; " + n.name + " said " + text
			}
		}
	}
	return out
}

// The state a node sends again can cross a newer change: here a resume, made after the
// seek that state is about. It is older, and says another state, but its sender missed
// nothing: answered as behind, it stopped the party in 9 seeds of 10.
func TestAResendCrossingANewerChangeStopsNobody(t *testing.T) {
	forManySeeds(t, 50, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, 0, "a", "b", "c")
		a, c := s.nodes[0], s.nodes[2]
		stopped(30, s.nodes...)
		s.lost[[2]string{"a", "b"}] = func(Event) bool { return true }
		s.script = []func(){seeks(a, 40)}
		s.run()
		delete(s.lost, [2]string{"a", "b"})
		s.script = []func(){s.chase, sets(c, false)}
		s.run()
		if got := s.outcome(0.5); got != "playing in step" {
			t.Errorf("seed %d: ended %s, want the party playing", seed, got)
		}
	})
}

// A member's process stops past the send timeout, so the resume sent to it looks lost and
// arrives once it runs again, late: it plays from where the party resumed, seconds behind.
// Found on real nodes (e2e [13d]). The state sent to it again is the same resume, which it
// already plays: that is how it knows, and it stops the party. Or it resumes on its own
// first, a tie with the party's, which the member it missed knows.
func TestAResumeThatArrivesLateEndsInStep(t *testing.T) {
	for name, own := range map[string]bool{"it takes the late one": false, "it resumes on its own": true} {
		t.Run(name, func(t *testing.T) {
			forManySeeds(t, 50, func(t *testing.T, seed uint64) {
				s := newSim(t, seed, 0, "a", "b", "c")
				a, b := s.nodes[0], s.nodes[1]
				stopped(30, s.nodes...)
				k := [2]string{"a", "b"}
				s.held[k] = true
				s.script = []func(){sets(a, false), func() {
					s.missed[k] = true
					s.tell(a, IncomingMessage{HostName: "b", From: "b", Missed: true})
				}, func() { s.ticks = 40 }}
				s.run()
				if own {
					s.script = []func(){sets(b, false)}
					s.run()
				}
				delete(s.held, k)
				s.script = []func(){s.chase}
				s.run()
				if got := s.outcome(2); !strings.HasPrefix(got, "stopped in step") {
					t.Errorf("seed %d: ended %s, want the party stopped in step", seed, got)
				}
			})
		})
	}
}

// A send can reach a member and still look lost, when its answer times out. The member
// missed nothing, so its resume, and the state sent to it again, stop nobody.
func TestASendThatOnlyLookedLostStopsNobody(t *testing.T) {
	forManySeeds(t, 50, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, 0, "a", "b", "c")
		a, b := s.nodes[0], s.nodes[1]
		stopped(30, s.nodes...)
		s.script = []func(){seeks(a, 40)}
		s.run()
		s.script = []func(){func() {
			s.missed[[2]string{"a", "b"}] = true
			s.tell(a, IncomingMessage{HostName: "b", From: "b", Missed: true})
		}, sets(b, false), s.chase}
		s.run()
		if got := s.outcome(0.5); got != "playing in step" {
			t.Errorf("seed %d: ended %s, want the party playing", seed, got)
		}
	})
}

// The relay never drops a frame for a member it keeps: a member it can't keep up with is
// closed, and joins again. So one that lost it while the party was stopped, and missed a
// resume, is answered as a joiner, and nothing tells it apart by seconds.
func TestAMemberThatLostTheRelayWhileStoppedEndsInStep(t *testing.T) {
	forManySeeds(t, 50, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, 0, "a", "b", "c")
		a, c := s.nodes[0], s.nodes[2]
		stopped(30, s.nodes...)
		s.ticks = 20
		s.script = []func(){s.lose(c), sets(a, false)}
		s.run()
		s.ticks = 20
		s.script = []func(){s.back(c)}
		s.run()
		s.together(seed)
	})
}

// Someone joins and does something before the party has answered: seeks to an earlier
// second, moves to a later entry, or resumes. The party never follows it, even one whose
// opener has done nothing yet: the joiner ends where the party is.
func TestNothingAJoinerDoesBeforeItIsAnsweredMovesTheParty(t *testing.T) {
	for name, does := range map[string]func(*simNode) func(){
		"seeks":   func(j *simNode) func() { return seeks(j, 10) },
		"moves":   func(j *simNode) func() { return moves(j, 2) },
		"resumes": func(j *simNode) func() { return sets(j, false) },
	} {
		t.Run(name, func(t *testing.T) {
			forManySeeds(t, 50, func(t *testing.T, seed uint64) {
				s := newSim(t, seed, simEntries, "a", "b", "j")
				a, b, j := s.nodes[0], s.nodes[1], s.nodes[2]
				stopped(30, a, b)
				for _, n := range []*simNode{a, b, j} {
					n.entry, n.c.current = 1, 1
				}
				joins(j)
				s.ticks = 20
				// before anything can have reached it: just before it says joined, or right after
				says := func() { j.out <- []byte(`{"name":"joined"}`) }
				s.script = []func(){does(j), says}
				if seed%2 == 0 {
					s.script = []func(){func() { says(); does(j)() }}
				}
				s.run()
				if a.entry != 1 || b.entry != 1 || j.entry != 1 {
					t.Errorf("seed %d: on entries %d, %d and %d, want 1: a heard %q", seed, a.entry, b.entry, j.entry, a.notices)
				}
				s.together(seed)
				if a.pos != 30 {
					t.Errorf("seed %d: the party went to %.1f, want it to stay at 30", seed, a.pos)
				}
			})
		})
	}
}

// A node misses a move, which a mesh allows, and then does something on the entry the
// party has left, before the transport sends it the party's state again. What it sends
// carries the same stamp as the move it never heard, so it is no older: it was dropped as
// belonging to another entry, and that node stayed behind until someone else did
// something. Now it is told where the party is.
func TestANodeThatMissedAMoveIsBroughtOver(t *testing.T) {
	for name, does := range map[string]func(*simNode) func(){
		"it seeks":   func(b *simNode) func() { return seeks(b, 40) },
		"it resumes": func(b *simNode) func() { return sets(b, false) },
	} {
		t.Run(name, func(t *testing.T) {
			forManySeeds(t, 50, func(t *testing.T, seed uint64) {
				s := newSim(t, seed, simEntries, "a", "b", "c")
				a, b := s.nodes[0], s.nodes[1]
				stopped(30, s.nodes...)
				// and what a sends again, while the link is down
				s.lost[[2]string{"a", "b"}] = func(Event) bool { return true }
				s.script = []func(){moves(a, 1)}
				s.run()
				delete(s.lost, [2]string{"a", "b"})
				s.ticks = 20
				s.script = []func(){does(b)}
				s.run()
				for _, n := range s.nodes {
					if n.entry != 1 {
						t.Errorf("seed %d: %s is on entry %d, want 1", seed, n.name, n.entry)
					}
				}
				s.together(seed)
			})
		})
	}
}

// A member seeks while it shows why it stops for a joiner: the seek goes out first, so where
// the member says it is, after it, is the second it sought (newest).
func TestASeekWhileAnsweringAJoinIsWhereTheMemberSaysItIs(t *testing.T) {
	forManySeeds(t, 300, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, 0, "a", "b", "j")
		a, b, j := s.nodes[0], s.nodes[1], s.nodes[2]
		stopped(30, a, b)
		joins(j)
		j.out <- []byte(`{"name":"joined"}`)
		s.script = []func(){seeks(b, 50)}
		s.run()
		s.together(seed)
	})
}

// Anyone acts at any moment: the people pause, resume and seek while the others' changes
// are on their way, and in the late pass while mpv still answers this node's own steps. The
// party may end anywhere, but in one state, and no node says a state older than one it said
// (newest): what the protocol promises, whatever anyone did.
func TestAnyoneActingAtAnyMomentEndsInStep(t *testing.T) {
	forManySeeds(t, 500, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, 0, "a", "b", "c")
		stopped(30, s.nodes...)
		rng := rand.New(rand.NewPCG(seed, 1))
		for range 6 {
			n := s.nodes[rng.IntN(len(s.nodes))]
			switch rng.IntN(3) {
			case 0:
				s.script = append(s.script, sets(n, true))
			case 1:
				s.script = append(s.script, sets(n, false))
			default:
				s.script = append(s.script, seeks(n, float64(rng.IntN(100))))
			}
		}
		s.ticks = 30
		s.run()
		// each plays from when the resume reached it, at worst after every tick
		s.inStep(seed, 3)
	})
}

// A speed change while playing stops the party, everyone on one second and at that speed,
// whichever way the messages go.
func TestASpeedChangeWhilePlayingStopsEveryoneAtIt(t *testing.T) {
	forManySeeds(t, 200, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, 0, "a", "b", "c")
		play(30, s.nodes...)
		s.ticks = 20
		s.script = []func(){speeds(s.nodes[0], 2)}
		s.run()
		s.together(seed)
		s.sameSpeed(seed)
	})
}

// Speed changes, resumes, stops and seeks from anyone at any moment: the party ends on one
// speed, in step, whatever crossed what.
func TestSpeedChangesAtAnyMomentEndOnOneSpeed(t *testing.T) {
	forManySeeds(t, 500, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, 0, "a", "b", "c")
		stopped(30, s.nodes...)
		rng := rand.New(rand.NewPCG(seed, 1))
		for range 6 {
			n := s.nodes[rng.IntN(len(s.nodes))]
			switch rng.IntN(4) {
			case 0:
				s.script = append(s.script, sets(n, true))
			case 1:
				s.script = append(s.script, sets(n, false))
			case 2:
				s.script = append(s.script, seeks(n, float64(rng.IntN(100))))
			default:
				s.script = append(s.script, speeds(n, []float64{0.5, 1, 1.25, 1.5, 2}[rng.IntN(5)]))
			}
		}
		s.ticks = 30
		s.run()
		s.inStep(seed, 3)
		s.sameSpeed(seed)
	})
}
