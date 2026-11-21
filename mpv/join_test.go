package mpv

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

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
			if scriptAnswers(theirs, req) {
				n--
				continue
			}
			if said, ok := scriptAcks(theirs, req); ok {
				sent <- said
				continue
			}
			fmt.Fprintf(theirs, `{"request_id":%d,"error":"success"}`+"\n", req.RequestId)
			if n == 2 {
				<-load
				fmt.Fprint(theirs, `{"event":"property-change","id":2,"name":"time-pos","data":"0.000000"}`+"\n")
			} else if req.Command[0] != "observe_property_string" {
				sent <- strings.TrimSpace(fmt.Sprintln(req.Command...))
			}
		}
		scanned(sc)
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
	// the last resume says where from, so it is the whole of what the party said: one set, to
	// 999 and playing (scriptAcks gives it as its seek)
	want := []string{"set_property time-pos 999.000000", "show-text resumed by a 5000", "show-text mark 5000"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("applied %q, want %q", got, want)
	}
}

// Once its file has loaded, a node says so, and takes where the party says it is.
func TestJoinerAnnouncesItselfOnceLoaded(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{joiner: true})
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q, want joined", got)
		}
		n.peerSays("a", stamped(1, seekEvent(40)))
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property time-pos 40.000000 show-text synced by a 5000]" {
			t.Fatalf("told mpv %q, want the party's position", got)
		}
		n.mpvObeys()
		// every member answers, and the rest say the same
		n.peerSays("b", stamped(1, seekEvent(40)))
		if got, told := n.broadcasts(), n.mpvWasTold(); len(got) != 0 || len(told) != 0 {
			t.Fatalf("broadcast %q and told mpv %q for the same answer again", said(got), told)
		}
	})
}

// What the party says while this node loads is applied once it has, before it says joined:
// the newest, which says the entry and where on it.
func TestWhatIsHeardWhileLoadingIsAppliedBeforeJoining(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{names: []string{"e1", "e2", "e3"}, joiner: true, late: true, heard: []IncomingMessage{
			peer("a", move(1, 2)),
			peer("a", stamped(2, tagged(2, seekEvent(50)))),
		}})
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property playlist-pos 2]" {
			t.Fatalf("told mpv %q, want the party's entry", got)
		}
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q before mpv took the party's entry", said(got))
		}
		// the seek waits for the file it is about
		n.mpvObeys()
		// the notices and the seek let go for the file are told side by side
		got := n.mpvWasTold()
		slices.Sort(got)
		if fmt.Sprint(got) != "[set_property time-pos 50.000000 show-text from e1 to e3, by a 5000 show-text synced by a 5000]" {
			t.Fatalf("told mpv %q once e3 loaded, want the party's second", got)
		}
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q, want joined", got)
		}
	})
}

// A node already in the party stops for a joiner, and its own pause is its answer: it
// says where, with a stamp newer than anything it could say before mpv had stopped.
func TestJoinPausesThePartyAndSaysWhere(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		resumed := n.broadcasts()[0].Stamp
		n.mpvSays(mpvAt(33.5))
		n.peerSays("j", `{"name":"joined"}`)
		// why first: the GUI's feed takes the pause that follows for this one
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text paused for j: joining 5000 set_property pause true]" {
			t.Fatalf("told mpv %q", got)
		}
		n.mpvObeys()
		got := n.broadcasts()
		if len(got) != 1 || got[0].Name != pause || got[0].Data != "yes" || got[0].At == nil || *got[0].At != 33.5 || got[0].Stamp <= resumed {
			t.Fatalf("broadcast %+v, want only its own pause, where it stopped, newer than its resume: a position beside it could carry the stamp from before", got)
		}
	})
}

// A node already paused, often by another node's pause for the same joiner, still
// says why the party is waiting.
func TestJoinWhilePausedStillShowsWhy(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("j", `{"name":"joined"}`)
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text paused for j: joining 5000]" {
			t.Fatalf("told mpv %q, want the notice and no pause", got)
		}
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != timePos {
			t.Fatalf("broadcast %q, want the position", said(got))
		}
	})
}

