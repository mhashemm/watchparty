package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/mhashemm/watchparty/mpv"
	"github.com/mhashemm/watchparty/party"
)

func TestConfig(t *testing.T) {
	t.Parallel()
	relay := form{Mode: "relay", File: " /m.mkv ", Mpv: "/bin/mpv", Relay: " ws://r ", Room: "a b", Secret: "s", Local: true, Port: "1", Addrs: "x:1"}
	got, err := relay.config()
	want := party.Config{File: "/m.mkv", Mpv: "/bin/mpv", Relay: "ws://r", Room: "a b", Secret: "s"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("relay config = %+v, %v; want %+v", got, err, want)
	}

	direct := form{Mode: "direct", File: "/m.mkv", Mpv: "/bin/mpv", Relay: "ws://r", Local: true, Port: "6970", Addrs: "1.2.3.4:6969", Room: "r", Secret: " s "}
	got, err = direct.config()
	want = party.Config{File: "/m.mkv", Mpv: "/bin/mpv", Local: true, Port: 6970, PublicPort: party.DefaultPort, Addrs: "1.2.3.4:6969", Secret: "s"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("direct config = %+v, %v; want %+v", got, err, want)
	}

	for _, file := range []string{"https://example.com/watch?v=x", "/videos/notes.txt"} {
		if _, err := (form{Mode: "relay", File: file, Relay: "ws://r"}).config(); err != nil {
			t.Errorf("typed %q was refused: %v; only the picker filters by type", file, err)
		}
	}

	for _, f := range []form{
		{Mode: "relay", Relay: "ws://r"},
		{Mode: "relay", File: "/m.mkv"},
		{Mode: "direct", File: "/m.mkv", Port: "abc"},
	} {
		if _, err := f.config(); err == nil {
			t.Errorf("config(%+v) succeeded, want an error", f)
		}
	}
}

func TestInstallStepsMatchTheInstaller(t *testing.T) {
	t.Parallel()
	app := `C:\Users\me\AppData\Local\Programs\watchparty`
	for name, want := range map[string][]string{
		"mpv":    {"mpv-<version>-x86_64-w64-mingw32-full.zip", "into " + app + `\mpv.`, "Add " + app + `\mpv to your Path`},
		"ffmpeg": {"ffmpeg-<version>-essentials_build.zip", "top folder into " + app + `\ffmpeg.`, "Add " + app + `\ffmpeg\bin to your Path`},
		"yt-dlp": {"yt-dlp.exe from github.com/yt-dlp/yt-dlp/releases/latest into " + app + `\yt-dlp.`, "Add " + app + `\yt-dlp to your Path`},
	} {
		steps := strings.Join(installSteps("windows", name, app), "\n")
		for _, w := range want {
			if !strings.Contains(steps, w) {
				t.Errorf("%s's steps lack %q:\n%s", name, w, steps)
			}
		}
	}
	if steps := installSteps("darwin", "yt-dlp", "/Applications"); steps[len(steps)-1] != "Run brew install yt-dlp in Terminal." {
		t.Errorf("macOS steps: %q", steps)
	}
}

// A message goes to the running party as typed; with no party, or nothing typed, it goes nowhere.
func TestChatGoesToTheRunningParty(t *testing.T) {
	t.Parallel()
	g := newGUI(context.Background(), openStore(t.TempDir()+"/gui.json"))
	if err := g.chat("hi"); !errors.Is(err, errNoParty) {
		t.Fatalf("with no party: %v", err)
	}
	chat := make(chan string, 1)
	g.cancel, g.chatTo = func() {}, chat
	if err := g.chat("  "); err != nil || len(chat) != 0 {
		t.Fatalf("nothing typed: %v, %d sent", err, len(chat))
	}
	if err := g.chat("hi there"); err != nil {
		t.Fatalf("a message: %v", err)
	}
	if err := g.chat("again"); !errors.Is(err, errBusy) {
		t.Fatalf("with the party's queue full: %v", err)
	}
	if got := <-chat; got != "hi there" {
		t.Fatalf("sent %q", got)
	}
}

// mpv's reaction picker shows when the mouse moves unless Settings says otherwise, one of its
// modes; a change reaches a running party at once, the latest if it hadn't taken the one before.
func TestThePickerSettingReachesTheRunningParty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	g := newGUI(context.Background(), openStore(dir+"/gui.json"))
	if g.store.Settings.MpvPicker != "" {
		t.Fatal("not the default before anyone set it")
	}
	if err := g.saveSettings(settings{MpvPicker: "sometimes"}); err == nil {
		t.Fatal("a mode that isn't one was saved")
	}
	if err := g.saveSettings(settings{MpvPicker: "off"}); err != nil || openStore(dir+"/gui.json").Settings.MpvPicker != "off" {
		t.Fatalf("saving off: %v, saved %q", err, openStore(dir+"/gui.json").Settings.MpvPicker)
	}
	picker := make(chan string, 1)
	g.cancel, g.pickerTo = func() {}, picker
	g.saveSettings(settings{MpvPicker: "corner"})
	g.saveSettings(settings{})
	select {
	case got := <-picker:
		if got != mpv.PickerMove || len(picker) != 0 {
			t.Fatalf("the party got %q, then %d more; want only the latest, the default", got, len(picker))
		}
	default:
		t.Fatal("the change never reached the party")
	}
}

