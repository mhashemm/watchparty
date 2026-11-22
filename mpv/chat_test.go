package mpv

import (
	"fmt"
	"strings"
	"testing"
)

// What the person types goes to the party and on their own screen; it holds no state, so
// their next change takes the very next stamp.
func TestAChatMessageGoesOutAndShowsAsYours(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		resumed := n.broadcasts()
		n.transportSays(IncomingMessage{Say: "  hi there \n"})
		if got := fmt.Sprint(said(n.broadcasts())); got != "[chat=hi there]" {
			t.Fatalf("broadcast %s, want the message trimmed", got)
		}
		if got := fmt.Sprint(n.mpvWasTold()); got != "[show-text you: hi there 5000]" {
			t.Fatalf("told mpv %s", got)
		}
		n.mpvSays(mpvAt(20), propertyChangeLine(pause, "yes", 0))
		paused := n.broadcasts()
		if len(resumed) == 0 || len(paused) == 0 || paused[0].Stamp != resumed[0].Stamp+1 {
			t.Fatalf("resumed %+v then paused %+v, want the next stamp", resumed, paused)
		}
	})
}

// A member's message is only shown, under their name.
func TestAMembersChatMessageIsOnlyShown(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("z", `{"name":"chat","data":"brb"}`)
		if got := fmt.Sprint(n.mpvWasTold()); got != "[show-text z: brb 5000]" {
			t.Fatalf("told mpv %s", got)
		}
		if got := said(n.broadcasts()); len(got) != 0 {
			t.Fatalf("broadcast %q", got)
		}
	})
}

// A message is cut to ChatLimit characters, not bytes, on both sides: a member's may not have
// been. One with nothing in it is neither sent nor shown.
func TestAChatMessageIsCutAndAnEmptyOneDropped(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		long, cut := strings.Repeat("é", ChatLimit+50), strings.Repeat("é", ChatLimit)
		n.peerSays("z", `{"name":"chat","data":"`+long+`"}`)
		n.transportSays(IncomingMessage{Say: long})
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[chat="+cut+"]" {
			t.Fatalf("broadcast %q, want it cut", got)
		}
		if got := fmt.Sprint(n.mpvWasTold()); got != "[show-text z: "+cut+" 5000 show-text you: "+cut+" 5000]" {
			t.Fatalf("told mpv %s, want both cut", got)
		}
		n.peerSays("z", `{"name":"chat","data":"  "}`)
		n.transportSays(IncomingMessage{Say: " \t"})
		if got := said(n.broadcasts()); len(got) != 0 {
			t.Fatalf("broadcast %q", got)
		}
		if got := n.mpvWasTold(); len(got) != 0 {
			t.Fatalf("told mpv %q", got)
		}
	})
}
