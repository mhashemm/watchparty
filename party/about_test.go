package party

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhashemm/watchparty/relay/wire"
	"github.com/mhashemm/watchparty/server"
)

// A refused joiner is told of the video the party opened on, and of its playlist, with
// checksums short enough to compare by eye.
func TestTheRecordNamesTheVideoThePartyOpenedOn(t *testing.T) {
	t.Parallel()
	dir := writeFiles(t, map[string]string{"e1.mkv": "a", "e2.mkv": "bb", "e3.mkv": "c"})
	v, err := media(filepath.Join(dir, "e2.mkv"), true)
	if err != nil {
		t.Fatal(err)
	}
	sum, _ := fingerprint(filepath.Join(dir, "e2.mkv"))
	want := about{Name: "e2.mkv", Size: 2, Sum: sum[:shown], Videos: 3, Playlist: v.id[:shown]}
	if got := v.about(); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if v, _ = media(filepath.Join(dir, "e2.mkv"), false); v.about() != (about{Name: "e2.mkv", Size: 2, Sum: sum[:shown]}) {
		t.Fatalf("one file: got %+v", v.about())
	}
	link := "https://example.com/a/v.mkv?token=secret#t=3"
	if v, _ = media(link, false); v.about().Name != "v.mkv" {
		t.Fatalf("a link is named %q", v.about().Name)
	}
}

// What travels is opaque to the relay and the mesh, fits the relay's cap whatever the
// file's name, and can't forge a log line when it comes back.
func TestTheRecordTravelsSafely(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("ü", 300) + ".mkv"
	v := video{files: []string{long}, names: []string{long}, sums: []string{strings.Repeat("a", 64)}, sizes: []int64{1}}
	if h := v.about().header(); len(h) > wire.MaxVideo {
		t.Fatalf("a long name made %d bytes", len(h))
	}
	forged := about{Name: "a\nyour address to share is -addrs 6.6.6.6:1", Sum: "abc"}
	got, ok := parseAbout(forged.header())
	if !ok || strings.ContainsAny(got.Name, "\n\r") {
		t.Fatalf("got %+v", got)
	}
	if _, ok := parseAbout("not base64!"); ok {
		t.Fatal("junk parsed")
	}
}

func TestARefusalSaysWhatDiffers(t *testing.T) {
	t.Parallel()
	one := about{Name: "holiday.mkv", Size: 1_400_000_000, Sum: "3fa2c1d907be"}
	other := about{Name: "holiday-720p.mkv", Size: 700_000_000, Sum: "9b1e04aa52c7"}
	list := about{Name: "e4.mkv", Size: 702_000_000, Sum: "3fa2c1d907be", Videos: 12, Playlist: "9b1e04aa52c7"}
	short := about{Name: "e4.mkv", Size: 650_000_000, Sum: "5c0d81fe2a91", Videos: 11, Playlist: "77ab3e10c4d2"}
	refusal := func(a about) *server.DifferentFile { return &server.DifferentFile{Video: a.header()} }
	for _, c := range []struct {
		name   string
		theirs *server.DifferentFile
		ours   about
		local  bool
		want   string
	}{
		{"files", refusal(one), other, true, `your video is not the same file as the party's:
  the party's: holiday.mkv · 1.4 GB · checksum 3fa2c1d907be
  yours:       holiday-720p.mkv · 700 MB · checksum 9b1e04aa52c7`},
		{"playlists", refusal(list), short, true, `your playlist is not the same file as the party's:
  the party's: e4.mkv · 702 MB · checksum 3fa2c1d907be
               12 videos · playlist checksum 9b1e04aa52c7
  yours:       e4.mkv · 650 MB · checksum 5c0d81fe2a91
               11 videos · playlist checksum 77ab3e10c4d2`},
		{"box unticked", refusal(list), other, true, "the party plays a playlist of 12 videos, and you picked one file: tick the box under File (-playlist)"},
		{"box ticked", refusal(one), short, true, "the party plays one file, and you picked a playlist of 11 videos: untick the box under File (no -playlist)"},
		// the box doesn't make a torrent or a link a playlist
		{"a torrent", refusal(list), other, false, `your video is not the same file as the party's:
  the party's: e4.mkv · 702 MB · checksum 3fa2c1d907be
               12 videos · playlist checksum 9b1e04aa52c7
  yours:       holiday-720p.mkv · 700 MB · checksum 9b1e04aa52c7`},
		{"an undescribed refusal", &server.DifferentFile{}, other, true, server.ErrDifferentFile.Error()},
	} {
		if got := differs(c.theirs, c.ours, c.local).Error(); got != c.want {
			t.Errorf("%s: got\n%s\nwant\n%s", c.name, got, c.want)
		}
	}
}
