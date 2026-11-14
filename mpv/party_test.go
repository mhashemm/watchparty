package mpv

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
)

// A whole party in one bubble: real Clients, a small model of mpv under each, and a
// network that a seed reorders. After each step everything has run as far as it can, so a
// failing seed fails again. Each sender's messages stay in order, as on a connection; what
// varies is who hears what first, and when mpv reports a seek. Checks are what the protocol
// guarantees, for every order: that is what e2e can only sample.
type simNode struct {
	name   string
	c      *Client
	out    chan []byte
	in     chan IncomingMessage
	writes chan string

	mu      sync.Mutex
	paused  bool
	pos     float64
	entry   int
	notices []string
	seeks   []string
	moves   []int
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
	ticks  int
	script []func()
}

const simEntries = 3

func newSim(t *testing.T, seed uint64, entries int, names ...string) *sim {
	s := &sim{t: t, rng: rand.New(rand.NewPCG(seed, 0)), links: map[[2]string][]IncomingMessage{}, down: map[string]bool{}, lost: map[[2]string]func(Event) bool{}, missed: map[[2]string]bool{}, told: map[*simNode][]IncomingMessage{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, name := range names {
		ours, theirs := net.Pipe()
		n := &simNode{name: name, out: make(chan []byte, 1024), in: make(chan IncomingMessage, 1024), writes: make(chan string, 1024), paused: true}
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
		go n.c.watch()
		go func() {
			for {
				select {
				case m := <-n.in:
					n.c.apply(m)
				case <-ctx.Done():
					return
				}
			}
		}()
		s.nodes = append(s.nodes, n)
	}
	return s
}

// model answers requests the way mpv does (MPV.md): a no-op set says nothing, and a seek
// reports later, ahead of a pause sent right behind it but after the reply to that pause.
// A set of playlist-pos replies first and reports later, so the next set can overtake the
// report, and two close together can report only the last (MPV.md).
func (n *simNode) model(ctx context.Context, conn net.Conn) {
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		req := Request{}
		json.Unmarshal(sc.Bytes(), &req)
		name := fmt.Sprint(req.Command[0])
		n.mu.Lock()
		switch {
		case name == showText:
			n.notices = append(n.notices, fmt.Sprint(req.Command[1]))
		case name == setProperty && req.Command[1] == pause:
			if len(n.seeks) > 0 {
				n.emit(fmt.Sprintf(`{"request_id":%d,"error":"success"}`+"\n", req.RequestId))
			}
			n.setPause(req.Command[2] == true)
		case name == setProperty && req.Command[1] == timePos:
			pos, _ := strconv.ParseFloat(fmt.Sprint(req.Command[2]), 64)
			n.seek(pos)
		case name == setProperty && req.Command[1] == playlistPos:
			i, _ := strconv.Atoi(fmt.Sprint(req.Command[2]))
			n.request(i)
		}
		n.mu.Unlock()
		select {
		case n.writes <- fmt.Sprintf(`{"request_id":%d,"error":"success"}`+"\n", req.RequestId):
		case <-ctx.Done():
			return
		}
	}
}

func (n *simNode) emit(line string) { n.writes <- line }

func (n *simNode) setPause(paused bool) {
	if n.paused == paused {
		return
	}
	n.paused = paused
	n.landSeekLocked()
	data := "no"
	if paused {
		data = "yes"
	}
	n.emit(propertyChangeLine(pause, data, 0))
}

func (n *simNode) seek(pos float64) {
	n.seeks = append(n.seeks, fmt.Sprintf("%f", pos))
}

func (n *simNode) landSeek() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.landSeekLocked()
}

func (n *simNode) landSeekLocked() {
	if len(n.seeks) == 0 {
		return
	}
	last := n.seeks[len(n.seeks)-1]
	n.seeks = nil
	n.pos, _ = strconv.ParseFloat(last, 64)
	n.emit(`{"event":"seek"}` + "\n")
	n.emit(propertyChangeLine(timePos, last, 0))
	n.emit(`{"event":"playback-restart"}` + "\n")
}

// request is a set or a key press: mpv takes them in the order they came.
func (n *simNode) request(i int) {
	if len(n.moves) > 0 || i != n.entry {
		n.moves = append(n.moves, i)
	}
}

func (n *simNode) landMove() {
	n.mu.Lock()
	defer n.mu.Unlock()
	i := n.moves[0]
	n.moves = n.moves[1:]
	n.move(i)
}

// foldMoves is mpv handling them too close together to report any but the last. Back
// where it started, it reports nothing at all and still loads the file.
func (n *simNode) foldMoves() {
	n.mu.Lock()
	defer n.mu.Unlock()
	i := n.moves[len(n.moves)-1]
	n.moves = nil
	if i == n.entry {
		n.emit(`{"event":"file-loaded"}` + "\n")
		return
	}
	n.move(i)
}

