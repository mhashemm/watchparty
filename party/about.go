package party

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mhashemm/watchparty/relay/wire"
	"github.com/mhashemm/watchparty/server"
)

// About is what a refused joiner is told of the party's video, to set beside its own: the
// video the party opened on and, for a playlist, how many it holds and its checksum. A
// playlist on disk also lists each video's checksum cut to `short` bytes, so a refusal can
// say which one differs, as names would outgrow the header (wire.MaxVideo). A link is its
// whole text, query too, which can hold a video's id. A torrent is its infohash, which
// people can match to a magnet link's btih.
type About struct {
	// Kind is "" for a file on disk, "link" or "torrent"
	Kind     string `json:"kind,omitempty"`
	Name     string `json:"name"`
	Size     int64  `json:"size,omitempty"`
	Sum      string `json:"sum,omitempty"`
	Videos   int    `json:"videos,omitempty"`
	Playlist string `json:"playlist,omitempty"`
	Sums     []byte `json:"sums,omitempty"`
}

const (
	// shown is how much of a checksum people see: enough to tell two files apart by eye.
	// The party compares the whole one.
	shown = 12
	// short is how much of each playlist video's checksum is sent
	short = 4
	// maxName keeps a record under wire.MaxVideo once a playlist's sums are dropped
	maxName = 300
)

func (v video) about() About {
	file := v.files[v.start]
	a := About{Name: v.names[v.start], Size: v.sizes[v.start]}
	switch {
	case v.infohash != "":
		a.Kind, a.Sum = "torrent", v.infohash[:shown]
	case strings.Contains(file, "://"):
		a.Kind, a.Name = "link", file
	default:
		a.Sum = v.sums[v.start][:shown]
		if v.playlist {
			for _, s := range v.sums {
				b, _ := hex.DecodeString(s[:2*short])
				a.Sums = append(a.Sums, b...)
			}
		}
	}
	if v.playlist {
		a.Videos, a.Playlist = len(v.files), v.id[:shown]
	}
	a.Name = flatten(a.Name)
	if r := []rune(a.Name); len(r) > maxName {
		a.Name = string(r[:maxName]) + "…"
	}
	// a playlist too long to list says less, rather than being refused by the relay
	if len(a.header()) > wire.MaxVideo {
		a.Sums = nil
	}
	return a
}

// header is a as it travels, opaque to the relay and the mesh: base64, since a name can
// hold anything and a header can't.
func (a About) header() string {
	b, _ := json.Marshal(a)
	return base64.RawURLEncoding.EncodeToString(b)
}

func parseAbout(h string) (About, bool) {
	var a About
	b, err := base64.RawURLEncoding.DecodeString(h)
	if err != nil || json.Unmarshal(b, &a) != nil || a.Name == "" {
		return About{}, false
	}
	// it came from another member, and ends up in a log line
	a.Kind, a.Name, a.Sum, a.Playlist = flatten(a.Kind), flatten(a.Name), flatten(a.Sum), flatten(a.Playlist)
	return a, true
}

// NotTheSameFile is a refusal for another file that says what differs. Rows are the facts,
// the party's beside yours, which the GUI shows as they are. Box is "tick" or "untick" when
// the check mark under File is the difference; Videos is then the party's count.
type NotTheSameFile struct {
	Box      string `json:"box,omitempty"`
	Videos   int    `json:"videos,omitempty"`
	Playlist bool   `json:"playlist"`
	Rows     []Row  `json:"rows"`
	text     string
}

// Row is one fact. Differs: both sides have it, and they differ.
type Row struct {
	Label   string `json:"label"`
	Party   string `json:"party"`
	Yours   string `json:"yours"`
	Mono    bool   `json:"mono,omitempty"`
	Differs bool   `json:"differs,omitempty"`
}

func (e *NotTheSameFile) Error() string { return e.text }

// refusedFile turns a refusal for this node's file, from either transport, into what
// differs between the party's video and ours.
func refusedFile(err error, ours video) error {
	var refusal *server.DifferentFile
	if errors.As(err, &refusal) {
		return differs(refusal, ours)
	}
	return err
}

func differs(refusal *server.DifferentFile, ours video) error {
	theirs, ok := parseAbout(refusal.Video)
	if !ok {
		return refusal
	}
	mine := ours.about()
	e := &NotTheSameFile{Videos: theirs.Videos, Playlist: theirs.Videos > 0 || mine.Videos > 0, Rows: facts(theirs, mine)}
	// the box decides only between files on disk
	onDisk := theirs.Kind == "" && mine.Kind == ""
	switch {
	case onDisk && theirs.Videos > 0 && mine.Videos == 0:
		e.Box = "tick"
		e.text = fmt.Sprintf("the party plays a playlist of %s, and you picked one file: tick the box under File (-playlist)", countVideos(theirs.Videos))
	case onDisk && theirs.Videos == 0 && mine.Videos > 0:
		e.Box = "untick"
		e.text = fmt.Sprintf("the party plays one file, and you picked a playlist of %s: untick the box under File (no -playlist)", countVideos(mine.Videos))
	case onDisk && mine.Videos > 0 && e.where(theirs, ours):
	default:
		what := "video"
		if mine.Videos > 0 {
			what = "playlist"
		}
		e.text = fmt.Sprintf("your %s is not the same file as the party's:\n  the party's: %s\n  yours:       %s", what, theirs.describe(), mine.describe())
	}
	return e
}

