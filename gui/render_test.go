package main

import (
	"encoding/json"
	"flag"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gioui.org/gpu/headless"
	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
	"gioui.org/widget"

	"github.com/mhashemm/watchparty/gui/art"
	"github.com/mhashemm/watchparty/party"
)

var renderTo = flag.String("render", "", "draw the window's screens into this folder as PNGs (gui/DESIGN.md)")

// TestRender draws each scene without a window, for looking at (gui/DESIGN.md), and writes
// beside each PNG the lines and state it shows as JSON, which a stand-in server can feed the
// browser page for a shot of the same moment. It needs a GPU or Mesa's software one
// (EGL_PLATFORM=surfaceless), and runs only when asked:
// go test ./gui -run TestRender -render <dir>.
func TestRender(t *testing.T) {
	t.Parallel()
	if *renderTo == "" {
		t.Skip("draws screens only with -render <dir>")
	}
	sh, err := newShaper()
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range scenes() {
		// 12 hours, as Chromium's en-US writes times for the page beside it
		img, err := s.draw(&theme{shaper: sh, clock: clock{hour12: true}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		f, err := os.Create(filepath.Join(*renderTo, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		png.Encode(f, img)
		f.Close()
		b, _ := json.MarshalIndent(s.fixture(), "", " ")
		os.WriteFile(filepath.Join(*renderTo, name+".json"), b, 0o644)
	}
}

// scene is a moment to draw: the saved settings, the party's lines and chat so far, and how
// it ended. js is what the page does to get there (a click, a field typed), setup the same here.
type scene struct {
	height   int
	at       time.Duration
	mood     art.Mood
	settings settings
	form     form
	lines    func(w *watch) []string
	said     []said
	ran      bool
	running  bool
	exitErr  string
	refusal  *party.NotTheSameFile
	js       string
	setup    func(v *view)
	// parties is the saved history, oldest first
	parties []session
	// update and tools are what About shows: GET /update and GET /tools
	update map[string]any
	tools  []tool
}

// fixture is what the stand-in server feeds the page: /state, /log and the scene's script.
func (s scene) fixture() any {
	var lines []string
	if s.lines != nil {
		lines = s.lines(newWatch(s.form))
	}
	run := 0
	if s.ran {
		run = 1
	}
	return map[string]any{
		"height": s.size().Y,
		"state":  map[string]any{"settings": s.settings, "history": append([]session{}, s.parties...), "path": "/home/you/.config/watchparty", "version": "dev", "defaultMpv": "/usr/bin/mpv", "defaultCacheDir": "/home/you/.cache/watchparty"},
		"log":    map[string]any{"run": run, "from": 0, "lines": lines, "saidFrom": 0, "said": s.said, "running": s.running, "exitErr": s.exitErr, "refusal": s.refusal},
		"js":     s.js,
		"update": s.update,
		"tools":  append([]tool{}, s.tools...),
	}
}

func (s scene) size() image.Point { return image.Pt(560, max(s.height, 940)) }

func (s scene) draw(th *theme) (*image.RGBA, error) {
	size := s.size()
	win, err := headless.NewWindow(size.X, size.Y)
	if err != nil {
		return nil, err
	}
	defer win.Release()
	v := newView(th, t0)
	v.mood, v.settings, v.parties = s.mood, s.settings, s.parties
	v.prefs.load(s.settings)
	v.about = about{version: "dev", tools: s.tools, storePath: "/home/you/.config/watchparty", defaultMpv: "/usr/bin/mpv", defaultCacheDir: "/home/you/.cache/watchparty"}
	if cur, ok := s.update["current"].(string); ok {
		v.about.version = cur
	}
	if l, ok := s.update["latest"].(string); ok && s.update["available"] == true {
		v.about.latest = l
		v.updateDot = true
	}
	t := v.party
	t.hostname.SetText(s.settings.Hostname)
	t.relay.SetText(s.settings.Relay)
	t.port.SetText(s.settings.Port)
	t.local.Value = s.settings.Local
	if s.ran {
		w := newWatch(s.form)
		var lines []string
		if s.lines != nil {
			lines = s.lines(w)
		}
		// twice, a second apart, as the page polls: the trail shows once a step has waited
		now := lineTime(lines[len(lines)-1])
		w.render(s.running, s.exitErr, s.refusal, s.form.Playlist, now)
		scr := w.render(s.running, s.exitErr, s.refusal, s.form.Playlist, now.Add(time.Second))
		v.now = &partyNow{ran: true, running: s.running, scr: scr, w: w, said: s.said, lines: lines}
		v.mood = art.Mood{Awake: !scr.sleep, Walking: scr.walking}
	}
	if s.setup != nil {
		s.setup(v)
	}
	var ops op.Ops
	// an input router, as a window has: focus and the like carry from frame to frame
	var router input.Router
	// three frames: the first lays out what the next ones measure, scroll, open and focus
	for range 3 {
		ops.Reset()
		gtx := layout.Context{Ops: &ops, Now: t0.Add(s.at), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}, Constraints: layout.Exact(size), Source: router.Source()}
		v.frame(gtx)
		router.Frame(&ops)
	}
	if err := win.Frame(&ops); err != nil {
		return nil, err
	}
	img := image.NewRGBA(image.Rectangle{Max: size})
	return img, win.Screenshot(img)
}

// recorded keeps the lines a watch reads, for the page to read the same ones.
func recorded(w *watch, s int, msgs ...string) []string {
	var lines []string
	for i, m := range msgs {
		l := at(s+i).Format("2006/01/02 15:04:05 ") + m
		w.line(l)
		lines = append(lines, l)
	}
	return lines
}

const (
	video  = "/home/you/Videos/Big Buck Bunny.mkv"
	invite = "-relay wss://relay.example.com -room K7Q2M9 -secret 4XJ8T2PLW3"
)

var (
	savedSettings = settings{Hostname: "Mo", Relay: "wss://relay.example.com", Local: true}
	withKey       = settings{Hostname: "Mo", Relay: "wss://relay.example.com", Local: true, CreateKey: "k3y", PPort: "7000", MpvFlags: "--fullscreen", MpvPicker: "corner"}
)

func scenes() map[string]scene {
	relay := form{Mode: "relay", File: video, Relay: savedSettings.Relay, Hostname: "Mo"}
	in := func(w *watch) []string {
		return recorded(w, 0,
			`checking 1 of 1: "Big Buck Bunny.mkv"`,
			"file fingerprint: 9f2c4e",
			"your address to share is "+invite,
			"relay: connected",
			"mpv is up, loading the video",
			`now playing: "Big Buck Bunny.mkv"`,
			"init done",
			`member here: {"id":"r1","name":"Ana"}`,
			`member joined: {"id":"r2","name":"Sam"}`,
			shown("paused for Sam: joining"),
			event("pause", "yes"),
			shown("resumed by Ana"),
			shown("paused by Sam"),
			shown("from 1:02 to 4:30, by Sam"),
			`buffered ahead: 14 for {"id":"r2","name":"Sam"}`,
		)
	}
	said := []said{
		{At: at(20).UnixMilli(), Who: "Ana", Text: "this part 😂 again"},
		{At: at(21).UnixMilli(), Who: "Mo", React: "1f602", Ours: true},
		{At: at(22).UnixMilli(), Who: "Sam", Text: "wait for me, still loading"},
		{At: at(23).UnixMilli(), Who: "Mo", Text: "ok, paused", Ours: true},
	}
	day := func(h, m int) time.Time { return t0.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	parties := []session{
		{Started: day(-50, 0), Ended: day(-48, 30), File: "/home/you/Videos/Trip/01 Arrival.mkv", Playlist: true, Mode: "relay", Relay: savedSettings.Relay, Room: "B3XQ7", Secret: "Q8WE2", Finished: true, People: []string{},
			Videos: []played{{Name: "01 Arrival.mkv", Started: day(-50, 0), Ended: day(-49, 20)}, {Name: "02 The Market.mkv", Started: day(-49, 20), Ended: day(-48, 50)}, {Name: "03 Home.mkv", Started: day(-48, 50), Ended: day(-48, 30)}}},
		{Started: day(-26, 0), Ended: day(-25, 10), File: "/home/you/Videos/Sintel.mkv", Mode: "direct", Addrs: "192.168.1.5:6969", Secret: "ZP4M1", People: []string{"Sam"},
			Videos: []played{{Name: "Sintel.mkv", Started: day(-26, 0), Ended: day(-25, 10)}}},
		{Started: day(-2, 0), Ended: day(-1, 0), File: video, Mode: "relay", Relay: savedSettings.Relay, Room: "K7Q2M9", Secret: "4XJ8T2PLW3", People: []string{"Ana", "Sam"},
			Videos: []played{{Name: "Big Buck Bunny.mkv", Started: day(-2, 0), Ended: day(-1, 0)}}},
	}
	return map[string]scene{
		"settings": {height: 1500, at: 27 * time.Second, settings: savedSettings, js: `show("settings")`, setup: func(v *view) { v.tab = "settings" }},
		"settings-update": {height: 1900, at: 27 * time.Second, settings: withKey,
			update: map[string]any{"current": "v0.0.58", "latest": "v0.0.59", "available": true},
			tools: []tool{
				{Name: "mpv", Version: "0.40.0", Path: `C:\Program Files\Watch Party\mpv\mpv.exe`},
				{Name: "ffmpeg", Update: "Not found.", Steps: []string{"Download the latest build from gyan.dev (ffmpeg-release-essentials.zip).", `Unzip it and copy bin\ffmpeg.exe into C:\Program Files\Watch Party.`}},
				{Name: "yt-dlp", Version: "2026.11.20", Path: `C:\Program Files\Watch Party\yt-dlp.exe`, Update: "Run yt-dlp -U in a terminal."},
			},
			js: `show("settings"); setTimeout(() => { document.querySelectorAll("#tools .info")[0].click(); $("settings-saved").textContent = "Saved."; $("settings-saved").hidden = false }, 300)`,
			setup: func(v *view) {
				v.tab = "settings"
				v.prefs.steps["ffmpeg"] = &widget.Bool{Value: true}
				v.prefs.saved, v.prefs.savedAt = "Saved.", t0.Add(26*time.Second)
			}},
		"tips": {at: 27 * time.Second, settings: savedSettings, js: `showTip(true)`, setup: func(v *view) { v.tipOpen = true }},
		"tips-end": {at: 27 * time.Second, settings: savedSettings, js: `showTip(true); $("tip").scrollTop = 1e6`, setup: func(v *view) {
			v.tipOpen = true
			v.tips.list.Position.First, v.tips.list.Position.Offset = 1, 0
		}},
		"history-empty": {at: 27 * time.Second, settings: savedSettings, js: `show("history")`, setup: func(v *view) { v.tab = "history" }},
		"history":       {height: 1300, at: 27 * time.Second, settings: savedSettings, parties: parties, js: `show("history")`, setup: func(v *view) { v.tab = "history" }},
		"dialog-restart": {at: 27 * time.Second, settings: savedSettings,
			js: `pendingRestart("v0.0.59"); $("restart-dialog").showModal()`,
			setup: func(v *view) {
				v.updateDot = true
				v.dialog = &modal{title: "Update installed", text: "Version v0.0.59 is installed. A restart is required to start using it.", later: "Restart later", now: "Restart now"}
			}},
		"party-form":   {height: 1350, at: 27 * time.Second, settings: savedSettings},
		"party-direct": {height: 1350, at: 27 * time.Second, settings: savedSettings, js: `setMode("direct")`, setup: func(v *view) { v.party.mode.Value = "direct" }},
		"party-starting": {at: 27 * time.Second, settings: savedSettings, form: relay, ran: true, running: true, lines: func(w *watch) []string {
			return recorded(w, 0, `checking 1 of 1: "Big Buck Bunny.mkv"`, "file fingerprint: 9f2c4e", "your address to share is "+invite)
		}},
		"confirm-stop": {at: 27 * time.Second, settings: savedSettings, form: relay, ran: true, running: true, lines: in, said: said,
			setup: func(v *view) {
				v.ask(confirm{title: "End the party?", text: "mpv closes for you, and everyone else keeps watching.", ok: "End the party"})
			}},
		"confirm-clear": {at: 27 * time.Second, settings: savedSettings, parties: parties, js: `show("history")`,
			setup: func(v *view) {
				v.tab = "history"
				v.ask(confirm{title: "Clear the whole party history?", ok: "Clear history"})
			}},
		"party-in": {height: 2000, at: 27 * time.Second, settings: savedSettings, form: relay, ran: true, running: true, lines: in, said: said},
		"party-open": {height: 2600, at: 27 * time.Second, settings: savedSettings, form: relay, ran: true, running: true, lines: in, said: said,
			js: `$("members").open = true; $("said-more").open = true; $("chart").open = true; openStrip(true)`,
			setup: func(v *view) {
				t := v.party
				t.members.Value, t.saidMore.Value, t.chart.Value, t.stripOpen = true, true, true, true
			}},
		"party-error": {height: 1500, at: 27 * time.Second, settings: savedSettings, form: relay, ran: true, exitErr: "mpv: exit status 2", lines: func(w *watch) []string {
			return recorded(w, 0, `checking 1 of 1: "Big Buck Bunny.mkv"`, "file fingerprint: 9f2c4e", "mpv is up, loading the video", "mpv: exit status 2")
		}},
		"party-refused": {height: 1500, at: 27 * time.Second, settings: savedSettings, form: relay, ran: true,
			exitErr: "not the same file",
			refusal: &party.NotTheSameFile{Rows: []party.Row{
				{Label: "Length", Party: "9:56", Yours: "10:34", Differs: true},
				{Label: "Size", Party: "263 MB", Yours: "276 MB", Differs: true},
				{Label: "Fingerprint", Party: "4be1a07c", Yours: "9f2c4e11", Mono: true, Differs: true},
			}},
			lines: func(w *watch) []string {
				return recorded(w, 0, `checking 1 of 1: "Big Buck Bunny.mkv"`, "file fingerprint: 9f2c4e")
			}},
	}
}
