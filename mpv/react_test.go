package mpv

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"testing"
	"time"

	"github.com/yuin/gopher-lua/parse"
)

// bursts is mpv told to burst code, by the reactions script.
func bursts(code string) string {
	return "script-message-to " + reactionsScript + " " + reactBurst + " " + code
}

func reaction(code string) string { return `{"name":"react","data":"` + code + `"}` }

// A reaction the person picks goes to the party, bursts on their own screen and is handed on
// as theirs; it holds no state, so their next change takes the very next stamp.
func TestAReactionGoesOutAndBurstsHere(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		resumed := n.broadcasts()
		n.personPicks("1f602")
		if got := fmt.Sprint(said(n.broadcasts())); got != "[react=1f602]" {
			t.Fatalf("broadcast %s", got)
		}
		if got := fmt.Sprint(n.mpvWasTold()); got != "["+bursts("1f602")+"]" {
			t.Fatalf("told mpv %s", got)
		}
		if got := fmt.Sprint(n.heardChats()); got != "[you (ours) reacts 1f602]" {
			t.Fatalf("handed on %s", got)
		}
		n.mpvSays(mpvAt(20), propertyChangeLine(pause, "yes", 0))
		if paused := n.broadcasts(); len(resumed) == 0 || len(paused) == 0 || paused[0].Stamp != resumed[0].Stamp+1 {
			t.Fatalf("resumed %+v then paused %+v, want the next stamp", resumed, paused)
		}
	})
}

// One picked in mpv's own picker goes out the same way.
func TestAReactionPickedInMpvGoesOut(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(`{"event":"client-message","args":["` + reactPicked + `","1f62d"]}`)
		if got := fmt.Sprint(said(n.broadcasts())); got != "[react=1f62d]" {
			t.Fatalf("broadcast %s", got)
		}
		if got := fmt.Sprint(n.mpvWasTold()); got != "["+bursts("1f62d")+"]" {
			t.Fatalf("told mpv %s", got)
		}
	})
}

// A member's reaction only bursts, under their name.
func TestAMembersReactionBursts(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("z", reaction("1f494"))
		if got := fmt.Sprint(n.mpvWasTold()); got != "["+bursts("1f494")+"]" {
			t.Fatalf("told mpv %s", got)
		}
		if got := fmt.Sprint(n.heardChats()); got != "[z reacts 1f494]" {
			t.Fatalf("handed on %s", got)
		}
		if got := said(n.broadcasts()); len(got) != 0 {
			t.Fatalf("broadcast %q", got)
		}
	})
}

// Each person bursts once every reactEvery at most, this node's own person too: a burst covers
// the screen, and a member picking without end would cover it for good. Another person's
// reaction meanwhile still bursts.
func TestEachPersonBurstsOnceEveryTwoSeconds(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("z", reaction("1f602"))
		n.personPicks("1f602")
		time.Sleep(reactEvery - time.Millisecond)
		n.peerSays("z", reaction("1f62d"))
		n.personPicks("1f62d")
		n.peerSays("y", reaction("1f970"))
		if got, want := fmt.Sprint(n.heardChats()), "[z reacts 1f602 you (ours) reacts 1f602 y reacts 1f970]"; got != want {
			t.Fatalf("handed on %s, want %s", got, want)
		}
		if got := fmt.Sprint(said(n.broadcasts())); got != "[react=1f602]" {
			t.Fatalf("broadcast %s, want only the first", got)
		}
		time.Sleep(time.Millisecond)
		n.peerSays("z", reaction("1f62d"))
		n.personPicks("1f62d")
		if got, want := fmt.Sprint(n.heardChats()), "[z reacts 1f62d you (ours) reacts 1f62d]"; got != want {
			t.Fatalf("%v after the first, handed on %s, want %s", reactEvery, got, want)
		}
	})
}

