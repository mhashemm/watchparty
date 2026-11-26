package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mhashemm/watchparty/party"
)

// t0 is when the test's lines are printed, second by second (at).
var t0 = time.Date(2026, 11, 26, 21, 4, 0, 0, time.Local)

func at(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

// feed reads lines as printed at at(s), at(s+1), …
func feed(w *watch, s int, msgs ...string) {
	for i, m := range msgs {
		w.line(at(s+i).Format("2006/01/02 15:04:05 ") + m)
	}
}

func shown(text string) string {
	return fmt.Sprintf(`do: sent request: {"command":["show-text",%q,3000],"request_id":7}`, text)
}

func event(name, data string) string {
	return fmt.Sprintf(`Watch: broadcasted event: {"event":"property-change","name":%q,"data":%q,"stamp":1}`, name, data)
}

func relayForm() form {
	return form{Mode: "relay", File: "/v/film.mkv", Relay: "ws://r"}
}

func items(g *feedGroup) []string {
	var out []string
	for _, it := range g.items {
		out = append(out, it.text)
	}
	return out
}

func TestStepsToGetIn(t *testing.T) {
	t.Parallel()
	w := newWatch(relayForm())
	if want := []string{"file", "relay", "mpv", "video"}; !slices.Equal(w.planned, want) {
		t.Fatalf("plan %v, want %v", w.planned, want)
	}
	s := w.render(true, "", nil, false, at(0))
	if s.text != "Checking the video…" || !s.walking || s.trail != nil {
		t.Fatalf("at start: %+v", s)
	}
	feed(w, 0, `checking 1 of 1: "film.mkv"`)
	s = w.render(true, "", nil, false, at(0))
	if len(s.detail) != 1 || s.detail[0] != (bold{"", "film.mkv"}) || s.pct != -1 {
		t.Fatalf("one file's check: %+v", s)
	}
	// the trail shows once a step has taken half a second, and stays
	if s := w.render(true, "", nil, false, at(0).Add(trailAfter)); len(s.trail) != 4 || s.trail[0] != (trailStop{"File", "now"}) {
		t.Fatalf("trail after half a second: %+v", s.trail)
	}
	feed(w, 1, "your address to share is -relay ws://r -room AB -secret CD ")
	s = w.render(true, "", nil, false, at(1))
	if s.text != "Connecting to the relay…" || s.invite != "-relay ws://r -room AB -secret CD" {
		t.Fatalf("after the invite: %+v", s)
	}
	if s.trail[0].state != "done" || s.trail[1].state != "now" {
		t.Fatalf("trail: %+v", s.trail)
	}
	feed(w, 2, "relay: connected", "mpv is up, loading the video")
	if s := w.render(true, "", nil, false, at(3)); s.text != "Loading the video…" {
		t.Fatalf("mpv up: %q", s.text)
	}
	feed(w, 4, "init done")
	s = w.render(true, "", nil, false, at(4))
	if s.text != "You're in. Pause or seek in mpv and everyone follows." || s.walking || s.trail != nil || !s.chat || !s.alive {
		t.Fatalf("in: %+v", s)
	}
	if len(s.members) != 1 || s.members[0].name != "You" || !s.members[0].since.Equal(at(4)) {
		t.Fatalf("members: %+v", s.members)
	}
}

func TestPlanFor(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		f    form
		want string
	}{
		{form{Mode: "relay", File: "a.mkv"}, "file relay mpv video"},
		{form{Mode: "relay", File: "a.mkv", Playlist: true}, "files relay mpv video"},
		{form{Mode: "relay", File: "magnet:?xt=1"}, "torrent relay mpv video"},
		{form{Mode: "direct", File: "x.TORRENT", Local: true}, "torrent mpv video"},
		{form{Mode: "direct", File: "https://h/v.mp4"}, "router mpv video"},
	} {
		if got := strings.Join(planFor(c.f), " "); got != c.want {
			t.Errorf("planFor(%+v) = %q, want %q", c.f, got, c.want)
		}
	}
}

