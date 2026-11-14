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
	t      *testing.T
	rng    *rand.Rand
	nodes  []*simNode
	links  map[[2]string][]IncomingMessage
	down   map[string]bool
	ticks  int
	script []func()
}

const simEntries = 3

func newSim(t *testing.T, seed uint64, entries int, names ...string) *sim {
	s := &sim{t: t, rng: rand.New(rand.NewPCG(seed, 0)), links: map[[2]string][]IncomingMessage{}, down: map[string]bool{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, name := range names {
		ours, theirs := net.Pipe()
		n := &simNode{name: name, out: make(chan []byte, 1024), in: make(chan IncomingMessage, 1024), writes: make(chan string, 1024), paused: true}
		n.c = newTestClient(n.out, bufio.NewScanner(ours))
		n.c.ctx, n.c.conn.conn, n.c.paused, n.c.names, n.c.movedFrom = ctx, ours, true, slices.Repeat([]string{"e"}, entries), -1
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

func (n *simNode) move(i int) {
	if i == n.entry {
		return
	}
	n.entry, n.pos = i, 0
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
					if to != from && !s.down[from.name] && !s.down[to.name] {
						k := [2]string{from.name, to.name}
						s.links[k] = append(s.links[k], IncomingMessage{HostName: from.name, Event: b})
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
					})
				}
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

func (n *simNode) said(text string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Contains(n.notices, text)
}

// play starts these nodes at pos as if someone had resumed long ago.
func play(pos float64, nodes ...*simNode) {
	for _, n := range nodes {
		n.paused, n.pos = false, pos
		n.c.paused, n.c.pos = false, pos
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
		j.c.joining = true
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
	forManySeeds(t, 1000, func(t *testing.T, seed uint64) {
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
		// only once the move has reached everyone still there: a join that crosses a move
		// still in flight can strand the joiner, a first join as much as this one
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
	forManySeeds(t, 1000, func(t *testing.T, seed uint64) {
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
		a.entry, a.c.current, a.c.joining = 0, 0, true
		b.entry, b.c.current, b.c.joining = 2, 2, true
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
			n.entry, n.c.current, n.c.joining = 1, 1, true
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
// who stopped elsewhere, on other entries, all end up where the key holder is. Seed 1675
// is a fellow joiner's entry winning over the party's. The key holder says joined too,
// before the relay tells it it opened the room: seed 0 is a joiner taking it for a fellow.
func TestJoinersTakeTheOpenersSpot(t *testing.T) {
	forManySeeds(t, 2000, func(t *testing.T, seed uint64) {
		s := newSim(t, seed, simEntries, "o", "g", "h")
		o, g, h := s.nodes[0], s.nodes[1], s.nodes[2]
		for n, at := range map[*simNode][2]float64{o: {1, 40}, g: {2, 10}, h: {0, 5}} {
			n.entry, n.c.current, n.c.joining = int(at[0]), int(at[0]), true
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
