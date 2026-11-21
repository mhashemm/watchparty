package mpv

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// b applies 30 and then 40: mpv runs them as one seek to 40, or one after the other, and
// each landing carries b's id, so none goes out as b's own seek, or the whole party would
// jump back to 30. Seeking back there afterwards is the person's own.
func TestTwoSeeksAppliedTogetherSendNothing(t *testing.T) {
	t.Parallel()
	for name, apart := range map[string]bool{"folded into one": false, "one after the other": true} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.peerSays("a", stamped(5, seekEvent(30)))
				if apart {
					n.mpvObeys()
				}
				n.peerSays("a", stamped(6, seekEvent(40)))
				n.mpvObeys()
				if got := n.broadcasts(); len(got) != 0 {
					t.Fatalf("rebroadcast mpv's landing: %q", said(got))
				}
				n.mpvWasTold()
				n.mpvSays(mpvAt(30))
				n.keepsTheSeek("a")
			})
		})
	}
}

// --hr-seek=yes is pinned, so an echo is at most a frame off. A deliberate seek
// a fraction of a second away is still the user's own.
func TestSmallSeekIsNotAnEcho(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("a", stamped(5, seekEvent(12)))
		n.mpvWasTold()
		n.mpvSays(mpvAt(12.3))
		n.keepsTheSeek("a")
	})
}

// The person seeks or pauses while mpv takes a member's state: that change is newer than the
// state, and goes out; undone, it would leave this node alone where the member was, playing
// or on its second, as the party takes the change (SIM_SEED=10 found the first).
func TestAChangeMadeWhileTakingAStateIsNotUndone(t *testing.T) {
	t.Run("the resume after a seek", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.mpvAnswersLate()
			n.peerSays("a", stamped(5, `{"event":"property-change","name":"pause","data":"no","at":34}`))
			// the set seeks and plays at once: the person's seek lands after it, while playing
			n.mpvObeys(`{"event":"seek"}`, mpvAt(20), `{"event":"playback-restart"}`)
			if got := n.mpvWasTold(); !slices.Contains(got, "set_property pause true") {
				t.Fatalf("told mpv %q, want the party stopped", got)
			}
			if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes time-pos=20.000000 pause-reason=seek]" {
				t.Fatalf("broadcast %q, want its own seek, stopping the party there", got)
			}
		})
	})
	t.Run("the stop over a resume", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.plays()
			n.mpvWasTold()
			n.mpvAnswersLate()
			n.peerSays("a", stamped(5, `{"event":"property-change","name":"pause","data":"yes","at":0}`))
			n.mpvObeys(propertyChangeLine(pause, "no", 0))
			n.broadcasts()
			n.mpvSays(mpvAt(1))
			if got := said(n.broadcasts()); len(got) != 0 {
				t.Fatalf("a frame while playing broadcast %q: the node took itself for stopped", got)
			}
		})
	})
	t.Run("the resume over a pause", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.mpvAnswersLate()
			n.peerSays("a", stamped(5, `{"event":"property-change","name":"pause","data":"no","at":0}`))
			n.mpvObeys(propertyChangeLine(pause, "yes", 0))
			n.broadcasts()
			n.mpvSays(`{"event":"seek"}`, mpvAt(30), `{"event":"playback-restart"}`)
			if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=30.000000]" {
				t.Fatalf("a seek while stopped broadcast %q, want it to go out: the node took itself for playing", got)
			}
		})
	})
	t.Run("a seek while playing, before the peer's seek is sent", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.plays()
			n.mpvWasTold()
			n.mpvAnswersLate()
			n.peerSays("a", stamped(5, `{"event":"property-change","name":"pause","data":"yes","at":40}`))
			n.mpvObeys(propertyChangeLine(pause, "no", 0), `{"event":"seek"}`, mpvAt(90), `{"event":"playback-restart"}`)
			if got := said(n.broadcasts()); !slices.Contains(got, "pause-reason=seek") {
				t.Fatalf("broadcast %q, want its seek while playing to stop the party", got)
			}
		})
	})
	t.Run("the seek after a stop", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.plays()
			n.mpvWasTold()
			n.mpvAnswersLate()
			n.peerSays("a", stamped(5, `{"event":"property-change","name":"pause","data":"yes","at":40}`))
			n.mpvObeys(`{"event":"seek"}`, mpvAt(50), `{"event":"playback-restart"}`)
			// stopped on the member's state, it follows: the person's seek stays here, and the
			// stop's seek to the member's second still goes to mpv
			told := n.mpvWasTold()
			if got := seeksIn(told); fmt.Sprint(got) != "[set_property time-pos 40.000000]" {
				t.Fatalf("seeks %q, want the member's second", got)
			}
			if !slices.Contains(told, "show-text only a can seek until resumed 5000") {
				t.Fatalf("told mpv %q, want the person told who holds the seek", told)
			}
			if got := said(n.broadcasts()); len(got) != 0 {
				t.Fatalf("broadcast %q, want nothing", got)
			}
		})
	})
}

