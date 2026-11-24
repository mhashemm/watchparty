package mpv

import (
	"fmt"
	"slices"
	"testing"
)

// speedEvent is a member's speed change as it arrives, sent from a stop.
func speedEvent(stamp int, speed float64) string {
	return fmt.Sprintf(`{"stamp":%d,"paused":true,"at":0,"speed":%g,"event":"property-change","name":"speed","data":"%f"}`, stamp, speed, speed)
}

// personSpeeds is the person's speed key in mpv.
func (n *node) personSpeeds(v float64) {
	n.t.Helper()
	n.mpvSays(propertyChangeLine(playSpeed, seconds(v), 0))
}

// The person's speed while stopped goes out as their own change, with the next stamp, and
// this node leads the stop, as for a seek.
func TestTheSpeedChangedWhileStoppedGoesOut(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.personSpeeds(1.5)
		got := n.broadcasts()
		if len(got) != 1 || got[0].Name != playSpeed || got[0].Speed == nil || *got[0].Speed != 1.5 || got[0].Free {
			t.Fatalf("broadcast %+v, want the speed 1.5, led by this node", got)
		}
		if told := n.mpvWasTold(); len(told) != 0 {
			t.Fatalf("told mpv %q, want nothing", told)
		}
		n.personSpeeds(2)
		if again := n.broadcasts(); len(again) != 1 || again[0].Stamp != got[0].Stamp+1 {
			t.Fatalf("broadcast %+v, want the leader's next speed at stamp %d", again, got[0].Stamp+1)
		}
	})
}

// A follower's speed stays on its screen, as its seek does, until it resumes the party from
// there.
func TestAFollowersSpeedStaysOnItsScreen(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takesFrom("a", 3, true, 20)
		n.personSpeeds(1.5)
		n.keepsTheSeek("a")
		// until then it speaks for the party's speed: a joiner takes what it says
		n.peerSays("j", `{"name":"joined"}`)
		if got := n.broadcasts(); len(got) != 1 || got[0].Speed == nil || *got[0].Speed != 1 {
			t.Fatalf("answered %+v, want the party's speed 1", got)
		}
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		got := n.broadcasts()
		if len(got) != 1 || got[0].Data != "no" || got[0].Speed == nil || *got[0].Speed != 1.5 {
			t.Fatalf("broadcast %+v, want the resume at 1.5", got)
		}
	})
}

// While playing, each node would take the speed at another moment, so the party stops
// first, led by the person, and the stop carries the speed.
func TestTheSpeedChangedWhilePlayingStopsTheParty(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvSays(mpvAt(30))
		n.personSpeeds(2)
		n.mpvObeys()
		got := n.broadcasts()
		if fmt.Sprint(said(got)) != "[pause=yes time-pos=30.000000 pause-reason=speed changed]" {
			t.Fatalf("broadcast %q, want the party stopped for the speed", said(got))
		}
		for _, e := range got {
			if e.Speed == nil || *e.Speed != 2 || e.Free {
				t.Fatalf("broadcast %+v, want the speed 2, led by this node", e)
			}
		}
		if told := n.mpvWasTold(); !slices.Contains(told, "show-text paused for everyone: speed changed 5000") {
			t.Fatalf("told mpv %q, want the person told why", told)
		}
	})
}

// A member's speed is set on mpv with nothing else; mpv's report of it is no change of the
// person's.
func TestAMembersSpeedIsTaken(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("a", speedEvent(5, 1.25))
		want := "[set_property speed 1.250000 show-text speed 1.25× by a 5000]"
		if told := n.mpvWasTold(); fmt.Sprint(told) != want {
			t.Fatalf("told mpv %q, want %s", told, want)
		}
		n.mpvObeys()
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q, want nothing", said(got))
		}
	})
}

// The stop a member's speed change made comes with the speed: one set takes both. The stop
// passes on the speed its sender held, maybe from someone else, so it names nobody.
func TestAStopWithANewSpeedIsOneSet(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.peerSays("a", `{"stamp":5,"paused":true,"at":20,"speed":2,"event":"property-change","name":"pause","data":"yes"}`)
		want := []string{"set_property speed 2.000000", "set_property pause true", "set_property time-pos 20.000000", "show-text speed 2× 5000", "show-text paused by a 5000"}
		if told := n.mpvWasTold(); fmt.Sprint(told) != fmt.Sprint(want) {
			t.Fatalf("told mpv %q, want %q", told, want)
		}
		n.mpvObeys()
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q, want nothing", said(got))
		}
	})
}