// A reaction goes to the running party too, on its own channel, and only one of the picker's.
func TestAReactionGoesToTheRunningParty(t *testing.T) {
	t.Parallel()
	g := newGUI(context.Background(), openStore(t.TempDir()+"/gui.json"))
	if err := g.react("1f602"); !errors.Is(err, errNoParty) {
		t.Fatalf("with no party: %v", err)
	}
	react, chat := make(chan string, 1), make(chan string, 1)
	g.cancel, g.reactTo, g.chatTo = func() {}, react, chat
	if err := g.react("1f600"); err == nil || len(react) != 0 {
		t.Fatalf("one not in the picker: %v, %d sent", err, len(react))
	}
	if err := g.react("1f602"); err != nil {
		t.Fatalf("a reaction: %v", err)
	}
	if err := g.react("1f62d"); !errors.Is(err, errBusy) {
		t.Fatalf("with the party's queue full: %v", err)
	}
	if got := <-react; got != "1f602" || len(chat) != 0 {
		t.Fatalf("sent %q, and %d as chat", got, len(chat))
	}
}

// Chat is kept only for the window, beside the log and never in it, and only while its party
// runs: one from an earlier party is dropped, and the end takes them all.
func TestChatIsKeptOnlyWhileThePartyRuns(t *testing.T) {
	t.Parallel()
	g := newGUI(context.Background(), openStore(t.TempDir()+"/gui.json"))
	type said struct {
		Who, Text string
		Ours      bool
	}
	poll := func(from int) (lines []string, chat []said) {
		t.Helper()
		n := g.news(g.run, 0, from)
		for _, m := range n.Said {
			chat = append(chat, said{m.Who, m.Text, m.Ours})
		}
		return n.Lines, chat
	}
	g.run, g.cancel = 1, func() {}
	earlier, heard := g.heard(0), g.heard(1)
	heard(mpv.Said{Who: "you", Text: "hi", Ours: true})
	earlier(mpv.Said{Who: "sam", Text: "from before", Ours: false})
	heard(mpv.Said{Who: "sam", Text: "yo", Ours: false})
	if lines, chat := poll(0); fmt.Sprint(chat) != "[{you hi true} {sam yo false}]" || len(lines) != 0 {
		t.Fatalf("chat %v, lines %q", chat, lines)
	}
	if _, chat := poll(1); fmt.Sprint(chat) != "[{sam yo false}]" {
		t.Fatalf("from the second: %v", chat)
	}
	g.ended(nil)
	heard(mpv.Said{Who: "sam", Text: "after the end", Ours: false})
	if _, chat := poll(0); len(chat) != 0 {
		t.Fatalf("after the end: %v", chat)
	}
	// a member sending without end: only the last saidKept are kept, and a read counts from
	// the run's first message, dropped or not
	g.run, g.cancel = 2, func() {}
	heard = g.heard(2)
	for i := range saidKept + 3 {
		heard(mpv.Said{Who: "sam", Text: fmt.Sprint(i), Ours: false})
	}
	if _, chat := poll(0); len(chat) != saidKept || chat[0].Text != "3" || chat[len(chat)-1].Text != fmt.Sprint(saidKept+2) {
		t.Fatalf("kept %d, from %v to %v", len(chat), chat[0], chat[len(chat)-1])
	}
	if _, chat := poll(saidKept + 1); fmt.Sprint(chat) != fmt.Sprintf("[{sam %d false} {sam %d false}]", saidKept+1, saidKept+2) {
		t.Fatalf("from the %dth: %v", saidKept+1, chat)
	}
}

// The window reads the log from where it stopped; a new party's lines start from its first, so
// two runs never splice.
func TestNewsNeverSplicesTwoRuns(t *testing.T) {
	t.Parallel()
	g := newGUI(context.Background(), openStore(t.TempDir()+"/gui.json"))
	g.run = 1
	fmt.Fprintln(g, "a\nb")
	n := g.news(1, 0, 0)
	if strings.Join(n.Lines, ",") != "a,b" || n.Run != 1 {
		t.Fatalf("first read: %+v", n)
	}
	fmt.Fprintln(g, "c")
	if n := g.news(1, 2, 0); strings.Join(n.Lines, ",") != "c" || n.From != 2 {
		t.Fatalf("from 2: %+v", n)
	}
	g.run, g.lines = 2, []string{"x"}
	if n := g.news(1, 3, 0); strings.Join(n.Lines, ",") != "x" || n.From != 0 || n.Run != 2 {
		t.Fatalf("a read from the run before: %+v", n)
	}
}

// Changes reach the window: a line, a message, the party's end each ask it for a frame.
func TestTheWindowHearsOfEachChange(t *testing.T) {
	t.Parallel()
	g := newGUI(context.Background(), openStore(t.TempDir()+"/gui.json"))
	asked := 0
	g.changed = func() { asked++ }
	fmt.Fprintln(g, "a line")
	g.run, g.cancel = 1, func() {}
	g.heard(1)(mpv.Said{Who: "sam", Text: "hi"})
	g.ended(nil)
	if asked != 3 {
		t.Fatalf("asked for %d frames, want 3", asked)
	}
}
