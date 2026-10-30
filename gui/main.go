package main

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
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

func (f form) args() []string {
	args := []string{}
	add := func(name, value string) {
		if value = strings.TrimSpace(value); value != "" {
			args = append(args, "-"+name+"="+value)
		}
	}
	add("file", f.File)
	add("hostname", f.Hostname)
	add("mpv", f.Mpv)
	add("mpvFlags", f.MpvFlags)
	add("cooldown", f.Cooldown)
	if f.Mode == "relay" {
		add("relay", f.Relay)
		add("room", f.Room)
		add("secret", f.Secret)
		add("createKey", f.CreateKey)
		return args
	}
	if f.Local {
		args = append(args, "-local")
	}
	add("port", f.Port)
	add("pport", f.PPort)
	add("addrs", f.Addrs)
	return args
}

type gui struct {
	token, host, bin string
	page             []byte
	mux              *http.ServeMux

	mu       sync.Mutex
	cmd      *exec.Cmd
	run      int
	lines    []string
	exitErr  string
	lastPoll time.Time
}

func newGUI(host, bin string) *gui {
	g := &gui{token: rand.Text(), host: host, bin: bin, lastPoll: time.Now().Add(time.Minute), mux: http.NewServeMux()}
	g.page = bytes.ReplaceAll(page, []byte("{{TOKEN}}"), []byte(g.token))
	g.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(g.page)
	})
	g.mux.HandleFunc("POST /pick", g.pick)
	g.mux.HandleFunc("POST /start", g.start)
	g.mux.HandleFunc("POST /stop", g.stop)
	g.mux.HandleFunc("GET /log", g.log)
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
	if strings.TrimSpace(f.File) == "" {
		http.Error(w, "pick a movie first", http.StatusBadRequest)
		return
	}
	f.Mpv = cmp.Or(strings.TrimSpace(f.Mpv), sibling("mpv"+exeSuffix))

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cmd != nil {
		http.Error(w, "a party is already running", http.StatusConflict)
		return
	}
	cmd := exec.Command(g.bin, f.args()...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	cmd.WaitDelay = 5 * time.Second
	hide(cmd)
	err = cmd.Start()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	g.cmd, g.lines, g.exitErr = cmd, nil, ""
	g.run++
	go g.read(pr)
	go func() {
		err := cmd.Wait()
		pw.Close()
		g.mu.Lock()
		defer g.mu.Unlock()
		g.cmd = nil
		if err != nil {
			g.exitErr = err.Error()
		}
	}()
}

func (g *gui) read(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			g.mu.Lock()
			g.lines = append(g.lines, strings.TrimRight(line, "\r\n"))
			g.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (g *gui) stop(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	cmd := g.cmd
	g.mu.Unlock()
	if cmd == nil {
		return
	}
	interrupt(cmd.Process, false)
	time.AfterFunc(5*time.Second, func() {
		g.mu.Lock()
		alive := g.cmd == cmd
		g.mu.Unlock()
		if alive {
			interrupt(cmd.Process, true)
		}
	})
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
	}{g.run, from, slices.Clone(g.lines[from:]), g.cmd != nil, g.exitErr}
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func (g *gui) idle() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cmd == nil && time.Since(g.lastPoll) > 2*time.Minute
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

func watchpartyBin() string {
	exe, _ := os.Executable()
	if base := filepath.Base(exe); strings.Contains(base, "-gui") {
		if path := sibling(strings.Replace(base, "-gui", "", 1)); path != "" {
			return path
		}
	}
	return cmp.Or(sibling("watchparty"+exeSuffix), "watchparty")
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
	bin := flag.String("watchparty", watchpartyBin(), "path to the watchparty binary")
	flag.Parse()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	g := newGUI(ln.Addr().String(), *bin)
	url := "http://" + g.host + "/"
	log.Printf("gui: %s\n", url)
	openWindow(url)

	go func() {
		for range time.Tick(5 * time.Second) {
			if g.idle() {
				os.Exit(0)
			}
		}
	}()
	panic(http.Serve(ln, g))
}