// Everyone gets the notice at their own moment, so everyone pauses themselves, and any
// one of those pauses is enough for a node that missed the notice.
func TestMemberLeavingPausesTheParty(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvSays(mpvAt(33.5))
		n.peerSays("x", `{"name":"show-text","data":"x has left"}`)
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text x has left 5000 show-text paused for x: left 5000 set_property pause true]" {
			t.Fatalf("told mpv %q, want the notice, the reason, then the party paused", got)
		}
		n.mpvObeys()
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != pause || got[0].Data != "yes" || got[0].At == nil || *got[0].At != 33.5 {
			t.Fatalf("broadcast %+v, want the pause, where it stopped", got)
		}
	})
}

func TestMemberLeavingChangesNothingWhileAlreadyPaused(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("x", `{"name":"show-text","data":"x has left"}`)
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text x has left 5000]" {
			t.Fatalf("told mpv %q, want the notice and no pause", got)
		}
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q", said(got))
		}
	})
}

// The video ending is how a party normally finishes. Each player reaches the end at
// its own moment, and whoever gets there first leaves; a pause would hold the rest a
// moment before the end, with nothing telling them why.
func TestMemberLeavingNearTheEndDoesNotPause(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvSays(propertyChangeLine(duration, "100.000000", 0), mpvAt(96))
		n.peerSays("x", `{"name":"show-text","data":"x has left"}`)
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text x has left 5000]" {
			t.Fatalf("told mpv %q, want no pause", got)
		}
	})
}

// Only the notice about its own sender counts, which is the shape both transports send.
func TestOnlyALeaveNoticePauses(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.peerSays("x", `{"name":"show-text","data":"y has left"}`)
		n.peerSays("x", `{"name":"show-text","data":"x has joined"}`)
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text y has left 5000 show-text x has joined 5000]" {
			t.Fatalf("told mpv %q, want no pause", got)
		}
	})
}

// Everyone stops a moment apart for a leave or a join, each with the same stamp, so they
// settle on the earliest stop, or they would resume that far apart and stay so. A joiner
// takes the first it hears, then the earliest too.
func TestStopsAMomentApartSettleOnTheEarliest(t *testing.T) {
	t.Parallel()
	stop := func(at float64) string {
		return fmt.Sprintf(`{"stamp":2,"at":%f,"name":"pause","data":"yes"}`, at)
	}
	t.Run("a member", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(2, true, 40)
			n.peerSays("a", stop(41))
			n.peerSays("b", stop(39))
			if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 39.000000]" {
				t.Fatalf("seeks %q, want only the earlier stop followed", got)
			}
		})
	})
	t.Run("a joiner", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{joiner: true})
			n.peerSays("a", stop(40))
			n.peerSays("b", stop(39))
			if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 40.000000 set_property time-pos 39.000000]" {
				t.Fatalf("seeks %q, want the first, then the earlier", got)
			}
		})
	})
}

// Two people start at once on different entries, and each answers the other. Neither
// knows anything yet, so their stamps tie and the higher entry wins, as it does for two
// moves: each following the other would swap them, for good. A party's answer has a
// stamp, and that is followed, lower or not.
func TestTwoJoinersSettleOnTheHigherEntry(t *testing.T) {
	t.Parallel()
	names := []string{"e1", "e2", "e3"}
	answer := func(i int) string { return move(unheard, i) }
	for _, tc := range []struct {
		name     string
		start    int
		messages []string
		want     string
	}{
		{"the higher one stays", 2, []string{`{"name":"joined"}`, answer(0)}, ""},
		{"the lower one follows", 0, []string{`{"name":"joined"}`, answer(2)}, "set_property playlist-pos 2"},
		{"the party's answer is followed, lower or not", 2, []string{move(1, 0)}, "set_property playlist-pos 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{names: names, start: tc.start, joiner: true})
				for _, m := range tc.messages {
					n.peerSays("a", m)
				}
				moved := ""
				for _, s := range n.mpvWasTold() {
					if strings.HasPrefix(s, "set_property playlist-pos") {
						moved = s
					}
				}
				if moved != tc.want {
					t.Fatalf("told mpv %q, want %q", moved, tc.want)
				}
			})
		})
	}
}

