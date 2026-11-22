package party

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhashemm/watchparty/mpv"
)

// Only the content counts: a copy under another name and folder, with another
// timestamp, is the same video, and a change in any sample or in the size is not.
func TestFingerprintIsContentOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name string, size int64, at ...int64) string {
		t.Helper()
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		for _, off := range at {
			f.WriteAt([]byte{1}, off)
		}
		return path
	}
	sum := func(path string) string {
		t.Helper()
		s, _, err := fingerprint(path)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	for _, size := range []int64{1000, 64 << 20} {
		orig := sum(write("video.mkv", size))
		os.Mkdir(filepath.Join(dir, "elsewhere"), 0o755)
		copied := write("elsewhere/renamed.mp4", size)
		os.Chtimes(copied, time.Unix(0, 0), time.Unix(0, 0))
		if sum(copied) != orig {
			t.Errorf("%d bytes: a copy under another name, path and time got another fingerprint", size)
		}
		if sum(write("first.mkv", size, 0)) == orig {
			t.Errorf("%d bytes: a change in the first byte kept the fingerprint", size)
		}
		if sum(write("last.mkv", size, size-1)) == orig {
			t.Errorf("%d bytes: a change in the last byte kept the fingerprint", size)
		}
		if sum(write("longer.mkv", size+1)) == orig {
			t.Errorf("%d bytes: a longer file kept the fingerprint", size)
		}
	}
	if _, _, err := fingerprint(dir); err == nil {
		t.Error("a folder got a fingerprint")
	}
}

// mpv plays URLs too. Nothing is fetched: the same URL is the same video.
func TestFingerprintOfAURLIsTheURL(t *testing.T) {
	t.Parallel()
	a, _, err := fingerprint("https://example.com/video.mkv")
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := fingerprint("https://example.com/video.mkv")
	c, _, _ := fingerprint("https://example.com/other.mkv")
	if a != b || a == c {
		t.Errorf("same URL matched: %v, different URL matched: %v", a == b, a == c)
	}
}

