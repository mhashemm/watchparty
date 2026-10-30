package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mhashemm/watchparty/party"
)

func TestConfig(t *testing.T) {
	relay := form{Mode: "relay", File: " /m.mkv ", Mpv: "/bin/mpv", Relay: " ws://r ", Room: "a b", Secret: "s", Local: true, Port: "1", Addrs: "x:1"}
	got, err := relay.config()
	want := party.Config{File: "/m.mkv", Mpv: "/bin/mpv", Cooldown: party.DefaultCooldown, Relay: "ws://r", Room: "a b", Secret: "s"}
	if err != nil || got != want {
		t.Fatalf("relay config = %+v, %v; want %+v", got, err, want)
	}

	direct := form{Mode: "direct", File: "/m.mkv", Mpv: "/bin/mpv", Relay: "ws://r", Local: true, Port: "6970", Addrs: "1.2.3.4:6969", Cooldown: "2"}
	got, err = direct.config()
	want = party.Config{File: "/m.mkv", Mpv: "/bin/mpv", Cooldown: 2, Local: true, Port: 6970, PublicPort: party.DefaultPort, Addrs: "1.2.3.4:6969"}
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
	g := newGUI(context.Background(), "127.0.0.1:1")
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
