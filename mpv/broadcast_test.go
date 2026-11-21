package mpv

import (
	"fmt"
	"slices"
	"testing"
)

func TestFileNameIsNeverBroadcast(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(propertyChangeLine(filename, "a.mkv", 0), propertyChangeLine(filename, "b.mkv", 0))
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("a file name went over the wire: %+v", got)
		}
	})
}

// mpv fires one property-change per observed property the moment it is
// registered. A fresh node is paused, so an unswallowed initial time-pos would be
// broadcast as time-pos=0 and rewind the whole party.
func TestInitialPropertyChangeIsNotBroadcast(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{})
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q, want only joined: mpv's answer to the observe went out", got)
		}
		n.mpvSays(mpvAt(77))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=77.000000]" {
			t.Fatalf("broadcast %q, want the real seek after the initial one", got)
		}
	})
}

// Observed before the file loads, time-pos first comes with no data, then the real
// position once it has loaded. The empty one must not count as the first change,
// or the real 0 is broadcast and rewinds the party. Quitting while paused also
// sends an empty one, which must not go out either.
func TestDatalessPropertyChangeIsIgnored(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		empty := `{"event":"property-change","id":2,"name":"time-pos"}`
		n := newNode(t, setup{first: []string{empty + "\n"}})
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[joined=]" {
			t.Fatalf("broadcast %q, want only joined: the position the file loaded at went out", got)
		}
		n.mpvSays(mpvAt(42), empty)
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=42.000000]" {
			t.Fatalf("broadcast %q, want only the real seek", got)
		}
	})
}

// b applies a's seek, mpv echoes the change back, and b must not rebroadcast it
// to a. Without this the event ping-pongs; relay mode has no counters to stop it.
func TestAppliedValueIsNotRebroadcast(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("a", stamped(5, seekEvent(77)))
		n.mpvSays(mpvAt(77))
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("rebroadcast the seek it had just applied: %q", said(got))
		}
		// a seek's echo can land a frame off the applied value
		n.peerSays("a", stamped(6, seekEvent(12)))
		n.mpvSays(mpvAt(12.041))
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("a frame-off echo escaped the tolerance: %q", said(got))
		}
		// but the person's own seek is not an echo: a follower keeps it on its screen
		n.mpvWasTold()
		n.mpvSays(mpvAt(42))
		n.keepsTheSeek("a")
	})
}

// b applies a's pause, and mpv reports it before it answers the set: that report is the
// echo, and it is also when b stopped, so a position mpv reports before its answer is a
// seek while paused, not playback.
func TestAppliedPauseIsNotRebroadcast(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvAnswersLate()
		n.peerSays("a", stamped(5, `{"event":"property-change","name":"pause","data":"yes","at":0}`))
		n.mpvObeys(mpvAt(55))
		// only the seek made once stopped, which a follower keeps: not the echo, and not the
		// seek lost as playback
		n.keepsTheSeek("a")
		// the echo used up the marker, so pausing again later is a real event
		n.plays()
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes]" {
			t.Fatalf("broadcast %q, want the genuine pause", got)
		}
	})
}

// A seek while playing would leave every peer where it was, since positions only
// sync while paused. The node pauses the party first, and peers get the pause, then
// where it landed, then why.
func TestSeekWhilePlayingPausesTheParty(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvSays(mpvAt(10), mpvAt(10.041), `{"event":"seek"}`, mpvAt(40), `{"event":"playback-restart"}`)
		// what it says once mpv has stopped, where the script's ack says it did; a playing second
		// mpv reports after is from before the stop
		n.mpvSays(mpvAt(40.041), propertyChangeLine(pause, "yes", 0))
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true show-text paused for everyone: seek 5000]" {
			t.Fatalf("told mpv %q, want the node to pause itself and say why", got)
		}
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes time-pos=40.000000 pause-reason=seek]" {
			t.Fatalf("broadcast %q, want the pause, where it paused, then why", got)
		}
		// mpv's frame-landing second value is the position just sent, not a new seek
		n.mpvSays(mpvAt(40.042), mpvAt(50))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=50.000000]" {
			t.Fatalf("broadcast %q, want only the next real seek", got)
		}
	})
}

// Someone seeks and resumes at once, and both reach this node together. mpv reports the
// seek after the resume, which must not look like someone seeking while playing.
func TestPeerSeekReportedAfterAResumeIsNotASeek(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("a", stamped(5, seekEvent(40)))
		n.peerSays("a", stamped(6, `{"event":"property-change","name":"pause","data":"no","at":40}`))
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		n.mpvWasTold()
		n.mpvSays(`{"event":"seek"}`, mpvAt(40), `{"event":"playback-restart"}`, mpvAt(40.041), mpvAt(40.2), mpvAt(40.5))
		if got, told := said(n.broadcasts()), n.mpvWasTold(); len(got) != 0 || len(told) != 0 {
			t.Fatalf("broadcast %q and told mpv %q for the peer's own seek", got, told)
		}
		n.mpvSays(`{"event":"seek"}`, mpvAt(50), `{"event":"playback-restart"}`)
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true show-text paused for everyone: seek 5000]" {
			t.Fatalf("told mpv %q, want a real seek to pause the party", got)
		}
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes time-pos=50.000000 pause-reason=seek]" {
			t.Fatalf("broadcast %q, want the party paused at 50 only", got)
		}
	})
}

