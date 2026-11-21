package mpv

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Someone can add to the playlist from inside mpv, so the entry it reports may be one we never named.
func TestAnEntryWeDidNotNameDoesNotPanic(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: []string{"e1", "e2"}})
		n.mpvSays(propertyChangeLine(playlistPos, "5", 0))
		n.broadcasts()
		n.peerSays("a", move(5, 0))
		n.mpvObeys()
		if got := n.mpvWasTold(); !slices.Contains(got, "show-text from another video to e1, by a 5000") {
			t.Fatalf("told mpv %q, want the entry it never named called another video", got)
		}
	})
}

// Moving on while playing, at the end of an entry or by hand, stops the party on the
// next one. Peers hear which entry first, so the pause and position land on it.
func TestMovingToAnEntryWhilePlayingPausesTheParty(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.plays()
		n.mpvSays(mpvAt(90))
		n.mpvSays(started(1), propertyChangeLine(playlistPos, "1", 0))
		if got := n.mpvWasTold(); len(got) == 0 || got[0] != "set_property pause true" {
			t.Fatalf("told mpv %q, want the party paused", got)
		}
		// the stop goes out once the script acks it, where mpv stopped
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[playlist-pos=1 pause=yes time-pos=0.000000 pause-reason=video changed]" {
			t.Fatalf("broadcast %q, want the entry, then the pause, the new entry's start (not the old position), and why", got)
		}
	})
}

// A move is a seek far enough to land in another file: a follower's stays on its screen, and
// the party is still where it was. Resumed there, the party stops on that file and second,
// as for a move while playing, and this node leads: the others must load it first, and
// would play that far apart.
func TestAFollowersMoveStaysOnItsScreen(t *testing.T) {
	t.Parallel()
	t.Run("kept", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos})
			n.takes(3, true, 30)
			n.mpvSays(propertyChangeLine(playlistPos, "1", 0))
			n.keepsTheSeek("z")
			// a joiner is told the party's entry and second, not this screen's
			n.peerSays("j", `{"name":"joined"}`)
			got := n.broadcasts()
			if len(got) != 1 || *got[0].Entry != 0 || *got[0].At != 30 {
				t.Fatalf("answered %+v, want the party's entry 0 at 30", got)
			}
			// and where the party continues from, if it ends now
			if entry, at := n.c.Position(); entry != 0 || at != 30 {
				t.Fatalf("Position %d at %v, want the party's entry 0 at 30", entry, at)
			}
		})
	})
	t.Run("resumed there", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos})
			n.takes(3, true, 30)
			n.mpvSays(propertyChangeLine(playlistPos, "1", 0), mpvAt(12))
			n.keepsTheSeek("z")
			// no resume: the stop goes out once the script acks it
			n.mpvSays(propertyChangeLine(pause, "no", 0))
			got := n.broadcasts()
			if fmt.Sprint(said(got)) != "[pause=yes time-pos=12.000000 pause-reason=video changed]" || *got[0].Entry != 1 || *got[0].At != 12 || got[0].Free {
				t.Fatalf("broadcast %+v, want no resume, and the party stopped on entry 1 at 12, led by this node", got)
			}
		})
	})
}

// A new file reports time-pos 0 while paused. Sent on, it would seek a peer still
// playing the end of the last entry.
func TestMovingWhilePausedSendsOnlyTheEntry(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.mpvSays(propertyChangeLine(playlistPos, "2", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[playlist-pos=2]" {
			t.Fatalf("broadcast %q, want the entry", got)
		}
		n.mpvSays(mpvAt(0), propertyChangeLine(pause, "no", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=no]" {
			t.Fatalf("broadcast %q, want nothing before the resume", got)
		}
	})
}

// mpv reports where it starts as soon as it is observed, and -1 once the list runs out.
// Neither is someone moving.
func TestPlaylistPositionThatIsNotAMoveStaysLocal(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 2})
		n.mpvSays(propertyChangeLine(playlistPos, "-1", 0), propertyChangeLine(playlistPos, "0", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[playlist-pos=0]" {
			t.Fatalf("broadcast %q, want only the move to the first", got)
		}
	})
}

