package mpv

import (
	"fmt"
	"slices"
	"testing"
)

// speedEvent is a member's speed change as it arrives, sent from a stop.
func speedEvent(stamp int, speed float64, speedStamp int) string {
	return fmt.Sprintf(`{"stamp":%d,"paused":true,"at":0,"speed":%g,"speedStamp":%d,"event":"property-change","name":"speed","data":"%f"}`, stamp, speed, speedStamp, speed)
}

// personSpeeds is the person's speed key in mpv.
func (n *node) personSpeeds(v float64) {
	n.t.Helper()
	n.mpvSays(propertyChangeLine(playSpeed, seconds(v), 0))
}

// The person's speed goes out with a clock of its own: the party's stamp stays, so the
// stop keeps its leader, and anyone can change it.
func TestTheSpeedChangedWhileStoppedGoesOut(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takesFrom("a", 3, true, 20)
		n.personSpeeds(1.5)
		got := n.broadcasts()
		if len(got) != 1 || got[0].Name != playSpeed || got[0].Speed == nil || *got[0].Speed != 1.5 || got[0].SpeedStamp != 1 || got[0].Stamp != 3 {
			t.Fatalf("broadcast %+v, want the speed 1.5 at speedStamp 1, with the party's stamp 3", got)
		}
		if told := n.mpvWasTold(); len(told) != 0 {
			t.Fatalf("told mpv %q, want nothing", told)
		}
		// a follower still: the stop's leader is a
		n.mpvSays(mpvAt(50))
		n.keepsTheSeek("a")
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
			if e.Speed == nil || *e.Speed != 2 || e.SpeedStamp != 1 || e.Free {
				t.Fatalf("broadcast %+v, want the speed 2 at speedStamp 1, led by this node", e)
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
		n.peerSays("a", speedEvent(1, 1.25, 1))
		want := "[show-text speed 1.25× by a 5000 set_property speed 1.250000]"
		if told := n.mpvWasTold(); fmt.Sprint(told) != want {
			t.Fatalf("told mpv %q, want %s", told, want)
		}
		n.mpvObeys()
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q, want nothing", said(got))
		}
	})
}

// A member's speed reaching a node that plays (it was changed from a stop that resumed
// meanwhile) stops the party: nobody leads that stop.
func TestAMembersSpeedReachingAPlayerStopsTheParty(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.peerSays("a", speedEvent(1, 1.5, 1))
		told := n.mpvWasTold()
		if !slices.Contains(told, "show-text paused: speed changed by a 5000") || !slices.Contains(told, "set_property speed 1.500000") || !slices.Contains(told, "set_property pause true") {
			t.Fatalf("told mpv %q, want it stopped at the new speed", told)
		}
		n.mpvObeys()
		got := n.broadcasts()
		if len(got) != 1 || got[0].Name != pause || got[0].Data != "yes" || !got[0].Free || got[0].Speed == nil || *got[0].Speed != 1.5 {
			t.Fatalf("broadcast %+v, want a free stop at 1.5", got)
		}
	})
}

// The stop a member's speed change made comes with the speed: one set takes both, and the
// node stops nothing itself.
func TestAStopWithANewSpeedIsOneSet(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.peerSays("a", `{"stamp":3,"paused":true,"at":20,"speed":2,"speedStamp":1,"event":"property-change","name":"pause","data":"yes"}`)
		told := n.mpvWasTold()
		// a's stop says the speed it heard, maybe from someone else: it names nobody
		want := []string{"show-text speed 2× 5000", "set_property speed 2.000000", "set_property pause true", "set_property time-pos 20.000000", "show-text paused by a 5000"}
		if fmt.Sprint(told) != fmt.Sprint(want) {
			t.Fatalf("told mpv %q, want %q", told, want)
		}
		n.mpvObeys()
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q, want nothing", said(got))
		}
	})
}