// Members answer a joiner with where each stopped, a moment apart and with one stamp. The
// joiner takes the first, then an earlier one, and not a later one after that.
func TestAJoinerFollowsAnEarlierAnswer(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{joiner: true})
		n.peerSays("a", stamped(1, seekEvent(40)))
		n.peerSays("b", stamped(1, seekEvent(39)))
		n.peerSays("c", stamped(1, seekEvent(40)))
		if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 40.000000 set_property time-pos 39.000000]" {
			t.Fatalf("seeks %q", got)
		}
	})
}

// A joiner already where the party is takes its stamp and seeks nowhere.
func TestAnAnswerWhereItAlreadyIsSeeksNowhere(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{joiner: true})
		n.mpvSays(mpvAt(40))
		n.broadcasts()
		n.peerSays("a", stamped(1, seekEvent(40.05)))
		n.peerSays("b", stamped(1, seekEvent(55)))
		if got := seeksIn(n.mpvWasTold()); len(got) != 0 {
			t.Fatalf("seeks %q, want neither answer to seek: it is there, and the later one loses", got)
		}
		// its next change is newer than the party's answer
		n.plays()
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		if got := n.broadcasts(); len(got) != 1 || got[0].Stamp != 3 {
			t.Fatalf("broadcast %+v, want its pause at stamp 3: it took the party's 1, then resumed at 2", got)
		}
	})
}

// A node that loses the relay plays on, as the party most likely does, and says so. Back
// in the room it says, quietly, the state it holds; the party stops only for a state that
// isn't its own (take). A joiner that was never answered has none, and joins again.
func TestLosingTheRelayPlaysOnAndComingBackSaysWhereItIs(t *testing.T) {
	t.Parallel()
	t.Run("a member", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(4, false, 10)
			n.transportSays(IncomingMessage{Lost: true})
			if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text lost the relay, reconnecting 5000]" {
				t.Fatalf("told mpv %q, want only the notice", got)
			}
			n.transportSays(IncomingMessage{Back: true})
			got := n.broadcasts()
			if len(got) != 1 || !got[0].Check || got[0].Name != pause || got[0].Stamp != 4 || got[0].Paused == nil || *got[0].Paused || got[0].At == nil || *got[0].At != 10 {
				t.Fatalf("broadcast %+v, want its state, playing from 10 at stamp 4, as a check", got)
			}
		})
	})
	t.Run("a joiner never answered", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{joiner: true})
			n.transportSays(IncomingMessage{Lost: true})
			n.transportSays(IncomingMessage{Back: true})
			if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
				t.Fatalf("broadcast %q, want it to join again", got)
			}
		})
	})
}

// The last seconds are played out, as when a member leaves: a state that stops the party
// would hold everyone else just short of the end.
func TestComingBackNearTheEndSaysNothing(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(4, false, 117)
		n.transportSays(IncomingMessage{Lost: true})
		n.transportSays(IncomingMessage{Back: true})
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q, want nothing this close to the end", said(got))
		}
		// stopped by hand there, it says where like anywhere else
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		n.broadcasts()
		n.transportSays(IncomingMessage{Back: true})
		if got := n.broadcasts(); len(got) != 1 || !got[0].Check {
			t.Fatalf("broadcast %+v, want its state", got)
		}
	})
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
		{"the party's own, playing", false, check(4, false, 30), nil},
		{"the party's own, stopped", true, check(4, true, 30), nil},
		{"older, while the party moved on", false, check(3, false, 10), []string{"show-text paused for c: catching up 5000", "set_property pause true"}},
		{"a newer pause", false, check(5, true, 25), []string{"set_property pause true", "set_property time-pos 25.000000", "show-text paused by c 5000"}},
		{"a newer resume, the party stopped", true, check(5, false, 30), []string{"show-text paused: out of sync with c 5000"}},
		{"a newer resume, the party playing", false, check(5, false, 30), []string{"show-text paused: out of sync with c 5000", "set_property pause true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.takes(4, tc.paused, 30)
				n.peerSays("c", tc.event)
				n.mpvObeys()
				if got := n.mpvWasTold(); !slices.Equal(got, tc.want) {
					t.Fatalf("told mpv %q, want %q", got, tc.want)
				}
			})
		})
	}
}