// mpv can refuse a set (the property is unavailable while it quits or between files).
// The echo marker for a pause that never happened matched the user's own next pause,
// which then never went out. The script hears only its seek refused (a stop from playing
// seeks its second), so mpv refuses both.
func TestARefusedPauseLeavesNoMarker(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvRefuses(setProperty, "property unavailable")
		n.peerSays("a", stamped(5, `{"event":"property-change","name":"pause","data":"yes","at":0}`))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q, want it to ask the party where it is", got)
		}
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes]" {
			t.Fatalf("broadcast %q, want the user's own pause", got)
		}
	})
}

// A seek mpv refused never happened: the user's own seek to that second, once mpv takes
// seeks again, is no echo and goes out. So does one for a file whose move mpv refused, or
// refused once that file loaded.
func TestARefusedSeekLeavesNoMarker(t *testing.T) {
	t.Parallel()
	userSeeks := func(n *node, pos float64) {
		n.mpvTakesAgain()
		n.mpvSays(`{"event":"seek"}`, mpvAt(pos), `{"event":"playback-restart"}`)
		if got := said(n.broadcasts()); fmt.Sprint(got) != fmt.Sprintf("[time-pos=%f]", pos) {
			t.Fatalf("broadcast %q, want the user's seek", got)
		}
	}
	t.Run("a seek", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.takes(7, true, 0)
			n.mpvRefuses("set_property time-pos", "property unavailable")
			n.peerSays("a", stamped(8, seekEvent(20)))
			n.broadcasts()
			userSeeks(n, 20)
		})
	})
	t.Run("a seek held for a move", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos})
			n.takes(7, true, 0)
			n.mpvRefuses("set_property playlist-pos", "property unavailable")
			n.peerSays("a", `{"stamp":8,"at":20,"entry":1,"event":"property-change","name":"time-pos","data":"20.000000"}`)
			// the script never reads a set's result: the set waits for a file that never opens,
			// until the node gives up on it
			time.Sleep(moveLimit)
			n.broadcasts()
			userSeeks(n, 20)
		})
	})
	t.Run("a seek once the file loaded", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos})
			n.takes(7, true, 0)
			n.mpvRefuses("set_property time-pos", "property unavailable")
			n.peerSays("a", `{"stamp":8,"at":20,"entry":1,"event":"property-change","name":"time-pos","data":"20.000000"}`)
			n.mpvObeys()
			if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 20.000000]" {
				t.Fatalf("seeks %q, want the seek made once the file loaded", got)
			}
			n.broadcasts()
			userSeeks(n, 20)
		})
	})
}

// mpv's first report of where it started can land after a peer already sent this node to
// another video. That report isn't the echo of the move, so it mustn't use up the marker,
// or the real echo goes out as this node moving the party.
func TestAnEarlierPositionDoesNotUseUpTheMarker(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, opening: []string{strings.TrimSpace(propertyChangeLine(pause, "yes", 0)), strings.TrimSpace(mpvAt(0))}})
		n.peerSays("a", move(5, 1))
		if got := n.mpvWasTold(); len(got) == 0 || got[0] != "set_property playlist-pos 1" {
			t.Fatalf("told mpv %q", got)
		}
		n.mpvSays(propertyChangeLine(playlistPos, "0", 0), propertyChangeLine(playlistPos, "1", 0), started(1), `{"event":"file-loaded"}`, `{"event":"playback-restart"}`)
		if got := said(n.broadcasts()); len(got) != 0 {
			t.Fatalf("broadcast %q, want nothing before the resume", got)
		}
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=no]" {
			t.Fatalf("broadcast %q, want the resume", got)
		}
	})
}

