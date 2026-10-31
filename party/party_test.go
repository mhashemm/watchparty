package party

import (
	"os"
	"path/filepath"
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
		orig := sum(write("movie.mkv", size))
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
	a, err := fingerprint("https://example.com/movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := fingerprint("https://example.com/movie.mkv")
	c, _ := fingerprint("https://example.com/other.mkv")
	if a != b || a == c {
		t.Errorf("same URL matched: %v, different URL matched: %v", a == b, a == c)
	}
}