// A node that hasn't heard the party answers too, with what it holds: its stamp is below
// any the party has, so only a fellow joiner can take it.
func TestJoiningNodeAnswersWithAnUnheardStamp(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 2, joiner: true})
		n.mpvSays(mpvAt(7))
		n.broadcasts()
		n.peerSays("j", `{"name":"joined"}`)
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != timePos || got[0].Stamp >= 0 || got[0].Entry == nil || *got[0].Entry != 2 || got[0].At == nil || *got[0].At != 7 {
			t.Fatalf("broadcast %+v, want its state, unheard", got)
		}
	})
}

// An answer is one message: the entry, and where on it.
func TestJoinAnswerSaysTheEntryAndWhereOnIt(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 2})
		n.mpvSays(mpvAt(33))
		n.broadcasts()
		n.peerSays("j", `{"name":"joined"}`)
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != timePos || got[0].Data != "33.000000" || got[0].Entry == nil || *got[0].Entry != 2 {
			t.Fatalf("broadcast %+v, want the entry and the position", got)
		}
	})
}

// A joiner knows nothing, whatever it knew before, and whoever opened the party is the
// party from the start. Its own first change shows the stamp it holds.
func TestAJoinForgetsTheStampAndOpeningTakesOne(t *testing.T) {
	t.Parallel()
	stampOfASeek := func(n *node) int {
		n.t.Helper()
		n.mpvSays(mpvAt(9))
		got := n.broadcasts()
		if len(got) != 1 {
			n.t.Fatalf("broadcast %+v, want the seek", got)
		}
		return got[0].Stamp
	}
	t.Run("a joiner", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			if got := stampOfASeek(ready(t, setup{joiner: true})); got != unheard+1 {
				t.Fatalf("stamp %d, want one past unheard", got)
			}
		})
	})
	t.Run("an opener", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			if got := stampOfASeek(ready(t, setup{})); got != 2 {
				t.Fatalf("stamp %d, want one past 1", got)
			}
		})
	})
	t.Run("one who reopens keeps its stamp", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(7, true, 0)
			n.transportSays(IncomingMessage{Opened: true})
			// a follower's seek stays on its screen: its resume shows the stamp
			n.mpvSays(propertyChangeLine(pause, "no", 0))
			if got := n.broadcasts(); len(got) != 1 || got[0].Stamp != 8 {
				t.Fatalf("broadcast %+v, want its resume, one past 7", got)
			}
		})
	})
	t.Run("one that joins again forgets it", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(7, true, 0)
			// mpv refusing a step it was told is what makes a member ask the party again
			n.mpvRefuses("set_property time-pos", "property unavailable")
			n.peerSays("a", stamped(8, seekEvent(20)))
			if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
				t.Fatalf("broadcast %q, want joined", got)
			}
			if got := stampOfASeek(n); got != unheard+1 {
				t.Fatalf("stamp %d, want one past unheard", got)
			}
		})
	})
}

// Where a node stopped is where a continued party starts, except right at an entry's
// end (mpv's keep-open=always stays there): then it is the next entry, from its start.
func TestPositionToContinueFrom(t *testing.T) {
	t.Parallel()
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
		{threeVideos, 1, 50, 100, 1, 50},
		{threeVideos, 1, 97, 100, 2, 0},
		{threeVideos, 2, 97, 100, 2, 0},
	} {
		t.Run(fmt.Sprintf("%d entries, on %d at %v of %v", len(c.names), c.current, c.pos, c.dur), func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{names: c.names, start: c.current})
				n.mpvSays(propertyChangeLine(duration, fmt.Sprintf("%f", c.dur), 0), mpvAt(c.pos))
				if entry, at := n.c.Position(); entry != c.entry || at != c.at {
					t.Fatalf("entry %d at %v, want %d at %v", entry, at, c.entry, c.at)
				}
			})
		})
	}
}

// Only the end of the last entry, or of the only file, finishes the party: any other
// entry has a next one to continue from.
func TestFinishedOnlyAtTheEndOfTheLastEntry(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		names    []string
		current  int
		pos      float64
		finished bool
	}{{nil, 0, 97, true}, {nil, 0, 50, false}, {threeVideos, 2, 97, true}, {threeVideos, 1, 97, false}, {threeVideos, 2, 50, false}} {
		t.Run(fmt.Sprintf("%d entries, on %d at %v", len(c.names), c.current, c.pos), func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{names: c.names, start: c.current})
				n.mpvSays(propertyChangeLine(duration, "100.000000", 0), mpvAt(c.pos))
				if got := n.c.Finished(); got != c.finished {
					t.Fatalf("finished %v, want %v", got, c.finished)
				}
			})
		})
	}
}