// mpv can report an entry before the file is loaded: the one it starts on, or one a key
// pressed while it loads took it to. That isn't a move, and the file's first time-pos must
// still count as loaded.
func TestStartingEntryBeforeTheFileLoadsIsNotAMove(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		// newNode returns once the node knows the file loaded
		n := newNode(t, setup{names: threeVideos, opening: []string{
			strings.TrimSpace(propertyChangeLine(playlistPos, "0", 0)), strings.TrimSpace(propertyChangeLine(playlistPos, "1", 0)), strings.TrimSpace(propertyChangeLine(pause, "yes", 0)), strings.TrimSpace(mpvAt(0)),
		}})
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q, want only joined", got)
		}
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != pause || got[0].Entry == nil || *got[0].Entry != 1 {
			t.Fatalf("broadcast %+v, want the resume, on entry 1", got)
		}
	})
}

// A peer's move opens the next file stopped, and mpv's report of it isn't sent back.
func TestFollowingAnEntryStopsFirstAndStaysQuiet(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.plays()
		n.peerSays("a", move(5, 2))
		n.mpvObeys()
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true set_property playlist-pos 2 show-text paused by a 5000 show-text from e1 to e3, by a 5000]" {
			t.Fatalf("told mpv %q, want it stopped, then moved", got)
		}
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=no]" {
			t.Fatalf("broadcast %q, want nothing before the resume", got)
		}
	})
}

// Every member answers a joiner, and two nodes can reach the end together.
func TestEntryAlreadyHeadedThereIsNotRepeated(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 1})
		n.peerSays("a", move(2, 1))
		n.peerSays("a", move(3, 2))
		n.peerSays("b", move(3, 2))
		n.peerSays("b", move(4, 3))
		n.mpvObeys()
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property playlist-pos 2 show-text from e2 to e3, by a 5000]" {
			t.Fatalf("told mpv %q, want one move, and none to an entry it has no file for", got)
		}
	})
}

// A peer's move to a file that takes long to load, as a slow stream or a torrent can: the set
// is acked once the file loads, so it waits for it. Given up after 5s, the node asked the
// party again for a move that was only slow.
func TestAMoveWaitsForASlowLoad(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.peerSays("a", move(5, 1))
		time.Sleep(time.Minute)
		n.mpvObeys()
		if got := said(n.broadcasts()); len(got) != 0 {
			t.Fatalf("broadcast %q, want nothing: the move was only slow", got)
		}
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property playlist-pos 1 show-text from e1 to e2, by a 5000]" {
			t.Fatalf("told mpv %q, want the move, shown once loaded", got)
		}
	})
}

// A move whose file never starts nor fails (a stream that hangs, under --network-timeout=0):
// the node waits past the 81s mpv took to give up on one, then gives up on it, as on any set
// mpv never acks, and asks the party where it is.
func TestAMoveThatNeverStartsIsGivenUp(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.mpvAnswersLate()
		n.peerSays("a", move(5, 1))
		time.Sleep(2 * time.Minute)
		if got := said(n.broadcasts()); len(got) != 0 {
			t.Fatalf("broadcast %q after 2 minutes, want the move waited for", got)
		}
		time.Sleep(8 * time.Minute)
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q, want it to ask the party again", got)
		}
	})
}

// A peer's move to an entry mpv can't open: mpv says so at its end-file and moves on to the
// next entry by itself. The script acks the move there, so the node goes on; mpv's own move
// stays on this follower's screen, as for any follower's move, and the leader's own skip,
// which goes out, takes the party there.
func TestAMoveToAFileMpvCantOpenGoesOn(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.peerSays("a", move(5, 1))
		n.mpvWasTold()
		n.mpvSays(started(1), `{"event":"end-file","reason":"error"}`, started(2), propertyChangeLine(playlistPos, "2", 0), `{"event":"file-loaded"}`, mpvAt(0))
		n.keepsTheSeek("a")
		n.peerSays("a", move(6, 2))
		if got, told := n.broadcasts(), n.mpvWasTold(); len(got) != 0 || len(told) != 0 {
			t.Fatalf("broadcast %q and told mpv %q, want the leader's skip taken where mpv is", said(got), told)
		}
		if entry, at := n.c.Position(); entry != 2 || at != 0 {
			t.Fatalf("Position %d at %v, want entry 2 at 0", entry, at)
		}
	})
}