// Only the reactions in the picker burst: anything else is neither sent nor shown.
func TestAnUnknownReactionIsDropped(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvWasTold()
		n.peerSays("z", reaction("1f600"))
		n.peerSays("z", reaction(`1f602\" x`))
		n.personPicks("hi")
		n.mpvSays(`{"event":"client-message","args":["` + reactPicked + `","1f4a9"]}`)
		if got := n.mpvWasTold(); len(got) != 0 {
			t.Fatalf("told mpv %q", got)
		}
		if got := said(n.broadcasts()); len(got) != 0 {
			t.Fatalf("broadcast %q", got)
		}
		if got := n.heardChats(); len(got) != 0 {
			t.Fatalf("handed on %q", got)
		}
	})
}

// A reaction is never logged, as chat isn't: not what goes out, comes in, is picked in mpv,
// or mpv refuses. A notice after them is logged, so the log was read past them.
func TestAReactionIsNeverLogged(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		after := "notice-after-77c1"
		logged.watch(reactBurst, `"react"`, after)
		n.personPicks("1f602")
		n.peerSays("z", reaction("1f62d"))
		time.Sleep(reactEvery)
		n.mpvSays(`{"event":"client-message","args":["` + reactPicked + `","1f970"]}`)
		n.mpvRefuses("script-message-to", "error running command")
		n.peerSays("z", reaction("1f494"))
		n.mpvTakesAgain()
		n.peerSays("z", `{"name":"pause-reason","data":"`+after+`"}`)
		if told := n.mpvWasTold(); len(told) != 5 || !logged.saw(after) {
			t.Fatalf("told mpv %q, or the log never had the notice after them", told)
		}
		for _, text := range []string{reactBurst, `"react"`} {
			if logged.saw(text) {
				t.Errorf("the log has %q", text)
			}
		}
	})
}

// A reaction picked while the relay is lost isn't sent, nor burst as sent: the screen says so.
func TestAReactionPickedWhileTheRelayIsLostIsNotSent(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.transportSays(IncomingMessage{Lost: true})
		n.mpvWasTold()
		n.broadcasts()
		n.personPicks("1f602")
		if got := said(n.broadcasts()); len(got) != 0 {
			t.Fatalf("broadcast %q while the relay was lost", got)
		}
		if got := fmt.Sprint(n.mpvWasTold()); got != "[show-text "+reactNotSent+" 5000]" {
			t.Fatalf("told mpv %s", got)
		}
		if got := n.heardChats(); len(got) != 0 {
			t.Fatalf("handed on %q", got)
		}
	})
}

// The person turning mpv's picker off or on in Settings reaches the script at once, mid-party.
func TestThePickerFollowsTheSetting(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvWasTold()
		n.personSetsPicker(false)
		n.personSetsPicker(true)
		want := "[script-message-to " + reactionsScript + " watchparty-picker no script-message-to " + reactionsScript + " watchparty-picker yes]"
		if got := fmt.Sprint(n.mpvWasTold()); got != want {
			t.Fatalf("told mpv %s, want %s", got, want)
		}
	})
}

// Every reaction in the picker has its art, the script mpv loads parses with it, and the GUI's
// SVG of each is well formed.
func TestEveryReactionHasItsArt(t *testing.T) {
	t.Parallel()
	codes := Reactions()
	if len(codes) != 26 {
		t.Fatalf("%d reactions, want 26", len(codes))
	}
	for _, code := range codes {
		svg := EmojiSVG(code)
		if len(emoji().Art[code]) == 0 || svg == "" {
			t.Fatalf("%s has no art", code)
		}
		d := xml.NewDecoder(bytes.NewReader([]byte(svg)))
		for {
			if _, err := d.Token(); err != nil {
				if err.Error() != "EOF" {
					t.Fatalf("%s's SVG: %v", code, err)
				}
				break
			}
		}
	}
	if _, err := parse.Parse(bytes.NewReader(ReactionsScript()), ReactionsFile); err != nil {
		t.Fatal(err)
	}
}
