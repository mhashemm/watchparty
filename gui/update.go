package main

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mhashemm/watchparty/update"
)

var version = "dev"

func (g *gui) checkUpdate(w http.ResponseWriter, r *http.Request) {
	res := struct {
		Current   string `json:"current"`
		Latest    string `json:"latest"`
		Available bool   `json:"available"`
		Installed string `json:"installed,omitempty"`
		Error     string `json:"error,omitempty"`
	}{Current: version}
	g.mu.Lock()
	res.Installed = g.installed
	g.mu.Unlock()
	if version != "dev" {
		latest, err := update.Latest(r.Context())
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Latest = latest
			res.Available = update.Newer(latest, version)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func (g *gui) update(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	running := g.cancel != nil
	g.mu.Unlock()
	if running {
		http.Error(w, "end the party before updating", http.StatusConflict)
		return
	}
	if version == "dev" {
		http.Error(w, "development builds do not update themselves", http.StatusBadRequest)
		return
	}
	tag, err := update.Install(r.Context(), g.exe, version, "watchparty-gui")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	recordVersion(g.exe, tag)
	g.mu.Lock()
	g.installed = tag
	g.mu.Unlock()
	io.WriteString(w, tag)
}

func (g *gui) restart(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	running, installed := g.cancel != nil, g.installed
	g.mu.Unlock()
	if running {
		http.Error(w, "end the party before restarting", http.StatusConflict)
		return
	}
	if installed == "" {
		http.Error(w, "no update is waiting for a restart", http.StatusBadRequest)
		return
	}
	err := exec.Command(g.exe).Start()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	io.WriteString(w, installed)
	go func() {
		time.Sleep(500 * time.Millisecond)
		g.closeWindow()
		g.quit()
	}()
}

type tool struct {
	Name    string   `json:"name"`
	Version string   `json:"version,omitempty"`
	Path    string   `json:"path,omitempty"`
	Update  string   `json:"update,omitempty"`
	Steps   []string `json:"steps,omitempty"`
}

// the flag that prints a version, and which word of its first line is the version
var versionFlags = map[string]struct {
	flag string
	word int
}{"mpv": {"--version", 1}, "ffmpeg": {"-version", 2}, "yt-dlp": {"--version", 0}}

// About's rows: each tool's version and path, how to update the installer's yt-dlp, which
// breaks as sites change, and for a missing one how to install it by hand
func (g *gui) tools(w http.ResponseWriter, r *http.Request) {
	list := []tool{}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		mpv := defaultMpv()
		g.store.mu.Lock()
		mpv = cmp.Or(g.store.Settings.Mpv, mpv)
		g.store.mu.Unlock()
		list = []tool{{Name: "mpv", Path: mpv}, {Name: "ffmpeg"}, {Name: "yt-dlp"}}
		var wg sync.WaitGroup
		for i := range list {
			wg.Go(func() {
				t := &list[i]
				t.Path, t.Version = probe(r.Context(), t.Name, cmp.Or(t.Path, t.Name))
				switch {
				case t.Path == "":
					t.Update = "Not found."
					t.Steps = installSteps(runtime.GOOS, t.Name, filepath.Dir(g.exe))
				case t.Name == "yt-dlp" && strings.EqualFold(t.Path, ownCopy(t.Name)):
					t.Update = "Run yt-dlp -U in a terminal."
				}
			})
		}
		wg.Wait()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// cmd on PATH, else for ffmpeg and yt-dlp the installer's copy; mpv's cmd is already
// defaultMpv's choice or the path set in Settings
func probe(ctx context.Context, name, cmd string) (path, version string) {
	path, err := exec.LookPath(cmd)
	if err != nil && cmd == name {
		path = ownCopy(name)
	}
	if path == "" {
		return "", ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	v := versionFlags[name]
	c := exec.CommandContext(ctx, path, v.flag)
	hide(c)
	out, _ := c.Output()
	line, _, _ := strings.Cut(string(out), "\n")
	if words := strings.Fields(line); v.word < len(words) {
		version = strings.TrimPrefix(words[v.word], "v")
	}
	return path, version
}

// by hand, what setup.nsi and install.sh do
func installSteps(goos, name, appDir string) []string {
	if goos != "windows" {
		return []string{"Install Homebrew from brew.sh if you don't have it.", "Run brew install " + name + " in Terminal."}
	}
	dir := appDir + `\` + name
	var get []string
	switch name {
	case "mpv":
		get = []string{"Download mpv-<version>-x86_64-w64-mingw32-full.zip from github.com/mpv-player/mpv/releases/latest.", "Unpack it into " + dir + "."}
	case "ffmpeg":
		get = []string{"Download ffmpeg-<version>-essentials_build.zip from github.com/GyanD/codexffmpeg/releases/latest.", "Unpack what is inside its top folder into " + dir + "."}
	default:
		get = []string{"Download yt-dlp.exe from github.com/yt-dlp/yt-dlp/releases/latest into " + dir + "."}
	}
	bin := dir
	if name == "ffmpeg" {
		bin += `\bin`
	}
	return append(get, "Add "+bin+" to your Path: Start, type environment variables, open Edit environment variables for your account, then Path, Edit, New, and paste it.", "Restart Watchparty, which only sees Path changes when it starts.")
}
