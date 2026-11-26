package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mhashemm/watchparty/party"
)

// watch is what the log lines of one run say: the invite, how far the node is in, who is in
// the party, who is slow, and the feed. It reads only lines (root CLAUDE.md), right after the
// timestamp (message), never a substring: a stranger's text can sit mid-line.
type watch struct {
	// file is the form's, whose name heads the feed until mpv names the file it plays
	file string

	ready, relay, connected, reconnecting, waiting bool
	// busy is why the relay turned this node away: "limited" (429) or "full" (503)
	busy           string
	unreachable    bool
	leftPause      bool
	stopping       *stopping
	shared         bool
	torrent, found bool
	check          *checking
	router, mpvUp  bool
	swarm          *swarm
	buffering      int // percent, or -1
	inAt           time.Time
	members        []member
	myAhead        int // seconds ready while the party waits for this node, or -1
	aheadOf        []ahead
	invite         string
	feed           []*feedGroup // newest first
	trailStep      string
	trailAt        time.Time
	trailShown     bool
	planned        []string
}

// stopping is the feed's line for a stop this node is about to make itself, and why
type stopping struct {
	item *feedItem
	why  string
}

type checking struct {
	i, n int
	name string
}

type swarm struct {
	peers int
	speed string
}

// member is one other node, by the id its transport gave it, so two with one name stay two.
// A zero since is one who was there before this node.
type member struct {
	id, name string
	since    time.Time
}

type ahead struct {
	id, name string
	secs     int
}

type feedGroup struct {
	name  string
	open  bool
	items []*feedItem // newest first
}

type feedItem struct {
	at   time.Time
	text string
}

const (
	// feedKept is the most lines a file's group keeps
	feedKept = 20
	// trailAfter: the steps show once one has taken this long, then stay until the node is
	// in; a quick start would only flash them
	trailAfter = 500 * time.Millisecond
)

// newWatch reads a run started from f: its steps come from the form.
func newWatch(f form) *watch {
	return &watch{file: f.File, buffering: -1, myAhead: -1, planned: planFor(f)}
}

var (
	reInvite   = regexp.MustCompile(`^your address to share is (.*)$`)
	reSwarm    = regexp.MustCompile(`^torrent: (\d+) peers?, (.+)$`)
	reChecking = regexp.MustCompile(`^checking (\d+) of (\d+): (".*")$`)
	reBuffer   = regexp.MustCompile(`^buffering: (\d+)%$`)
	reMyAhead  = regexp.MustCompile(`^buffered ahead: (\d+)$`)
	reAheadOf  = regexp.MustCompile(`^buffered ahead: (\d+) for (\{.*\})$`)
	reMember   = regexp.MustCompile(`^member (joined|here|left): (\{.*\})$`)
	reShown    = regexp.MustCompile(`^do: sent request: (\{"command":\["show-text".*)$`)
	reSlow     = regexp.MustCompile(`(?s)^(your|.*'s) network is slow: \d+s ready$`)
	reLeft     = regexp.MustCompile(`^paused for .*: left$`)
	reWhy      = regexp.MustCompile(`^paused for .*: (joining|catching up)$|^paused: (out of sync) with .*$`)
	rePlaying  = regexp.MustCompile(`^now playing: (".*")$`)
	reEvent    = regexp.MustCompile(`^Watch: broadcasted event: (\{.*\})$`)
	reMoves    = regexp.MustCompile(`(?s)^from .+, by |: video changed$|^synced by `)
)

// lineTime is when a log line was printed, by its timestamp in local time.
func lineTime(line string) time.Time {
	at, _ := time.ParseInLocation("2006/01/02 15:04:05", line[:min(len(line), 19)], time.Local)
	return at
}

