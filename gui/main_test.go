package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mhashemm/watchparty/party"
)

func TestConfig(t *testing.T) {
	t.Parallel()
	relay := form{Mode: "relay", File: " /m.mkv ", Mpv: "/bin/mpv", Relay: " ws://r ", Room: "a b", Secret: "s", Local: true, Port: "1", Addrs: "x:1"}
	got, err := relay.config()
	want := party.Config{File: "/m.mkv", Mpv: "/bin/mpv", Relay: "ws://r", Room: "a b", Secret: "s"}
	if err != nil || got != want {
		t.Fatalf("relay config = %+v, %v; want %+v", got, err, want)
	}

	direct := form{Mode: "direct", File: "/m.mkv", Mpv: "/bin/mpv", Relay: "ws://r", Local: true, Port: "6970", Addrs: "1.2.3.4:6969", Room: "r", Secret: " s "}
	got, err = direct.config()
	want = party.Config{File: "/m.mkv", Mpv: "/bin/mpv", Local: true, Port: 6970, PublicPort: party.DefaultPort, Addrs: "1.2.3.4:6969", Secret: "s"}
	if err != nil || got != want {
		t.Fatalf("direct config = %+v, %v; want %+v", got, err, want)
	}

	for _, file := range []string{"https://example.com/watch?v=x", "/videos/notes.txt"} {
		if _, err := (form{Mode: "relay", File: file, Relay: "ws://r"}).config(); err != nil {
			t.Errorf("typed %q was refused: %v; only the picker filters by type", file, err)
		}
	}

	for _, f := range []form{
		{Mode: "relay", Relay: "ws://r"},
		{Mode: "relay", File: "/m.mkv"},
		{Mode: "direct", File: "/m.mkv", Port: "abc"},
	} {
		if _, err := f.config(); err == nil {
			t.Errorf("config(%+v) succeeded, want an error", f)
		}
	}
}

func TestGuard(t *testing.T) {
	t.Parallel()
	g := newGUI(context.Background(), "127.0.0.1:1", openStore(t.TempDir()+"/gui.json"))
	cases := []struct {
		method, path, host, token string
		want                      int
	}{
		{"GET", "/", "127.0.0.1:1", "", http.StatusOK},
		{"GET", "/", "evil.com", "", http.StatusForbidden},
		{"GET", "/log", "127.0.0.1:1", "", http.StatusForbidden},
		{"GET", "/log", "127.0.0.1:1", "wrong", http.StatusForbidden},
		{"GET", "/log", "127.0.0.1:1", g.token, http.StatusOK},
		{"POST", "/start", "evil.com", g.token, http.StatusForbidden},
		{"POST", "/start", "127.0.0.1:1", "", http.StatusForbidden},
		{"POST", "/start", "127.0.0.1:1", g.token, http.StatusBadRequest},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		r.Host = c.host
		r.Header.Set("X-Token", c.token)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s %s host=%s token=%q: %d, want %d", c.method, c.path, c.host, c.token, w.Code, c.want)
		}
	}
}

// One syntax error anywhere in the page's script kills every button, and nothing
// on the Go side notices. vm.Script parses it the way a browser parses a classic
// <script>: no top-level await, no top-level return.
func TestPageScriptParses(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	_, script, ok := bytes.Cut(page, []byte("<script>"))
	script, _, ok2 := bytes.Cut(script, []byte("</script>"))
	if !ok || !ok2 {
		t.Fatal("no <script> in index.html")
	}
	cmd := exec.Command(node, "-e", `new (require("vm").Script)(require("fs").readFileSync(0, "utf8"), { filename: "index.html" })`)
	cmd.Stdin = bytes.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the page script does not parse:\n%s", out)
	}
}

func TestOnlyEdgeGetsTheEdgeFlag(t *testing.T) {
	t.Parallel()
	for browser, want := range map[string]bool{
		"google-chrome":                 false,
		"chromium":                      false,
		"brave-browser":                 false,
		"org.chromium.Chromium.desktop": false,
		"com.microsoft.Edge.desktop":    true,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`: false,
		"microsoft-edge": true,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`: true,
	} {
		if got := slices.Contains(browserArgs(browser, "u", "p"), "--disable-features=msImplicitSignin"); got != want {
			t.Errorf("%s: Edge flag = %v, want %v", browser, got, want)
		}
	}
}