func TestRelayTroubles(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ line, want string }{
		{"relay: dial: room AB is not open yet: …", "Waiting for the host to open this room. It starts by itself once they're in."},
		{"relay: dial: 429 Too Many Requests", "This relay has had too many attempts from your network. Trying again; it clears within the hour."},
		{"relay: dial: 503 Service Unavailable", "The relay is full right now. Trying again…"},
		{"relay: disconnected", "Lost the relay. Reconnecting…"},
	} {
		w := newWatch(relayForm())
		feed(w, 0, "relay: connected", "init done", `member here: {"id":"1","name":"Sam"}`, c.line)
		s := w.render(true, "", nil, false, at(5))
		if s.text != c.want || !s.walking || !s.away {
			t.Errorf("%s: %+v", c.line, s)
		}
	}
	// back, the relay names everyone again: those who left meanwhile are gone
	w := newWatch(relayForm())
	feed(w, 0, "relay: connected", "init done", `member here: {"id":"1","name":"Sam"}`, "relay: disconnected", "relay: connected")
	if s := w.render(true, "", nil, false, at(5)); len(s.members) != 1 || s.away {
		t.Fatalf("after reconnecting: %+v", s)
	}
}

func TestMembersAndSlowNetworks(t *testing.T) {
	t.Parallel()
	w := newWatch(relayForm())
	feed(w, 0, "relay: connected", "init done",
		`member here: {"id":"1","name":"Sam"}`,
		`member joined: {"id":"2","name":"Ana"}`,
		`member joined: {"id":"3","name":"Ana"}`,
		`member left: {"id":"3","name":"Ana"}`)
	s := w.render(true, "", nil, false, at(9))
	if len(s.members) != 3 || !s.members[1].since.IsZero() || !s.members[2].since.Equal(at(3)) {
		t.Fatalf("members: %+v", s.members)
	}
	feed(w, 10, `buffered ahead: 4 for {"id":"1","name":"Sam"}`)
	s = w.render(true, "", nil, false, at(10))
	if s.text != "Sam's network is slow. Stay paused while it buffers." || s.members[1].ahead != 4 {
		t.Fatalf("one slow: %+v", s)
	}
	feed(w, 11, `buffered ahead: 7 for {"id":"2","name":"Ana"}`, `buffered ahead: 5 for {"id":"1","name":"Sam"}`)
	s = w.render(true, "", nil, false, at(12))
	want := []bold{{"5 seconds", " ready on Sam's side"}, {"", ", "}, {"7 seconds", " ready on Ana's side"}}
	if s.text != "Sam and Ana have slow networks. Stay paused while they buffer." || !slices.Equal(s.detail, want) {
		t.Fatalf("two slow: %+v", s)
	}
	feed(w, 13, `member left: {"id":"1","name":"Sam"}`)
	if s := w.render(true, "", nil, false, at(13)); s.text != "Ana's network is slow. Stay paused while it buffers." {
		t.Fatalf("a slow one left: %q", s.text)
	}
	feed(w, 14, "buffered ahead: 1")
	if s := w.render(true, "", nil, false, at(14)); s.text != "Your network is slow. Everyone sees how much you have ready." || s.detail[0] != (bold{"1 second", " of video ready"}) {
		t.Fatalf("this node slow: %+v", s)
	}
	feed(w, 15, shown("resumed by Ana"))
	if s := w.render(true, "", nil, false, at(15)); s.text != "You're in. Pause or seek in mpv and everyone follows." {
		t.Fatalf("resumed: %q", s.text)
	}
	feed(w, 16, "buffering: 40%", "torrent: 3 peers, 1.2 MB/s")
	s = w.render(true, "", nil, false, at(17))
	if s.text != "Your player is buffering…" || s.pct != 40 || len(s.detail) != 3 || s.detail[2] != (bold{"3 peers", ", 1.2 MB/s"}) {
		t.Fatalf("buffering: %+v", s)
	}
	feed(w, 18, "buffering: done")
	if s := w.render(true, "", nil, false, at(18)); s.pct != -1 {
		t.Fatalf("done buffering: %+v", s)
	}
}