// A node that opens on a later entry can hear the party name that entry before mpv has
// reported where it is. Taking itself for entry 0, it set the entry it was already on,
// which mpv answers with nothing, and waited for that load forever.
func TestJoinerKnowsItsStartingEntryBeforeMpvSaysSo(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{names: threeVideos, start: 1, joiner: true,
			heard:   []IncomingMessage{peer("a", `{"event":"property-change","name":"playlist-pos","data":"1"}`)},
			opening: []string{strings.TrimSpace(propertyChangeLine(pause, "yes", 0)), strings.TrimSpace(mpvAt(0))},
		})
		if got := n.mpvWasTold(); len(got) != 0 {
			t.Fatalf("told mpv %q, want nothing: it is already on that entry", got)
		}
	})
}

// A continued party seeks the file it continues on once it is open, and only it. Then
// whatever the party sent while it loaded is applied, so the party's position wins.
// The seek is its own echo, so nobody else is moved by it.
func TestContinuingSeeksBeforeThePartyIsHeard(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{at: 40, joiner: true, heard: []IncomingMessage{peer("a", stamped(1, seekEvent(55)))}})
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property time-pos 40.000000 set_property time-pos 55.000000 show-text synced by a 5000]" {
			t.Fatalf("told mpv %q, want its own spot, then the party's", got)
		}
		n.mpvObeys()
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q: continuing moved the party", got)
		}
	})
}

// A joiner takes the party's stamp with its position. Left unheard, the first thing it did
// would be older than what the party holds, and nobody would follow it.
func TestAJoinerTakesThePartysStampWithItsPosition(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{joiner: true})
		n.peerSays("a", stamped(5, seekEvent(40)))
		if got := n.mpvWasTold(); len(got) == 0 || got[0] != "set_property time-pos 40.000000" {
			t.Fatalf("told mpv %q, want the party's position", got)
		}
		n.mpvObeys()
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != pause || got[0].Stamp != 6 {
			t.Fatalf("resumed with %+v, want a stamp newer than the party's", got)
		}
	})
}

// What is kept while loading is the newest by its stamp, not the last to arrive: members'
// messages come in any order, and an older one applied after a newer one would be answered
// as if its sender had fallen behind, by a node that hasn't even joined.
func TestTheNewestHeardWhileLoadingIsByStampNotArrival(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{joiner: true, heard: []IncomingMessage{
			peer("a", `{"stamp":5,"at":30,"name":"pause","data":"yes"}`),
			peer("b", `{"stamp":3,"event":"property-change","name":"time-pos","data":"10.000000"}`),
		}})
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property time-pos 30.000000]" {
			t.Fatalf("told mpv %q, want only the newer of the two", got)
		}
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q, want joined and nothing before it", got)
		}
	})
}

// What a joiner does before it has heard the party is not a member falling behind: its
// join is being answered, so nobody stops again or says so.
func TestAJoinersOwnChangeIsNotCaughtUp(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(3, true, 30)
		n.peerSays("j", fmt.Sprintf(`{"stamp":%d,"event":"property-change","name":"time-pos","data":"10.000000"}`, unheard+1))
		n.peerSays("j", fmt.Sprintf(`{"stamp":%d,"at":10,"name":"pause","data":"no"}`, unheard+2))
		if got, told := n.broadcasts(), n.mpvWasTold(); len(got) != 0 || len(told) != 0 {
			t.Fatalf("answered %q and told mpv %q, want nothing", said(got), told)
		}
	})
}

