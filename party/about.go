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
	opened := v.entries[v.start]
	a := About{Name: opened.name, Size: opened.size}
	switch {
	case v.infohash != "":
		a.Kind, a.Sum = "torrent", v.infohash[:shown]
	case strings.Contains(opened.file, "://"):
		a.Kind, a.Name = "link", opened.file
	default:
		a.Sum = opened.sum[:shown]
		if v.playlist {
			for _, e := range v.entries {
				b, _ := hex.DecodeString(e.sum[:2*short])
				a.Sums = append(a.Sums, b...)
			}
		}
	}
	if v.playlist {
		a.Videos, a.Playlist = len(v.entries), v.id[:shown]
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

// NotTheSameFile is a refusal for another file that says what differs, which the GUI shows
// as it is and the CLI as text made from the same facts. Rows are the facts both sides
// have, the party's beside yours, so no cell is ever empty; Detail says what only one side
// has, as the page's detail line ([bold, plain] pairs). Box is "tick" or "untick" when the
// check mark under File is the difference; Videos is the party's count.
type NotTheSameFile struct {
	Box      string      `json:"box,omitempty"`
	Videos   int         `json:"videos,omitempty"`
	Playlist bool        `json:"playlist"`
	Detail   [][2]string `json:"detail,omitempty"`
	Rows     []Row       `json:"rows"`
	// yours is our count, for the CLI's untick
	yours int
}

// Row is one fact both sides have. Differs: they differ.
type Row struct {
	Label   string `json:"label"`
	Party   string `json:"party"`
	Yours   string `json:"yours"`
	Mono    bool   `json:"mono,omitempty"`
	Differs bool   `json:"differs,omitempty"`
}

func (e *NotTheSameFile) Error() string {
	switch e.Box {
	case "tick":
		return fmt.Sprintf("the party plays a playlist of %s, and you picked one file: tick the box under File (-playlist)", countVideos(e.Videos))
	case "untick":
		return fmt.Sprintf("the party plays one file, and you picked a playlist of %s: untick the box under File (no -playlist)", countVideos(e.yours))
	}
	what := "video"
	if e.yours > 0 {
		what = "playlist"
	}
	s := "your " + what + " is not the same file as the party's:"
	if len(e.Detail) > 0 {
		// the detail line is one sentence, its bold parts plain here
		var sentence strings.Builder
		for _, d := range e.Detail {
			sentence.WriteString(d[0] + d[1])
		}
		s = "your " + what + " is not the same file as the party's. " + sentence.String()
	}
	return s + "\n  the party's: " + side(e.Rows, func(r Row) string { return r.Party }) +
		"\n  yours:       " + side(e.Rows, func(r Row) string { return r.Yours })
}

// side is one side's rows in a line: the video, then a playlist's on a second line.
func side(rows []Row, of func(Row) string) string {
	var video, playlist []string
	for _, r := range rows {
		v := of(r)
		switch r.Label {
		case "Checksum", "Infohash", "Playlist checksum":
			v = strings.ToLower(r.Label) + " " + v
		case "Videos":
			if n, err := strconv.Atoi(v); err == nil {
				v = countVideos(n)
			}
		}
		if r.Label == "Videos" || r.Label == "Playlist checksum" {
			playlist = append(playlist, v)
		} else {
			video = append(video, v)
		}
	}
	switch {
	case len(video) == 0:
		return strings.Join(playlist, " · ")
	case len(playlist) == 0:
		return strings.Join(video, " · ")
	}
	return strings.Join(video, " · ") + "\n               " + strings.Join(playlist, " · ")
}

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
	e := &NotTheSameFile{Videos: theirs.Videos, Playlist: theirs.Videos > 0 || mine.Videos > 0, Rows: facts(theirs, mine), yours: mine.Videos}
	if theirs.Kind != mine.Kind {
		e.Detail = [][2]string{{"", "The party plays "}, {kinds[theirs.Kind], ", and you picked "}, {kinds[mine.Kind], "."}}
	}
	// the box decides only between files on disk
	if theirs.Kind == "" && mine.Kind == "" {
		switch {
		case theirs.Videos > 0 && mine.Videos == 0:
			e.Box = "tick"
		case theirs.Videos == 0 && mine.Videos > 0:
			e.Box = "untick"
		case mine.Videos > 0:
			e.where(theirs, ours)
		}
	}
	return e
}

// where finds the first video of two playlists on disk that differs, by position, and says
// it, naming ours. It says nothing when the party sent no sums, or they agree as far as both
// go and the counts do too.
func (e *NotTheSameFile) where(theirs About, ours video) {
	n := len(theirs.Sums) / short
	if n == 0 || len(theirs.Sums)%short != 0 {
		return
	}
	mine := ours.entries
	videos := Row{Label: "Videos", Party: strconv.Itoa(n), Yours: strconv.Itoa(len(mine)), Differs: n != len(mine)}
	for i := range min(n, len(mine)) {
		party, yours := hex.EncodeToString(theirs.Sums[i*short:(i+1)*short]), mine[i].sum[:2*short]
		if party == yours {
			continue
		}
		at := [][2]string{{"", "They differ at video "}, {strconv.Itoa(i + 1), " of "}, {strconv.Itoa(n), ". Yours is "}}
		if videos.Differs {
			at = [][2]string{{"", "They differ at video "}, {strconv.Itoa(i + 1), ". Yours is "}}
		}
		e.Detail = append(at, [2]string{flatten(mine[i].name), ", "}, [2]string{size(mine[i].size), "."})
		e.Rows = []Row{{Label: "Checksum", Party: party, Yours: yours, Mono: true, Differs: true}, videos}
		return
	}
	switch {
	case len(mine) > n:
		e.Detail = [][2]string{{"", "Yours goes on with "}, {flatten(mine[n].name), ", after the party's last video."}}
	case len(mine) < n:
		e.Detail = [][2]string{{"", "The party's goes on after your "}, {flatten(mine[len(mine)-1].name), "."}}
	default:
		return
	}
	e.Rows = []Row{videos}
}

// kinds names an About's kind in a sentence.
var kinds = map[string]string{"": "a file", "link": "a link", "torrent": "a torrent"}

// facts are the facts both sides have, as rows. One side's alone (a torrent's infohash
// beside a file's checksum, a link's missing size) can't be compared.
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
		if p == "" || y == "" {
			continue
		}
		mono := label == "Checksum" || label == "Infohash" || label == "Playlist checksum"
		rows = append(rows, Row{Label: label, Party: p, Yours: y, Mono: mono, Differs: p != y})
	}
	return rows
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
	case n == 1:
		return "1 byte"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}