// line reads one log line.
func (w *watch) line(line string) {
	msg, at := message(line), lineTime(line)
	if m := reInvite.FindStringSubmatch(msg); m != nil {
		w.invite = strings.TrimSpace(m[1])
		w.relay = strings.HasPrefix(w.invite, "-relay ")
		w.shared = true
	}
	if strings.HasPrefix(msg, "relay: connected") {
		// back after a drop, the relay names everyone still here again, and missed leaves are gone
		if w.connected {
			w.members = nil
		}
		w.connected, w.reconnecting, w.waiting, w.busy = true, false, false, ""
	}
	if strings.HasPrefix(msg, "relay: dial") || strings.HasPrefix(msg, "relay: disconnected") {
		w.reconnecting = true
		w.waiting = strings.Contains(msg, "is not open yet")
		switch {
		case strings.HasPrefix(msg, "relay: dial: 429 "):
			w.busy = "limited"
		case strings.HasPrefix(msg, "relay: dial: 503 "):
			w.busy = "full"
		default:
			w.busy = ""
		}
	}
	if strings.HasPrefix(msg, "init done") {
		w.ready, w.inAt, w.swarm = true, at, nil
	}
	if strings.HasPrefix(msg, "torrent: finding ") {
		w.torrent = true
	}
	if strings.HasPrefix(msg, "torrent: found ") {
		w.found = true
	}
	if m := reSwarm.FindStringSubmatch(msg); m != nil {
		n, _ := strconv.Atoi(m[1])
		w.swarm = &swarm{n, m[2]}
	}
	if m := reChecking.FindStringSubmatch(msg); m != nil {
		var name string
		if json.Unmarshal([]byte(m[3]), &name) == nil {
			i, _ := strconv.Atoi(m[1])
			n, _ := strconv.Atoi(m[2])
			w.check = &checking{i, n, name}
		}
	}
	if msg == "asking the router for a port" {
		w.router = true
	}
	if strings.HasPrefix(msg, "mpv is up, ") {
		w.mpvUp = true
	}
	if m := reBuffer.FindStringSubmatch(msg); m != nil {
		w.buffering, _ = strconv.Atoi(m[1])
	}
	if msg == "buffering: done" {
		w.buffering, w.swarm = -1, nil
	}
	// how many seconds a member that ran out has ready, while the party waits for it
	if m := reMyAhead.FindStringSubmatch(msg); m != nil {
		w.myAhead, _ = strconv.Atoi(m[1])
	}
	if m := reAheadOf.FindStringSubmatch(msg); m != nil {
		var p struct{ ID, Name string }
		if json.Unmarshal([]byte(m[2]), &p) == nil {
			secs, _ := strconv.Atoi(m[1])
			w.setAhead(p.ID, p.Name, secs)
		}
	}
	if m := reMember.FindStringSubmatch(msg); m != nil {
		var p struct{ ID, Name string }
		if json.Unmarshal([]byte(m[2]), &p) == nil {
			w.member(m[1], p.ID, p.Name, at)
		}
	}
	if strings.HasPrefix(msg, "unreachable address:") {
		w.unreachable = true
	}
	if m := reShown.FindStringSubmatch(msg); m != nil {
		var req struct{ Command []any }
		if json.Unmarshal([]byte(m[1]), &req) == nil && len(req.Command) > 1 {
			if text, ok := req.Command[1].(string); ok {
				// a slow member's seconds ready, on mpv's screen every few seconds: the status has them
				if reSlow.MatchString(text) {
					return
				}
				item := w.add(text, at)
				if strings.HasPrefix(text, "resumed by ") {
					w.resumed()
				}
				if reLeft.MatchString(text) {
					w.leftPause = true
				}
				// if this node was playing it stops next, and that is this line, not a pause of its own
				w.stopping = nil
				if why := reWhy.FindStringSubmatch(text); why != nil {
					w.stopping = &stopping{item, why[1] + why[2]}
				}
			}
		}
	}
	if m := rePlaying.FindStringSubmatch(msg); m != nil {
		var name string
		if json.Unmarshal([]byte(m[1]), &name) == nil {
			w.nowPlaying(name, at)
		}
	}
	if m := reEvent.FindStringSubmatch(msg); m != nil {
		var e struct {
			Name string
			Data any
		}
		if json.Unmarshal([]byte(m[1]), &e) == nil {
			paused := e.Data == "yes"
			switch {
			case e.Name == "pause" && paused && w.leftPause:
				w.leftPause = false
			case e.Name == "pause" && paused && w.stopping != nil:
				w.stopping.item.text = "paused by you: " + w.stopping.why
			case e.Name == "pause" && paused:
				w.add("paused by you", at)
			case e.Name == "pause":
				w.add("resumed by you", at)
			}
			if e.Name == "pause" && e.Data == "no" {
				w.resumed()
			}
			if e.Name == "pause" {
				w.stopping = nil
			}
			if e.Name == "pause-reason" {
				w.dropFirst("paused by you")
			}
		}
	}
}