// A joiner that pressed play before it was answered is not out of sync with the party: it
// isn't in it yet, and stopping the party with its own second would drag the party there.
func TestAJoinerPlayingIsNotOutOfSync(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{joiner: true})
		n.plays()
		n.mpvSays(mpvAt(40))
		n.transportSays(IncomingMessage{HostName: "a", From: "a", Missed: true})
		n.peerSays("a", `{"stamp":2,"at":30,"name":"pause","data":"no"}`)
		if got := n.mpvWasTold(); slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "out of sync") }) {
			t.Fatalf("told mpv %q, want nothing until its join is answered", got)
		}
	})
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
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(3, false, 40)
			n.transportSays(IncomingMessage{HostName: "laptop", From: "l1", Missed: true})
			n.transportSays(laptop("l2", `{"stamp":3,"at":40,"name":"pause","data":"no"}`))
			if got := n.mpvWasTold(); len(got) != 0 {
				t.Fatalf("told mpv %q for l2, which missed nothing", got)
			}
			n.transportSays(laptop("l1", `{"stamp":3,"at":40,"name":"pause","data":"no"}`))
			if got := n.mpvWasTold(); len(got) == 0 || got[0] != "show-text paused: out of sync with laptop 5000" {
				t.Fatalf("told mpv %q, want the party stopped for l1", got)
			}
		})
	})
	t.Run("synced by", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(3, true, 40)
			n.transportSays(laptop("l1", `{"stamp":4,"at":40,"name":"pause","data":"yes"}`))
			n.transportSays(laptop("l2", `{"stamp":4,"at":40,"name":"time-pos","data":"40.000000"}`))
			n.transportSays(laptop("l1", `{"stamp":4,"at":40,"name":"time-pos","data":"40.000000"}`))
			if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text synced by laptop 5000]" {
				t.Fatalf("told mpv %q, want one synced by, for l1, whose state it took", got)
			}
		})
	})
}

// A message that comes while a joiner applies what it heard waits for all of it: the seek
// to where it continues, the party's state, the join, and the notices heard while loading.
func TestAMessageDuringTheJoinWaitsForTheNotices(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{at: 40, joiner: true, late: true, heard: []IncomingMessage{peer("a", `{"name":"show-text","data":"heard"}`)}})
		n.peerSays("a", `{"name":"show-text","data":"later"}`)
		n.mpvObeys()
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property time-pos 40.000000 show-text heard 5000 show-text later 5000]" {
			t.Fatalf("told mpv %q, want the notice heard while loading first", got)
		}
	})
}

// The person seeks to 50 and quits before the script says where the seek landed: a script's
// last messages are lost as mpv closes (ORDER.md), so the party continues from the second mpv
// reported, not from the one it was on (e2e [G19d]).
func TestAPartyContinuesFromTheSecondMpvLastReported(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(`{"event":"seek"}`, mpvAt(50))
		if _, at := n.c.Position(); at != 50 {
			t.Fatalf("Position at %v, want 50", at)
		}
	})
}

// A member line comes only from a notice about its own sender, by the id the transport gave
// it: another's name in the text, or a name dressed as a notice, makes none.
func TestMemberLinesComeOnlyFromNoticesAboutTheirSender(t *testing.T) {
	t.Parallel()
	omar := IncomingMessage{HostName: "Omar", From: "ID1"}
	for _, c := range []struct {
		text string
		e    IncomingMessage
		want string
	}{
		{"Omar has joined", omar, `member joined: {"id":"ID1","name":"Omar"}`},
		{"connected to Omar", omar, `member here: {"id":"ID1","name":"Omar"}`},
		{"Omar has left", omar, `member left: {"id":"ID1","name":"Omar"}`},
		{"dropped 1.2.3.4:6969 (Omar)", IncomingMessage{HostName: "Omar", From: "1.2.3.4:6969"}, `member left: {"id":"1.2.3.4:6969","name":"Omar"}`},
		{"Lina has joined", omar, ""},
		{"resumed by Mallory has joined", IncomingMessage{HostName: "Mallory has joined", From: "ID3"}, ""},
		{"paused for Omar: left", omar, ""},
		{"dropped 9.9.9.9:1 (Omar)", omar, ""},
		{"Eve\n2026/11/21 00:00:00 init done has joined", IncomingMessage{HostName: "Eve\n2026/11/21 00:00:00 init done", From: "ID2"}, `member joined: {"id":"ID2","name":"Eve\n2026/11/21 00:00:00 init done"}`},
	} {
		got, ok := memberLine(c.text, c.e)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("memberLine(%q, %+v) = %q, %v; want %q", c.text, c.e, got, ok, c.want)
		}
	}
}
