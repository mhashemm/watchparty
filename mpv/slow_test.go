package mpv

import (
	"fmt"
	"testing"
	"time"
)

// A node waiting on its cache falls behind everyone still playing. It pauses the
// party instead, and stays paused for a manual resume once the cache refills.
func TestBufferingPausesTheParty(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		n.mpvSays(mpvAt(20), propertyChangeLine(pausedForCache, "yes", 0))
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true show-text paused for everyone: buffering 5000]" {
			t.Fatalf("told mpv %q, want the node to pause itself", got)
		}
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes time-pos=20.000000 pause-reason=buffering]" {
			t.Fatalf("broadcast %q", got)
		}
		// a resume while still stalled is not passed on, and pauses again
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[set_property pause true show-text paused for everyone: still buffering 5000]" {
			t.Fatalf("told mpv %q, want the node to pause itself again", got)
		}
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[time-pos=20.000000 pause-reason=still buffering]" {
			t.Fatalf("broadcast %q", got)
		}
	})
}

// Buffering says when mpv waits on its cache, so a torrent can say how its download goes
// only then; how full the cache is goes nowhere but the log.
func TestBufferingIsWhileMpvWaitsOnItsCache(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.plays()
		if n.c.Buffering() {
			t.Fatal("buffering before mpv said so")
		}
		n.mpvSays(mpvAt(20), propertyChangeLine(pausedForCache, "yes", 0), propertyChangeLine(cacheFill, "40", 0))
		if !n.c.Buffering() {
			t.Fatal("not buffering while mpv waits on its cache")
		}
		n.mpvSays(propertyChangeLine(pause, "yes", 0))
		n.mpvWasTold()
		n.broadcasts()
		n.mpvSays(propertyChangeLine(cacheFill, "80", 0), propertyChangeLine(pausedForCache, "no", 0), propertyChangeLine(cacheFill, "100", 0))
		if n.c.Buffering() {
			t.Fatal("still buffering once the cache refilled")
		}
		if got := n.mpvWasTold(); len(got) != 0 {
			t.Fatalf("told mpv %q", got)
		}
		if got := said(n.broadcasts()); len(got) != 0 {
			t.Fatalf("broadcast %q", got)
		}
	})
}

// stallsOnAStream is a node playing a stream whose cache ran out: the party paused for it.
func stallsOnAStream(n *node, network string) {
	n.t.Helper()
	n.mpvSays(propertyChangeLine(viaNetwork, network, 0))
	n.plays()
	n.mpvSays(mpvAt(20), propertyChangeLine(pausedForCache, "yes", 0))
	n.mpvSays(propertyChangeLine(pause, "yes", 0))
	n.mpvWasTold()
	n.broadcasts()
}

// A member that ran out of cache on a stream tells the party how many whole seconds it has
// ready while the party waits: at once, then once it had that many seconds, 5s to a minute
// apart, only when the number changed, and never once it plays again. Its own mpv shows the
// first, then only one at least twice the last shown and at least 10s.
func TestASlowMemberSaysHowMuchItHasReady(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		stallsOnAStream(n, "yes")
		has := func(secs string) string {
			n.mpvSays(propertyChangeLine(cacheAhead, secs, 0))
			return fmt.Sprint(said(n.broadcasts()))
		}
		if got := has("0.4"); got != "[buffered=0]" {
			t.Fatalf("with 0.4s ready: broadcast %s", got)
		}
		if got := fmt.Sprint(n.mpvWasTold()); got != "[show-text your network is slow: 0s ready 5000]" {
			t.Fatalf("told mpv %s, want the number on its screen too", got)
		}
		for _, step := range []struct {
			wait               time.Duration
			has, wants, screen string
		}{
			{4 * time.Second, "3.2", "[]", ""},       // under the 5s floor
			{time.Second, "7.0", "[buffered=7]", ""}, // under 10s: not on the screen
			{6 * time.Second, "12.0", "[]", ""},      // its 7 seconds haven't played out
			{time.Second, "13.0", "[buffered=13]", "13"},
			{13 * time.Second, "13.9", "[]", ""}, // due, but the same whole second
			{0, "75.0", "[buffered=75]", "75"},
			{59 * time.Second, "130.0", "[]", ""},        // a minute at most, but a minute
			{time.Second, "131.0", "[buffered=131]", ""}, // under twice 75
		} {
			time.Sleep(step.wait)
			if got := has(step.has); got != step.wants {
				t.Fatalf("with %ss ready, %v later: broadcast %s, want %s", step.has, step.wait, got, step.wants)
			}
			want := "[]"
			if step.screen != "" {
				want = "[show-text your network is slow: " + step.screen + "s ready 5000]"
			}
			if got := fmt.Sprint(n.mpvWasTold()); got != want {
				t.Fatalf("with %ss ready: told mpv %s, want %s", step.has, got, want)
			}
		}
		n.mpvSays(propertyChangeLine(pausedForCache, "no", 0), propertyChangeLine(pause, "no", 0))
		n.broadcasts()
		time.Sleep(time.Minute)
		if got := has("140.0"); got != "[]" {
			t.Fatalf("broadcast %s while playing", got)
		}
	})
}