// Two speeds changed at once, with one stamp: the slower wins, on every node. One outside
// mpv's range is refused.
func TestOfTwoSpeedsAtOnceTheSlowerWins(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.personSpeeds(1.5)
		stamp := n.broadcasts()[0].Stamp
		for _, c := range []struct {
			stamp int
			speed float64
			takes bool
		}{
			{stamp, 2, false},
			{stamp + 9, 1000, false},
			{stamp, 1.25, true},
			{stamp + 1, 3, true},
		} {
			n.peerSays("a", speedEvent(c.stamp, c.speed))
			told := n.mpvWasTold()
			if took := slices.Contains(told, "set_property speed "+seconds(c.speed)); took != c.takes {
				t.Fatalf("speed %g at stamp %d: told mpv %q, want it taken: %v", c.speed, c.stamp, told, c.takes)
			}
			n.mpvObeys()
			n.broadcasts()
		}
	})
}

// A member whose message says an older stamp and another speed missed the change: the party
// says where it is, speed included, as for any state it missed.
func TestAMemberThatMissedASpeedIsAnswered(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.personSpeeds(1.5)
		stamp := n.broadcasts()[0].Stamp
		n.peerSays("b", fmt.Sprintf(`{"stamp":%d,"paused":true,"at":0,"speed":1,"check":true,"event":"property-change","name":"pause","data":"yes"}`, stamp-1))
		got := n.broadcasts()
		if len(got) == 0 || got[len(got)-1].Speed == nil || *got[len(got)-1].Speed != 1.5 || got[len(got)-1].Stamp != stamp {
			t.Fatalf("broadcast %+v, want the party's state at 1.5", got)
		}
		if told := n.mpvWasTold(); !slices.Contains(told, "show-text paused for b: "+catchingUp+" 5000") {
			t.Fatalf("told mpv %q, want the party stopped for b", told)
		}
	})
}

// A message without the speed is refused, as one without any part of its sender's state.
func TestAMessageWithoutTheSpeedIsRefused(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.transportSays(IncomingMessage{HostName: "a", From: "a", Event: []byte(`{"stamp":9,"paused":true,"at":40,"event":"property-change","name":"time-pos","data":"40.000000"}`)})
		if told := n.mpvWasTold(); len(told) != 0 {
			t.Fatalf("told mpv %q, want nothing", told)
		}
	})
}

// A joiner takes the party's speed with where it is; the answer passes on the speed its
// sender heard, so it names nobody.
func TestAJoinerTakesThePartysSpeed(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{joiner: true})
		n.broadcasts()
		n.peerSays("a", `{"stamp":4,"paused":true,"at":40,"speed":1.5,"event":"property-change","name":"time-pos","data":"40.000000"}`)
		if told := n.mpvWasTold(); !slices.Contains(told, "set_property speed 1.500000") || !slices.Contains(told, "show-text speed 1.5× 5000") {
			t.Fatalf("told mpv %q, want the party's speed, naming nobody", told)
		}
	})
}

// A joiner's speed key before its answer loses to the party, as anything a joiner does.
func TestAJoinersSpeedKeyLosesToTheParty(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{joiner: true})
		n.broadcasts()
		n.personSpeeds(1.5)
		n.broadcasts()
		n.peerSays("a", `{"stamp":4,"paused":true,"at":40,"speed":1,"event":"property-change","name":"time-pos","data":"40.000000"}`)
		if told := n.mpvWasTold(); !slices.Contains(told, "set_property speed 1.000000") {
			t.Fatalf("told mpv %q, want the party's speed back", told)
		}
	})
}

// The person's speed key while a set is pending is theirs: the ack says it, and it goes out.
func TestASpeedKeyWhileASetIsPendingGoesOut(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvAnswersLate()
		// a stop nobody leads, so the first seek or speed leads
		n.peerSays("a", `{"stamp":2,"paused":true,"at":40,"free":true,"speed":1,"event":"property-change","name":"time-pos","data":"40.000000"}`)
		n.personSpeeds(1.25)
		n.mpvObeys()
		got := n.broadcasts()
		if len(got) != 1 || got[0].Name != playSpeed || got[0].Speed == nil || *got[0].Speed != 1.25 {
			t.Fatalf("broadcast %+v, want the person's speed", got)
		}
	})
}

// A set that undoes a speed key the node hadn't read says so: the key came as the party
// changed, and the person is told to press it again.
func TestASetUndoingASpeedKeySaysSo(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.personSpeedsFirst("1.250000")
		n.peerSays("a", speedEvent(5, 2))
		n.mpvObeys()
		if told := n.mpvWasTold(); !slices.Contains(told, "show-text "+overruled+" 5000") {
			t.Fatalf("told mpv %q, want the person told their key was undone", told)
		}
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q, want nothing", said(got))
		}
	})
}
