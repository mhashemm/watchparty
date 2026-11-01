//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
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

func TestExecArgs(t *testing.T) {
	t.Parallel()
	for line, want := range map[string][]string{
		"chromium %U": {"chromium"},
		"/usr/bin/flatpak run --branch=stable --arch=x86_64 --command=/app/bin/chromium --file-forwarding org.chromium.Chromium @@u %U @@": {"/usr/bin/flatpak", "run", "--branch=stable", "--arch=x86_64", "--command=/app/bin/chromium", "--file-forwarding", "org.chromium.Chromium"},
		"env BAMF_DESKTOP_FILE_HINT=/var/lib/snapd/desktop/applications/chromium_chromium.desktop /snap/bin/chromium %U":                   {"env", "BAMF_DESKTOP_FILE_HINT=/var/lib/snapd/desktop/applications/chromium_chromium.desktop", "/snap/bin/chromium"},
		`"/opt/My Browser/chrome"  --zoom=100%% %u`: {"/opt/My Browser/chrome", "--zoom=100%"},
		"": nil,
	} {
		if got := execArgs(line); !slices.Equal(got, want) {
			t.Errorf("execArgs(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestBrowsersComeFromDesktopEntries(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	write := func(path, content string) {
		os.MkdirAll(filepath.Dir(path), 0o700)
		os.WriteFile(path, []byte(content), 0o600)
	}
	entry := func(exec, mime, extra string) string {
		return "[Desktop Entry]\nName=x\nExec=" + exec + "\nMimeType=" + mime + "\n" + extra + "[Desktop Action new-window]\nExec=ignored\n"
	}
	https := "text/html;x-scheme-handler/http;x-scheme-handler/https;"
	home, sys := filepath.Join(tmp, "home"), filepath.Join(tmp, "sys")
	write(filepath.Join(sys, "applications", "chromium.desktop"), entry("chromium %U", https, ""))
	write(filepath.Join(sys, "applications", "firefox.desktop"), entry("firefox %u", https, ""))
	write(filepath.Join(sys, "applications", "google-chrome.desktop"), entry("/usr/bin/google-chrome-stable %U", https, ""))
	write(filepath.Join(home, "applications", "google-chrome.desktop"), entry("/usr/bin/google-chrome-stable %U", "", ""))
	write(filepath.Join(sys, "applications", "microsoft-edge.desktop"), entry("microsoft-edge %U", https, "Hidden=true\n"))
	write(filepath.Join(sys, "applications", "chrome-abc-Default.desktop"), entry("chrome --app-id=abc", "x-scheme-handler/web+abc;", ""))
	write(filepath.Join(home, "flatpak", "exports", "share", "applications", "org.chromium.Chromium.desktop"),
		entry("/usr/bin/flatpak run --command=chromium --file-forwarding org.chromium.Chromium @@u %U @@", https, "X-Flatpak=org.chromium.Chromium\n"))
	write(filepath.Join(sys, "applications", "brave_brave.desktop"), entry("/snap/bin/brave %U", https, "X-SnapInstanceName=brave\n"))
	write(filepath.Join(tmp, "config", "cosmic-mimeapps.list"), "[Added Associations]\nx-scheme-handler/https=chromium.desktop;\n[Default Applications]\nx-scheme-handler/https=org.chromium.Chromium.desktop;chromium.desktop;\n")
	env := map[string]string{"XDG_DATA_HOME": home, "XDG_DATA_DIRS": sys, "XDG_CONFIG_HOME": filepath.Join(tmp, "config"), "XDG_CURRENT_DESKTOP": "COSMIC"}
	ours := func(found []browser) []browser {
		return slices.DeleteFunc(found, func(b browser) bool {
			return !slices.Contains([]string{"chromium.desktop", "org.chromium.Chromium.desktop", "brave_brave.desktop", "google-chrome.desktop", "microsoft-edge.desktop", "chrome-abc-Default.desktop", "firefox.desktop"}, b.name)
		})
	}
	want := []browser{
		{"org.chromium.Chromium.desktop", []string{"/usr/bin/flatpak", "run", "--command=chromium", "--file-forwarding", "org.chromium.Chromium"}, "/home/u/.var/app/org.chromium.Chromium/cache"},
		{"brave_brave.desktop", []string{"/snap/bin/brave"}, "/home/u/snap/brave/common"},
		{"chromium.desktop", []string{"chromium"}, os.TempDir()},
	}
	if got := ours(desktopBrowsers(func(k string) string { return env[k] }, "/home/u")); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("with the default in cosmic-mimeapps.list:\n got %v\nwant %v", got, want)
	}
	delete(env, "XDG_CURRENT_DESKTOP")
	want = []browser{want[1], want[2], want[0]}
	if got := ours(desktopBrowsers(func(k string) string { return env[k] }, "/home/u")); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("with no default:\n got %v\nwant %v", got, want)
	}
}