// The newer speedStamp wins; of the same, the slower, on every node.
func TestTheNewerSpeedWinsAndOfTwoAtOnceTheSlower(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.personSpeeds(1.5)
		n.broadcasts()
		for _, c := range []struct {
			speed      float64
			speedStamp int
			want       string
		}{
			{2, 1, "[]"},
			{0.5, 0, "[]"},
			// mpv's range is 0.01 to 100
			{1000, 9, "[]"},
			{1.25, 1, "[show-text speed 1.25× by a 5000 set_property speed 1.250000]"},
			{3, 2, "[show-text speed 3× by a 5000 set_property speed 3.000000]"},
		} {
			n.peerSays("a", speedEvent(1, c.speed, c.speedStamp))
			if told := n.mpvWasTold(); fmt.Sprint(told) != c.want {
				t.Fatalf("speed %g at speedStamp %d: told mpv %q, want %s", c.speed, c.speedStamp, told, c.want)
			}
			n.mpvObeys()
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

// A joiner takes the party's speed with where it is.
func TestAJoinerTakesThePartysSpeed(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{joiner: true})
		n.broadcasts()
		n.peerSays("a", `{"stamp":4,"paused":true,"at":40,"speed":1.5,"speedStamp":2,"event":"property-change","name":"time-pos","data":"40.000000"}`)
		// an answer passes on the speed its sender heard: it names nobody
		if told := n.mpvWasTold(); !slices.Contains(told, "set_property speed 1.500000") || !slices.Contains(told, "show-text speed 1.5× 5000") {
			t.Fatalf("told mpv %q, want the party's speed, naming nobody", told)
		}
	})
}

// The person's speed key while a set is pending is theirs: the ack says it, and it goes out.
func TestASpeedKeyWhileASetIsPendingGoesOut(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvAnswersLate()
		n.peerSays("a", stamped(2, seekEvent(40)))
		n.personSpeeds(1.25)
		n.mpvObeys()
		got := n.broadcasts()
		if len(got) != 1 || got[0].Name != playSpeed || got[0].Speed == nil || *got[0].Speed != 1.25 || got[0].SpeedStamp != 1 {
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
		n.peerSays("a", speedEvent(1, 2, 1))
		n.mpvObeys()
		if told := n.mpvWasTold(); !slices.Contains(told, "show-text "+overruled+" 5000") {
			t.Fatalf("told mpv %q, want the person told their key was undone", told)
		}
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %q, want nothing", said(got))
		}
	})
}

// A joiner's speed key before its answer stays home: the party's answer sets mpv back.
func TestAJoinersSpeedKeyWaitsForTheParty(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := newNode(t, setup{joiner: true})
		n.broadcasts()
		n.personSpeeds(1.5)
		if got := n.broadcasts(); len(got) != 0 {
			t.Fatalf("broadcast %+v, want nothing", got)
		}
		n.peerSays("a", `{"stamp":4,"paused":true,"at":40,"speed":1,"speedStamp":0,"event":"property-change","name":"time-pos","data":"40.000000"}`)
		if told := n.mpvWasTold(); !slices.Contains(told, "set_property speed 1.000000") {
			t.Fatalf("told mpv %q, want the party's speed back", told)
		}
		n.mpvObeys()
		if got := n.broadcasts(); slices.ContainsFunc(got, func(e Event) bool { return e.SpeedStamp != 0 || *e.Speed != 1 }) {
			t.Fatalf("broadcast %+v, want the party's speed", got)
		}
	})
}

// A newer speed reaching a player on a message that isn't a speed's stops the party naming
// nobody: its sender passes on what it heard.
func TestASpeedHeardSecondhandStopsNamingNobody(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.takesFrom("a", 5, false, 10)
		n.peerSays("b", `{"stamp":5,"paused":false,"at":10,"speed":1.5,"speedStamp":1,"event":"property-change","name":"pause","data":"no"}`)
		told := n.mpvWasTold()
		if !slices.Contains(told, "show-text paused: speed changed 5000") || !slices.Contains(told, "set_property pause true") {
			t.Fatalf("told mpv %q, want it stopped, naming nobody", told)
		}
	})
}