// mpv's output arrives in arbitrary chunks; each line is logged once, whole.
func TestLineLogSplitsLines(t *testing.T) {
	t.Parallel()
	out := bytes.Buffer{}
	w := &lineLog{l: log.New(&out, "", 0)}
	for _, chunk := range []string{"Failed to rec", "ognize file format.\nExiting... (Err", "ors when loading file)\n", "partial"} {
		if n, err := w.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	want := "mpv: Failed to recognize file format.\nmpv: Exiting... (Errors when loading file)\n"
	if out.String() != want {
		t.Fatalf("logged %q, want %q", out.String(), want)
	}
}

// A hang-up before the file loads usually means mpv is exiting, so its exit status is
// worth waiting for. But the socket can also drop with mpv alive, and it never exits.
func TestWaitExitDoesNotWaitForever(t *testing.T) {
	t.Parallel()
	if err := waitExit(make(chan error), time.Millisecond); !errors.Is(err, errHungUp) {
		t.Errorf("a live mpv: got %v, want errHungUp", err)
	}
	exited := make(chan error, 1)
	failed := errors.New("exit status 2")
	exited <- failed
	if err := waitExit(exited, time.Minute); !errors.Is(err, failed) || !strings.Contains(err.Error(), "before the video was ready") {
		t.Errorf("an exiting mpv: got %v, want its status", err)
	}
	exited <- nil
	if err := waitExit(exited, time.Minute); err != nil {
		t.Errorf("mpv closed by the user: got %v, want nil", err)
	}
}

// Split on spaces alone, a path with a space became two arguments and mpv took the
// second half for another file to play.
func TestMpvArgsKeepQuotedSpaces(t *testing.T) {
	t.Parallel()
	for flags, want := range map[string][]string{
		"":                                   nil,
		"  --fullscreen\t--volume=70 ":       {"--fullscreen", "--volume=70"},
		`--sub-file="C:\My Subs\a.srt" --fs`: {`--sub-file=C:\My Subs\a.srt`, "--fs"},
		`"--title=two words"`:                {"--title=two words"},
		`--title=""`:                         {"--title="},
		`--title="never closed`:              {"--title=never closed"},
	} {
		if got := mpvArgs(flags); !slices.Equal(got, want) {
			t.Errorf("mpvArgs(%q) = %q, want %q", flags, got, want)
		}
	}
}

// What Run returns is decided from what is known when it ends, in party/CLAUDE.md's
// order, never from which channel fired first.
func TestOutcomeFollowsTheFactsInOrder(t *testing.T) {
	t.Parallel()
	exited := errors.New("mpv exited before the video was ready: signal: killed")
	stopped, stop := context.WithCancel(context.Background())
	stop()
	rejected := errors.New("rejected by relay: 403 Forbidden")
	refused, refuse := context.WithCancelCause(context.Background())
	refuse(rejected)
	live := context.Background()
	for _, c := range []struct {
		name      string
		parent, c context.Context
		quit      bool
		err, want error
	}{
		{"the caller stopped it, and mpv was killed for it", stopped, stopped, false, exited, nil},
		{"the user quit mpv", live, stopped, true, exited, nil},
		{"something inside stopped it", live, stopped, false, exited, errStopped},
		{"the relay rejected it, and said why", live, refused, false, exited, rejected},
		{"mpv's own reason", live, live, false, exited, exited},
		{"a normal end", live, live, false, nil, nil},
	} {
		if got := outcome(c.parent, c.c, c.quit, c.err); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// A relay can refuse before mpv has started. exec then says only "context canceled"; Run
// must say why the party stopped.
func TestAStopBeforeMpvStartsSaysWhy(t *testing.T) {
	t.Parallel()
	rejected := errors.New("rejected by relay: 409 Conflict")
	c, stop := context.WithCancelCause(context.Background())
	stop(rejected)
	var playing atomic.Pointer[mpv.Client]
	v := video{entries: []entry{{file: "video.mkv", name: "video.mkv"}}}
	err := runMpv(context.Background(), c, func() {}, Config{Mpv: os.Args[0]}, v, 0, make(chan mpv.IncomingMessage), make(chan []byte), &playing)
	if err != rejected {
		t.Fatalf("got %v, want %v", err, rejected)
	}
}

// Stop pressed while mpv starts, before its socket answers, is a normal stop. Stop kills
// mpv, so its exit and the cancel both arrive: whichever Run sees first, it returns nil,
// not "mpv exited before the video was ready", which the GUI would diagnose.
func TestStopWhileMpvStartsIsANormalStop(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("a shell script stands in for mpv")
	}
	dir := t.TempDir()
	video, started, fake := filepath.Join(dir, "v.mkv"), filepath.Join(dir, "started"), filepath.Join(dir, "mpv")
	os.WriteFile(video, []byte("a video"), 0o600)
	// an mpv that never opens its socket
	os.WriteFile(fake, []byte("#!/bin/sh\ntouch '"+started+"'\nexec sleep 30\n"), 0o700)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{File: video, Mpv: fake, Relay: "ws://127.0.0.1:1", Room: "r", Secret: "s", Hostname: "h", CacheDir: dir})
	}()
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Run ended before mpv started: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("got %v, want a normal stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run never ended")
	}
}

// What the person types reaches the node in order, and the forwarding ends with the party.
func TestChatReachesTheNodeInOrder(t *testing.T) {
	t.Parallel()
	c, cancel := context.WithCancel(context.Background())
	chat, incoming, done := make(chan string), make(chan mpv.IncomingMessage, 2), make(chan struct{})
	go func() {
		forwardChat(c, chat, incoming)
		close(done)
	}()
	chat <- "a"
	chat <- "b"
	if a, b := <-incoming, <-incoming; a.Say != "a" || b.Say != "b" {
		t.Fatalf("got %+v then %+v", a, b)
	}
	cancel()
	<-done
}
