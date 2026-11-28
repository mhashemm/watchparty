package main

//go:generate go run ./winres icon.ico rsrc_windows_amd64.syso

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gioui.org/app"

	"github.com/mhashemm/watchparty/mpv"
	"github.com/mhashemm/watchparty/party"
	"github.com/mhashemm/watchparty/update"
)

type form struct {
	Mode      string `json:"mode"`
	File      string `json:"file"`
	Hostname  string `json:"hostname"`
	Relay     string `json:"relay"`
	Room      string `json:"room"`
	Secret    string `json:"secret"`
	CreateKey string `json:"createKey"`
	CacheDir  string `json:"cacheDir"`
	Local     bool   `json:"local"`
	Playlist  bool   `json:"playlist"`
	Port      string `json:"port"`
	PPort     string `json:"pport"`
	Addrs     string `json:"addrs"`
	Mpv       string `json:"mpv"`
	MpvFlags  string `json:"mpvFlags"`
	Continue  string `json:"continue"`
}

var pickExts = append(slices.Clone(party.VideoExts), ".torrent")

func (f form) config() (party.Config, error) {
	cfg := party.Config{
		File:     strings.TrimSpace(f.File),
		Hostname: strings.TrimSpace(f.Hostname),
		Mpv:      cmp.Or(strings.TrimSpace(f.Mpv), defaultMpv()),
		MpvFlags: f.MpvFlags,
	}
	var err error
	number := func(name, value string, fallback int) int {
		value = strings.TrimSpace(value)
		if value == "" {
			return fallback
		}
		n, convErr := strconv.Atoi(value)
		if convErr != nil && err == nil {
			err = fmt.Errorf("%s must be a number", name)
		}
		return n
	}
	if cfg.File == "" {
		return cfg, errors.New("pick a video first")
	}
	cfg.Playlist = f.Playlist
	cfg.CacheDir = strings.TrimSpace(f.CacheDir)
	if f.Mode == "relay" {
		cfg.Relay = strings.TrimSpace(f.Relay)
		cfg.Room = strings.TrimSpace(f.Room)
		cfg.Secret = strings.TrimSpace(f.Secret)
		cfg.CreateKey = strings.TrimSpace(f.CreateKey)
		if cfg.Relay == "" {
			return cfg, errors.New("enter the relay address, or paste an invite")
		}
		return cfg, err
	}
	cfg.Local = f.Local
	cfg.Port = number("your port", f.Port, party.DefaultPort)
	cfg.PublicPort = number("public port", f.PPort, party.DefaultPort)
	cfg.Addrs = strings.TrimSpace(f.Addrs)
	cfg.Secret = strings.TrimSpace(f.Secret)
	return cfg, err
}

// gui is the app behind the window: the party, its log and chat, the store. The window's loop
// calls it; the party's goroutines write to it, and changed asks the window for a frame.
type gui struct {
	ctx  context.Context
	quit func()
	exe  string
	wg   sync.WaitGroup

	mu sync.Mutex
	// changed asks the window to draw again: new lines, a message, the party's end
	changed   func()
	cancel    context.CancelFunc
	store     *store
	session   int
	installed string
	run       int
	lines     []string
	exitErr   string
	// refusal is what each side plays, when the party refused this node's file
	refusal *party.NotTheSameFile
	// chatTo and reactTo are the running party's Config.Chat and React; said, the last saidKept messages mpv showed
	// in it, saidGone the ones dropped before them. Kept only here, never in the log or on
	// disk, and gone once the party ends.
	chatTo  chan<- string
	reactTo chan<- string
	// pickerTo is the running party's Config.Picker: Settings changing when mpv's picker shows
	pickerTo chan string
	said     []said
	saidGone int
}

func newGUI(ctx context.Context, st *store) *gui {
	return &gui{ctx: ctx, quit: func() {}, store: st, session: -1, changed: func() {}}
}

// Errors chat and react give for a message the party didn't take.
var (
	errNoParty = errors.New("the party has ended")
	errBusy    = errors.New("too many at once")
)

func (g *gui) start(f form) error {
	cfg, err := f.config()
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancel != nil {
		return errors.New("a party is already running")
	}
	c, cancel := context.WithCancel(g.ctx)
	g.cancel, g.lines, g.exitErr, g.refusal = cancel, nil, "", nil
	g.run++
	// its own channel: a message the party never took dies with it
	chat := make(chan string, 16)
	react := make(chan string, 4)
	g.chatTo, cfg.Chat, cfg.Heard, g.said, g.saidGone = chat, chat, g.heard(g.run), nil, 0
	g.reactTo, cfg.React = react, react
	picker := make(chan string, 1)
	g.pickerTo, cfg.Picker = picker, picker
	g.store.mu.Lock()
	cfg.PickerMode = g.store.Settings.MpvPicker
	g.store.mu.Unlock()
	g.session = g.store.begin(f)
	if f.Continue != "" && f.Mode == "relay" {
		cfg.Entry, cfg.Start = g.store.stoppedAt(g.session)
	}
	g.wg.Go(func() {
		err := party.Run(c, cfg)
		cancel()
		g.ended(err)
	})
	g.changed()
	return nil
}