func TestIdle(t *testing.T) {
	t.Parallel()
	g := newGUI(context.Background(), "127.0.0.1:1", openStore(t.TempDir()+"/gui.json"))
	g.idleAfter = 0
	if g.idle() {
		t.Fatal("idle before the page's first poll, which gets a grace period")
	}
	g.idleAfter = time.Hour
	g.lastPoll = time.Now().Add(-2 * time.Hour)
	if !g.idle() {
		t.Fatal("not idle with no party and no poll for longer than idleAfter")
	}
	req := httptest.NewRequest("GET", "/log", nil)
	req.Host = g.host
	req.Header.Set("X-Token", g.token)
	g.ServeHTTP(httptest.NewRecorder(), req)
	if g.idle() {
		t.Fatal("still idle after the page polled")
	}
	g.lastPoll = time.Now().Add(-2 * time.Hour)
	g.cancel = func() {}
	if g.idle() {
		t.Fatal("idle during a party")
	}
	g.cancel, g.pages = nil, 1
	if g.idle() {
		t.Fatal("idle with a page connected")
	}
}

func TestTheLastPageGoingEndsTheGUI(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := newGUI(t.Context(), ln.Addr().String(), openStore(t.TempDir()+"/gui.json"))
	quit := make(chan struct{})
	g.quit = func() { close(quit) }
	g.closeGrace = 0
	srv := &http.Server{Handler: g}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	open := func() io.ReadCloser {
		req, _ := http.NewRequest("GET", "http://"+g.host+"/alive", nil)
		req.Header.Set("X-Token", g.token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if ct := res.Header.Get("Content-Type"); res.StatusCode != 200 || ct != "text/event-stream" {
			t.Fatalf("GET /alive = %d %q, want 200 text/event-stream", res.StatusCode, ct)
		}
		buf := make([]byte, 3)
		if _, err := io.ReadFull(res.Body, buf); err != nil || string(buf) != ":\n\n" {
			t.Fatalf("first heartbeat = %q, %v", buf, err)
		}
		return res.Body
	}
	open().Close()
	<-quit
}

func TestTheGUIStaysWhileAPageIsConnected(t *testing.T) {
	t.Parallel()
	g := newGUI(t.Context(), "127.0.0.1:1", openStore(t.TempDir()+"/gui.json"))
	quits := 0
	g.quit = func() { quits++ }
	g.pages = 2
	g.pageGone()
	g.checkPages()
	if quits != 0 {
		t.Fatal("quit with a page still connected")
	}
	g.pages = 1
	g.checkPages()
	if quits != 0 {
		t.Fatal("quit although the page reconnected within the grace")
	}
	g.pages = 0
	g.checkPages()
	if quits != 1 {
		t.Fatal("didn't quit with no page back after the grace")
	}
}

func TestShuttingDownIsNotAClosedWindow(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	g := newGUI(ctx, "127.0.0.1:1", openStore(t.TempDir()+"/gui.json"))
	g.quit = func() { t.Fatal("quit") }
	cancel()
	g.checkPages()
}

func TestInstallStepsMatchTheInstaller(t *testing.T) {
	t.Parallel()
	app := `C:\Users\me\AppData\Local\Programs\watchparty`
	for name, want := range map[string][]string{
		"mpv":    {"mpv-<version>-x86_64-w64-mingw32-full.zip", "into " + app + `\mpv.`, "Add " + app + `\mpv to your Path`},
		"ffmpeg": {"ffmpeg-<version>-essentials_build.zip", "top folder into " + app + `\ffmpeg.`, "Add " + app + `\ffmpeg\bin to your Path`},
		"yt-dlp": {"yt-dlp.exe from github.com/yt-dlp/yt-dlp/releases/latest into " + app + `\yt-dlp.`, "Add " + app + `\yt-dlp to your Path`},
	} {
		steps := strings.Join(installSteps("windows", name, app), "\n")
		for _, w := range want {
			if !strings.Contains(steps, w) {
				t.Errorf("%s's steps lack %q:\n%s", name, w, steps)
			}
		}
	}
	if steps := installSteps("darwin", "yt-dlp", "/Applications"); steps[len(steps)-1] != "Run brew install yt-dlp in Terminal." {
		t.Errorf("macOS steps: %q", steps)
	}
}
