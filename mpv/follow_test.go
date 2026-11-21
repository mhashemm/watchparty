package mpv

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

// A stop nobody chose has no leader, and the first seek leads: one that answers a join or a
// leave, as each node's own. A joiner is answered with it, so its seek goes out (e2e [5]);
// a leader that left leaves its stop with none, told by its id, as names may be shared.
func TestAStopNobodyChoseHasNoLeader(t *testing.T) {
	t.Parallel()
	free := func(t *testing.T, got []Event, name string) {
		t.Helper()
		for _, e := range got {
			if e.Name == name && e.Free {
				return
			}
		}
		t.Fatalf("broadcast %+v, want its %s marked free", got, name)
	}
	seekGoesOut := func(n *node) {
		n.t.Helper()
		n.mpvWasTold()
		n.mpvSays(mpvAt(50))
		// not free: whoever takes it follows this node, the first to seek
		if got := n.broadcasts(); fmt.Sprint(said(got)) != "[time-pos=50.000000]" || got[0].Free {
			n.t.Fatalf("broadcast %+v, want the person's seek, which leads", got)
		}
	}
	t.Run("a joiner answered", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{joiner: true})
			n.peerSays("a", `{"stamp":3,"paused":true,"at":30,"free":true,"event":"property-change","name":"time-pos","data":"30.000000"}`)
			n.mpvObeys()
			n.broadcasts()
			seekGoesOut(n)
		})
	})
	t.Run("answering a join, stopped", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(3, true, 30)
			n.peerSays("j", `{"name":"joined"}`)
			free(t, n.broadcasts(), timePos)
			seekGoesOut(n)
		})
	})
	t.Run("answering a join, playing", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.plays()
			n.peerSays("j", `{"name":"joined"}`)
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
			free(t, n.broadcasts(), pause)
		})
	})
	t.Run("a leave, playing", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.plays()
			n.peerSays("x", `{"name":"show-text","data":"x has left"}`)
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
			free(t, n.broadcasts(), pause)
		})
	})
	t.Run("the leader leaves", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(3, true, 30)
			n.peerSays("z", `{"name":"show-text","data":"z has left"}`)
			seekGoesOut(n)
		})
	})
	t.Run("another member of the leader's name leaves", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(3, true, 30)
			n.transportSays(IncomingMessage{HostName: "z", From: "z2", Event: []byte(`{"name":"show-text","data":"z has left"}`)})
			n.mpvWasTold()
			n.mpvSays(mpvAt(50))
			n.keepsTheSeek("z")
		})
	})
	t.Run("another member leaves", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(3, true, 30)
			n.peerSays("x", `{"name":"show-text","data":"x has left"}`)
			n.mpvWasTold()
			n.mpvSays(mpvAt(50))
			n.keepsTheSeek("z")
		})
	})
}

// A joiner is sent to 30 by an answer, which it doesn't follow, and the person seeks to 39
// and back to 30. Their seek to 39 means mpv is done with 30 (MPV.md), so the way back is
// theirs, not the answer's echo.
func TestASeekBackToWhereAnAnswerSentItIsThePersonsOwn(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{joiner: true})
		n.peerSays("a", `{"stamp":3,"paused":true,"at":30,"free":true,"event":"property-change","name":"time-pos","data":"30.000000"}`)
		n.mpvObeys()
		n.mpvSays(mpvAt(39))
		n.broadcasts()
		n.mpvSays(mpvAt(30))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=30.000000]" {
			t.Fatalf("broadcast %q, want the seek back to where the answer had sent it", got)
		}
	})
}