// heard keeps a chat message mpv showed in run, while that party runs: the last saidKept,
// so a member sending without end can't fill this process.
func (g *gui) heard(run int) func(mpv.Said) {
	return func(m mpv.Said) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.run == run && g.cancel != nil {
			g.said = append(g.said, said{At: time.Now().UnixMilli(), Who: m.Who, Text: m.Text, React: m.React, Ours: m.Ours})
			if over := len(g.said) - saidKept; over > 0 {
				g.said, g.saidGone = slices.Clone(g.said[over:]), g.saidGone+over
			}
			g.changed()
		}
	}
}

// saidKept is the most chat messages kept, and so the most the window lists.
const saidKept = 500

// ended is the party gone, Run having returned err. Its chat goes with it.
func (g *gui) ended(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.store.end(g.session)
	g.cancel, g.session, g.said, g.saidGone = nil, -1, nil, 0
	if err != nil {
		g.exitErr = err.Error()
	}
	errors.As(err, &g.refusal)
	g.changed()
}

func (g *gui) Write(p []byte) (int, error) {
	g.mu.Lock()
	dirty := false
	for line := range strings.SplitSeq(strings.TrimRight(string(p), "\n"), "\n") {
		g.lines = append(g.lines, line)
		if g.session >= 0 && g.store.note(g.session, line) {
			dirty = true
		}
	}
	g.changed()
	g.mu.Unlock()
	// the disk write waits until g.mu is free, which the window needs every frame. What it
	// writes is the store as it is by then, so a party ending in between loses nothing.
	if dirty {
		g.store.save()
	}
	return len(p), nil
}

// said is a chat message or reaction mpv showed: who, the text or the reaction's code, when
// (ms), and whether it is this node's own, which only that says: a member can be named "you".
type said struct {
	At    int64  `json:"at"`
	Who   string `json:"who"`
	Text  string `json:"text"`
	React string `json:"react,omitempty"`
	Ours  bool   `json:"ours"`
}

// news is what changed since the window last looked: the lines from From, the messages from
// SaidFrom (counted from the run's first, dropped or not), and how the party stands.
type news struct {
	Run      int
	From     int
	Lines    []string
	SaidFrom int
	Said     []said
	Running  bool
	ExitErr  string
	Refusal  *party.NotTheSameFile
}

// news answers what is new since from and saidFrom in run; a different run starts from 0, so
// two runs never splice.
func (g *gui) news(run, from, saidFrom int) news {
	g.mu.Lock()
	defer g.mu.Unlock()
	if run != g.run {
		from, saidFrom = 0, 0
	}
	from = min(max(from, 0), len(g.lines))
	saidFrom = min(max(saidFrom, g.saidGone), g.saidGone+len(g.said))
	return news{g.run, from, slices.Clone(g.lines[from:]), saidFrom, slices.Clone(g.said[saidFrom-g.saidGone:]), g.cancel != nil, g.exitErr, g.refusal}
}

// appState is what the window shows from the store, and the defaults it shows as hints.
type appState struct {
	settings        settings
	history         []session
	path            string
	defaultMpv      string
	defaultCacheDir string
}

func (g *gui) state() appState {
	cache, _ := party.DefaultCacheDir()
	s := appState{defaultMpv: defaultMpv(), defaultCacheDir: cache}
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	s.settings, s.history, s.path = g.store.Settings, slices.Clone(g.store.History), filepath.Dir(g.store.path)
	return s
}

func (g *gui) saveSettings(in settings) error {
	if in.MpvPicker != "" && !slices.Contains(mpv.PickerModes, in.MpvPicker) {
		return errors.New("no such picker mode")
	}
	g.store.mu.Lock()
	s := &g.store.Settings
	s.Relay, s.Mpv, s.MpvFlags = strings.TrimSpace(in.Relay), strings.TrimSpace(in.Mpv), strings.TrimSpace(in.MpvFlags)
	s.CreateKey, s.PPort = strings.TrimSpace(in.CreateKey), strings.TrimSpace(in.PPort)
	s.CacheDir = strings.TrimSpace(in.CacheDir)
	s.MpvPicker = in.MpvPicker
	err := g.store.saveSettings()
	g.store.mu.Unlock()
	// the running party's mpv follows at once; an earlier change it hasn't taken yet gives way
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancel != nil {
		select {
		case <-g.pickerTo:
		default:
		}
		select {
		case g.pickerTo <- cmp.Or(in.MpvPicker, mpv.PickerMove):
		default:
		}
	}
	return err
}