func TestFeed(t *testing.T) {
	t.Parallel()
	w := newWatch(relayForm())
	feed(w, 0, "relay: connected", "init done", shown("Sam has joined"), event("pause", "yes"), event("pause", "no"))
	if len(w.feed) != 1 || w.feed[0].name != "film.mkv" || !slices.Equal(items(w.feed[0]), []string{"resumed by you", "paused by you", "Sam has joined"}) {
		t.Fatalf("feed: %+v", items(w.feed[0]))
	}
	// a slow member's seconds ready are the status's, not the feed's
	feed(w, 5, shown("Sam's network is slow: 4s ready"), shown("your network is slow: 2s ready"))
	if len(w.feed[0].items) != 3 {
		t.Fatalf("slow lines in the feed: %v", items(w.feed[0]))
	}
	// this node's own stop after a join is said with why
	feed(w, 7, shown("paused for Ana: joining"), event("pause", "yes"))
	if got := w.feed[0].items[0].text; got != "paused by you: joining" {
		t.Fatalf("stop for a join: %q", got)
	}
	// a pause for a member who left is not a pause of this node's own
	feed(w, 9, shown("paused for Sam: left"), event("pause", "yes"))
	if got := w.feed[0].items[0].text; got != "paused for Sam: left" {
		t.Fatalf("stop for a leave: %v", items(w.feed[0]))
	}
	// a pause-reason takes back the plain "paused by you" it explains
	feed(w, 11, event("pause", "yes"), event("pause-reason", "seek"))
	if slices.Contains(items(w.feed[0])[:1], "paused by you") {
		t.Fatalf("pause-reason kept the pause: %v", items(w.feed[0]))
	}
	for i := range 30 {
		feed(w, 20+i, shown(fmt.Sprintf("line %d", i)))
	}
	if len(w.feed[0].items) != feedKept || w.feed[0].items[0].text != "line 29" {
		t.Fatalf("kept %d, newest %q", len(w.feed[0].items), w.feed[0].items[0].text)
	}
}

func TestFeedFollowsTheFile(t *testing.T) {
	t.Parallel()
	w := newWatch(form{Mode: "relay", File: "/v", Playlist: true})
	feed(w, 0, `now playing: "a.mkv"`, shown("Sam has joined"), shown("from a.mkv to b.mkv, by Sam"))
	// what was said about the move a moment before belongs to the new file
	feed(w, 3, `now playing: "b.mkv"`)
	if len(w.feed) != 2 || w.feed[0].name != "b.mkv" || !w.feed[0].open || w.feed[1].open {
		t.Fatalf("groups: %+v", w.feed)
	}
	if !slices.Equal(items(w.feed[0]), []string{"from a.mkv to b.mkv, by Sam"}) || !slices.Equal(items(w.feed[1]), []string{"Sam has joined"}) {
		t.Fatalf("moved: %v | %v", items(w.feed[0]), items(w.feed[1]))
	}
	// a sync alone stays where it was said
	feed(w, 10, shown("synced by Sam"), `now playing: "c.mkv"`)
	if len(w.feed[0].items) != 0 || w.feed[1].items[0].text != "synced by Sam" {
		t.Fatalf("sync moved: %v | %v", items(w.feed[0]), items(w.feed[1]))
	}
	// back to a file: its group comes first again, with what it had
	feed(w, 20, `now playing: "a.mkv"`)
	if w.feed[0].name != "a.mkv" || w.feed[0].items[0].text != "Sam has joined" || len(w.feed) != 3 {
		t.Fatalf("back to a.mkv: %+v", w.feed[0])
	}
}

func TestDiagnoses(t *testing.T) {
	t.Parallel()
	w := newWatch(relayForm())
	for _, c := range []struct {
		err      string
		playlist bool
		want     string
	}{
		{"rejected by relay: not the same file", false, "Diagnosis: your video is not the same file as the party's. Pick the same video and start again. If the party is watching a whole folder, tick the box under File."},
		{"rejected by relay: not the same file", true, "Diagnosis: this folder doesn't match the party's. Everyone needs the same videos in the same order, with the box ticked."},
		{"rejected by relay: the room did not open", false, "Diagnosis: nobody opened this room for 30 minutes. Start again once someone with the create key is in."},
		{"rejected by relay: opening a room needs -createKey", false, "Diagnosis: this relay needs a create key to open a new party. Add it in Settings and start again."},
		{"rejected by relay: wrong secret", false, "Diagnosis: the relay rejected this room or secret. Check the invite and start again."},
		{`exec: "mpv": executable file not found in $PATH`, false, "Diagnosis: mpv isn't installed, or this app can't find it. Install mpv, or choose mpv on the Settings page."},
		{"the disk is full", false, "Diagnosis: the disk is full. The patient chart below has the details."},
	} {
		s := w.render(false, c.err, nil, c.playlist, at(0))
		if s.text != c.want || !s.bad {
			t.Errorf("%q: %q", c.err, s.text)
		}
	}
	if s := w.render(false, "", nil, false, at(0)); s.text != "The party has ended." || !s.sleep || s.bad {
		t.Fatalf("ended: %+v", s)
	}
	var r *party.NotTheSameFile
	if !errors.As(error(&party.NotTheSameFile{Box: "tick", Videos: 3, Detail: [][2]string{{"a", "b"}}}), &r) {
		t.Fatal("not a refusal")
	}
	s := w.render(false, r.Error(), r, false, at(0))
	if s.text != "Diagnosis: the party plays a playlist of 3 videos, and you picked one file. Tick the box under File and start again." || s.refusal != r || s.detail[0] != (bold{"a", "b"}) {
		t.Fatalf("refusal: %+v", s)
	}
}

