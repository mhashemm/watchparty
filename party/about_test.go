package party

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mhashemm/watchparty/relay/wire"
	"github.com/mhashemm/watchparty/server"
)

// load is what Run plays for pick, in a new folder of files.
func load(t *testing.T, files map[string]string, pick string, playlist bool) video {
	t.Helper()
	v, err := media(filepath.Join(writeFiles(t, files), pick), playlist)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// A refused joiner is told of the video the party opened on, of its playlist with a short
// checksum per video, of a link by its whole text and of a torrent by its infohash.
func TestTheRecordNamesWhatThePartyPlays(t *testing.T) {
	t.Parallel()
	v := load(t, map[string]string{"e1.mkv": "a", "e2.mkv": "bb", "e3.mkv": "c"}, "e2.mkv", true)
	a := v.about()
	if a.Kind != "" || a.Name != "e2.mkv" || a.Size != 2 || a.Sum != v.sums[1][:shown] || a.Videos != 3 || a.Playlist != v.id[:shown] {
		t.Fatalf("a playlist: %+v", a)
	}
	if want := v.sums[0][:2*short] + v.sums[1][:2*short] + v.sums[2][:2*short]; fmt.Sprintf("%x", a.Sums) != want {
		t.Fatalf("sums %x, want %s", a.Sums, want)
	}
	if a := load(t, map[string]string{"e1.mkv": "a"}, "e1.mkv", false).about(); a.Videos != 0 || a.Sums != nil || a.Sum == "" {
		t.Fatalf("one file: %+v", a)
	}
	link := "https://example.com/watch?v=abc#t=3"
	if v, err := media(link, false); err != nil || v.about().Name != link || v.about().Kind != "link" || v.about().Sum != "" {
		t.Fatalf("a link: %+v %v", v.about(), err)
	}
	torrent := video{files: []string{"http://127.0.0.1:1/t/0/a.mkv"}, names: []string{"a.mkv"}, sizes: []int64{5}, infohash: strings.Repeat("ab", 20)}
	if a := torrent.about(); a.Kind != "torrent" || a.Sum != strings.Repeat("ab", 6) || a.Name != "a.mkv" {
		t.Fatalf("a torrent: %+v", a)
	}
}

// What travels is opaque to the relay and the mesh, fits the relay's cap whatever the
// name or the playlist, and can't forge a log line when it comes back.
func TestTheRecordTravelsSafely(t *testing.T) {
	t.Parallel()
	playlist := func(n int, name string) video {
		v := video{playlist: true, id: strings.Repeat("f", 64)}
		for range n {
			v.files, v.names = append(v.files, name), append(v.names, name)
			v.sums, v.sizes = append(v.sums, strings.Repeat("a", 64)), append(v.sizes, 1)
		}
		return v
	}
	long := strings.Repeat("ü", 400) + ".mkv"
	for _, c := range []struct {
		name string
		v    video
		sums bool
	}{{"a long name", playlist(1, long), true}, {"a long name in a long playlist", playlist(400, long), false}, {"a playlist that fits", playlist(200, "e.mkv"), true}} {
		a := c.v.about()
		if h := a.header(); len(h) > wire.MaxVideo || (a.Sums != nil) != c.sums {
			t.Errorf("%s: %d bytes, sums %v", c.name, len(h), a.Sums != nil)
		}
	}
	forged := About{Name: "a\nyour address to share is -addrs 6.6.6.6:1", Sum: "abc"}
	got, ok := parseAbout(forged.header())
	if !ok || strings.ContainsAny(got.Name, "\n\r") {
		t.Fatalf("got %+v", got)
	}
	if _, ok := parseAbout("not base64!"); ok {
		t.Fatal("junk parsed")
	}
}

// Either transport's refusal, however wrapped, becomes what differs; anything else is left
// alone.
func TestRunSaysWhatDiffersWhicheverTransportRefused(t *testing.T) {
	t.Parallel()
	party := load(t, map[string]string{"a.mkv": "a"}, "a.mkv", false).about().header()
	ours := load(t, map[string]string{"b.mkv": "b"}, "b.mkv", false)
	for _, err := range []error{
		fmt.Errorf("%w: 409 Conflict: %w", errRejected, &server.DifferentFile{Video: party}),
		fmt.Errorf("127.0.0.1:1/hi: %w", &server.DifferentFile{Video: party}),
	} {
		var e *NotTheSameFile
		if !errors.As(refusedFile(err, ours), &e) {
			t.Errorf("%v: got %v", err, refusedFile(err, ours))
		}
	}
	if other := errors.New("mpv exited"); refusedFile(other, ours) != other {
		t.Error("another error was changed")
	}
}

func TestARefusalSaysWhatDiffers(t *testing.T) {
	t.Parallel()
	abc := map[string]string{"e1.mkv": "a", "e2.mkv": "b", "e3.mkv": "c"}
	theirs := func(v video) *server.DifferentFile { return &server.DifferentFile{Video: v.about().header()} }
	sum := func(v video, i int) string { return v.sums[i][:2*short] }

	one, other := load(t, map[string]string{"one.mkv": "aaaa"}, "one.mkv", false), load(t, map[string]string{"other.mkv": "bb"}, "other.mkv", false)
	folder := load(t, abc, "e1.mkv", true)
	changed := load(t, map[string]string{"e1.mkv": "a", "e2.mkv": "B", "e3.mkv": "c"}, "e1.mkv", true)
	longer := load(t, map[string]string{"e1.mkv": "a", "e2.mkv": "b", "e3.mkv": "c", "e4.mkv": "d"}, "e1.mkv", true)
	shorter := load(t, map[string]string{"e1.mkv": "a", "e2.mkv": "b"}, "e1.mkv", true)
	link := func(q string) video { v, _ := media("https://example.com/watch?v="+q, false); return v }
	torrent := video{files: []string{"http://127.0.0.1:1/t/0/one.mkv"}, names: []string{"one.mkv"}, sizes: []int64{4}, infohash: strings.Repeat("ab", 20)}
	// the party's, as if too long to list
	bigAbout := folder.about()
	bigAbout.Sums = nil

	for _, c := range []struct {
		name    string
		theirs  *server.DifferentFile
		ours    video
		box     string
		text    string
		differs []string
	}{
		{"files", theirs(one), other, "", fmt.Sprintf("your video is not the same file as the party's:\n  the party's: one.mkv · 4 bytes · checksum %s\n  yours:       other.mkv · 2 bytes · checksum %s", one.sums[0][:shown], other.sums[0][:shown]), []string{"Video", "Size", "Checksum"}},
		{"box unticked", theirs(folder), load(t, abc, "e1.mkv", false), "tick", "the party plays a playlist of 3 videos, and you picked one file: tick the box under File (-playlist)", []string{"Videos"}},
		{"box ticked", theirs(one), folder, "untick", "the party plays one file, and you picked a playlist of 3 videos: untick the box under File (no -playlist)", []string{"Video", "Size", "Checksum", "Videos"}},
		{"one video differs", theirs(folder), changed, "", fmt.Sprintf("your playlist is not the same file as the party's: video 2 of 3 differs\n  the party's: checksum %s\n  yours:       e2.mkv · 1 byte · checksum %s", sum(folder, 1), sum(changed, 1)), []string{"Checksum"}},
		{"yours goes on", theirs(folder), longer, "", "your playlist is not the same file as the party's: yours has 4 videos and the party's 3\n  yours goes on with e4.mkv", []string{"Videos"}},
		{"the party's goes on", theirs(folder), shorter, "", "your playlist is not the same file as the party's: yours has 2 videos and the party's 3\n  the party's goes on after your e2.mkv", []string{"Videos"}},
		{"a playlist too long to list", &server.DifferentFile{Video: bigAbout.header()}, changed, "", fmt.Sprintf("your playlist is not the same file as the party's:\n  the party's: e1.mkv · 1 byte · checksum %s\n               3 videos · playlist checksum %s\n  yours:       e1.mkv · 1 byte · checksum %s\n               3 videos · playlist checksum %s", folder.sums[0][:shown], folder.id[:shown], changed.sums[0][:shown], changed.id[:shown]), []string{"Playlist checksum"}},
		{"links", theirs(link("abc")), link("xyz"), "", "your video is not the same file as the party's:\n  the party's: https://example.com/watch?v=abc\n  yours:       https://example.com/watch?v=xyz", []string{"Video"}},
		// the box doesn't make a torrent a playlist, and an infohash is no checksum
		{"a torrent", theirs(torrent), one, "", fmt.Sprintf("your video is not the same file as the party's:\n  the party's: one.mkv · 4 bytes · infohash %s\n  yours:       one.mkv · 4 bytes · checksum %s", strings.Repeat("ab", 6), one.sums[0][:shown]), nil},
	} {
		var e *NotTheSameFile
		if !errors.As(differs(c.theirs, c.ours), &e) {
			t.Fatalf("%s: got %v", c.name, differs(c.theirs, c.ours))
		}
		differ := []string{}
		for _, r := range e.Rows {
			if r.Differs {
				differ = append(differ, r.Label)
			}
		}
		if e.Box != c.box || e.Error() != c.text || !slices.Equal(differ, c.differs) && (len(differ) > 0 || len(c.differs) > 0) {
			t.Errorf("%s: box %q, rows differ at %q, got\n%s\nwant\n%s", c.name, e.Box, differ, e.Error(), c.text)
		}
	}
	if got := differs(&server.DifferentFile{}, other); got.Error() != server.ErrDifferentFile.Error() {
		t.Errorf("an undescribed refusal: %v", got)
	}
}