// mpv refuses a step while it quits or between files, and the script hears its seek refused.
// commit marked every step before mpv was told, so whatever mpv never did is let go of: a seek
// marker would say this node is on a second it never reached. It holds the party's stamp
// where mpv isn't, so it asks the party where it is; not once mpv is gone.
func TestARefusedStepLeavesNoMarker(t *testing.T) {
	t.Parallel()
	t.Run("a seek", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos})
			n.mpvSays(mpvAt(5))
			n.broadcasts()
			n.mpvRefuses(setProperty, "property unavailable")
			n.peerSays("a", `{"stamp":3,"paused":true,"at":30,"entry":0,"name":"time-pos","data":"30.000000"}`)
			if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
				t.Fatalf("broadcast %q, want it to ask the party where it is", got)
			}
			// not on its way to 30: asked where it is, it is still at 5, and the person's seek goes out
			n.peerSays("j", `{"name":"joined"}`)
			if got := n.broadcasts(); len(got) != 1 || got[0].At == nil || *got[0].At != 5 {
				t.Fatalf("answered %+v, want 5", got)
			}
			n.mpvSays(mpvAt(12))
			if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=12.000000]" {
				t.Fatalf("broadcast %q, want the person's seek", got)
			}
		})
	})
	t.Run("anything, with mpv gone", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos})
			n.plays()
			n.mpvQuitsOn(setProperty)
			n.peerSays("a", `{"stamp":3,"paused":true,"at":30,"entry":2,"name":"time-pos","data":"30.000000"}`)
			if got := n.broadcasts(); len(got) != 0 {
				t.Fatalf("broadcast %q, want nothing: it is leaving", said(got))
			}
		})
	})
}

// A member's change is applied to its end, each step as mpv takes it, before the next
// message is weighed: weighed in between, it would meet a node half way there.
func TestAMessageWaitsForTheChangeBeforeIt(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvAnswersLate()
		n.peerSays("a", `{"stamp":5,"at":30,"name":"pause","data":"yes"}`)
		n.peerSays("b", `{"stamp":6,"at":40,"name":"pause","data":"yes"}`)
		n.mpvObeys()
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true set_property time-pos 30.000000 show-text paused by a 5000 set_property time-pos 40.000000]" {
			t.Fatalf("told mpv %q, want a's change whole, then b's", got)
		}
	})
}

// mpv hanging up while a member's change waits on it asks nobody anything, then or later:
// the node is leaving, and a request it never got an answer to is not a refusal.
func TestMpvGoneMidChangeAsksNobodyEvenLater(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvQuitsOn(setProperty)
		n.peerSays("a", `{"stamp":3,"at":30,"name":"pause","data":"yes"}`)
		time.Sleep(10 * time.Second)
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q, want nothing: it is leaving", said(got))
		}
	})
}

