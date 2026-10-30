package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"

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

	direct := form{Mode: "direct", File: "/m.mkv", Mpv: "/bin/mpv", Relay: "ws://r", Local: true, Port: "6970", Addrs: "1.2.3.4:6969"}
	got, err = direct.config()
	want = party.Config{File: "/m.mkv", Mpv: "/bin/mpv", Local: true, Port: 6970, PublicPort: party.DefaultPort, Addrs: "1.2.3.4:6969"}
	if err != nil || got != want {
		t.Fatalf("direct config = %+v, %v; want %+v", got, err, want)
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

func TestNewer(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		latest, current string
		want            bool
	}{
		{"v0.0.12", "v0.0.11", true},
		{"v0.0.10", "v0.0.9", true},
		{"v0.1.0", "v0.0.99", true},
		{"v0.0.11", "v0.0.11", false},
		{"v0.0.9", "v0.0.10", false},
		{"v1.0", "v1.0.0", false},
		{"v1.0.1", "v1.0", true},
	} {
		if got := newer(c.latest, c.current); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestAssetName(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ goos, goarch, want string }{
		{"windows", "amd64", "watchparty-gui.exe"},
		{"windows", "arm64", ""},
		{"linux", "amd64", "watchparty-gui_amd64"},
		{"linux", "arm64", "watchparty-gui_arm64"},
		{"darwin", "amd64", "watchparty-gui_mac_amd64"},
		{"darwin", "arm64", "watchparty-gui_mac_arm64"},
		{"freebsd", "amd64", ""},
	} {
		if got := assetName(c.goos, c.goarch); got != c.want {
			t.Errorf("assetName(%q, %q) = %q, want %q", c.goos, c.goarch, got, c.want)
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
