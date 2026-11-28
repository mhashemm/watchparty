package main

import (
	"cmp"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mhashemm/watchparty/update"
)

var version = "dev"

// latest is a release newer than this build, or "" when there is none (or this is a dev
// build, which never updates itself).
func (g *gui) latest(ctx context.Context) (string, error) {
	if version == "dev" {
		return "", nil
	}
	tag, err := update.Latest(ctx)
	if err != nil || !update.Newer(tag, version) {
		return "", err
	}
	return tag, nil
}

// install downloads the latest release over this binary; it runs after a restart.
func (g *gui) install() (string, error) {
	g.mu.Lock()
	running := g.cancel != nil
	g.mu.Unlock()
	if running {
		return "", errors.New("end the party before updating")
	}
	if version == "dev" {
		return "", errors.New("development builds do not update themselves")
	}
	tag, err := update.Install(g.ctx, g.exe, version, "watchparty-gui")
	if err != nil {
		return "", err
	}
	recordVersion(g.exe, tag)
	g.mu.Lock()
	g.installed = tag
	g.mu.Unlock()
	return tag, nil
}

// restart starts the installed update and closes this app.
func (g *gui) restart() error {
	g.mu.Lock()
	running, installed := g.cancel != nil, g.installed
	g.mu.Unlock()
	if running {
		return errors.New("end the party before restarting")
	}
	if installed == "" {
		return errors.New("no update is waiting for a restart")
	}
	if err := exec.Command(g.exe).Start(); err != nil {
		return err
	}
	g.quit()
	return nil
}

type tool struct {
	Name    string   `json:"name"`
	Version string   `json:"version,omitempty"`
	Path    string   `json:"path,omitempty"`
	Update  string   `json:"update,omitempty"`
	Steps   []string `json:"steps,omitempty"`
}

// versionFlags is, per tool, the flag that prints its version and which word of the first
// line is the version.
var versionFlags = map[string]struct {
	flag string
	word int
}{"mpv": {"--version", 1}, "ffmpeg": {"-version", 2}, "yt-dlp": {"--version", 0}}

// tools answers About's rows: each tool's version and path, how to update the installer's
// yt-dlp (it breaks as sites change), and for a missing tool how to install it by hand.
func (g *gui) tools(ctx context.Context) []tool {
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
				t.Path, t.Version = probe(ctx, t.Name, cmp.Or(t.Path, t.Name))
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
	return list
}

// probe finds a tool and its version: cmd on PATH, else for ffmpeg and yt-dlp the
// installer's copy. For mpv, cmd is already defaultMpv's choice or the path in Settings.
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

// installSteps are the steps to install a tool by hand: what setup.nsi and install.sh do.
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