// Playback moves time-pos on every frame, and none of it may go out or pause anyone.
func TestPlaybackIsNotASeek(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		for i := range 48 {
			n.mpvSays(mpvAt(10 + float64(i)/24))
		}
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes]" {
			t.Fatalf("broadcast %q, want only the later pause", got)
		}
		if got := n.mpvWasTold(); len(got) != 0 {
			t.Fatalf("told mpv %q during playback", got)
		}
	})
}

// A pause says where the pauser stopped. Over a slow link the others stop a round trip
// later; without this they resume that far apart and stay so.
func TestOwnPauseSaysWhereItStopped(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvSays(mpvAt(61.25), propertyChangeLine(pause, "yes", 0))
		if got := n.broadcasts(); len(got) != 1 || got[0].Name != pause || got[0].At == nil || *got[0].At != 61.25 {
			t.Fatalf("broadcast %+v, want the pause, where it stopped", got)
		}
	})
}

// A pause says where its sender stopped, and a seek that it was stopped: in a mesh a send
// can fail, and either one alone must leave the node that got it stopped on that second.
func TestAPauseSaysWhereAndASeekSaysStopped(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ event, want string }{
		"a pause alone": {`{"stamp":5,"at":40,"name":"pause","data":"yes"}`, "[set_property pause true set_property time-pos 40.000000 show-text paused by a 5000]"},
		"a seek alone":  {`{"stamp":5,"name":"time-pos","data":"40.000000"}`, "[set_property pause true set_property time-pos 40.000000 show-text paused by a 5000 show-text synced by a 5000]"},
	} {
		t.Run(name, func(t *testing.T) {
			nodeTest(t, func(t *testing.T) {
				n := ready(t, setup{})
				n.plays()
				n.mpvSays(mpvAt(30))
				n.peerSays("a", c.event)
				n.mpvObeys()
				if got := n.mpvWasTold(); fmt.Sprint(got) != c.want {
					t.Fatalf("told mpv %q, want %q", got, c.want)
				}
			})
		})
	}
}

// A resume says where from, so a node that missed the seek before it starts there too.
func TestAResumeSaysWhereFrom(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(mpvAt(30))
		n.peerSays("a", `{"stamp":5,"at":40,"name":"pause","data":"no"}`)
		if got := n.mpvWasTold(); len(got) < 2 || fmt.Sprint(got[:2]) != "[set_property time-pos 40.000000 set_property pause false]" {
			t.Fatalf("told mpv %q, want to seek where the resume is from, then play", got)
		}
	})
}

// The person presses play while a peer's seek to 60 is on its way, then seeks to 42 while it
// plays, and mpv folds the two: it lands on 42 with the seek to 60's id and plays on. That is the
// person's seek while playing, so it pauses the party, and the pause says where mpv stopped,
// not the second it landed on (SIM_SEED=831497494691).
func TestAFoldedSeekWhilePlayingPausesTheParty(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takes(2, true, 30)
		n.mpvAnswersLate()
		n.peerSays("a", stamped(3, seekEvent(60)))
		n.mpvWasTold()
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		n.mpvSays(`{"event":"seek"}`, mpvAt(42), `{"event":"playback-restart"}`, mpvAt(42.1), mpvAt(42.2))
		// the ack comes before the seek event, and says mpv plays: the person's resume, from 60
		if got := n.broadcasts(); len(got) == 0 || said(got)[0] != "pause=no" || *got[0].At != 60 {
			t.Fatalf("broadcast %+v, want the person's resume, from 60, first", got)
		}
		if told := n.mpvWasTold(); !slices.Contains(told, "set_property pause true") {
			t.Fatalf("told mpv %q, want the party paused", told)
		}
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		n.mpvAnswers()
		got := n.broadcasts()
		if fmt.Sprint(said(got)) != "[pause=yes time-pos=42.200000 pause-reason=seek]" || *got[0].At != 42.2 {
			t.Fatalf("broadcast %+v, want the party paused at 42.2 for a seek", got)
		}
	})
}

// A peer's stop at 9.5 reaches this node while it plays on 9.45: mpv answers the stop, then
// reports one more playing position, 9.5, and only then the stop (mpv 0.40, e2e [R6c] under
// --jitter 14). The node took the stop at the answer, so that position read as the person's
// seek while stopped, and the party stopped for it, catching up. It is mpv playing on. A stop
// nobody chose (a leave's), as a follower of a person's stop sends no seek anyway.
func TestAPositionBeforeAStopIsReportedIsNoSeek(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(mpvAt(9))
		n.plays()
		n.mpvSays(mpvAt(9.45))
		n.peerSays("a", `{"stamp":9,"at":9.5,"free":true,"name":"pause","data":"yes"}`)
		if told := n.mpvWasTold(); len(told) == 0 || told[0] != "set_property pause true" {
			t.Fatalf("told mpv %q, want the stop", told)
		}
		n.mpvSays(mpvAt(9.5))
		n.mpvObeys()
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q, want nothing", said(got))
		}
		// stopped, a position is the person's seek again
		n.mpvSays(mpvAt(20))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=20.000000]" {
			t.Fatalf("broadcast %q, want the person's seek", got)
		}
	})
}