// A peer's move to the last entry, which mpv can't open, with nothing to load after it (the
// person's --idle keeps mpv open): only its end-file says so, and the script acks the move
// there, so the next message is taken. Unacked, every later message waited for good.
func TestAMoveToAFileMpvCantOpenIsAckedAtItsEnd(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.peerSays("a", move(5, 2))
		n.mpvSays(started(2), `{"event":"end-file","reason":"error"}`)
		n.peerSays("a", move(6, 0))
		if got := n.mpvWasTold(); !slices.Contains(got, "set_property playlist-pos 0") {
			t.Fatalf("told mpv %q, want the next move taken", got)
		}
	})
}

// A seek right after a move would land on the old file, which then ends and loses it.
// A joiner hits this every time: it is told the entry, then the position.
func TestSeekAfterFollowingAnEntryWaitsForTheFile(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.peerSays("a", move(5, 1))
		n.peerSays("a", stamped(6, tagged(1, seekEvent(40))))
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property playlist-pos 1]" {
			t.Fatalf("told mpv %q, want the move and no seek yet", got)
		}
		n.mpvObeys()
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text from e1 to e2, by a 5000 set_property time-pos 40.000000 show-text synced by a 5000]" {
			t.Fatalf("told mpv %q once e2 loaded, want the seek", got)
		}
	})
}

// The seek a move waits for is made once the new file has started, at its first
// playback-restart: one made at its file-loaded, a slow stream can drop (ORDER.md).
func TestAMovesSeekWaitsForTheFileToStart(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.peerSays("a", stamped(5, tagged(1, seekEvent(40))))
		n.mpvWasTold()
		n.mpvSays(started(1), `{"event":"file-loaded"}`)
		if got := seeksIn(n.mpvWasTold()); len(got) != 0 {
			t.Fatalf("seeks %q at file-loaded, want none before the file starts", got)
		}
		n.mpvSays(`{"event":"playback-restart"}`)
		if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 40.000000]" {
			t.Fatalf("seeks %q, want the seek once the file started", got)
		}
	})
}

// While a move loads, this node is at the new file's start, not on the second it left: a
// seek there on the new file is still to make. Seed 436890989886 of
// TestAJoinCrossingAMoveEndsOnThePartysEntry found it.
func TestASeekWhileLoadingToTheSecondItLeftIsMade(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.takes(3, true, 30)
		n.peerSays("a", move(5, 1))
		n.peerSays("a", stamped(6, tagged(1, seekEvent(30))))
		n.mpvObeys()
		if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 30.000000]" {
			t.Fatalf("seeks %q once e2 loaded, want the seek to 30", got)
		}
	})
}

// A key press opens a file just before a peer's stop reaches mpv, and the node hasn't read it.
// The stop is for the file this node left: its seek would land on the new one and go out as this
// node's own second there, after its move, so the party would follow it there (SIM_SEED=6 of
// TestAJoinCrossingAMoveEndsOnThePartysEntry, mpv answers late). The script only stops mpv, and
// the ack's entry sends the move out, newer than the stop, from 0.
func TestAStopTakenAsAKeyPressOpensAFileDoesNotSeekIt(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.personMovesFirst(1)
		// a stop nobody chose: one that leads would keep the person's move on this screen
		n.peerSays("b", `{"stamp":3,"at":30,"entry":0,"free":true,"event":"property-change","name":"pause","data":"yes"}`)
		n.mpvObeys()
		got := n.broadcasts()
		if fmt.Sprint(said(got)) != "[playlist-pos=1]" || got[0].Stamp <= 3 || got[0].At == nil || *got[0].At != 0 {
			t.Fatalf("broadcast %+v, want only the move, newer than the stop, from 0", got)
		}
		if told := n.mpvWasTold(); len(seeksIn(told)) != 0 || slices.Contains(told, "set_property playlist-pos 0") {
			t.Fatalf("told mpv %q, want no seek and no move back", told)
		}
	})
}

