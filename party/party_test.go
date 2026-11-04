package party

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		s, err := fingerprint(path)
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
	if _, err := fingerprint(dir); err == nil {
		t.Error("a folder got a fingerprint")
	}
}

// mpv plays URLs too. Nothing is fetched: the same URL is the same video.
func TestFingerprintOfAURLIsTheURL(t *testing.T) {
	t.Parallel()
	a, err := fingerprint("https://example.com/video.mkv")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := fingerprint("https://example.com/video.mkv")
	c, _ := fingerprint("https://example.com/other.mkv")
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
