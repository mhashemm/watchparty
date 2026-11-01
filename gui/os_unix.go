//go:build !windows

package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/godbus/dbus/v5"
)

const exeSuffix = ""

var browsers = append([]string{"google-chrome", "chromium", "chromium-browser", "microsoft-edge", "brave-browser"}, flatpaks(os.Getenv("XDG_DATA_HOME"), os.Getenv("HOME"))...)

func flatpaks(dataHome, home string) []string {
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	var paths []string
	for _, dir := range []string{filepath.Join(dataHome, "flatpak"), "/var/lib/flatpak"} {
		for _, id := range []string{"com.google.Chrome", "org.chromium.Chromium", "com.microsoft.Edge", "com.brave.Browser"} {
			paths = append(paths, filepath.Join(dir, "exports", "bin", id))
		}
	}
	return paths
}

func openURL(url string) *exec.Cmd {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", url)
	}
	return exec.Command("xdg-open", url)
}

func pickFile() (string, error) {
	if runtime.GOOS == "darwin" {
		types := `"public.movie"`
		for _, ext := range videoExts {
			types += `, "` + ext[1:] + `"`
		}
		return dialog("osascript", "-e", "POSIX path of (choose file of type {"+types+"})")
	}
	return portalPick()
}

type portalRule struct {
	Kind    uint32
	Pattern string
}

type portalFilter struct {
	Name  string
	Rules []portalRule
}

func videoFilter() portalFilter {
	f := portalFilter{Name: "Videos", Rules: []portalRule{{1, "video/*"}}}
	for _, ext := range videoExts {
		f.Rules = append(f.Rules, portalRule{0, "*" + ext}, portalRule{0, "*" + strings.ToUpper(ext)})
	}
	return f
}

func portalPick() (string, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return "", err
	}
	defer conn.Close()

	token := rand.Text()
	sender := strings.ReplaceAll(strings.TrimPrefix(conn.Names()[0], ":"), ".", "_")
	request := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/" + sender + "/" + token)
	err = conn.AddMatchSignal(dbus.WithMatchObjectPath(request), dbus.WithMatchInterface("org.freedesktop.portal.Request"), dbus.WithMatchMember("Response"))
	if err != nil {
		return "", err
	}
	signals := make(chan *dbus.Signal, 1)
	conn.Signal(signals)

	err = conn.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop").
		Call("org.freedesktop.portal.FileChooser.OpenFile", 0, "", "Pick a video", map[string]dbus.Variant{
			"handle_token": dbus.MakeVariant(token),
			"filters":      dbus.MakeVariant([]portalFilter{videoFilter()}),
		}).Err
	if err != nil {
		return "", err
	}
	for sig := range signals {
		if sig.Path != request || len(sig.Body) < 2 {
			continue
		}
		if code, _ := sig.Body[0].(uint32); code != 0 {
			return "", nil
		}
		results, _ := sig.Body[1].(map[string]dbus.Variant)
		uris, _ := results["uris"].Value().([]string)
		if len(uris) == 0 {
			return "", nil
		}
		return fileURIPath(uris[0])
	}
	return "", errors.New("portal: connection closed before a response")
}

func fileURIPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return "", fmt.Errorf("portal: unexpected uri %q", uri)
	}
	return u.Path, nil
}

func hide(_ *exec.Cmd) {}

func replaceExe(src, dst string) error {
	return os.Rename(src, dst)
}
