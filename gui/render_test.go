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
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"

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
		"state":  map[string]any{"settings": s.settings, "history": []any{}, "path": "/home/you/.config/watchparty", "version": "dev", "defaultMpv": "/usr/bin/mpv", "defaultCacheDir": "/home/you/.cache/watchparty"},
		"log":    map[string]any{"run": run, "from": 0, "lines": lines, "saidFrom": 0, "said": s.said, "running": s.running, "exitErr": s.exitErr, "refusal": s.refusal},
		"js":     s.js,
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
	v.mood, v.settings = s.mood, s.settings
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
	// three frames: the first lays out what the next ones measure, scroll and open
	for range 3 {
		ops.Reset()
		gtx := layout.Context{Ops: &ops, Now: t0.Add(s.at), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}, Constraints: layout.Exact(size)}
		v.frame(gtx)
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

var savedSettings = settings{Hostname: "Mo", Relay: "wss://relay.example.com", Local: true}

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
	return map[string]scene{
		"party-form":   {height: 1350, at: 27 * time.Second, settings: savedSettings},
		"party-direct": {height: 1350, at: 27 * time.Second, settings: savedSettings, js: `setMode("direct")`, setup: func(v *view) { v.party.mode.Value = "direct" }},
		"party-starting": {at: 27 * time.Second, settings: savedSettings, form: relay, ran: true, running: true, lines: func(w *watch) []string {
			return recorded(w, 0, `checking 1 of 1: "Big Buck Bunny.mkv"`, "file fingerprint: 9f2c4e", "your address to share is "+invite)
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
