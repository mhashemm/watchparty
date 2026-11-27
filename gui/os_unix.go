//go:build !windows

package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strings"

	gioevent "gioui.org/io/event"
	"github.com/godbus/dbus/v5"
)

const exeSuffix = ""

func pickFile() (string, error) {
	if runtime.GOOS == "darwin" {
		types := []string{`"public.movie"`}
		for _, ext := range pickExts {
			types = append(types, `"`+ext[1:]+`"`)
		}
		return dialog("osascript", "-e", "POSIX path of (choose file of type {"+strings.Join(types, ", ")+"})")
	}
	return portalPick("Pick a video", map[string]dbus.Variant{"filters": dbus.MakeVariant([]portalFilter{videoFilter()})})
}

func pickMpv() (string, error) {
	if runtime.GOOS == "darwin" {
		return dialog("osascript", "-e", "POSIX path of (choose file)")
	}
	return portalPick("Pick mpv", map[string]dbus.Variant{})
}

func pickFolder() (string, error) {
	if runtime.GOOS == "darwin" {
		return dialog("osascript", "-e", "POSIX path of (choose folder)")
	}
	return portalPick("Pick a folder", map[string]dbus.Variant{"directory": dbus.MakeVariant(true)})
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
	for _, ext := range pickExts {
		f.Rules = append(f.Rules, portalRule{0, "*" + ext}, portalRule{0, "*" + strings.ToUpper(ext)})
	}
	return f
}

// pickSave asks where to save a file, name suggested.
func pickSave(name string) (string, error) {
	if runtime.GOOS == "darwin" {
		return dialog("osascript", "-e", `POSIX path of (choose file name default name "`+name+`")`)
	}
	return portalAsk("SaveFile", "Save the chart", map[string]dbus.Variant{"current_name": dbus.MakeVariant(name)})
}

func portalPick(title string, options map[string]dbus.Variant) (string, error) {
	return portalAsk("OpenFile", title, options)
}

// portalAsk calls the portal's FileChooser method (OpenFile or SaveFile) and answers the path
// chosen, or "" when the person cancelled.
func portalAsk(method, title string, options map[string]dbus.Variant) (string, error) {
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

	options["handle_token"] = dbus.MakeVariant(token)
	err = conn.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop").
		Call("org.freedesktop.portal.FileChooser."+method, 0, "", title, options).Err
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

// viewEvent is a window's handle once it has one: nothing to do with it here.
func viewEvent(gioevent.Event, string) {}

// viewHandle is a Windows window's handle: none here.
func viewHandle(gioevent.Event) uintptr { return 0 }

// darkFrame: macOS's title bar follows the appearance by itself, and a Linux desktop draws
// its own (the themed one is the palette's).
func darkFrame(uintptr, bool) {}

func recordVersion(string, string) {}