func (w *watch) setAhead(id, name string, secs int) {
	for i := range w.aheadOf {
		if w.aheadOf[i].id == id {
			w.aheadOf[i].name, w.aheadOf[i].secs = name, secs
			return
		}
	}
	w.aheadOf = append(w.aheadOf, ahead{id, name, secs})
}

func (w *watch) aheadSecs(id string) int {
	for _, a := range w.aheadOf {
		if a.id == id {
			return a.secs
		}
	}
	return -1
}

// member: whoever was there before this node is "already here", as only their names come
func (w *watch) member(what, id, name string, at time.Time) {
	i := -1
	for j, m := range w.members {
		if m.id == id {
			i = j
		}
	}
	if what == "left" {
		if i >= 0 {
			w.members = append(w.members[:i:i], w.members[i+1:]...)
		}
		for j, a := range w.aheadOf {
			if a.id == id {
				w.aheadOf = append(w.aheadOf[:j:j], w.aheadOf[j+1:]...)
				break
			}
		}
		return
	}
	if i < 0 {
		m := member{id: id, name: name}
		if what == "joined" {
			m.since = at
		}
		w.members = append(w.members, m)
	}
}

// resumed: once the party plays again, nobody is waited for
func (w *watch) resumed() {
	w.myAhead, w.aheadOf = -1, nil
}

// group is the feed's group for a file, made the newest and the only open one.
func (w *watch) group(name string) *feedGroup {
	var g *feedGroup
	for i, x := range w.feed {
		if x.name == name {
			g = x
			w.feed = append(w.feed[:i:i], w.feed[i+1:]...)
			break
		}
	}
	if g == nil {
		g = &feedGroup{name: name}
	}
	for _, x := range w.feed {
		x.open = false
	}
	g.open = true
	w.feed = append([]*feedGroup{g}, w.feed...)
	return g
}

// nowPlaying starts the file's group. What was said about the move a moment before (from A
// to B, the video changed) belongs to the new file, unless all of it was a sync.
func (w *watch) nowPlaying(name string, at time.Time) {
	var previous *feedGroup
	if len(w.feed) > 0 {
		previous = w.feed[0]
	}
	g := w.group(name)
	if previous == nil || previous == g {
		return
	}
	n := 0
	for _, it := range previous.items {
		if at.Sub(it.at) > time.Second || !reMoves.MatchString(it.text) {
			break
		}
		n++
	}
	moving := previous.items[:n]
	for _, it := range moving {
		if !strings.HasPrefix(it.text, "synced by ") {
			g.items = append(append([]*feedItem{}, moving...), g.items...)
			previous.items = previous.items[n:]
			return
		}
	}
}

// add is a line in the newest group's feed, the form's file's if there is none yet.
func (w *watch) add(text string, at time.Time) *feedItem {
	g := (*feedGroup)(nil)
	if len(w.feed) > 0 {
		g = w.feed[0]
	} else {
		g = w.group(fileName(w.file))
	}
	it := &feedItem{at: at, text: text}
	g.items = append([]*feedItem{it}, g.items...)
	if len(g.items) > feedKept {
		g.items = g.items[:feedKept]
	}
	return it
}

// dropFirst removes the first line saying text, newest group and line first.
func (w *watch) dropFirst(text string) {
	for _, g := range w.feed {
		for i, it := range g.items {
			if it.text == text {
				g.items = append(g.items[:i:i], g.items[i+1:]...)
				return
			}
		}
	}
}

// fileName is how a file is named in the feed and history: a magnet link by its dn.
func fileName(path string) string {
	if len(path) >= 7 && strings.EqualFold(path[:7], "magnet:") {
		_, query, _ := strings.Cut(path, "?")
		v, _ := url.ParseQuery(query)
		if dn := v.Get("dn"); dn != "" {
			return dn
		}
		return "Magnet link"
	}
	name := path[strings.LastIndexAny(path, `\/`)+1:]
	if name == "" {
		return path
	}
	return name
}

// names lists people: "A", "A and B", "A, B and C".
func names(a []string) string {
	if len(a) < 3 {
		return strings.Join(a, " and ")
	}
	return strings.Join(a[:len(a)-1], ", ") + " and " + a[len(a)-1]
}

