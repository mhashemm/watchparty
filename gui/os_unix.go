//go:build !windows

package main

import (
	"cmp"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/godbus/dbus/v5"
)

const exeSuffix = ""

var chromiumFamily = regexp.MustCompile(`(?i)chrom|edge|brave|vivaldi`)

func browsers() []browser {
	home, _ := os.UserHomeDir()
	return desktopBrowsers(os.Getenv, home)
}

func desktopBrowsers(getenv func(string) string, home string) []browser {
	dataHome := cmp.Or(getenv("XDG_DATA_HOME"), filepath.Join(home, ".local", "share"))
	data := append([]string{dataHome}, filepath.SplitList(cmp.Or(getenv("XDG_DATA_DIRS"), "/usr/local/share:/usr/share"))...)
	for _, d := range []string{filepath.Join(dataHome, "flatpak", "exports", "share"), "/var/lib/flatpak/exports/share", "/var/lib/snapd/desktop"} {
		if !slices.Contains(data, d) {
			data = append(data, d)
		}
	}
	seen := map[string]bool{}
	var found []browser
	for _, d := range data {
		dir := filepath.Join(d, "applications")
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			id := f.Name()
			if !strings.HasSuffix(id, ".desktop") || seen[id] {
				continue
			}
			seen[id] = true
			e := keyFile(filepath.Join(dir, id), "[Desktop Entry]")
			cmd := execArgs(e["Exec"])
			if e["Hidden"] == "true" || len(cmd) == 0 || !chromiumFamily.MatchString(id) || !slices.Contains(strings.Split(e["MimeType"], ";"), "x-scheme-handler/https") {
				continue
			}
			root := os.TempDir()
			if app := e["X-Flatpak"]; app != "" {
				root = filepath.Join(home, ".var", "app", app, "cache")
			} else if name := e["X-SnapInstanceName"]; name != "" {
				root = filepath.Join(home, "snap", name, "common")
			}
			found = append(found, browser{id, cmd, root})
		}
	}
	def := defaultBrowser(getenv, home, data)
	if i := slices.IndexFunc(found, func(b browser) bool { return b.name == def }); i > 0 {
		b := found[i]
		found = slices.Insert(slices.Delete(found, i, i+1), 0, b)
	}
	return found
}

func defaultBrowser(getenv func(string) string, home string, data []string) string {
	dirs := append([]string{cmp.Or(getenv("XDG_CONFIG_HOME"), filepath.Join(home, ".config"))}, filepath.SplitList(cmp.Or(getenv("XDG_CONFIG_DIRS"), "/etc/xdg"))...)
	for _, d := range data {
		dirs = append(dirs, filepath.Join(d, "applications"))
	}
	var names []string
	for _, desktop := range strings.Split(strings.ToLower(getenv("XDG_CURRENT_DESKTOP")), ":") {
		if desktop != "" {
			names = append(names, desktop+"-mimeapps.list")
		}
	}
	names = append(names, "mimeapps.list")
	for _, d := range dirs {
		for _, name := range names {
			if v := keyFile(filepath.Join(d, name), "[Default Applications]")["x-scheme-handler/https"]; v != "" {
				return strings.Split(v, ";")[0]
			}
		}
	}
	return ""
}

func keyFile(path, group string) map[string]string {
	data, _ := os.ReadFile(path)
	keys := map[string]string{}
	in := false
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			in = line == group
		} else if k, v, ok := strings.Cut(line, "="); in && ok {
			keys[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return keys
}

func execArgs(line string) []string {
	var args []string
	var arg strings.Builder
	started, quoted, escaped := false, false, false
	for _, r := range line {
		switch {
		case escaped:
			arg.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			quoted = !quoted
			started = true
		case (r == ' ' || r == '\t') && !quoted:
			if started {
				args = append(args, arg.String())
				arg.Reset()
				started = false
			}
		default:
			arg.WriteRune(r)
			started = true
		}
	}
	if started {
		args = append(args, arg.String())
	}
	args = slices.DeleteFunc(args, func(a string) bool {
		return a == "@@" || a == "@@u" || len(a) == 2 && a[0] == '%' && a != "%%"
	})
	for i := range args {
		args[i] = strings.ReplaceAll(args[i], "%%", "%")
	}
	return args
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