// A peer sends this node to 30, and the person seeks to 39 and back to 30: a follower keeps
// both on its screen, and tells the person once for that state, so a held key doesn't flood
// the GUI's feed; again once it takes the leader's next seek.
func TestASeekBackToWhereAPeerSentItIsThePersonsOwn(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("a", stamped(5, seekEvent(30)))
		n.mpvObeys()
		n.mpvWasTold()
		n.mpvSays(mpvAt(39))
		n.keepsTheSeek("a")
		n.mpvSays(mpvAt(30))
		if got, told := said(n.broadcasts()), n.mpvWasTold(); len(got) != 0 || len(told) != 0 {
			t.Fatalf("broadcast %q and told mpv %q, want nothing more", got, told)
		}
		n.peerSays("a", stamped(6, seekEvent(50)))
		n.mpvObeys()
		n.mpvWasTold()
		n.mpvSays(mpvAt(55))
		n.keepsTheSeek("a")
	})
}

// A peer sends this node to 20 and resumes the party; later the person stops and seeks back
// to 20. A resume taken from a peer ends the seeks this node applied, as its own does.
func TestASeekBackAfterAPeersResumeIsThePersonsOwn(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("a", stamped(5, seekEvent(20)))
		n.mpvObeys()
		n.peerSays("a", stamped(6, `{"event":"property-change","name":"pause","data":"no","at":20}`))
		n.mpvObeys()
		n.mpvSays(mpvAt(25), propertyChangeLine(pause, "yes", 0))
		n.broadcasts()
		n.mpvSays(`{"event":"seek"}`, mpvAt(20), `{"event":"playback-restart"}`)
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=20.000000]" {
			t.Fatalf("broadcast %q, want the seek back to where the peer had sent it", got)
		}
	})
}

// Only the person's own pause, seek or move leads a stop, while playing too: the party stops
// for it first. Any other stop is free, and the first seek leads: mpv moving on from the end
// of an entry (two players reach the end together, and the tie's winner would lead, e2e
// [R15c]), or running out of cache.
func TestOnlyThePersonsPauseOrSeekLeads(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		does func(n *node)
		free bool
	}{
		"the person's pause": {func(n *node) {
			n.plays()
			n.mpvSays(propertyChangeLine(pause, "yes", 0))
		}, false},
		"the person's seek": {func(n *node) { n.mpvSays(mpvAt(30)) }, false},
		"a seek while playing": {func(n *node) {
			n.plays()
			n.mpvSays(`{"event":"seek"}`, mpvAt(50), `{"event":"playback-restart"}`)
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
		}, false},
		"a move at the end": {func(n *node) {
			n.plays()
			n.mpvSays(mpvAt(118), `{"event":"end-file","reason":"eof"}`, propertyChangeLine(playlistPos, "1", 0))
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
		}, true},
		"a move by hand": {func(n *node) {
			n.plays()
			n.mpvSays(mpvAt(90), propertyChangeLine(playlistPos, "1", 0))
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
		}, false},
		"a move while stopped": {func(n *node) { n.mpvSays(propertyChangeLine(playlistPos, "1", 0)) }, false},
		"the person's pause after a stop nobody chose": {func(n *node) {
			n.plays()
			n.mpvSays(propertyChangeLine(pausedForCache, "yes", 0))
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
			n.mpvSays(propertyChangeLine(pausedForCache, "no", 0))
			n.plays()
			n.mpvSays(propertyChangeLine(pause, "yes", 0))
		}, false},
		"a peer's resume refused while out of cache": {func(n *node) {
			n.plays()
			n.mpvSays(propertyChangeLine(pausedForCache, "yes", 0))
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
			n.broadcasts()
			n.peerSays("a", stamped(20, `{"event":"property-change","name":"pause","data":"no","at":0,"entry":0}`))
		}, true},
		"running out of cache": {func(n *node) {
			n.plays()
			n.mpvSays(propertyChangeLine(pausedForCache, "yes", 0))
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{names: threeVideos})
				c.does(n)
				got := n.broadcasts()
				if len(got) == 0 {
					t.Fatal("broadcast nothing")
				}
				for _, e := range got {
					if e.Free != c.free {
						t.Fatalf("broadcast %+v, want free %v", e, c.free)
					}
				}
			})
		})
	}
}