func seconds(n int) string {
	if n == 1 {
		return "1 second"
	}
	return fmt.Sprintf("%d seconds", n)
}

func peers(n int) string {
	if n == 1 {
		return "1 peer"
	}
	return fmt.Sprintf("%d peers", n)
}

// planFor is the steps a party takes to start, from its form.
func planFor(f form) []string {
	file := strings.TrimSpace(f.File)
	lower := strings.ToLower(file)
	var steps []string
	switch {
	case strings.HasPrefix(lower, "magnet:") || strings.HasSuffix(lower, ".torrent"):
		steps = []string{"torrent"}
	case strings.Contains(file, "://"):
	case f.Playlist:
		steps = []string{"files"}
	default:
		steps = []string{"file"}
	}
	if f.Mode == "relay" {
		steps = append(steps, "relay")
	} else if !f.Local {
		steps = append(steps, "router")
	}
	return append(steps, "mpv", "video")
}

func (w *watch) done(step string) bool {
	// the invite comes once the file is known, before mpv starts
	checked := w.shared || w.router || w.mpvUp || w.ready
	switch step {
	case "torrent":
		return w.found || w.shared
	case "file", "files":
		return checked
	case "relay":
		return w.connected
	case "router":
		return w.shared
	case "mpv":
		return w.mpvUp || w.ready
	case "video":
		return w.ready
	}
	return false
}

// bold is a piece of the status's detail line: the bold part, then the plain one.
type bold struct{ b, plain string }

// stepText is what the status says while a step runs.
func (w *watch) stepText(step string) (text string, detail []bold, pct float64) {
	pct = -1
	c, sw := w.check, w.swarm
	many := step == "files"
	if c != nil {
		many = c.n > 1
	}
	switch step {
	case "torrent":
		text = "Looking for this torrent's peers. It can take a while…"
		if sw != nil && sw.peers > 0 {
			detail = []bold{{peers(sw.peers), " so far"}}
		} else if sw != nil {
			detail = []bold{{"No peers", " yet"}}
		}
	case "file", "files":
		text = "Checking the video…"
		if many {
			text = "Checking the videos…"
		}
		if c != nil && many {
			detail = []bold{{fmt.Sprintf("%d of %d:", c.i, c.n), " " + c.name}}
			pct = float64(c.i) / float64(c.n) * 100
		} else if c != nil {
			detail = []bold{{"", c.name}}
		}
	case "router":
		text = "Asking your router to open a port. This takes a few seconds…"
	case "relay":
		text = "Connecting to the relay…"
	case "mpv":
		text = "Opening mpv. This takes a few seconds…"
	case "video":
		text = "Loading the video…"
		if sw != nil {
			detail = []bold{{peers(sw.peers), ", " + sw.speed}}
		}
	}
	return
}

var stepLabels = map[string]string{"torrent": "Torrent", "file": "File", "files": "Files", "relay": "Relay", "router": "Router", "mpv": "mpv", "video": "Video"}

// stop is one step on the trail: "now", "done" or "".
type trailStop struct{ label, state string }

// screen is the party's status as the window shows it.
type screen struct {
	text           string
	bad            bool
	sleep, walking bool
	detail         []bold
	pct            float64 // -1: no bar
	trail          []trailStop
	refusal        *party.NotTheSameFile
	members        []memberRow
	chat, away     bool
	invite         string
	alive          bool
	chartOpen      bool
	running        bool
}

// memberRow is one line of "In the party": since is zero for who was already here, ahead is
// the seconds they have ready while the party waits for them, or -1.
type memberRow struct {
	name  string
	since time.Time
	ahead int
}

