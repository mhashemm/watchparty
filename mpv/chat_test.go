package mpv

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
)

// logged is everything the package's tests logged, kept only as whether it held a text.
var logged = &logSpy{seen: map[string]bool{}}

func TestMain(m *testing.M) {
	log.SetOutput(io.MultiWriter(os.Stderr, logged))
	os.Exit(m.Run())
}

// logSpy remembers which of the texts it watches for were written.
type logSpy struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (l *logSpy) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for text := range l.seen {
		if bytes.Contains(p, []byte(text)) {
			l.seen[text] = true
		}
	}
	return len(p), nil
}

func (l *logSpy) watch(texts ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, text := range texts {
		l.seen[text] = false
	}
}

func (l *logSpy) saw(text string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen[text]
}

// What the person types goes to the party and on their own screen; it holds no state, so
// their next change takes the very next stamp.
func TestAChatMessageGoesOutAndShowsAsYours(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		resumed := n.broadcasts()
		n.personTypes("  hi there \n")
		if got := fmt.Sprint(said(n.broadcasts())); got != "[chat=hi there]" {
			t.Fatalf("broadcast %s, want the message trimmed", got)
		}
		if got := fmt.Sprint(n.mpvWasTold()); got != "[show-text you: hi there 5000]" {
			t.Fatalf("told mpv %s", got)
		}
		if got := fmt.Sprint(n.heardChats()); got != "[you: hi there]" {
			t.Fatalf("handed on %s", got)
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
		if got := fmt.Sprint(n.heardChats()); got != "[z: brb]" {
			t.Fatalf("handed on %s", got)
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
		n.personTypes(long)
		if got := said(n.broadcasts()); fmt.Sprint(got) != "[chat="+cut+"]" {
			t.Fatalf("broadcast %q, want it cut", got)
		}
		if got := fmt.Sprint(n.mpvWasTold()); got != "[show-text z: "+cut+" 5000 show-text you: "+cut+" 5000]" {
			t.Fatalf("told mpv %s, want both cut", got)
		}
		n.peerSays("z", `{"name":"chat","data":"  "}`)
		n.personTypes(" \t")
		if got := said(n.broadcasts()); len(got) != 0 {
			t.Fatalf("broadcast %q", got)
		}
		if got := n.mpvWasTold(); len(got) != 0 {
			t.Fatalf("told mpv %q", got)
		}
	})
}

// No chat message reaches the log, which a desktop can keep on disk: neither one shown, nor
// one mpv refuses to show. A notice after them is logged, so the log was read past them.
func TestAChatMessageIsNeverLogged(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		ours, theirs, refused, after := "chat-ours-5b1e", "chat-theirs-9c2d", "chat-refused-77af", "notice-after-3e0a"
		logged.watch(ours, theirs, refused, after)
		n.personTypes(ours)
		n.peerSays("z", `{"name":"chat","data":"`+theirs+`"}`)
		n.mpvRefuses("show-text", "error running command")
		n.peerSays("z", `{"name":"chat","data":"`+refused+`"}`)
		n.mpvTakesAgain()
		n.peerSays("z", `{"name":"pause-reason","data":"`+after+`"}`)
		if len(n.mpvWasTold()) != 4 || !logged.saw(after) {
			t.Fatal("mpv wasn't told all four, or the log never had the notice after them")
		}
		for _, text := range []string{ours, theirs, refused} {
			if logged.saw(text) {
				t.Errorf("the log has %q", text)
			}
		}
	})
}
