//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestFileURIPath(t *testing.T) {
	t.Parallel()
	for uri, want := range map[string]string{
		"file:///home/me/video.mkv":                                      "/home/me/video.mkv",
		"file:///home/me/My%20Videos/%D9%81%D9%8A%D8%AF%D9%8A%D9%88.mkv": "/home/me/My Videos/فيديو.mkv",
	} {
		got, err := fileURIPath(uri)
		if err != nil || got != want {
			t.Errorf("fileURIPath(%q) = %q, %v; want %q", uri, got, err, want)
		}
	}
	if _, err := fileURIPath("https://example.com/video.mkv"); err == nil {
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

func TestProbeReadsEachToolsVersionLine(t *testing.T) {
	dir := t.TempDir()
	for name, out := range map[string]string{
		"mpv":    "mpv v0.42.0 Copyright © 2000-2026 mpv/MPlayer/mplayer2 projects",
		"ffmpeg": "ffmpeg version 9.0.2-full_build-www.gyan.dev Copyright (c) 2000-2026 the FFmpeg developers",
		"yt-dlp": "2026.10.26",
	} {
		script := "#!/bin/sh\n[ \"$1\" = " + versionFlags[name].flag + " ] && printf '%s\\nmore\\n' '" + out + "'\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// only now: a parallel test forking while a script is still open for writing hands the
	// child that fd, and exec of the script fails with "text file busy" (golang/go#22315)
	t.Parallel()
	for name, want := range map[string]string{"mpv": "0.42.0", "ffmpeg": "9.0.2-full_build-www.gyan.dev", "yt-dlp": "2026.10.26"} {
		path, version := probe(t.Context(), name, filepath.Join(dir, name))
		if path != filepath.Join(dir, name) || version != want {
			t.Errorf("probe(%s) = %q, %q, want %q", name, path, version, want)
		}
	}
	if path, version := probe(t.Context(), "mpv", filepath.Join(dir, "missing")); path != "" || version != "" {
		t.Errorf("a missing tool probed as %q, %q", path, version)
	}
}