// Everyone hears the answer to a joiner, and it can arrive after someone has moved on. It
// carries the stamp its sender had, so one sent just before two people crossed is older
// than both moves and sends nobody to the other's entry. One from someone who knows of a
// later move is followed by anyone, joiner or not.
func TestAnAnswerIsFollowedOnlyIfItIsNewer(t *testing.T) {
	t.Parallel()
	t.Run("an older answer reaching a member", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos, start: 1})
			n.takes(3, true, 0)
			n.peerSays("a", move(2, 2))
			if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text paused for a: catching up 5000]" {
				t.Fatalf("told mpv %q, want the party stopped for a, and no move", got)
			}
		})
	})
	t.Run("a fellow joiner's, to a joiner who moved by hand", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos, start: 1, joiner: true})
			n.mpvSays(propertyChangeLine(playlistPos, "0", 0))
			n.broadcasts()
			n.peerSays("a", move(unheard, 2))
			if got := n.mpvWasTold(); len(got) != 0 {
				t.Fatalf("told mpv %q, want nothing", got)
			}
		})
	})
	t.Run("a newer answer reaching a member", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos, start: 1})
			n.takes(3, true, 0)
			// an answer is a stop nobody chose: this node doesn't follow it, and its move goes out
			n.peerSays("a", `{"stamp":4,"free":true,"event":"property-change","name":"playlist-pos","data":"0"}`)
			if got := n.mpvWasTold(); len(got) == 0 || got[0] != "set_property playlist-pos 0" {
				t.Fatalf("told mpv %q, want to follow it", got)
			}
			n.mpvObeys()
			n.mpvSays(propertyChangeLine(playlistPos, "2", 0))
			if got := n.broadcasts(); len(got) != 1 || got[0].Stamp != 5 {
				t.Fatalf("moved with %+v, want stamp 5: one past the one it followed", got)
			}
		})
	})
}

// A newer stamp for the entry this node is already on moves nothing, and is still taken:
// its next move must beat it.
func TestANewerStampForTheSameEntryIsTaken(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 1})
		// mpv moving a on from the end: nobody leads, so this node's move goes out
		n.peerSays("a", `{"stamp":5,"free":true,"event":"property-change","name":"playlist-pos","data":"1"}`)
		if got := n.mpvWasTold(); len(got) != 0 {
			t.Fatalf("told mpv %q, want nothing for the entry it is on", got)
		}
		n.mpvSays(propertyChangeLine(playlistPos, "2", 0))
		if got := n.broadcasts(); len(got) != 1 || got[0].Stamp != 6 {
			t.Fatalf("moved with %+v, want one past the highest it heard", got)
		}
	})
}

// A pause, a resume or a seek older than what this node holds comes from a node that
// missed something. It isn't applied: the party stops if it was playing, says why, and
// tells that node where it is, as a seek, which that node takes as newer than its own.
func TestAnOlderChangeIsAnsweredNotApplied(t *testing.T) {
	t.Parallel()
	for name, old := range map[string]string{
		"a resume": `{"stamp":2,"at":10,"name":"pause","data":"no"}`,
		"a seek":   `{"stamp":2,"name":"time-pos","data":"10.000000"}`,
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.takes(3, true, 30)
				n.peerSays("b", old)
				if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text paused for b: catching up 5000]" {
					t.Fatalf("told mpv %q, want only why", got)
				}
				if got := n.broadcasts(); len(got) != 1 || got[0].Name != timePos || got[0].Data != "30.000000" || got[0].Stamp != 3 {
					t.Fatalf("answered %+v, want where the party is, with its stamp", got)
				}
			})
		})
	}
	t.Run("a pause, to a party playing", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(3, false, 30)
			n.peerSays("b", `{"stamp":2,"at":10,"name":"pause","data":"yes"}`)
			// why first, so the GUI's feed doesn't take the pause for the person's own
			if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text paused for b: catching up 5000 set_property pause true]" {
				t.Fatalf("told mpv %q", got)
			}
			n.mpvObeys()
			// its own pause is the answer: newer, and where it stopped
			if got := n.broadcasts(); len(got) != 1 || got[0].Name != pause || got[0].Stamp != 4 || got[0].At == nil || *got[0].At != 30 {
				t.Fatalf("broadcast %+v, want its own pause, newer, where it stopped", got)
			}
		})
	})
}