// move drops a seek that hasn't landed: mpv reports one sent just before a switch ahead
// of it or not at all, never on the new file, which always opens at 0 (MPV.md).
func (n *simNode) move(i int) {
	if i == n.entry {
		return
	}
	n.entry, n.pos, n.seeks = i, 0, nil
	n.emit(propertyChangeLine(playlistPos, strconv.Itoa(i), 0))
	n.emit(`{"event":"file-loaded"}` + "\n")
	n.emit(propertyChangeLine(timePos, "0.000000", 0))
}

// run goes until nothing is left to happen, picking at random among what could happen next.
func (s *sim) run() {
	for {
		synctest.Wait()
		for _, from := range s.nodes {
			for len(from.out) > 0 {
				b := <-from.out
				for _, to := range s.nodes {
					// a node without its relay connection sends and hears nothing
					k := [2]string{from.name, to.name}
					if to == from || s.down[from.name] || s.down[to.name] {
						continue
					}
					if !s.loses(k, b) {
						s.links[k] = append(s.links[k], IncomingMessage{HostName: from.name, From: from.name, Event: b})
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
				if k := [2]string{from.name, to.name}; len(s.links[k]) > 0 {
					next = append(next, func() {
						to.in <- s.links[k][0]
						s.links[k] = s.links[k][1:]
						// a send got through: the transport's chase ends
						delete(s.missed, k)
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
				next = append(next, from.landMove)
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

// chase is the transport asking every node to send its state again to each member it
// missed, as it does every few seconds until a send gets through.
func (s *sim) chase() {
	for k := range s.missed {
		for _, n := range s.nodes {
			if n.name == k[0] {
				s.tell(n, IncomingMessage{HostName: k[1], From: k[1], Resend: true})
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
		n.seek(pos)
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

func sets(n *simNode, paused bool) func() {
	return func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.setPause(paused)
	}
}

// inStep is everyone stopped on the same second, or everyone playing from it: then each
// started when the resume reached it, at most lag seconds after the first.
func (s *sim) inStep(seed uint64, lag float64) {
	s.t.Helper()
	apart := 0.15
	if !s.nodes[0].paused {
		apart += lag
	}
	for _, n := range s.nodes {
		if d := n.pos - s.nodes[0].pos; n.paused != s.nodes[0].paused || d > apart || d < -apart {
			s.t.Errorf("seed %d: %s is at %.2f (paused %v), %s at %.2f (paused %v)", seed, n.name, n.pos, n.paused, s.nodes[0].name, s.nodes[0].pos, s.nodes[0].paused)
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
		n.c.paused, n.c.pos, n.c.stamp = false, pos, 1
	}
}

func (s *sim) together(seed uint64) {
	s.t.Helper()
	for _, n := range s.nodes {
		if !n.paused {
			s.t.Errorf("seed %d: %s is playing", seed, n.name)
		}
		if d := n.pos - s.nodes[0].pos; d > 0.15 || d < -0.15 {
			s.t.Errorf("seed %d: %s is at %.2f, %s at %.2f", seed, n.name, n.pos, s.nodes[0].name, s.nodes[0].pos)
		}
	}
}

func forSeeds(t *testing.T, f func(t *testing.T, seed uint64)) {
	forManySeeds(t, 200, f)
}

func forManySeeds(t *testing.T, seeds uint64, f func(t *testing.T, seed uint64)) {
	t.Parallel()
	for seed := range seeds {
		synctest.Test(t, func(t *testing.T) { f(t, seed) })
		if t.Failed() {
			return
		}
	}
}

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

// One member's connection drops while the party plays, and the rest play on without
// knowing. Whatever it does meanwhile reaches nobody, so it stops at once, and its return
// stops the party, on whichever entry and second the party is by then.
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
			a.setPause(false)
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
			t.Errorf("seed %d: c never said why it stopped: %q", seed, c.notices)
		}
		if c.entry != a.entry || b.entry != a.entry {
			t.Errorf("seed %d: on entries %d, %d and %d", seed, a.entry, b.entry, c.entry)
		}
	})
}

// The relay itself goes away: everyone stops where they are, the key holder opens the
// room again, and the others rejoin it there.
func TestPartySettlesAfterTheRelayRestarts(t *testing.T) {
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
		s.together(seed)
		for _, n := range s.nodes {
			if !n.said(lostRelay) {
				t.Errorf("seed %d: %s never said why it stopped: %q", seed, n.name, n.notices)
			}
		}
	})
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
				// a marker left behind would swallow this as an echo, and nobody would follow
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
			a.seek(40)
			a.mu.Unlock()
			a.landSeek()
			a.mu.Lock()
			defer a.mu.Unlock()
			a.setPause(false)
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
		n.pos, n.c.pos, n.c.stamp = pos, pos, 1
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