// render is the status of the run: running, how it ended (exitErr, refusal), with the form's
// playlist box as it is now, at now.
func (w *watch) render(running bool, exitErr string, refusal *party.NotTheSameFile, playlist bool, now time.Time) screen {
	s := screen{pct: -1, running: running}
	step := ""
	if running {
		for _, k := range w.planned {
			if !w.done(k) {
				step = k
				break
			}
		}
	}
	notSameFile := "Diagnosis: your video is not the same file as the party's. Pick the same video and start again. If the party is watching a whole folder, tick the box under File."
	if playlist {
		notSameFile = "Diagnosis: this folder doesn't match the party's. Everyone needs the same videos in the same order, with the box ticked."
	}
	// the relay's rejection, as Run returned it
	rejected := ""
	if strings.HasPrefix(exitErr, "rejected by relay") {
		switch {
		case strings.Contains(exitErr, "not the same file"):
			rejected = "file"
		case strings.Contains(exitErr, "did not open"):
			rejected = "unopened"
		case strings.Contains(exitErr, "-createKey"):
			rejected = "key"
		default:
			rejected = "secret"
		}
	}
	switch {
	case refusal != nil:
		s.text, s.bad = refusedFor(refusal), true
	case rejected == "file":
		s.text, s.bad = notSameFile, true
	case rejected == "unopened":
		s.text, s.bad = "Diagnosis: nobody opened this room for 30 minutes. Start again once someone with the create key is in.", true
	case rejected == "key":
		s.text, s.bad = "Diagnosis: this relay needs a create key to open a new party. Add it in Settings and start again.", true
	case rejected != "":
		s.text, s.bad = "Diagnosis: the relay rejected this room or secret. Check the invite and start again.", true
	case running && w.waiting:
		s.text, s.walking = "Waiting for the host to open this room. It starts by itself once they're in.", true
	case running && w.busy == "limited":
		s.text, s.walking = "This relay has had too many attempts from your network. Trying again; it clears within the hour.", true
	case running && w.busy == "full":
		s.text, s.walking = "The relay is full right now. Trying again…", true
	case running && w.reconnecting:
		s.text, s.walking = "Lost the relay. Reconnecting…", true
	case step != "":
		// the first step not done yet: the relay's handshake can end after mpv is ready, and
		// this node is in only once the room has it
		s.text, s.detail, s.pct = w.stepText(step)
		s.walking = true
	case running && w.buffering >= 0:
		s.text = "Your player is buffering…"
		s.detail = []bold{{fmt.Sprintf("%d%%", w.buffering), " ready"}}
		if w.swarm != nil {
			s.detail = append(s.detail, bold{"", ", "}, bold{peers(w.swarm.peers), ", " + w.swarm.speed})
		}
		s.pct = float64(w.buffering)
	case running && w.myAhead >= 0:
		s.text = "Your network is slow. Everyone sees how much you have ready."
		s.detail = []bold{{seconds(w.myAhead), " of video ready"}}
	case running && len(w.aheadOf) > 0:
		if len(w.aheadOf) == 1 {
			s.text = w.aheadOf[0].name + "'s network is slow. Stay paused while it buffers."
		} else {
			var who []string
			for _, a := range w.aheadOf {
				who = append(who, a.name)
			}
			s.text = names(who) + " have slow networks. Stay paused while they buffer."
		}
		for i, a := range w.aheadOf {
			s.detail = append(s.detail, bold{seconds(a.secs), " ready on " + a.name + "'s side"})
			if i < len(w.aheadOf)-1 {
				s.detail = append(s.detail, bold{"", ", "})
			}
		}
	case running && w.unreachable:
		s.text = "You're in, but friends outside this network can't reach this invite: your connection has no public address. Use a relay for them."
	case running:
		s.text = "You're in. Pause or seek in mpv and everyone follows."
	case exitErr != "":
		lower := strings.ToLower(exitErr)
		switch {
		case strings.Contains(exitErr, "not the same file"):
			s.text = notSameFile
		case strings.Contains(exitErr, "mpv") && (strings.Contains(lower, "not found") || strings.Contains(lower, "cannot find") || strings.Contains(lower, "no such file")):
			s.text = "Diagnosis: mpv isn't installed, or this app can't find it. Install mpv, or choose mpv on the Settings page."
		default:
			s.text = "Diagnosis: " + exitErr + ". The patient chart below has the details."
		}
		s.bad, s.chartOpen = true, true
	default:
		s.text, s.sleep = "The party has ended.", true
	}
	if refusal != nil && len(refusal.Detail) > 0 {
		s.detail = nil
		for _, d := range refusal.Detail {
			s.detail = append(s.detail, bold{d[0], d[1]})
		}
	}
	s.refusal = refusal
	s.trail = w.renderTrail(step, now)
	in := running && w.ready && step == ""
	if in {
		s.members = append(s.members, memberRow{"You", w.inAt, w.myAhead})
		for _, m := range w.members {
			s.members = append(s.members, memberRow{m.name, m.since, w.aheadSecs(m.id)})
		}
	}
	s.chat = in
	// the relay would drop a message: the node says so on mpv's screen, and the box waits
	s.away = running && w.reconnecting
	if running {
		s.invite = w.invite
	}
	s.alive = running && w.ready
	return s
}