func TestHistoryWords(t *testing.T) {
	t.Parallel()
	if got := names([]string{"A", "B", "C"}); got != "A, B and C" {
		t.Errorf("names: %q", got)
	}
	if got := names([]string{"A", "B"}); got != "A and B" {
		t.Errorf("names: %q", got)
	}
	for _, c := range []struct{ path, want string }{
		{"/v/film.mkv", "film.mkv"},
		{`C:\v\film.mkv`, "film.mkv"},
		{"magnet:?xt=urn:btih:x&dn=Big+Film", "Big Film"},
		{"MAGNET:?xt=urn:btih:x", "Magnet link"},
		{"/v/", "/v/"},
	} {
		if got := fileName(c.path); got != c.want {
			t.Errorf("fileName(%q) = %q, want %q", c.path, got, c.want)
		}
	}
	for _, c := range []struct {
		p     session
		files int
		want  string
	}{
		{session{File: "/v/a.mkv"}, 1, "/v/a.mkv"},
		{session{File: "/v/a.mkv", Playlist: true}, 3, "/v (3 videos)"},
		{session{File: "a.mkv", Playlist: true}, 2, "2 videos"},
		{session{File: "/v/x.torrent", Playlist: true}, 2, "/v/x.torrent (2 videos)"},
		{session{File: "magnet:?dn=Film"}, 0, "Film"},
	} {
		if got := partyTitle(c.p, c.files); got != c.want {
			t.Errorf("partyTitle(%+v, %d) = %q, want %q", c.p, c.files, got, c.want)
		}
	}
	now := t0
	ok := session{Mode: "relay", Room: "R", Secret: "S", Started: now.Add(-forgetAfter + time.Hour)}
	if !continuable(ok, now) {
		t.Error("a young relay party can't continue")
	}
	for _, p := range []session{
		{Mode: "direct", Room: "R", Secret: "S", Started: now},
		{Mode: "relay", Secret: "S", Started: now},
		{Mode: "relay", Room: "R", Secret: "S", Started: now, Finished: true},
		{Mode: "relay", Room: "R", Secret: "S", Started: now.Add(-2 * forgetAfter), Ended: now.Add(-forgetAfter)},
	} {
		if continuable(p, now) {
			t.Errorf("continuable: %+v", p)
		}
	}
	if n := partyFiles(session{Videos: []played{{Name: "a"}, {Name: "b"}, {Name: "a"}}}); n != 2 {
		t.Errorf("partyFiles = %d", n)
	}
}

func TestInviteFields(t *testing.T) {
	t.Parallel()
	mode, f, pl, filled := inviteFields("2026/11/26 21:04:00 your address to share is -relay ws://r -room AB -secret CD -playlist")
	if mode != "relay" || !filled || !pl || f["relay"] != "ws://r" || f["room"] != "AB" || f["secret"] != "CD" {
		t.Fatalf("relay invite: %q %v %v %v", mode, f, pl, filled)
	}
	mode, f, pl, filled = inviteFields("-addrs 1.2.3.4:6969 -secret S")
	if mode != "direct" || !filled || pl || f["addrs"] != "1.2.3.4:6969" || f["psecret"] != "S" {
		t.Fatalf("direct invite: %q %v %v %v", mode, f, pl, filled)
	}
	if mode, _, _, filled := inviteFields("-relay"); mode != "relay" || filled {
		t.Fatalf("a bare -relay switches the mode, fills nothing: %q %v", mode, filled)
	}
	if mode, _, _, filled := inviteFields("hello"); mode != "" || filled {
		t.Fatalf("not an invite: %q %v", mode, filled)
	}
}
