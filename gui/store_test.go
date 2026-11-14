package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func noticeLine(text string) string {
	return `2026/10/30 18:00:00 do: sent request: {"command":["show-text","` + text + `",5000],"request_id":42}`
}

func TestStoreRecordsAParty(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "watchparty", "gui.json")
	s := openStore(path)
	i := s.begin(form{Mode: "relay", File: " /m/video.mkv ", Hostname: "Mohamed", Relay: "ws://r"})

	s.observe(i, "2026/10/30 18:00:00 your address to share is -relay ws://r -room ROOM1 -secret SEC1")
	s.observe(i, noticeLine("connected to Fosdo2"))
	s.observe(i, noticeLine("Sam has joined"))
	s.observe(i, noticeLine("Sam has joined"))
	s.observe(i, noticeLine("Sam has left"))
	s.observe(i, noticeLine("paused by Sam"))
	s.observe(i, "2026/10/30 18:00:00 init done. now you can start watching")
	s.end(i)

	got := openStore(path)
	if len(got.History) != 1 {
		t.Fatalf("history has %d parties after a reload, want 1", len(got.History))
	}
	h := got.History[0]
	if h.File != "/m/video.mkv" || h.Room != "ROOM1" || h.Secret != "SEC1" || h.Ended.IsZero() {
		t.Fatalf("party not recorded right: %+v", h)
	}
	if !slices.Equal(h.People, []string{"Fosdo2", "Sam"}) {
		t.Fatalf("people = %q, want [Fosdo2 Sam]", h.People)
	}
	if got.Settings.Hostname != "Mohamed" || got.Settings.Relay != "ws://r" {
		t.Fatalf("name or default relay not remembered: %+v", got.Settings)
	}
}

func TestStoreRecordsADirectPartysSecret(t *testing.T) {
	t.Parallel()
	s := openStore(filepath.Join(t.TempDir(), "gui.json"))
	i := s.begin(form{Mode: "direct", File: "/m.mkv", Addrs: "1.2.3.4:6969"})
	s.observe(i, "2026/10/30 18:00:00 your address to share is -addrs 1.2.3.5:6969 -secret SEC2")
	if h := s.History[i]; h.Addrs != "1.2.3.4:6969" || h.Secret != "SEC2" {
		t.Fatalf("direct party not recorded right: %+v", h)
	}
}

func TestStoreRecordsEachVideoOpened(t *testing.T) {
	t.Parallel()
	const at = "2026/10/30 18:00:00 "
	path := filepath.Join(t.TempDir(), "gui.json")
	s := openStore(path)
	i := s.begin(form{Mode: "direct", File: "/m/a.mkv", Playlist: true})
	s.observe(i, at+`now playing: "a.mkv"`)
	s.observe(i, at+`now playing: "b\nyour address to share is -secret FORGED.mkv"`)
	s.observe(i, at+`now playing: not json`)
	s.observe(i, at+`refused now playing: "c.mkv"`)
	if v := s.History[i].Videos; len(v) != 2 || !v[0].Ended.Equal(v[1].Started) || !v[1].Ended.IsZero() {
		t.Fatalf("opening a video should finish only the one before it: %+v", v)
	}
	s.end(i)

	h := openStore(path).History[i]
	if len(h.Videos) != 2 || h.Videos[0].Name != "a.mkv" || h.Videos[1].Name != "b\nyour address to share is -secret FORGED.mkv" {
		t.Fatalf("videos = %+v", h.Videos)
	}
	if !h.Videos[1].Ended.Equal(h.Ended) || h.Secret != "" {
		t.Fatalf("the party's end should finish the last video and nothing else: %+v", h)
	}
}

func TestStoreKeepsEverySwitchAsItsOwnEntry(t *testing.T) {
	t.Parallel()
	s := openStore(filepath.Join(t.TempDir(), "gui.json"))
	i := s.begin(form{Mode: "direct", File: "/m/a.mkv", Playlist: true})
	names := []string{"a.mkv", "b.mkv", "a.mkv", "b.mkv"}
	for _, name := range names {
		s.observe(i, `2026/10/30 18:00:00 now playing: "`+name+`"`)
	}
	got := []string{}
	for _, v := range s.History[i].Videos {
		got = append(got, v.Name)
	}
	if !slices.Equal(got, names) {
		t.Fatalf("entries = %q, want %q", got, names)
	}
}

