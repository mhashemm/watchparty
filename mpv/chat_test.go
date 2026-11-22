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
	"testing/synctest"
	"time"
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

// shows is mpv told to show these lines of chat, in their own layer.
func shows(lines ...string) string {
	return "osd-overlay 1 ass-events " + chatStyle + strings.Join(lines, `\N`) + " 0 720 0"
}

// ourLine and theirLine are one message as shown: whose it is in the colour, never the name.
func ourLine(text string) string { return `{\1c&HA6E3B8&}{\b1}you{\b0}: ` + text }
func theirLine(who, text string) string {
	return `{\1c&HFFFFFF&}{\b1}` + who + `{\b0}: ` + text
}

const noChat = "osd-overlay 1 none"

// What the person types goes to the party and on their own screen; it holds no state, so
// their next change takes the very next stamp.
func TestAChatMessageGoesOutAndShowsAsYours(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.mpvSays(propertyChangeLine(pause, "no", 0))
		resumed := n.broadcasts()
		n.personTypes("  hi \t there \n")
		if got := fmt.Sprint(said(n.broadcasts())); got != "[chat=hi there]" {
			t.Fatalf("broadcast %s, want the message on one line", got)
		}
		if got, want := n.mpvWasTold(), []string{shows(ourLine("hi there"))}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("told mpv %q, want %q", got, want)
		}
		if got := fmt.Sprint(n.heardChats()); got != "[you (ours): hi there]" {
			t.Fatalf("handed on %s", got)
		}
		n.mpvSays(mpvAt(20), propertyChangeLine(pause, "yes", 0))
		paused := n.broadcasts()
		if len(resumed) == 0 || len(paused) == 0 || paused[0].Stamp != resumed[0].Stamp+1 {
			t.Fatalf("resumed %+v then paused %+v, want the next stamp", resumed, paused)
		}
	})
}

// A member's message is only shown, under their name; one named "you" is still theirs, in
// their colour, and what they wrote shows as written, never as ASS tags.
func TestAMembersChatMessageIsOnlyShown(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.peerSays("z", `{"name":"chat","data":"brb"}`)
		n.peerSays("you", `{"name":"chat","data":"{\\an5}big\\N{\\fs99}"}`)
		want := []string{shows(theirLine("z", "brb")), shows(theirLine("z", "brb"), theirLine("you", "\\{\\\ufeffan5\\}big\\\ufeffN\\{\\\ufefffs99\\}"))}
		if got := n.mpvWasTold(); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("told mpv %q, want %q", got, want)
		}
		if got := fmt.Sprint(n.heardChats()); got != `[z: brb you: {\an5}big\N{\fs99}]` {
			t.Fatalf("handed on %s", got)
		}
		if got := said(n.broadcasts()); len(got) != 0 {
			t.Fatalf("broadcast %q", got)
		}
	})
}

// mpv shows the last three messages, each for chatOnScreen, then none: it never covers the
// notices, which have the screen's other corner.
func TestChatShowsTheLastThreeForAWhile(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		for _, text := range []string{"one", "two", "three"} {
			n.peerSays("z", `{"name":"chat","data":"`+text+`"}`)
			time.Sleep(time.Second)
		}
		n.mpvWasTold()
		n.personTypes("four")
		if got, want := n.mpvWasTold(), []string{shows(theirLine("z", "two"), theirLine("z", "three"), ourLine("four"))}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("with a fourth, told mpv %q, want %q", got, want)
		}
		// one came at 0s, two at 1s, three at 2s, four at 3s: one's time is up at 8s, but four
		// had already pushed it off, so only two going at 9s changes the screen
		for _, step := range []struct {
			wait time.Duration
			want []string
		}{
			{chatOnScreen - 3*time.Second, nil},
			{time.Second, []string{shows(theirLine("z", "three"), ourLine("four"))}},
			{time.Second, []string{shows(ourLine("four"))}},
			{time.Second, []string{noChat}},
		} {
			time.Sleep(step.wait)
			synctest.Wait()
			if got := n.mpvWasTold(); fmt.Sprint(got) != fmt.Sprint(step.want) {
				t.Fatalf("%v later, told mpv %q, want %q", step.wait, got, step.want)
			}
		}
	})
}

// A long message is broken into lines of chatWrap characters at most, a word longer than one
// cut: ASS would run it across the whole screen, over the notices.
func TestALongChatMessageIsBrokenIntoLines(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", chatWrap+2)
	for _, c := range []struct{ text, want string }{
		{"short one", "[short one]"},
		{strings.Repeat("ab ", 20), "[" + strings.TrimSpace(strings.Repeat("ab ", 16)) + " " + strings.TrimSpace(strings.Repeat("ab ", 4)) + "]"},
		{"a " + long + " b", "[a " + strings.Repeat("x", chatWrap) + " xx b]"},
	} {
		if got := fmt.Sprint(wrapped(chatText(c.text))); got != c.want {
			t.Errorf("%q: got %s, want %s", c.text, got, c.want)
		}
	}
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
		if got := fmt.Sprint(n.heardChats()); got != "[z: "+cut+" you (ours): "+cut+"]" {
			t.Fatalf("handed on %s, want both cut", got)
		}
		n.mpvWasTold()
		n.peerSays("z", `{"name":"chat","data":" \u0007 "}`)
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
		n.mpvRefuses("osd-overlay", "error running command")
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

// A message typed while the relay is lost isn't sent, as the relay would drop it, nor shown as
// sent: the screen says so. Once back, messages go out again.
func TestAChatMessageTypedWhileTheRelayIsLostIsNotSent(t *testing.T) {
	nodeTest(t, func(t *testing.T) {
		n := ready(t, setup{})
		n.transportSays(IncomingMessage{Lost: true})
		n.mpvWasTold()
		n.broadcasts()
		n.personTypes("anyone there?")
		if got := said(n.broadcasts()); len(got) != 0 {
			t.Fatalf("broadcast %q while the relay was lost", got)
		}
		if got := fmt.Sprint(n.mpvWasTold()); got != "[show-text "+chatNotSent+" 5000]" {
			t.Fatalf("told mpv %s", got)
		}
		if got := n.heardChats(); len(got) != 0 {
			t.Fatalf("handed on %q as shown", got)
		}
		n.transportSays(IncomingMessage{Back: true})
		n.broadcasts()
		n.personTypes("back")
		if got := fmt.Sprint(said(n.broadcasts())); got != "[chat=back]" {
			t.Fatalf("once back, broadcast %s", got)
		}
	})
}
