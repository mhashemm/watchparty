package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mhashemm/watchparty/party"
)

//go:embed index.html
var page []byte

type form struct {
	Mode      string `json:"mode"`
	File      string `json:"file"`
	Hostname  string `json:"hostname"`
	Relay     string `json:"relay"`
	Room      string `json:"room"`
	Secret    string `json:"secret"`
	CreateKey string `json:"createKey"`
	Local     bool   `json:"local"`
	Port      string `json:"port"`
	PPort     string `json:"pport"`
	Addrs     string `json:"addrs"`
	Mpv       string `json:"mpv"`
	MpvFlags  string `json:"mpvFlags"`
	Cooldown  string `json:"cooldown"`
}

func (f form) config() (party.Config, error) {
	cfg := party.Config{
		File:     strings.TrimSpace(f.File),
		Hostname: strings.TrimSpace(f.Hostname),
		Mpv:      cmp.Or(strings.TrimSpace(f.Mpv), sibling("mpv"+exeSuffix), "mpv"),
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
		return cfg, errors.New("pick a movie first")
	}
	cfg.Cooldown = number("seconds to wait for mpv", f.Cooldown, party.DefaultCooldown)
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
	return cfg, err
}

type gui struct {
	token, host string
	page        []byte
	mux         *http.ServeMux
	ctx         context.Context
	quit        func()
	wg          sync.WaitGroup

	mu       sync.Mutex
	cancel   context.CancelFunc
	store    *store
	session  int
	run      int
	lines    []string
	exitErr  string
	lastPoll time.Time
}

func newGUI(ctx context.Context, host string, st *store) *gui {
	g := &gui{ctx: ctx, token: rand.Text(), host: host, lastPoll: time.Now().Add(time.Minute), mux: http.NewServeMux(), store: st, session: -1}
	g.page = bytes.ReplaceAll(page, []byte("{{TOKEN}}"), []byte(g.token))
	g.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(g.page)
	})
	g.mux.HandleFunc("POST /pick", g.pick)
	g.mux.HandleFunc("POST /start", g.start)
	g.mux.HandleFunc("POST /stop", g.stop)
	g.mux.HandleFunc("GET /log", g.log)
	g.mux.HandleFunc("GET /update", g.checkUpdate)
	g.mux.HandleFunc("POST /update", g.update)
	g.mux.HandleFunc("GET /state", g.state)
	g.mux.HandleFunc("POST /settings", g.saveSettings)
	g.mux.HandleFunc("POST /history/clear", g.clearHistory)
	return g
}

func (g *gui) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Host != g.host || (r.URL.Path != "/" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Token")), []byte(g.token)) != 1) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	g.mux.ServeHTTP(w, r)
}

func (g *gui) pick(w http.ResponseWriter, r *http.Request) {
	path, err := pickFile()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	io.WriteString(w, path)
}

func (g *gui) start(w http.ResponseWriter, r *http.Request) {
	f := form{}
	err := json.NewDecoder(r.Body).Decode(&f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cfg, err := f.config()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancel != nil {
		http.Error(w, "a party is already running", http.StatusConflict)
		return
	}
	c, cancel := context.WithCancel(g.ctx)
	g.cancel, g.lines, g.exitErr = cancel, nil, ""
	g.run++
	g.session = g.store.begin(f)
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		err := party.Run(c, cfg)
		cancel()
		g.mu.Lock()
		defer g.mu.Unlock()
		g.store.end(g.session)
		g.cancel, g.session = nil, -1
		if err != nil {
			g.exitErr = err.Error()
		}
	}()
}

func (g *gui) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for line := range strings.SplitSeq(strings.TrimRight(string(p), "\n"), "\n") {
		g.lines = append(g.lines, line)
		if g.session >= 0 {
			g.store.observe(g.session, line)
		}
	}
	return len(p), nil
}

func (g *gui) state(w http.ResponseWriter, r *http.Request) {
	g.store.mu.Lock()
	b, err := json.Marshal(struct {
		Settings settings  `json:"settings"`
		History  []session `json:"history"`
		Path     string    `json:"path"`
		Version  string    `json:"version"`
	}{g.store.Settings, g.store.History, g.store.path, version})
	g.store.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (g *gui) saveSettings(w http.ResponseWriter, r *http.Request) {
	in := settings{}
	err := json.NewDecoder(r.Body).Decode(&in)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	s := &g.store.Settings
	s.Relay, s.Mpv, s.MpvFlags = strings.TrimSpace(in.Relay), strings.TrimSpace(in.Mpv), strings.TrimSpace(in.MpvFlags)
	s.Cooldown, s.CreateKey, s.PPort = strings.TrimSpace(in.Cooldown), strings.TrimSpace(in.CreateKey), strings.TrimSpace(in.PPort)
	err = g.store.save()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (g *gui) clearHistory(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	g.store.History, g.session = nil, -1
	err := g.store.save()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (g *gui) stop(w http.ResponseWriter, r *http.Request) {
	g.stopParty()
}

func (g *gui) stopParty() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancel != nil {
		g.cancel()
	}
}

func (g *gui) log(w http.ResponseWriter, r *http.Request) {
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	run, _ := strconv.Atoi(r.URL.Query().Get("run"))
	g.mu.Lock()
	g.lastPoll = time.Now()
	if run != g.run {
		from = 0
	}
	from = min(max(from, 0), len(g.lines))
	res := struct {
		Run     int      `json:"run"`
		From    int      `json:"from"`
		Lines   []string `json:"lines"`
		Running bool     `json:"running"`
		ExitErr string   `json:"exitErr"`
	}{g.run, from, slices.Clone(g.lines[from:]), g.cancel != nil, g.exitErr}
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func (g *gui) idle() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cancel == nil && time.Since(g.lastPoll) > 2*time.Minute
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

func dialog(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	hide(cmd)
	out, err := cmd.Output()
	if _, cancelled := err.(*exec.ExitError); cancelled {
		return "", nil
	}
	return strings.TrimSpace(string(out)), err
}

func openWindow(url string) {
	for _, browser := range browsers {
		cmd := exec.Command(browser, "--app="+url, "--window-size=560,940")
		if cmd.Start() == nil {
			go cmd.Wait()
			return
		}
	}
	cmd := openURL(url)
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

func main() {
	c, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	g := newGUI(c, ln.Addr().String(), openStore(storePath()))
	g.quit = stop
	if exe, err := os.Executable(); err == nil {
		os.Remove(exe + ".old")
	}
	log.SetOutput(io.MultiWriter(g, os.Stderr))
	url := "http://" + g.host + "/"
	log.Printf("gui: %s\n", url)
	openWindow(url)

	go http.Serve(ln, g)
	go func() {
		for range time.Tick(5 * time.Second) {
			if g.idle() {
				stop()
			}
		}
	}()
	<-c.Done()
	g.stopParty()
	g.wg.Wait()
}