// mpv folds a seek into the one right behind it: a peer sends this node to 70, and before
// mpv gets there the person seeks back to 3, where mpv already was. mpv says it began a
// seek and played back, and reports no second, as it never changed (MPV.md); or reports 3,
// which a marker of the person's earlier seek there would swallow (SIM_SEED=60097). Asked
// where it is, mpv says 3: the person's seek, which a follower keeps on its screen.
func TestAFoldedSeekBackIsThePersonsOwn(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		playing bool     // the person's first seek to 3 is made while playing, which marks it
		reports []string // what mpv says once the two seeks fold
	}{
		"nothing reported":     {false, []string{`{"event":"seek"}`, `{"event":"playback-restart"}`}},
		"reported on a marker": {true, []string{`{"event":"seek"}`, mpvAt(3), `{"event":"playback-restart"}`}},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				if c.playing {
					n.plays()
					n.mpvSays(`{"event":"seek"}`, mpvAt(3), `{"event":"playback-restart"}`)
					n.mpvObeys()
				} else {
					n.mpvSays(mpvAt(3))
				}
				got := n.broadcasts()
				if len(got) == 0 {
					t.Fatal("broadcast nothing for the person's seek to 3")
				}
				stamp := got[0].Stamp + 1
				n.mpvWasTold()
				n.peerSays("a", stamped(stamp, seekEvent(70)))
				if told := n.mpvWasTold(); len(told) == 0 || told[0] != "set_property time-pos 70.000000" {
					t.Fatalf("told mpv %q, want the peer's seek", told)
				}
				n.mpvSays(c.reports...)
				// the person's seek back to 3, which a follower keeps
				n.keepsTheSeek("a")
			})
		})
	}
	// the person seeks on to 5 once the folded seek landed: theirs too, and kept
	t.Run("moved meanwhile", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.peerSays("a", stamped(5, seekEvent(70)))
			n.mpvSays(`{"event":"seek"}`, mpvAt(3), `{"event":"playback-restart"}`, `{"event":"seek"}`, mpvAt(5), `{"event":"playback-restart"}`)
			n.keepsTheSeek("a")
		})
	})
	// a peer's resume from 70: this node's seek there lands as it plays, with its id, and is
	// no seek of the person's to stop the party for
	t.Run("playing", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.mpvSays(mpvAt(30))
			n.broadcasts()
			n.peerSays("a", `{"stamp":5,"at":70,"name":"pause","data":"no"}`)
			n.mpvObeys()
			if got := n.mpvWasTold(); slices.Contains(got, "set_property pause true") {
				t.Fatalf("told mpv %q, want it left playing", got)
			}
			if got := n.broadcasts(); len(got) != 0 {
				t.Fatalf("broadcast %q, want nothing", said(got))
			}
		})
	})
	// mpv landed the seek that carried this node's on its second: nothing goes out, and a
	// report of that second after it is no seek either
	t.Run("landed where sent", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.peerSays("a", stamped(5, seekEvent(70)))
			n.mpvSays(`{"event":"seek"}`, mpvAt(70), `{"event":"playback-restart"}`)
			if got := n.broadcasts(); len(got) != 0 {
				t.Fatalf("broadcast %q, want nothing", said(got))
			}
			n.mpvSays(mpvAt(70))
			if got := n.broadcasts(); len(got) != 0 {
				t.Fatalf("broadcast %q, want nothing", said(got))
			}
		})
	})
}

// A peer's stop at 60 reaches this node while it plays on 60; but mpv plays on until the stop
// lands, to 60.3 or a frame on. The set seeks the stop's second with the stop, so the node
// stops on it, as its own step, and says nothing (SIM_SEED=514736544449).
func TestAStopThatLandsOffItsSecondIsSeekedBack(t *testing.T) {
	t.Parallel()
	for name, played := range map[string][]string{
		"0.3s on":    {mpvAt(60.1), mpvAt(60.2), mpvAt(60.3)},
		"a frame on": {mpvAt(60.04)},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.mpvSays(mpvAt(60))
				n.plays()
				n.mpvAnswersLate()
				n.peerSays("a", `{"stamp":9,"at":60,"name":"pause","data":"yes"}`)
				if told := n.mpvWasTold(); fmt.Sprint(told) != "[set_property pause true set_property time-pos 60.000000]" {
					t.Fatalf("told mpv %q, want the stop on its second", told)
				}
				n.mpvSays(played...)
				n.mpvObeys()
				if got := n.broadcasts(); len(got) != 0 {
					t.Fatalf("broadcast %q, want nothing", said(got))
				}
			})
		})
	}
}