// lastWindow is the window as it was last left; zero the first time.
func (g *gui) lastWindow() windowSize {
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	return g.store.Window
}

// saveTheme remembers the theme as it is picked. The Settings tab's Save leaves it alone, so
// that tab's copy of the settings can't put an older one back.
func (g *gui) saveTheme(theme string) error {
	if !slices.Contains(themes, theme) {
		return errors.New("no such theme")
	}
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	g.store.Settings.Theme = theme
	return g.store.saveSettings()
}

// saveWindow remembers the window as it is left, for the next start.
func (g *gui) saveWindow(w windowSize) error {
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	g.store.Window = w
	return g.store.saveSettings()
}

func (g *gui) clearHistory() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	g.store.History, g.session = nil, -1
	return g.store.saveHistory()
}

func (g *gui) stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancel != nil {
		g.cancel()
	}
}

// chat hands the running party a message the person typed; nothing typed goes nowhere.
func (g *gui) chat(text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return g.toParty(func() chan<- string { return g.chatTo }, text)
}

// react hands the running party a reaction the person picked, one of mpv.Reactions.
func (g *gui) react(code string) error {
	if !slices.Contains(mpv.Reactions(), code) {
		return errors.New("no such reaction")
	}
	return g.toParty(func() chan<- string { return g.reactTo }, code)
}

// toParty hands the running party v on its channel, to (read under g.mu).
func (g *gui) toParty(to func() chan<- string, v string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancel == nil {
		return errNoParty
	}
	select {
	case to() <- v:
		return nil
	default:
		return errBusy
	}
}

// pick asks the OS for a path: the video, mpv, or the cache folder.
func (g *gui) pick(field string) (string, error) {
	switch field {
	case "mpv":
		return pickMpv()
	case "cacheDir":
		return pickFolder()
	}
	return pickFile()
}

// saveChart saves the whole log, not the window's trimmed copy, where the OS's Save dialog
// says; it answers the file's name, or "" when the person cancelled.
func (g *gui) saveChart() (string, error) {
	g.mu.Lock()
	text := strings.Join(g.lines, "\n") + "\n"
	g.mu.Unlock()
	path, err := pickSave("watchparty-" + time.Now().Format("2006-01-02-15-04") + ".log")
	if err != nil || path == "" {
		return "", err
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func defaultMpv() string {
	if path, err := exec.LookPath("mpv"); err == nil {
		return path
	}
	return cmp.Or(sibling("mpv"+exeSuffix), ownCopy("mpv"), "mpv")
}

// ownCopy is the copy of a tool setup.nsi put in the app's folder, or "".
func ownCopy(name string) string {
	dir := name
	if name == "ffmpeg" {
		dir = filepath.Join(name, "bin")
	}
	return sibling(filepath.Join(dir, name+exeSuffix))
}

func sibling(name string) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	path := filepath.Join(filepath.Dir(exe), name)
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// dialog runs a picker and answers what it printed; a non-zero exit is a cancel.
func dialog(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	hide(cmd)
	out, err := cmd.Output()
	if _, cancelled := err.(*exec.ExitError); cancelled {
		return "", nil
	}
	return strings.TrimSpace(string(out)), err
}

// windowTitle is the window's, the page's <title> before
const windowTitle = "Hahas and Hehes Watch Party"

func main() {
	// the heap may grow half its live size before a collection, not Go's default whole:
	// a frame's garbage is small enough that BenchmarkFrame's CPU doesn't change (25 cost 5%)
	debug.SetGCPercent(50)
	c, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	g := newGUI(c, openStore(storePath()))
	g.quit = stop
	var err error
	g.exe, err = update.Executable()
	if err == nil {
		os.Remove(g.exe + ".old")
	}
	log.SetOutput(io.MultiWriter(g, os.Stderr))
	// the desktop entry install.sh writes: Wayland and X11 match the window to it by this
	app.ID = "watchparty"
	go func() {
		if err := g.window(new(app.Window)); err != nil {
			log.Printf("gui: %v\n", err)
		}
		log.Println("gui: window closed")
		// the party ends with the window, and Run's defers finish before the process does
		stop()
		g.stop()
		g.wg.Wait()
		os.Exit(0)
	}()
	app.Main()
}