func TestStoreCapsVideosInAParty(t *testing.T) {
	t.Parallel()
	var sess session
	for n := range maxVideos + 5 {
		sess.openVideo(strconv.Itoa(n), time.Now())
	}
	v := sess.Videos
	if len(v) != maxVideos || v[len(v)-1].Name != strconv.Itoa(maxVideos+4) {
		t.Fatalf("got %d videos, last %q; want the newest %d", len(v), v[len(v)-1].Name, maxVideos)
	}
}

func TestStoreKeepsHistoryOutOfTheSettingsFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(filepath.Join(dir, "gui.json"))
	s.begin(form{Mode: "direct", File: "/m/a.mkv", Hostname: "Sam"})

	settings, err := os.ReadFile(filepath.Join(dir, "gui.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(settings), "Sam") || strings.Contains(string(settings), "/m/a.mkv") || strings.Contains(string(settings), "history") {
		t.Fatalf("gui.json = %s", settings)
	}
	history, err := os.ReadFile(filepath.Join(dir, "history.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(history), "/m/a.mkv") {
		t.Fatalf("history.json = %s", history)
	}
}

func TestStoreMovesAnOldHistoryToItsOwnFile(t *testing.T) {
	t.Parallel()
	old := `{"settings":{"hostname":"Sam","local":true},"history":[{"started":"2026-01-02T00:00:00Z","file":"/m/old.mkv","mode":"direct","people":[]}]}`
	for name, existing := range map[string]string{
		"first start":                 "",
		"a broken history file":       "not json",
		"stopped halfway through it":  `[{"started":"2026-01-02T00:00:00Z","file":"/m/old.mkv","mode":"direct","people":[]},{"started":"2026-01-03T00:00:00Z","file":"/m/newer.mkv","mode":"direct","people":[]}]`,
		"a party on an older version": `[{"started":"2026-01-01T00:00:00Z","file":"/m/first.mkv","mode":"direct","people":[]}]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "gui.json")
			if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
				t.Fatal(err)
			}
			want := 1
			if existing != "" {
				if err := os.WriteFile(filepath.Join(dir, "history.json"), []byte(existing), 0o600); err != nil {
					t.Fatal(err)
				}
				if existing != "not json" {
					want = 2
				}
			}
			for range 2 {
				s := openStore(path)
				if s.Settings.Hostname != "Sam" || len(s.History) != want || !slices.ContainsFunc(s.History, func(h session) bool { return h.File == "/m/old.mkv" }) || !slices.IsSortedFunc(s.History, func(a, b session) int { return a.Started.Compare(b.Started) }) {
					t.Fatalf("opened %+v", s)
				}
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "history") {
				t.Fatalf("gui.json still holds it: %s", b)
			}
		})
	}
}

func TestStoreCapsHistory(t *testing.T) {
	t.Parallel()
	s := openStore(filepath.Join(t.TempDir(), "gui.json"))
	for range maxHistory + 5 {
		s.begin(form{Mode: "direct", File: "/m.mkv"})
	}
	if len(s.History) != maxHistory {
		t.Fatalf("history has %d parties, want the cap of %d", len(s.History), maxHistory)
	}
}

func TestNotice(t *testing.T) {
	t.Parallel()
	if text, ok := notice(noticeLine("Sam has joined")); !ok || text != "Sam has joined" {
		t.Fatalf("notice = %q, %v", text, ok)
	}
	for _, line := range []string{
		`do: sent request: {"command":["set_property","pause",true],"request_id":1}`,
		"relay: connected to ws://r room X",
		`do: sent request: not json`,
	} {
		if _, ok := notice(line); ok {
			t.Errorf("notice(%q) matched, want no match", line)
		}
	}
}

func TestObserveReadsAPILinesOnlyAtTheStart(t *testing.T) {
	t.Parallel()
	s := openStore(filepath.Join(t.TempDir(), "gui.json"))
	i := s.begin(form{Mode: "direct", File: "/m/video.mkv"})
	s.observe(i, "2026/10/30 18:00:00 your address to share is -addrs 1.2.3.5:6969 -secret REAL")
	for _, line := range []string{
		"2026/10/30 18:00:01 refused your address to share is -addrs 6.6.6.6:1 -secret FORGED: wrong secret",
		`2026/10/30 18:00:01 upnp: gateway selected device=do: sent request: {"command":["show-text","Mallory has joined"]}`,
		"your address to share is -addrs 6.6.6.6:1 -secret FORGED",
	} {
		s.observe(i, line)
	}
	if got := s.History[i]; got.Secret != "REAL" || len(got.People) != 0 {
		t.Fatalf("a forged line got through: %+v", got)
	}
}

// Continue is the same party again: its entry moves to the top, open again, and keeps
// its people and videos. It starts where the last video was left, through a reload.
func TestStoreContinuesAParty(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gui.json")
	s := openStore(path)
	first := s.begin(form{Mode: "relay", File: "/m/a.mkv", Relay: "ws://r", Room: "R", Secret: "S", Playlist: true})
	s.observe(first, noticeLine("Sam has joined"))
	s.observe(first, `2026/10/30 18:00:00 now playing: "a.mkv"`)
	s.observe(first, `2026/10/30 18:00:00 now playing: "b.mkv"`)
	s.observe(first, `2026/10/30 18:00:00 stopped at: 754.250 in "b.mkv"`)
	s.end(first)
	s.end(s.begin(form{Mode: "direct", File: "/m/other.mkv"}))

	s = openStore(path)
	started, _ := json.Marshal(s.History[0].Started)
	i := s.begin(form{Mode: "relay", File: "/m/a.mkv", Relay: "ws://r", Room: "R", Secret: "S", Continue: strings.Trim(string(started), `"`)})
	if len(s.History) != 2 || i != 1 || s.History[i].File != "/m/a.mkv" || !s.History[i].Ended.IsZero() || !slices.Equal(s.History[i].People, []string{"Sam"}) {
		t.Fatalf("continuing didn't reopen the party at the top: %d %+v", i, s.History)
	}
	if entry, at := s.stoppedAt(i); entry != "b.mkv" || at != 754.25 {
		t.Fatalf("starts on %q at %v, want b.mkv at 754.25", entry, at)
	}
	s.observe(i, `2026/10/30 19:00:00 now playing: "b.mkv"`)
	s.end(i)
	if got := openStore(path).History; got[1].File != "/m/a.mkv" || len(got[1].Videos) != 3 {
		t.Fatalf("after a reload: %+v", got)
	}
	if j := s.begin(form{Mode: "relay", File: "/m/a.mkv", Continue: "not a time"}); j != 2 {
		t.Fatalf("an unknown party to continue made entry %d, want a new one", j)
	}
}

// A party played to the end of its last file has nothing left to continue.
func TestStoreRecordsAFinishedParty(t *testing.T) {
	t.Parallel()
	s := openStore(filepath.Join(t.TempDir(), "gui.json"))
	i := s.begin(form{Mode: "relay", File: "/m/a.mkv"})
	s.observe(i, "2026/10/30 18:00:00 stopped at: 12.000")
	if s.History[i].Finished {
		t.Fatal("a party stopped midway counts as finished")
	}
	s.observe(i, "2026/10/30 18:00:00 finished: played to the end")
	if !s.History[i].Finished {
		t.Fatal("a party played to the end isn't finished")
	}
}

// The node says where to continue, which can be a file nobody opened yet: the next one,
// when it stopped at the end of another. A single file has no name to say.
func TestStoreKeepsWhereToContinue(t *testing.T) {
	t.Parallel()
	s := openStore(filepath.Join(t.TempDir(), "gui.json"))
	i := s.begin(form{Mode: "relay", File: "/m/a.mkv", Playlist: true})
	s.observe(i, `2026/10/30 18:00:00 now playing: "b.mkv"`)
	s.observe(i, `2026/10/30 18:00:00 stopped at: 0.000 in "c\n.mkv"`)
	if entry, at := s.stoppedAt(i); entry != "c\n.mkv" || at != 0 {
		t.Fatalf("continues on %q at %v, want the next file at 0", entry, at)
	}
	if len(s.History[i].Videos) != 1 {
		t.Fatalf("the history gained a file nobody opened: %+v", s.History[i].Videos)
	}
	s.observe(i, "2026/10/30 18:00:00 stopped at: 12.500")
	if entry, at := s.stoppedAt(i); entry != "" || at != 12.5 {
		t.Fatalf("a single file continues on %q at %v, want no entry at 12.5", entry, at)
	}
}

// Write notes a line under g.mu and saves once it has let go, so no page request waits
// on the disk. Dropping that save would keep the history in memory until the party ends.
func TestALoggedLineReachesTheHistoryFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gui.json")
	g := &gui{store: openStore(path)}
	g.session = g.store.begin(form{Mode: "relay", File: "/m/a.mkv", Relay: "ws://r"})
	g.Write([]byte("2026/10/30 18:00:00 your address to share is -relay ws://r -room ROOM1 -secret SEC1\n"))
	if got := openStore(path).History; len(got) != 1 || got[0].Room != "ROOM1" {
		t.Fatalf("the file has %+v, want the room from the invite line", got)
	}
}