// A key opens a file once a peer's seek has begun in the script, so mpv refuses the seek: the
// start came while the set was on its way, nobody's, and the refusal says the entry mpv ended on.
// Another than the one this node heads to is the person's move, after the seek, and goes out.
func TestAMoveThatMakesMpvRefuseAGoGoesOut(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.personMovesDuring(1)
		n.mpvRefuses("set_property time-pos", "property unavailable")
		// a seek nobody leads: one that leads would keep the person's move on this screen
		n.peerSays("b", `{"stamp":3,"at":30,"entry":0,"free":true,"event":"property-change","name":"time-pos","data":"30.000000"}`)
		n.mpvObeys()
		if got := n.broadcasts(); len(got) == 0 || said(got)[0] != "playlist-pos=1" || got[0].Stamp <= 3 {
			t.Fatalf("broadcast %+v, want the person's move, newer than the seek", got)
		}
	})
}

// Everything a node sends says its stamp, which each change of its own raises, where it
// is, and in a playlist which entry it belongs to.
func TestMessagesSayWhichEntryTheyBelongTo(t *testing.T) {
	t.Parallel()
	t.Run("a playlist", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{names: threeVideos})
			n.mpvSays(propertyChangeLine(playlistPos, "2", 0))
			if got := n.broadcasts(); len(got) != 1 || got[0].Name != playlistPos || got[0].Stamp != 2 || got[0].Entry == nil || *got[0].Entry != 2 {
				t.Fatalf("broadcast %+v, want a move to 2 with a new stamp", got)
			}
			n.mpvSays(mpvAt(0), mpvAt(9))
			if got := n.broadcasts(); len(got) != 1 || got[0].Name != timePos || got[0].Stamp != 3 || got[0].At == nil || *got[0].At != 9 || got[0].Entry == nil || *got[0].Entry != 2 {
				t.Fatalf("broadcast %+v, want a seek on entry 2, at 9, with the next stamp", got)
			}
		})
	})
	t.Run("a single file", func(t *testing.T) {
		nodeTest(t, func(t *testing.T) {
			n := ready(t, setup{})
			n.mpvSays(mpvAt(9))
			if got := n.broadcasts(); len(got) != 1 || got[0].Entry != nil || got[0].Stamp != 2 {
				t.Fatalf("broadcast %+v, want a stamp and no entry", got)
			}
		})
	})
}

// A seek or a resume made at the same moment as the move that brought this node here
// carries the move's stamp and a lower entry: it belongs to a file the party has left. A
// pause still stops us: stopping is always safe.
func TestMessagesFromAnEntryThePartyLeftAreDropped(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 1})
		n.takes(2, true, 0)
		n.peerSays("a", stamped(2, tagged(0, seekEvent(40))))
		n.peerSays("a", stamped(2, tagged(0, `{"name":"pause","data":"no"}`)))
		n.peerSays("a", stamped(3, tagged(1, seekEvent(41))))
		if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 41.000000]" {
			t.Fatalf("seeks %q, want only the seek on its entry", got)
		}
	})
}

// A newer pause or seek from another entry means the party is there: this node missed the
// move. It goes there first, then to the second it was told, once the file has loaded.
func TestANewerSeekFromAnotherEntryBringsThisNodeThere(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.peerSays("a", `{"stamp":3,"entry":2,"event":"property-change","name":"time-pos","data":"40.000000"}`)
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property playlist-pos 2]" {
			t.Fatalf("told mpv %q, want the move and no seek before the file loads", got)
		}
		n.mpvObeys()
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property time-pos 40.000000 show-text from e1 to e3, by a 5000 show-text synced by a 5000]" {
			t.Fatalf("told mpv %q, want the second it was told, on the new file", got)
		}
	})
}

// It loses to the higher entry, and this node, playing, answers by stopping where it is.
func TestAPauseFromAnotherEntryStillStops(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 1})
		n.plays()
		n.mpvSays(mpvAt(5))
		n.peerSays("a", `{"stamp":2,"at":30,"entry":0,"name":"pause","data":"yes"}`)
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true]" {
			t.Fatalf("told mpv %q, want the pause, and no seek", got)
		}
	})
}

