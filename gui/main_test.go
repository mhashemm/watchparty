package main

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestArgs(t *testing.T) {
	relay := form{Mode: "relay", File: " /m.mkv ", Relay: "ws://r", Room: "a b", Secret: "s", Local: true, Port: "1", Addrs: "x:1"}
	want := []string{"-file=/m.mkv", "-relay=ws://r", "-room=a b", "-secret=s"}
	if got := relay.args(); !slices.Equal(got, want) {
		t.Fatalf("relay args = %q, want %q", got, want)
	}
	direct := form{Mode: "direct", File: "/m.mkv", Relay: "ws://r", Local: true, Port: "6970", Addrs: "1.2.3.4:6969"}
	want = []string{"-file=/m.mkv", "-local", "-port=6970", "-addrs=1.2.3.4:6969"}
	if got := direct.args(); !slices.Equal(got, want) {
		t.Fatalf("direct args = %q, want %q", got, want)
	}
}

func TestGuard(t *testing.T) {
	g := newGUI("127.0.0.1:1", "/nonexistent")
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
