package main

import (
	"path/filepath"
	"slices"
	"testing"
)

func noticeLine(text string) string {
	return `2026/10/30 18:00:00 do: sent request: {"command":["show-text","` + text + `",5000],"request_id":42}`
}

func TestStoreRecordsAParty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchparty", "gui.json")
	s := openStore(path)
	i := s.begin(form{Mode: "relay", File: " /m/movie.mkv ", Hostname: "Mohamed", Relay: "ws://r"})

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
	if h.File != "/m/movie.mkv" || h.Room != "ROOM1" || h.Secret != "SEC1" || h.Ended.IsZero() {
		t.Fatalf("party not recorded right: %+v", h)
	}
	if !slices.Equal(h.People, []string{"Fosdo2", "Sam"}) {
		t.Fatalf("people = %q, want [Fosdo2 Sam]", h.People)
	}
	if got.Settings.Hostname != "Mohamed" || got.Settings.Relay != "ws://r" {
		t.Fatalf("name or default relay not remembered: %+v", got.Settings)
	}
}

func TestStoreCapsHistory(t *testing.T) {
	s := openStore(filepath.Join(t.TempDir(), "gui.json"))
	for range maxHistory + 5 {
		s.begin(form{Mode: "direct", File: "/m.mkv"})
	}
	if len(s.History) != maxHistory {
		t.Fatalf("history has %d parties, want the cap of %d", len(s.History), maxHistory)
	}
}

func TestNotice(t *testing.T) {
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