// While a move is on its way, the entry it is heading to is the one that counts.
func TestMessagesAreCheckedAgainstWhereWeAreHeading(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.peerSays("a", move(2, 1))
		n.peerSays("a", stamped(2, tagged(0, seekEvent(40))))
		n.peerSays("a", stamped(3, tagged(1, seekEvent(41))))
		n.mpvObeys()
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property playlist-pos 1 show-text from e1 to e2, by a 5000 set_property time-pos 41.000000 show-text synced by a 5000]" {
			t.Fatalf("told mpv %q, want the move, and only the seek on the new entry", got)
		}
	})
}

// move is a peer's playlist entry as it arrives: a move, or an answer to a joiner.
func move(stamp, to int) string {
	return fmt.Sprintf(`{"stamp":%d,"event":"property-change","name":"playlist-pos","data":"%d"}`, stamp, to)
}

// Two people move at once, one forward and one back, so their moves carry the same
// stamp. Without a rule each follows the other and they swap. Everyone applies the same
// rule, so they end up together on the higher one.
func TestTwoMovesFromTheSameEntrySettleOnTheHigher(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ mine, theirs int }{
		"we went higher": {2, 0},
		"we went lower":  {0, 2},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{names: threeVideos, start: 1})
				n.mpvSays(propertyChangeLine(playlistPos, fmt.Sprint(c.mine), 0))
				n.broadcasts()
				n.peerSays("a", move(2, c.theirs))
				moved := ""
				for _, s := range n.mpvWasTold() {
					if strings.HasPrefix(s, "set_property playlist-pos") {
						moved = s
					}
				}
				if want := map[bool]string{true: "", false: fmt.Sprintf("set_property playlist-pos %d", c.theirs)}[c.mine > c.theirs]; moved != want {
					t.Fatalf("told mpv %q, want %q: the higher one", moved, want)
				}
			})
		})
	}
}

// A third member hears both moves, in either order, and must land in the same place.
func TestAThirdMemberSettlesOnTheHigherInEitherOrder(t *testing.T) {
	t.Parallel()
	for name, moves := range map[string][2]int{"higher first": {2, 0}, "lower first": {0, 2}} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{names: threeVideos, start: 1})
				n.peerSays("a", move(2, moves[0]))
				n.peerSays("b", move(2, moves[1]))
				n.mpvObeys()
				last := ""
				for _, s := range n.mpvWasTold() {
					if strings.HasPrefix(s, "set_property playlist-pos") {
						last = s
					}
				}
				if last != "set_property playlist-pos 2" {
					t.Fatalf("settled with %q, want the higher one", last)
				}
			})
		})
	}
}

// Someone behind everyone else hears a tie too, and what it compares is the stamps, not
// where it happens to be.
func TestATieIsSeenFromAnyEntry(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.peerSays("a", move(2, 2))
		n.peerSays("b", move(2, 1))
		n.mpvObeys()
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property playlist-pos 2 show-text from e1 to e3, by a 5000]" {
			t.Fatalf("told mpv %q, want the higher one alone", got)
		}
	})
}

// The higher entry only settles a tie. A newer move is followed, lower or not, so someone
// who missed a message catches up on the next one instead of staying apart.
func TestALowerMoveThatIsNotATieIsFollowed(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 2})
		n.peerSays("a", move(2, 0))
		if got := n.mpvWasTold(); len(got) == 0 || got[0] != "set_property playlist-pos 0" {
			t.Fatalf("told mpv %q, want it followed", got)
		}
	})
}