// Two changes with the same stamp were made at the same moment, and everyone must pick
// the same one: stopped over playing, then the earlier second. The one that loses is
// told, quietly, since its sender may never have heard the winner.
func TestChangesMadeAtOnceGoToTheSaferOne(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		event, does string
		answers     bool
	}{
		"a resume loses to a seek":    {`{"stamp":3,"at":30,"name":"pause","data":"no"}`, "", true},
		"a later seek loses":          {`{"stamp":3,"name":"time-pos","data":"50.000000"}`, "", true},
		"a later stop loses":          {`{"stamp":3,"at":50,"name":"pause","data":"yes"}`, "", true},
		"an earlier seek wins":        {`{"stamp":3,"name":"time-pos","data":"20.000000"}`, "set_property time-pos 20.000000", false},
		"an earlier stop wins":        {`{"stamp":3,"at":20,"name":"pause","data":"yes"}`, "set_property time-pos 20.000000", false},
		"the same second is the same": {`{"stamp":3,"name":"time-pos","data":"40.000000"}`, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.takes(3, true, 40)
				n.peerSays("b", c.event)
				if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != fmt.Sprint(slices.DeleteFunc([]string{c.does}, func(s string) bool { return s == "" })) {
					t.Fatalf("seeks %q, want %q", got, c.does)
				}
				got := n.broadcasts()
				if c.answers && (len(got) != 1 || got[0].Name != timePos || got[0].Data != "40.000000" || got[0].Stamp != 3) {
					t.Fatalf("answered %+v, want where this node is", got)
				}
				if !c.answers && len(got) != 0 {
					t.Fatalf("broadcast %q, want nothing", said(got))
				}
			})
		})
	}
	t.Run("a seek made as a node resumed stops it", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(3, false, 40)
			n.peerSays("b", `{"stamp":3,"name":"time-pos","data":"50.000000"}`)
			n.mpvObeys()
			if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true set_property time-pos 50.000000 show-text paused by b 5000 show-text synced by b 5000]" {
				t.Fatalf("told mpv %q", got)
			}
		})
	})
}

// Where this node is, for a tie, is where it is heading: not any second it has been sent
// to since the last resume, which is kept only to know mpv's echo of that seek.
func TestATieIsWeighedAgainstWhereThisNodeIsHeading(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("a", `{"stamp":2,"name":"time-pos","data":"10.000000"}`)
		n.peerSays("a", `{"stamp":3,"name":"time-pos","data":"20.000000"}`)
		n.peerSays("b", `{"stamp":3,"at":10,"name":"pause","data":"yes"}`)
		if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 10.000000 set_property time-pos 20.000000 set_property time-pos 10.000000]" {
			t.Fatalf("seeks %q, want the earlier of two stops made at once, though this node was there before", got)
		}
	})
}

// Every node sends its whole state, so a message missing part of it is refused rather than
// filled in from this node's own: each of these is newer, and would move it.
func TestAMessageWithoutItsStateIsRefused(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.mpvSays(mpvAt(40))
		n.broadcasts()
		// raw, not through peer, which fills in what a node always sends
		for _, e := range []string{
			`{"stamp":3,"at":10,"entry":0,"name":"time-pos","data":"10.000000"}`,
			`{"stamp":3,"paused":true,"entry":0,"name":"pause","data":"yes"}`,
			`{"stamp":3,"paused":true,"at":10,"name":"time-pos","data":"10.000000"}`,
			`{"stamp":3,"paused":true,"at":0,"entry":3,"name":"playlist-pos","data":"3"}`,
			`{"stamp":3,"paused":true,"at":1e999,"entry":0,"name":"time-pos","data":"10.000000"}`,
		} {
			n.transportSays(IncomingMessage{HostName: "a", From: "a", Event: []byte(e)})
		}
		if got, told := n.broadcasts(), n.mpvWasTold(); len(got) != 0 || len(told) != 0 {
			t.Fatalf("broadcast %q and told mpv %q for messages without their state", said(got), told)
		}
	})
}