// A peer's stop reaches a node that plays, and the person presses pause and play again before
// the script's ack: what the script says while a set is on its way is from before it, which
// the set overwrites, so nothing goes out and the node stops with the party
// (SIM_SEED=13000355 found the order).
func TestKeysBeforeAGoIsAckedAreOverwritten(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(2, false, 30)
		n.mpvAnswersLate()
		n.peerSays("a", `{"stamp":3,"at":30,"name":"pause","data":"yes"}`)
		if told := n.mpvWasTold(); fmt.Sprint(told) != "[set_property pause true set_property time-pos 30.000000]" {
			t.Fatalf("told mpv %q, want the stop on its second", told)
		}
		key := `{"event":"client-message","args":["watchparty","pause","",%q,"30.000000"]}`
		n.mpvSays(fmt.Sprintf(key, "yes"), fmt.Sprintf(key, "no"))
		n.mpvObeys()
		if got, told := n.broadcasts(), n.mpvWasTold(); len(got) != 0 || fmt.Sprint(told) != "[show-text paused by a 5000]" {
			t.Fatalf("broadcast %q and told mpv %q, want only the stop shown: it overwrote the keys", said(got), told)
		}
	})
}

// A peer's stop or resume, and the person's key undoing it right after the script's set: its
// change mark comes after the ack, and differs from what this node holds, so it goes out,
// newer than the peer's. Swallowed as the echo, this node would sit apart from a party that
// believes it followed.
func TestAKeyUndoingAPeersChangeAsItIsSetGoesOut(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		paused bool
		want   string
	}{"a stop": {true, "[pause=no]"}, "a resume": {false, "[pause=yes]"}} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.takes(2, !c.paused, 30)
				n.personUndoesThePause()
				n.peerSays("a", stamped(3, fmt.Sprintf(`{"at":30,"name":"pause","data":%q}`, holds(c.paused))))
				n.mpvObeys()
				if got := n.broadcasts(); fmt.Sprint(said(got)) != c.want || got[0].Stamp != 4 {
					t.Fatalf("broadcast %+v, want the person's key, newer than the peer's", got)
				}
			})
		})
	}
}

// The person seeks to 60 just before the script plays a peer's resume from 30: the ack says
// mpv plays from 60, which isn't where this node was, so it is the person's seek while
// playing, and it stops the party there, led by this node (SIM_SEED=467222401814 found the
// order).
func TestAPlayAckedAwayFromItsSecondIsThePersonsSeek(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(2, true, 30)
		n.mpvAnswersLate()
		n.peerSays("a", `{"stamp":3,"at":30,"name":"pause","data":"no"}`)
		if told := n.mpvWasTold(); fmt.Sprint(told) != "[set_property pause false]" {
			t.Fatalf("told mpv %q, want the play", told)
		}
		n.mpvSays(`{"event":"seek"}`, mpvAt(60), `{"event":"playback-restart"}`)
		n.mpvObeys()
		got := n.broadcasts()
		if fmt.Sprint(said(got)) != "[pause=yes time-pos=60.000000 pause-reason=seek]" || got[0].Stamp != 4 || got[0].Free {
			t.Fatalf("broadcast %+v, want the party stopped at 60 for the person's seek, newer", got)
		}
	})
}

// The person resumes between the script's seek to 60 and its ack: the ack says mpv plays, so
// the resume goes out, from 60, where this node is heading. Said from 30, the others played
// from 30 and this node from 60.
func TestAResumeWhileASeekLandsPlaysFromItsSecond(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(2, true, 30)
		n.mpvAnswersLate()
		n.peerSays("a", stamped(3, seekEvent(60)))
		if told := n.mpvWasTold(); fmt.Sprint(told) != "[set_property time-pos 60.000000]" {
			t.Fatalf("told mpv %q, want the seek", told)
		}
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		n.mpvObeys()
		if got := n.broadcasts(); fmt.Sprint(said(got)) != "[pause=no]" || *got[0].At != 60 {
			t.Fatalf("broadcast %+v, want the person's resume, from 60", got)
		}
	})
}

// A peer's stop at 60 reaches this node while it plays on 30: one set stops it and seeks 60,
// so the person's resume right after plays from 60, where mpv is (SIM_SEED=736469209410 found
// the order when the stop and the seek were sent apart).
func TestAResumeRightAfterAStopPlaysFromItsSecond(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(2, false, 30)
		n.mpvAnswersLate()
		n.peerSays("a", `{"stamp":3,"at":60,"name":"pause","data":"yes"}`)
		if told := n.mpvWasTold(); fmt.Sprint(told) != "[set_property pause true set_property time-pos 60.000000]" {
			t.Fatalf("told mpv %q, want the stop on its second", told)
		}
		n.mpvObeys(propertyChangeLine(pause, "no", 0))
		if got := n.broadcasts(); fmt.Sprint(said(got)) != "[pause=no]" || *got[0].At != 60 {
			t.Fatalf("broadcast %+v, want the person's resume, from 60", got)
		}
	})
}