// Two players reach the end of an entry together. One decides to follow the other, and
// its own move arrives while it is pausing. The set that follows would change nothing, so
// mpv would never answer it with an event, and it would wait for a load that has passed,
// holding every later seek forever. mpv reports a property only once its event queue is
// empty (client.h), so its report of the entry can come before file-loaded or after it,
// and the load can be over before the move arrives. A seek held for the file and let go at
// its file-loaded must still be marked when the report comes: its echo is no one's seek.
func TestOwnMoveWhileFollowingDoesNotHoldLaterSeeks(t *testing.T) {
	t.Parallel()
	// mpv moving on from the entry's end by itself: the script says that start is mpv's own
	ended, started := `{"event":"end-file","reason":"eof"}`, strings.TrimSpace(started(2))
	// loaded: its first frame is ready too, which is when the script applies the rest of a set
	loaded, restarted := `{"event":"file-loaded"}`, `{"event":"playback-restart"}`
	reported := strings.TrimSpace(propertyChangeLine(playlistPos, "2", 0))
	for _, tc := range []struct {
		name                            string
		before, pausing, after, seeking []string
	}{
		{"reported, then loaded", nil, []string{ended, started, reported, loaded, restarted}, nil, nil},
		{"loaded, then reported", nil, []string{ended, started, loaded, restarted, reported}, nil, nil},
		{"loaded before the move came", []string{ended, started, loaded, restarted}, []string{reported}, nil, nil},
		{"loaded with a seek held, then reported", nil, nil, []string{ended, started, loaded, restarted}, []string{reported}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{names: threeVideos, start: 1})
				n.plays()
				n.mpvAnswersLate()
				n.mpvSays(tc.before...)
				n.peerSays("a", move(5, 2))
				// what mpv says while it pauses, before it answers
				n.mpvSays(tc.pausing...)
				n.mpvAnswers()
				// mpv's own move, there before the other's, went out, as a stop nobody leads
				n.broadcasts()
				// the seek waits for the move to be done: the script acks it once its file loads
				n.peerSays("a", stamped(6, tagged(2, seekEvent(5))))
				n.mpvSays(tc.after...)
				n.mpvAnswers()
				// setting the entry mpv reached by itself is a no-op (MPV.md), and loading, set
				// before mpv was told, was cleared by that file's file-loaded
				if got := seeksIn(n.mpvWasTold()); fmt.Sprint(got) != "[set_property time-pos 5.000000]" {
					t.Fatalf("seeks %q, want the seek let go", got)
				}
				// the person resumes once the seek has landed: what goes out first
				n.mpvSays(append(tc.seeking, `{"event":"seek"}`, mpvAt(5), `{"event":"playback-restart"}`, propertyChangeLine(pause, "no", 0))...)
				n.mpvAnswers()
				if got := said(n.broadcasts()); len(got) == 0 || got[0] != "pause=no" {
					t.Fatalf("broadcast %q, want the person's resume first", got)
				}
			})
		})
	}
}

// An entry a peer answers with says where on it, so a node that takes the party's stamp
// from it takes the party's second too. A node already on that entry does as well.
func TestAnEntryAnswerSaysWhereOnIt(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos, start: 1})
		n.mpvSays(mpvAt(10))
		n.peerSays("a", `{"stamp":4,"at":30,"event":"property-change","name":"playlist-pos","data":"1"}`)
		if got := n.mpvWasTold(); len(got) == 0 || got[0] != "set_property time-pos 30.000000" {
			t.Fatalf("told mpv %q, want the party's second on the entry it is already on", got)
		}
	})
}

// A node that answers once a move it followed has loaded, before mpv reports the entry, names
// the entry it moved to, and the start of that file: not the second it was on in the file it
// left. The join waits for the move.
func TestAnAnswerWhileAMoveLoadsIsAboutTheNewEntry(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.mpvSays(mpvAt(30))
		n.broadcasts()
		n.peerSays("a", move(3, 1))
		n.peerSays("j", `{"name":"joined"}`)
		n.mpvSays(started(1), `{"event":"file-loaded"}`, `{"event":"playback-restart"}`)
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != timePos || got[0].Data != "0.000000" || got[0].Entry == nil || *got[0].Entry != 1 || got[0].Stamp != 3 {
			t.Fatalf("answered %+v, want entry 1 at its start, with the move's stamp", got)
		}
	})
}

// mpv says playback-restart when a file is ready, and on a slow stream that can come after
// this node was already sent somewhere. It doesn't mean the seek has landed: until mpv
// reports that second, this node is still heading to it. Taken for where mpv last was, the
// start, a joiner lost a tie to the next member's stop and told the party it was at 0, and
// the party went there.
func TestASeekIsStillOnItsWayAfterAStrayPlaybackRestart(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{joiner: true})
		n.peerSays("a", `{"stamp":8,"at":53.1,"name":"pause","data":"yes"}`)
		n.mpvSays(`{"event":"playback-restart"}`)
		n.peerSays("b", `{"stamp":8,"at":53.3,"name":"pause","data":"yes"}`)
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property time-pos 53.100000]" {
			t.Fatalf("told mpv %q, want the party's second and nothing more", got)
		}
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=53.100000]" {
			t.Fatalf("answered %q, want the earlier of the two stops, where it is heading", got)
		}
	})
}

