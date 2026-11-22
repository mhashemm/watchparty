package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mhashemm/watchparty/mpv"
	"github.com/mhashemm/watchparty/party"
)

func TestConfig(t *testing.T) {
	t.Parallel()
	relay := form{Mode: "relay", File: " /m.mkv ", Mpv: "/bin/mpv", Relay: " ws://r ", Room: "a b", Secret: "s", Local: true, Port: "1", Addrs: "x:1"}
	got, err := relay.config()
	want := party.Config{File: "/m.mkv", Mpv: "/bin/mpv", Relay: "ws://r", Room: "a b", Secret: "s"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("relay config = %+v, %v; want %+v", got, err, want)
	}

	direct := form{Mode: "direct", File: "/m.mkv", Mpv: "/bin/mpv", Relay: "ws://r", Local: true, Port: "6970", Addrs: "1.2.3.4:6969", Room: "r", Secret: " s "}
	got, err = direct.config()
	want = party.Config{File: "/m.mkv", Mpv: "/bin/mpv", Local: true, Port: 6970, PublicPort: party.DefaultPort, Addrs: "1.2.3.4:6969", Secret: "s"}
	if err != nil || !reflect.DeepEqual(got, want) {
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

// The browser can't fetch app.css or app.js: only GET / is open without the token, so a tag
// inline didn't replace would serve a page with no style or no script.
func TestPageInlinesItsStyleAndScript(t *testing.T) {
	t.Parallel()
	// a Windows checkout has CRLF line endings (CI's windows job)
	crlf := inline(strings.ReplaceAll(pageHTML, "\n", "\r\n"), pageCSS, pageJS)
	for _, tag := range []string{`href="app.css"`, `src="app.js"`} {
		if bytes.Contains(page, []byte(tag)) {
			t.Errorf("the page still loads %s instead of inlining it", tag)
		}
		if bytes.Contains(crlf, []byte(tag)) {
			t.Errorf("with CRLF line endings, the page still loads %s instead of inlining it", tag)
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

// Chromium ignores a Preferences file it can't parse, so one typo would bring back both the
// password manager and the clipped download bubble
func TestTheProfileTurnsOffPasswordsAndTheDownloadBubble(t *testing.T) {
	t.Parallel()
	var p struct {
		Credentials bool `json:"credentials_enable_service"`
		Profile     struct {
			Passwords *bool `json:"password_manager_enabled"`
		} `json:"profile"`
		Downloads struct {
			Bubble *bool `json:"partial_view_enabled"`
		} `json:"download_bubble"`
	}
	if err := json.Unmarshal([]byte(profilePrefs), &p); err != nil {
		t.Fatal(err)
	}
	if p.Credentials || p.Profile.Passwords == nil || *p.Profile.Passwords || p.Downloads.Bubble == nil || *p.Downloads.Bubble {
		t.Fatalf("profilePrefs = %s", profilePrefs)
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

// A page holding /alive hears of new log lines at once, so it reads them without waiting for
// its next poll; the heartbeat stays a comment, which the page doesn't answer.
func TestAPageHearsOfNewLinesAtOnce(t *testing.T) {
	t.Parallel()
	g := newGUI(t.Context(), "127.0.0.1:1", openStore(t.TempDir()+"/gui.json"))
	g.quit = func() {}
	srv := httptest.NewServer(http.HandlerFunc(g.alive))
	defer srv.Close()
	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	stream := bufio.NewReader(res.Body)
	read := func() string {
		line, err := stream.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		stream.ReadString('\n') // the blank line ending the event
		return line
	}
	if got := read(); got != ":\n" {
		t.Fatalf("first said %q, want the heartbeat", got)
	}
	g.Write([]byte("2026/11/21 00:00:00 init done\n"))
	if got := read(); got != "data: lines\n" {
		t.Fatalf("after a line said %q, want the news", got)
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

// A message goes to the running party as typed; with no party, or nothing typed, it goes nowhere.
func TestChatGoesToTheRunningParty(t *testing.T) {
	t.Parallel()
	g := newGUI(context.Background(), "127.0.0.1:1", openStore(t.TempDir()+"/gui.json"))
	post := func(body string) int {
		r := httptest.NewRequest("POST", "/chat", strings.NewReader(body))
		r.Host = g.host
		r.Header.Set("X-Token", g.token)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w.Code
	}
	if got := post(`{"text":"hi"}`); got != http.StatusConflict {
		t.Fatalf("with no party: %d", got)
	}
	chat := make(chan string, 1)
	g.cancel, g.chatTo = func() {}, chat
	if got := post(`{"text":"  "}`); got != http.StatusOK || len(chat) != 0 {
		t.Fatalf("nothing typed: %d, %d sent", got, len(chat))
	}
	if got := post(`{"text":"hi there"}`); got != http.StatusOK {
		t.Fatalf("a message: %d", got)
	}
	if got := post(`{"text":"again"}`); got != http.StatusServiceUnavailable {
		t.Fatalf("with the party's queue full: %d", got)
	}
	if got := <-chat; got != "hi there" {
		t.Fatalf("sent %q", got)
	}
}

// A reaction goes to the running party too, on its own channel, and only one of the picker's.
func TestAReactionGoesToTheRunningParty(t *testing.T) {
	t.Parallel()
	g := newGUI(context.Background(), "127.0.0.1:1", openStore(t.TempDir()+"/gui.json"))
	post := func(body string) int {
		r := httptest.NewRequest("POST", "/react", strings.NewReader(body))
		r.Host = g.host
		r.Header.Set("X-Token", g.token)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w.Code
	}
	if got := post(`{"code":"1f602"}`); got != http.StatusConflict {
		t.Fatalf("with no party: %d", got)
	}
	react, chat := make(chan string, 1), make(chan string, 1)
	g.cancel, g.reactTo, g.chatTo = func() {}, react, chat
	if got := post(`{"code":"1f600"}`); got != http.StatusBadRequest || len(react) != 0 {
		t.Fatalf("one not in the picker: %d, %d sent", got, len(react))
	}
	if got := post(`{"code":"1f602"}`); got != http.StatusOK {
		t.Fatalf("a reaction: %d", got)
	}
	if got := post(`{"code":"1f62d"}`); got != http.StatusServiceUnavailable {
		t.Fatalf("with the party's queue full: %d", got)
	}
	if got := <-react; got != "1f602" || len(chat) != 0 {
		t.Fatalf("sent %q, and %d as chat", got, len(chat))
	}
}

// Chat is kept only for the page, beside the log and never in it, and only while its party
// runs: one from an earlier party is dropped, and the end takes them all.
func TestChatIsKeptOnlyWhileThePartyRuns(t *testing.T) {
	t.Parallel()
	g := newGUI(context.Background(), "127.0.0.1:1", openStore(t.TempDir()+"/gui.json"))
	type said struct {
		Who, Text string
		Ours      bool
	}
	poll := func(from int) (lines []string, chat []said) {
		t.Helper()
		r := httptest.NewRequest("GET", fmt.Sprintf("/log?run=%d&said=%d", g.run, from), nil)
		r.Host = g.host
		r.Header.Set("X-Token", g.token)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		res := struct {
			Lines []string
			Said  []said
		}{}
		if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
			t.Fatal(err)
		}
		return res.Lines, res.Said
	}
	g.run, g.cancel = 1, func() {}
	earlier, heard := g.heard(0), g.heard(1)
	heard(mpv.Said{Who: "you", Text: "hi", Ours: true})
	earlier(mpv.Said{Who: "sam", Text: "from before", Ours: false})
	heard(mpv.Said{Who: "sam", Text: "yo", Ours: false})
	if lines, chat := poll(0); fmt.Sprint(chat) != "[{you hi true} {sam yo false}]" || len(lines) != 0 {
		t.Fatalf("chat %v, lines %q", chat, lines)
	}
	if _, chat := poll(1); fmt.Sprint(chat) != "[{sam yo false}]" {
		t.Fatalf("from the second: %v", chat)
	}
	g.ended(nil)
	heard(mpv.Said{Who: "sam", Text: "after the end", Ours: false})
	if _, chat := poll(0); len(chat) != 0 {
		t.Fatalf("after the end: %v", chat)
	}
	// a member sending without end: only the last saidKept are kept, and a poll counts from
	// the run's first message, dropped or not
	g.run, g.cancel = 2, func() {}
	heard = g.heard(2)
	for i := range saidKept + 3 {
		heard(mpv.Said{Who: "sam", Text: fmt.Sprint(i), Ours: false})
	}
	if _, chat := poll(0); len(chat) != saidKept || chat[0].Text != "3" || chat[len(chat)-1].Text != fmt.Sprint(saidKept+2) {
		t.Fatalf("kept %d, from %v to %v", len(chat), chat[0], chat[len(chat)-1])
	}
	if _, chat := poll(saidKept + 1); fmt.Sprint(chat) != fmt.Sprintf("[{sam %d false} {sam %d false}]", saidKept+1, saidKept+2) {
		t.Fatalf("from the %dth: %v", saidKept+1, chat)
	}
}