// The script acks a set after this node gave up on it (no ack in 5s): the node asked the party
// again, and the ack is for nothing on its way, so nothing goes out for it, and the next
// message is taken as usual.
func TestAnAckAfterTheGoWasGivenUpIsDropped(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvAnswersLate()
		n.peerSays("a", `{"stamp":5,"at":30,"name":"pause","data":"yes"}`)
		time.Sleep(6 * time.Second)
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q, want it to ask the party again", got)
		}
		n.mpvWasTold()
		n.mpvAnswers()
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q for an ack of a go given up", said(got))
		}
		n.peerSays("a", stamped(6, seekEvent(40)))
		if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 40.000000]" {
			t.Fatalf("seeks %q, want the next message taken", got)
		}
	})
}

// The person presses space just before a peer's stop reaches mpv, before this node has read
// it: the stop undoes it, and the ack says so. Made as the party changed, the resume would lose
// to the stop anyway, so nothing goes out, and the person is told why their key did nothing.
func TestAKeyAGoUndoesIsShown(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.personPressesFirst()
		n.peerSays("a", `{"stamp":5,"at":30,"name":"time-pos","data":"30.000000","paused":true}`)
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q, want the undone resume kept from the party", said(got))
		}
		if got := n.mpvWasTold(); !slices.Contains(got, "show-text "+overruled+" 5000") {
			t.Fatalf("told mpv %q, want the person told", got)
		}
	})
}

// The person presses space while a peer's resume from 30 is on its way, after the set's stop
// and before mpv answers its seek: mpv plays before the set plays it, so the key is lost in the
// set, and the ack says so (over, ORDER.md). The person is told why it did nothing.
func TestASpaceBetweenAGosStopAndItsPlayIsShown(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvAnswersLate()
		n.peerSays("a", `{"stamp":5,"at":30,"name":"pause","data":"no","paused":false}`)
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		n.mpvObeys()
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q, want nothing: mpv plays, as the party does", said(got))
		}
		if got := n.mpvWasTold(); !slices.Contains(got, "show-text "+overruled+" 5000") {
			t.Fatalf("told mpv %q, want the person told", got)
		}
	})
}

// A seek of the person's just before a set that doesn't seek is no key lost: the ack says where
// mpv went, and it goes out as theirs (landed). Its ack says it came first, but nothing is shown.
func TestAKeyTheAckRevealsIsNotShown(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.personSeeksFirst(50)
		n.peerSays("a", `{"stamp":5,"at":0,"name":"pause","data":"no","paused":false}`)
		got := n.broadcasts()
		if !slices.Contains(said(got), "time-pos=50.000000") {
			t.Fatalf("broadcast %q, want the person's seek", said(got))
		}
		if told := n.mpvWasTold(); slices.Contains(told, "show-text "+overruled+" 5000") {
			t.Fatalf("told mpv %q, want no notice: the key went out", told)
		}
	})
}

// A peer's stop reaches this node while its own stop for the person's seek is on its way: the
// message waits for that set's ack, so this node's stop goes out, with where and why, before the
// peer's is weighed. Taken inside the wait, it sent a second set, the first ack went unweighed,
// and the reason stayed: the person's next resume said stopped (SIM_SEED=12000065).
func TestAMessageWaitsForTheGoOnItsWay(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvAnswersLate()
		n.mpvSays(`{"event":"seek"}`, mpvAt(38), `{"event":"playback-restart"}`)
		n.peerSays("b", `{"stamp":3,"at":30,"name":"pause","data":"yes"}`)
		n.mpvAnswers()
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes time-pos=38.000000 pause-reason=seek]" {
			t.Fatalf("broadcast %q, want this node's stop, where and why", got)
		}
		n.mpvObeys()
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != pause || *got[0].Paused {
			t.Fatalf("broadcast %+v, want the person's resume, playing", got)
		}
	})
}