// where finds the first video of two playlists on disk that differs, by position, and says
// it, naming ours. False when the party sent no sums, or they agree as far as both go and
// the counts do too.
func (e *NotTheSameFile) where(theirs About, ours video) bool {
	n := len(theirs.Sums) / short
	if n == 0 || len(theirs.Sums)%short != 0 {
		return false
	}
	videos := Row{Label: "Videos", Party: strconv.Itoa(n), Yours: strconv.Itoa(len(ours.sums)), Differs: n != len(ours.sums)}
	for i := range min(n, len(ours.sums)) {
		party, mine := hex.EncodeToString(theirs.Sums[i*short:(i+1)*short]), ours.sums[i][:2*short]
		if party == mine {
			continue
		}
		name := flatten(ours.names[i])
		at := fmt.Sprintf("video %d of %d differs", i+1, n)
		if videos.Differs {
			at = fmt.Sprintf("video %d differs, and yours has %s to the party's %d", i+1, countVideos(len(ours.sums)), n)
		}
		e.text = fmt.Sprintf("your playlist is not the same file as the party's: %s\n  the party's: checksum %s\n  yours:       %s · %s · checksum %s", at, party, name, size(ours.sizes[i]), mine)
		e.Rows = []Row{
			{Label: "Differs at", Party: fmt.Sprintf("video %d", i+1), Yours: fmt.Sprintf("video %d", i+1)},
			{Label: "Video", Yours: name},
			{Label: "Size", Yours: size(ours.sizes[i])},
			{Label: "Checksum", Party: party, Yours: mine, Mono: true, Differs: true},
			videos,
		}
		return true
	}
	switch last := min(n, len(ours.sums)) - 1; {
	case len(ours.sums) > n:
		e.text = fmt.Sprintf("your playlist is not the same file as the party's: yours has %s and the party's %d\n  yours goes on with %s", countVideos(len(ours.sums)), n, flatten(ours.names[n]))
		e.Rows = []Row{videos, {Label: "Only in yours", Yours: flatten(ours.names[n])}}
	case len(ours.sums) < n:
		e.text = fmt.Sprintf("your playlist is not the same file as the party's: yours has %s and the party's %d\n  the party's goes on after your %s", countVideos(len(ours.sums)), n, flatten(ours.names[last]))
		e.Rows = []Row{videos, {Label: "Last in common", Yours: flatten(ours.names[last])}}
	default:
		return false
	}
	return true
}

// facts are both sides' facts as rows, for the GUI.
func facts(theirs, mine About) []Row {
	playlist := theirs.Videos > 0 || mine.Videos > 0
	cells := func(a About) map[string]string {
		m := map[string]string{"Video": a.Name, "Size": size(a.Size)}
		switch a.Kind {
		case "":
			m["Checksum"] = a.Sum
		case "torrent":
			m["Infohash"] = a.Sum
		}
		if playlist {
			m["Videos"], m["Playlist checksum"] = "one file", ""
			if a.Videos > 0 {
				m["Videos"], m["Playlist checksum"] = strconv.Itoa(a.Videos), a.Playlist
			}
		}
		return m
	}
	party, yours := cells(theirs), cells(mine)
	rows := []Row{}
	for _, label := range []string{"Video", "Size", "Checksum", "Infohash", "Videos", "Playlist checksum"} {
		p, y := party[label], yours[label]
		if p == "" && y == "" {
			continue
		}
		mono := label == "Checksum" || label == "Infohash" || label == "Playlist checksum"
		rows = append(rows, Row{Label: label, Party: p, Yours: y, Mono: mono, Differs: p != "" && y != "" && p != y})
	}
	return rows
}

// describe is a's facts on one line, and a playlist's on a second.
func (a About) describe() string {
	parts := []string{a.Name}
	if a.Size > 0 {
		parts = append(parts, size(a.Size))
	}
	switch a.Kind {
	case "":
		parts = append(parts, "checksum "+a.Sum)
	case "torrent":
		parts = append(parts, "infohash "+a.Sum)
	}
	s := strings.Join(parts, " · ")
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

// size is "1.4 GB" or "702 MB", as file managers count them, and "" when unknown.
func size(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n >= 1e9:
		return strconv.FormatFloat(float64(n)/1e9, 'f', 1, 64) + " GB"
	case n >= 1e6:
		return strconv.FormatInt(n/1e6, 10) + " MB"
	case n >= 1e3:
		return strconv.FormatInt(n/1e3, 10) + " KB"
	}
	if n == 1 {
		return "1 byte"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}