// A resume reaching a node that plays says it is out of step: newer, it missed the pause
// in between, which no stamp shows; a tie from a member something this node sent never
// reached, it resumed from what it last heard. The party stops, each where it is, and
// settles on the earliest; nobody is moved by the resume.
func TestAResumeToANodeThatPlaysStopsTheParty(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		resume string
		missed bool
	}{
		"newer":                     {`{"stamp":4,"at":30,"name":"pause","data":"no"}`, false},
		"newer, from where it is":   {`{"stamp":4,"at":40,"name":"pause","data":"no"}`, false},
		"a tie, from one it missed": {`{"stamp":3,"at":40,"name":"pause","data":"no"}`, true},
		"a tie, from just ahead":    {`{"stamp":3,"at":40.15,"name":"pause","data":"no"}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.takes(3, false, 40)
				if c.missed {
					n.transportSays(IncomingMessage{HostName: "a", From: "a", Missed: true})
				}
				n.peerSays("a", c.resume)
				if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text paused: out of sync with a 5000 set_property pause true]" {
					t.Fatalf("told mpv %q", got)
				}
				n.mpvObeys()
				var r Event
				json.Unmarshal([]byte(c.resume), &r)
				if got := n.broadcasts(); len(got) != 1 || got[0].Name != pause || got[0].At == nil || *got[0].At != 40 || got[0].Stamp != r.Stamp+1 {
					t.Fatalf("broadcast %+v, want its own pause where it is, newer than the resume", got)
				}
			})
		})
	}
}

// A tie with the resume this node plays, from a member that heard it and resumed from the
// same stop, is the same resume made at once. Each stopped within timePosTolerance of the
// party's second, so the two can name seconds twice that apart.
func TestATieFromAMemberThatMissedNothingMovesNothing(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(3, false, 40)
		n.peerSays("a", `{"stamp":3,"at":39.82,"name":"pause","data":"no"}`)
		if got := n.mpvWasTold(); len(got) != 0 {
			t.Fatalf("told mpv %q, want nothing for the same resume", got)
		}
	})
}

// Two resumes with one stamp from different seconds were made from different stops: the
// person at one node resumed while it was still being moved to where the other stopped
// (SIM_SEED=10223). Neither is newer, so each would play on, that far apart for good. The
// party stops, each where it is, and settles on the earliest. The resume this node plays
// is its own or one it took. Sent again or back from losing the relay, a resume says where
// its sender is now, not where it resumed from, so it is weighed by the rules for those.
func TestATieFromAnotherSecondStopsTheParty(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		own    bool
		resume string
		want   string
	}{
		"taken":                  {false, `{"stamp":3,"at":30,"name":"pause","data":"no"}`, "[show-text paused: out of sync with a 5000 set_property pause true]"},
		"its own":                {true, `{"stamp":3,"at":40,"name":"pause","data":"no"}`, "[show-text paused: out of sync with a 5000 set_property pause true]"},
		"from ahead":             {false, `{"stamp":3,"at":50,"name":"pause","data":"no"}`, "[show-text paused: out of sync with a 5000 set_property pause true]"},
		"back from losing relay": {false, `{"stamp":3,"at":30,"check":true,"name":"pause","data":"no"}`, "[]"},
		"sent again":             {false, `{"stamp":3,"at":30,"again":true,"name":"pause","data":"no"}`, "[]"},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				if c.own {
					// the person moves on from the stop it took, which a follower keeps, then resumes
					// from there
					n.takes(2, true, 40)
					n.mpvSays(mpvAt(45))
					n.mpvSays(propertyChangeLine(pause, "no", 0))
					if got := n.broadcasts(); len(got) != 1 || got[0].Stamp != 3 || *got[0].At != 45 {
						t.Fatalf("broadcast %+v, want its resume from 45 with stamp 3", got)
					}
					n.mpvWasTold()
				} else {
					n.takes(3, false, 40)
				}
				n.peerSays("a", c.resume)
				if got := n.mpvWasTold(); fmt.Sprint(got) != c.want {
					t.Fatalf("told mpv %q, want %s", got, c.want)
				}
			})
		})
	}
}

// The state sent again to a member the mesh missed goes to everyone, and can cross a newer
// change on its way. Older, it is let go: its sender only hasn't heard that change yet. An
// older message that isn't sent again still says its sender is behind.
func TestAnOlderStateSentAgainIsLetGo(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		event string
		want  string
	}{
		"sent again": {`{"stamp":2,"at":30,"again":true,"name":"pause","data":"yes"}`, "[]"},
		"not":        {`{"stamp":2,"at":30,"name":"pause","data":"yes"}`, "[show-text paused for a: catching up 5000]"},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.takes(3, true, 40)
				n.peerSays("a", c.event)
				if got := n.mpvWasTold(); fmt.Sprint(got) != c.want {
					t.Fatalf("told mpv %q, want %s", got, c.want)
				}
			})
		})
	}
}

// A member that something this node sent never reached is sent its state again, as a
// pause marked again, each time the transport asks, and to that member alone: it goes
// back to the transport, not out to the party. Being told it missed something sends
// nothing: that comes right after the send that failed. A joiner has no state yet, and a
// state taken from that very member it has.
func TestAMemberThatMissedSomethingIsSentTheStateAgain(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		paused bool
		joiner bool
		by     string
		want   string
	}{
		"stopped":       {true, false, "z", "yes"},
		"playing":       {false, false, "z", "no"},
		"taken from b":  {false, false, "b", "no"},
		"joining":       {true, true, "", ""},
		"taken from it": {false, false, "a", ""},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{joiner: c.joiner})
				if c.joiner {
					n.mpvSays(mpvAt(40))
					n.broadcasts()
				} else {
					n.takesFrom(c.by, 3, c.paused, 40)
				}
				replies := make(chan []byte, 2)
				n.transportSays(IncomingMessage{HostName: "a", From: "a", Missed: true})
				n.transportSays(IncomingMessage{HostName: "a", From: "a", Resend: true, Reply: replies})
				n.transportSays(IncomingMessage{HostName: "a", From: "a", Resend: true, Reply: replies})
				if got := n.mpvWasTold(); len(got) != 0 {
					t.Fatalf("told mpv %q, want nothing", got)
				}
				for range 2 {
					b := <-replies
					if c.want == "" {
						if b != nil {
							t.Fatalf("handed back %s, want nothing to send", b)
						}
						continue
					}
					e := Event{}
					json.Unmarshal(b, &e)
					if e.Name != pause || !e.Again || e.Data != c.want || e.Stamp != 3 || *e.Paused != c.paused || *e.At != 40 {
						t.Fatalf("handed back %+v, want its state again", e)
					}
				}
				if got := n.broadcasts(); len(got) != 0 {
					t.Fatalf("broadcast %q, want it sent to a alone", said(got))
				}
			})
		})
	}
}

// What was sent to this node looked lost to its sender, which sends it again: the same
// resume, reaching a node that already plays the one it took from that sender. So the
// first one got here late, and this node plays behind: the party stops. One that plays a
// resume taken from someone else got it on time.
func TestAResumeSentAgainThatThisNodeTookFromItsSenderStopsTheParty(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		by   string
		want string
	}{"taken from a": {"a", "[show-text paused: out of sync with a 5000 set_property pause true]"}, "taken from c": {"c", "[]"}} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.takesFrom(c.by, 2, false, 30)
				n.peerSays("a", `{"stamp":2,"at":40,"again":true,"name":"pause","data":"no"}`)
				if got := n.mpvWasTold(); fmt.Sprint(got) != c.want {
					t.Fatalf("told mpv %q, want %s", got, c.want)
				}
			})
		})
	}
}

// A member that missed something stays missed until it sends something newer than this
// node holds, which it heard since or made, or joins again. Not when a send reaches it: a
// tie it sent before can still be on its way. Something older says nothing of it. Missed
// shows in what a tie from it does to a node that plays: it stops the party.
func TestAMemberThatMissedSomethingIsForgottenOnceItSendsNews(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		via    IncomingMessage
		missed bool
	}{
		"newer":    {peer("a", `{"stamp":4,"at":30,"name":"pause","data":"yes"}`), false},
		"rejoined": {peer("a", `{"name":"joined"}`), false},
		"a tie":    {peer("a", `{"stamp":3,"at":40,"name":"pause","data":"yes"}`), true},
		"older":    {peer("a", `{"stamp":2,"at":40,"name":"pause","data":"yes"}`), true},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.takes(3, true, 40)
				n.transportSays(IncomingMessage{HostName: "a", From: "a", Missed: true})
				n.transportSays(c.via)
				n.mpvObeys()
				n.broadcasts()
				n.mpvWasTold()
				n.mpvSays(propertyChangeLine(pause, "no", 0))
				resumed := n.broadcasts()[0]
				n.peerSays("a", fmt.Sprintf(`{"stamp":%d,"at":%f,"name":"pause","data":"no"}`, resumed.Stamp, *resumed.At))
				stopped := slices.Contains(n.mpvWasTold(), "show-text paused: out of sync with a 5000")
				if stopped != c.missed {
					t.Fatalf("a tie from a stopped the party: %v, want %v (a still missed)", stopped, c.missed)
				}
			})
		})
	}
}

// Two players reach the end of a file together, and each stops the party for it. One
// adopts the other's pause, then sends its own reason with the next stamp; the other's
// position, sent at the same moment, arrives after it. That message is older, but it
// says the state this node holds: its sender missed nothing, so the party isn't stopped.
func TestAnOlderMessageThatAgreesIsNotCaughtUp(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 2})
		n.takes(11, true, 0)
		n.peerSays("a", `{"stamp":10,"at":0,"entry":2,"name":"time-pos","data":"0.000000"}`)
		n.peerSays("a", `{"stamp":10,"at":0,"entry":2,"name":"pause","data":"yes"}`)
		if got := n.mpvWasTold(); len(got) != 0 {
			t.Fatalf("told mpv %q, want nothing for an older message that says what this node holds", got)
		}
	})
}

// A follower's seek to 20 replaces the leader's seek to 30 in mpv's queue: it lands with the
// set's id away from its second, so it is the person's, kept on the screen, and the person's
// resume plays from 20, where mpv is (SIM_SEED=2000545 found the order).
func TestAFollowersSeekOverTheLeadersIsKept(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(2, true, 40)
		n.mpvAnswersLate()
		n.peerSays("z", stamped(3, seekEvent(30)))
		n.mpvSays(`{"event":"seek"}`, mpvAt(20), `{"event":"playback-restart"}`)
		n.keepsTheSeek("z")
		n.mpvSays(`{"event":"client-message","args":["watchparty","pause","","no","20.000000"]}`)
		if got := n.broadcasts(); fmt.Sprint(said(got)) != "[pause=no]" || *got[0].At != 20 {
			t.Fatalf("broadcast %+v, want the person's resume, from 20", got)
		}
		n.mpvAnswers()
		if told := n.mpvWasTold(); slices.Contains(told, "set_property pause true") {
			t.Fatalf("told mpv %q, want it left playing", told)
		}
	})
}