// A peer's resume reaches this node while its seek to 35 still lands, and the person's seek to
// 30 folds into that seek while the resume is on its way: mpv plays from 30. The ack is weighed
// against the second this node held as it sent the resume, 35, so it is the person's seek while
// playing, and it stops the party. Weighed against where it stood before, 30, it played apart
// (SIM_SEED=92000517).
func TestASeekFoldedWhileAResumeIsOnItsWayStopsTheParty(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(2, true, 30)
		n.peerSays("a", stamped(3, seekEvent(35)))
		n.mpvWasTold()
		n.mpvAnswersLate()
		n.peerSays("a", `{"stamp":4,"at":35,"name":"pause","data":"no"}`)
		n.mpvSays(`{"event":"seek"}`, mpvAt(30), `{"event":"playback-restart"}`)
		n.mpvAnswers()
		n.mpvAnswers()
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes time-pos=30.000000 pause-reason=seek]" {
			t.Fatalf("broadcast %q, want the party stopped at 30 for the person's seek", got)
		}
	})
}

// A peer sent this node to 38, then to 7, and the person seeked back to 38: mpv folds the
// two, and lands on 38 with the seek to 7's id, the person's seek. The person presses play
// as it lands: mpv has run the seek to 7 already, so the resume plays from where mpv is
// (SIM_SEED=388032164346). Said from 7, it played on from 38.
func TestAResumeAfterAFoldedSeekPlaysFromWhereMpvIs(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(2, true, 30)
		n.peerSays("a", stamped(3, seekEvent(38)))
		n.mpvObeys()
		n.mpvWasTold()
		n.peerSays("a", stamped(4, seekEvent(7)))
		if told := n.mpvWasTold(); len(told) == 0 || told[0] != "set_property time-pos 7.000000" {
			t.Fatalf("told mpv %q, want the seek", told)
		}
		n.mpvSays(`{"event":"seek"}`, mpvAt(38), `{"event":"playback-restart"}`, propertyChangeLine(pause, "no", 0))
		if got := n.broadcasts(); fmt.Sprint(said(got)) != "[pause=no]" || *got[0].At != 38 {
			t.Fatalf("broadcast %+v, want the person's resume, from 38", got)
		}
	})
}

// The person presses play and pause again before the script's set to 82 is acked: what the
// script says meanwhile is from before the set, or from it, so nothing goes out, and mpv lands
// where this node holds (SIM_SEED=1000074 found the order).
func TestKeysBeforeASeekIsAckedLeaveItsSecond(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(2, true, 30)
		n.mpvAnswersLate()
		n.peerSays("a", stamped(3, seekEvent(82)))
		key := `{"event":"client-message","args":["watchparty","pause","",%q,"30.000000"]}`
		n.mpvSays(fmt.Sprintf(key, "no"), fmt.Sprintf(key, "yes"))
		n.mpvObeys()
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q, want nothing: mpv lands where this node holds", said(got))
		}
		if _, at := n.c.Position(); at != 82 {
			t.Fatalf("Position at %v, want 82", at)
		}
	})
}

// The person seeks to 5 while stopped and presses play at once: mpv reports the play before
// the seek lands, so the script says where a seek goes at its start, which is before the play.
// Told at the landing, the seek read as one made while playing, and paused the party
// (e2e [R6c]).
func TestASeekAndAResumeAtOnceIsASeekThenAResume(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(`{"event":"seek"}`, mpvAt(5),
			`{"event":"client-message","args":["watchparty","pause","","no","5.000000"]}`,
			`{"event":"playback-restart"}`)
		got := n.broadcasts()
		if fmt.Sprint(said(got)) != "[time-pos=5.000000 pause=no]" || *got[1].At != 5 {
			t.Fatalf("broadcast %+v, want the seek, then the play from 5", got)
		}
		if told := n.mpvWasTold(); slices.Contains(told, "set_property pause true") {
			t.Fatalf("told mpv %q, want it left playing", told)
		}
	})
}