// renderTrail is the steps to get in, once the current one has taken trailAfter, and until
// the node is in.
func (w *watch) renderTrail(step string, now time.Time) []trailStop {
	if step != w.trailStep {
		w.trailStep, w.trailAt = step, now
	}
	if step != "" && !w.trailShown && now.Sub(w.trailAt) >= trailAfter {
		w.trailShown = true
	}
	if step == "" || !w.trailShown {
		return nil
	}
	var t []trailStop
	for _, k := range w.planned {
		label := stepLabels[k]
		if k == "files" && w.check != nil && w.check.n == 1 {
			label = "File"
		}
		state := ""
		if k == step {
			state = "now"
		} else if w.done(k) {
			state = "done"
		}
		t = append(t, trailStop{label, state})
	}
	return t
}

// refusedFor is the diagnosis of a refusal that says what differs (party.NotTheSameFile).
func refusedFor(r *party.NotTheSameFile) string {
	switch {
	case r.Box == "tick":
		return fmt.Sprintf("Diagnosis: the party plays a playlist of %d videos, and you picked one file. Tick the box under File and start again.", r.Videos)
	case r.Box == "untick":
		return "Diagnosis: the party plays one file, and you picked a whole folder. Untick the box under File and start again."
	case r.Playlist:
		return "Diagnosis: this folder doesn't match the party's. Everyone needs the same videos in the same order."
	}
	return "Diagnosis: your video is not the same file as the party's. Pick the same video and start again."
}

// partyTitle names a party in History: the file, or the folder of a playlist, and how many
// videos were played; a magnet link by its name.
func partyTitle(p session, files int) string {
	lower := strings.ToLower(p.File)
	place := p.File
	switch {
	case strings.HasPrefix(lower, "magnet:"):
		place = fileName(p.File)
	case p.Playlist && !strings.HasSuffix(lower, ".torrent"):
		if i := strings.LastIndexAny(p.File, `\/`); i >= 0 {
			place = p.File[:i]
		} else {
			place = ""
		}
	}
	count := ""
	if files > 1 {
		count = fmt.Sprintf("%d videos", files)
	}
	switch {
	case place != "" && count != "":
		return place + " (" + count + ")"
	case place != "":
		return place
	}
	return count
}

// forgetAfter is how long the relay keeps a closed room (relay/rooms.go)
const forgetAfter = 30 * 24 * time.Hour

// continuable is a party Continue can open again: over a relay, with its room and secret,
// not finished, and young enough that the relay remembers the room.
func continuable(p session, now time.Time) bool {
	last := p.Ended
	if last.IsZero() {
		last = p.Started
	}
	return p.Mode != "direct" && p.Room != "" && p.Secret != "" && !p.Finished && now.Sub(last) < forgetAfter
}

// partyFiles is how many different videos a party played.
func partyFiles(p session) int {
	seen := map[string]bool{}
	for _, v := range p.Videos {
		seen[v.Name] = true
	}
	return len(seen)
}

// inviteFields is what a pasted invite fills in: the mode it switches to ("" for none), and
// each field it names; filled is whether it named any, and only then does it set the
// playlist box. Anything before "your address to share is" is ignored.
func inviteFields(text string) (mode string, fields map[string]string, playlist, filled bool) {
	if _, after, found := strings.Cut(text, "your address to share is"); found {
		text = after
	}
	t := strings.Fields(text)
	var names map[string]string
	switch {
	case containsField(t, "-relay"):
		mode, names = "relay", map[string]string{"-relay": "relay", "-room": "room", "-secret": "secret"}
	case containsField(t, "-addrs"):
		mode, names = "direct", map[string]string{"-addrs": "addrs", "-secret": "psecret"}
	default:
		return "", nil, false, false
	}
	fields = map[string]string{}
	for i := 0; i+1 < len(t); i++ {
		if k, found := names[t[i]]; found {
			fields[k] = t[i+1]
		}
	}
	return mode, fields, containsField(t, "-playlist"), len(fields) > 0
}

func containsField(t []string, f string) bool {
	for _, x := range t {
		if x == f {
			return true
		}
	}
	return false
}
