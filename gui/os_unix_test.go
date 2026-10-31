//go:build !windows

package main

import (
	"slices"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestFileURIPath(t *testing.T) {
	t.Parallel()
	for uri, want := range map[string]string{
		"file:///home/me/movie.mkv":                                "/home/me/movie.mkv",
		"file:///home/me/My%20Movies/%D9%81%D9%8A%D9%84%D9%85.mkv": "/home/me/My Movies/فيلم.mkv",
	} {
		got, err := fileURIPath(uri)
		if err != nil || got != want {
			t.Errorf("fileURIPath(%q) = %q, %v; want %q", uri, got, err, want)
		}
	}
	if _, err := fileURIPath("https://example.com/movie.mkv"); err == nil {
		t.Error("a non-file uri must be an error")
	}
}

// The portal spec types filters as a(sa(us)); anything else and OpenFile refuses
// the whole call, so Browse stops working.
func TestVideoFilterIsWhatThePortalExpects(t *testing.T) {
	t.Parallel()
	if sig := dbus.SignatureOf([]portalFilter{videoFilter()}).String(); sig != "a(sa(us))" {
		t.Fatalf("filters signature = %s, want a(sa(us))", sig)
	}
	rules := videoFilter().Rules
	for _, want := range []portalRule{{1, "video/*"}, {0, "*.mkv"}, {0, "*.MKV"}, {0, "*.mp4"}} {
		if !slices.Contains(rules, want) {
			t.Errorf("video filter is missing %+v", want)
		}
	}
}