// A peer's seconds ready show on mpv's screen as the node's own do: the first, then one twice
// the last shown, each member apart by its id (two named z here), afresh once the party plays.
func TestAMembersSecondsReadyShowWhenTheyDouble(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		says := func(from, secs string) string {
			n.transportSays(IncomingMessage{HostName: "z", From: from, Event: []byte(`{"name":"buffered","data":"` + secs + `"}`)})
			return fmt.Sprint(n.mpvWasTold())
		}
		shown := func(secs string) string { return "[show-text z's network is slow: " + secs + "s ready 5000]" }
		for _, step := range []struct{ from, secs, want string }{
			{"z1", "0", shown("0")},
			{"z1", "7", "[]"}, // under 10s
			{"z2", "3", shown("3")},
			{"z1", "12", shown("12")},
			{"z1", "23", "[]"}, // under twice 12
			{"z2", "9", "[]"},
			{"z1", "24", shown("24")},
		} {
			if got := says(step.from, step.secs); got != step.want {
				t.Fatalf("%s with %ss: told mpv %s, want %s", step.from, step.secs, got, step.want)
			}
		}
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		n.broadcasts()
		if got := says("z1", "4"); got != shown("4") {
			t.Fatalf("after a resume: told mpv %s, want the first again", got)
		}
	})
}

// A local file that ran out (a slow disk) says nothing: everyone has their own copy.
func TestALocalFileNeverSaysHowMuchItHasReady(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		stallsOnAStream(n, "no")
		n.mpvSays(propertyChangeLine(cacheAhead, "10.0", 0))
		if got := said(n.broadcasts()); len(got) != 0 {
			t.Fatalf("broadcast %q", got)
		}
	})
}

// A member's seconds ready are only for the GUI: they hold no state, so the node does nothing
// and its next change takes the very next stamp.
func TestAMembersSecondsReadyChangeNothing(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		resumed := n.broadcasts()
		n.peerSays("z", `{"name":"buffered","data":"45"}`)
		if got := fmt.Sprint(n.mpvWasTold()); got != "[show-text z's network is slow: 45s ready 5000]" {
			t.Fatalf("told mpv %s, want it only shown", got)
		}
		if got := said(n.broadcasts()); len(got) != 0 {
			t.Fatalf("broadcast %q", got)
		}
		n.mpvSays(mpvAt(20), propertyChangeLine(pause, "yes", 0))
		paused := n.broadcasts()
		if len(resumed) == 0 || len(paused) == 0 || paused[0].Stamp != resumed[0].Stamp+1 {
			t.Fatalf("resumed %+v then paused %+v, want the next stamp", resumed, paused)
		}
	})
}

// A peer's resume reaching a node that is still stalled is refused: the node never
// plays, and tells the party it is paused again and where. Applying the resume and
// pausing at once doesn't work, because mpv folds the two into no event at all.
func TestRemoteResumeWhileBufferingIsRefused(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(mpvAt(20), propertyChangeLine(pausedForCache, "yes", 0))
		n.broadcasts()
		n.peerSays("a", stamped(9, `{"event":"property-change","name":"pause","data":"no","at":20}`))
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[pause=yes time-pos=20.000000 pause-reason=still buffering]" {
			t.Fatalf("broadcast %q", got)
		}
		if got := n.mpvWasTold(); fmt.Sprint(got) != "[show-text paused for everyone: still buffering 5000]" {
			t.Fatalf("told mpv %q, want the resume refused", got)
		}
	})
}