// Its user's own change once a move it followed has loaded, before mpv reports the entry, is
// about the entry it moved to. Tagged with the file mpv was on, it carried the party's newest
// stamp on the entry the party left, and dragged everyone back there.
func TestOwnChangeWhileAMoveLoadsIsAboutTheNewEntry(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.mpvSays(mpvAt(30))
		n.broadcasts()
		n.peerSays("a", move(3, 1))
		n.mpvSays(started(1), `{"event":"file-loaded"}`, `{"event":"playback-restart"}`)
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != pause || got[0].Entry == nil || *got[0].Entry != 1 || got[0].At == nil || *got[0].At != 0 || got[0].Stamp != 4 {
			t.Fatalf("resumed with %+v, want entry 1 from its start, newer than the move", got)
		}
	})
}

// A newer resume from another entry means this node missed the move. It goes there
// stopped, as the file has to load, and stops the party instead of playing that far behind.
func TestANewerResumeFromAnotherEntryStopsTheParty(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.plays()
		n.mpvSays(mpvAt(5))
		n.peerSays("a", `{"stamp":3,"at":10,"entry":2,"name":"pause","data":"no"}`)
		n.mpvObeys()
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true set_property playlist-pos 2 set_property time-pos 10.000000 show-text paused by a 5000 show-text from e1 to e3, by a 5000]" {
			t.Fatalf("told mpv %q", got)
		}
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != timePos || got[0].Data != "10.000000" || got[0].Stamp != 3 || got[0].Entry == nil || *got[0].Entry != 2 {
			t.Fatalf("answered %+v, want where it holds the party is, stopped", got)
		}
	})
}

// A move while playing stops the party, so it says stopped before mpv has: whoever takes it
// opens the file stopped instead of answering a node it thinks plays on.
func TestAMoveWhilePlayingSaysStopped(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.plays()
		n.mpvSays(propertyChangeLine(playlistPos, "2", 0))
		if got := n.broadcasts(); len(got) == 0 || got[0].Name != playlistPos || got[0].Paused == nil || !*got[0].Paused {
			t.Fatalf("broadcast %+v, want the move, stopped", got)
		}
	})
}

// The person moves to another entry while a peer's stop is on its way to mpv: what the script
// says meanwhile is nobody's, and the ack says the entry mpv ended on. Another than the one the
// stop was for is the person's move, after it, and goes out, newer.
func TestAMoveWhileAGoIsOnItsWayGoesOutAtItsAck(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.mpvAnswersLate()
		n.peerSays("a", `{"stamp":3,"at":40,"entry":0,"free":true,"event":"property-change","name":"pause","data":"yes"}`)
		n.mpvSays(started(1), propertyChangeLine(playlistPos, "1", 0))
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q before the ack, want nothing: the go is on its way", said(got))
		}
		n.mpvAnswers()
		if got := n.broadcasts(); fmt.Sprint(said(got)) != "[playlist-pos=1]" || got[0].Stamp != 4 {
			t.Fatalf("broadcast %+v, want the person's move, newer", got)
		}
	})
}

// mpv reaches a file's end and opens the next by itself just before a peer's move reaches it,
// before this node has read it: the set undoes mpv's move, which was nobody's key, so the
// person isn't told to press anything again (macOS's probe found it).
func TestMpvsOwnMoveAGoUndoesIsNoKey(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{names: threeVideos})
		n.plays()
		n.mpvRunsOutFirst(1)
		n.peerSays("a", move(5, 2))
		n.mpvObeys()
		if got := n.mpvWasTold(); slices.Contains(got, "show-text "+overruled+" 5000") {
			t.Fatalf("told mpv %q, want no key reported lost", got)
		}
	})
}
