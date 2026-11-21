package party

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/mhashemm/watchparty/server"
)

// about is what a refused joiner is told of the party's video, to set beside its own: the
// video the party opened on and, for a playlist, how many it holds and its checksum. A list
// of every video would say which one differs, but a big playlist outgrows the header it
// travels in (nginx and Cloudflare refuse one over 8KB).
type about struct {
	Name     string `json:"name"`
	Size     int64  `json:"size,omitempty"`
	Sum      string `json:"sum"`
	Videos   int    `json:"videos,omitempty"`
	Playlist string `json:"playlist,omitempty"`
}

// shown is how much of a checksum people see: enough to tell two files apart by eye. The
// party compares the whole one.
const shown = 12

// maxName keeps a record well under wire.MaxVideo, whatever the file is called.
const maxName = 200

func (v video) about() about {
	name := flatten(v.names[v.start])
	if u, err := url.Parse(v.files[v.start]); err == nil && strings.Contains(v.files[v.start], "://") {
		// not the base name, which keeps the query, and a query can hold a token
		name = flatten(path.Base(u.Path))
	}
	if r := []rune(name); len(r) > maxName {
		name = string(r[:maxName]) + "…"
	}
	a := about{Name: name, Size: v.sizes[v.start], Sum: v.sums[v.start][:shown]}
	if v.playlist {
		a.Videos, a.Playlist = len(v.files), v.id[:shown]
	}
	return a
}

// header is a as it travels, opaque to the relay and the mesh: base64, since a name can
// hold anything and a header can't.
func (a about) header() string {
	b, _ := json.Marshal(a)
	return base64.RawURLEncoding.EncodeToString(b)
}

func parseAbout(h string) (about, bool) {
	var a about
	b, err := base64.RawURLEncoding.DecodeString(h)
	if err != nil || json.Unmarshal(b, &a) != nil || a.Sum == "" {
		return about{}, false
	}
	// it came from another member, and ends up in a log line
	a.Name, a.Sum, a.Playlist = flatten(a.Name), flatten(a.Sum), flatten(a.Playlist)
	return a, true
}

// differs says what differs between the party's video, as a refusal described it, and
// ours. local: ours is a file on disk, so the check mark under File decides whether it is
// a playlist.
func differs(refusal *server.DifferentFile, ours about, local bool) error {
	theirs, ok := parseAbout(refusal.Video)
	if !ok {
		return refusal
	}
	switch {
	case local && theirs.Videos > 0 && ours.Videos == 0:
		return fmt.Errorf("the party plays a playlist of %s, and you picked one file: tick the box under File (-playlist)", countVideos(theirs.Videos))
	case local && theirs.Videos == 0 && ours.Videos > 0:
		return fmt.Errorf("the party plays one file, and you picked a playlist of %s: untick the box under File (no -playlist)", countVideos(ours.Videos))
	}
	what := "video"
	if ours.Videos > 0 {
		what = "playlist"
	}
	return fmt.Errorf("your %s is not the same file as the party's:\n  the party's: %s\n  yours:       %s", what, theirs.describe(), ours.describe())
}

func (a about) describe() string {
	parts := []string{a.Name}
	if a.Size > 0 {
		parts = append(parts, size(a.Size))
	}
	s := strings.Join(append(parts, "checksum "+a.Sum), " · ")
	if a.Videos > 0 {
		s += "\n               " + countVideos(a.Videos) + " · playlist checksum " + a.Playlist
	}
	return s
}

func countVideos(n int) string {
	if n == 1 {
		return "1 video"
	}
	return strconv.Itoa(n) + " videos"
}

// size is "1.4 GB" or "702 MB", as file managers count them.
func size(n int64) string {
	switch {
	case n >= 1e9:
		return strconv.FormatFloat(float64(n)/1e9, 'f', 1, 64) + " GB"
	case n >= 1e6:
		return strconv.FormatInt(n/1e6, 10) + " MB"
	case n >= 1e3:
		return strconv.FormatInt(n/1e3, 10) + " KB"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}
