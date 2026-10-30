package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const latestReleaseURL = "https://api.github.com/repos/mhashemm/watchparty/releases/latest"

var version = "dev"

type release struct {
	Tag    string `json:"tag_name"`
	Assets []struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

func latestRelease(c context.Context) (release, error) {
	c, cancel := context.WithTimeout(c, 15*time.Second)
	defer cancel()
	rel := release{}
	req, err := http.NewRequestWithContext(c, http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		return rel, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return rel, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return rel, fmt.Errorf("github: %s", res.Status)
	}
	return rel, json.NewDecoder(res.Body).Decode(&rel)
}

func assetName(goos, goarch string) string {
	switch goos {
	case "windows":
		if goarch == "amd64" {
			return "watchparty-gui.exe"
		}
	case "darwin":
		return "watchparty-gui_mac_" + goarch
	case "linux":
		return "watchparty-gui_" + goarch
	}
	return ""
}

func newer(latest, current string) bool {
	l, c := strings.Split(strings.TrimPrefix(latest, "v"), "."), strings.Split(strings.TrimPrefix(current, "v"), ".")
	for i := range max(len(l), len(c)) {
		a, b := 0, 0
		if i < len(l) {
			a, _ = strconv.Atoi(l[i])
		}
		if i < len(c) {
			b, _ = strconv.Atoi(c[i])
		}
		if a != b {
			return a > b
		}
	}
	return false
}

func (g *gui) checkUpdate(w http.ResponseWriter, r *http.Request) {
	res := struct {
		Current   string `json:"current"`
		Latest    string `json:"latest"`
		Available bool   `json:"available"`
		Error     string `json:"error,omitempty"`
	}{Current: version}
	if version != "dev" {
		rel, err := latestRelease(r.Context())
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Latest = rel.Tag
			res.Available = newer(rel.Tag, version)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func (g *gui) update(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	running := g.cancel != nil
	g.mu.Unlock()
	if running {
		http.Error(w, "end the party before updating", http.StatusConflict)
		return
	}
	if version == "dev" {
		http.Error(w, "development builds do not update themselves", http.StatusBadRequest)
		return
	}
	tag, err := install(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	io.WriteString(w, tag)
	go func() {
		time.Sleep(time.Second)
		g.quit()
	}()
}

func install(c context.Context) (string, error) {
	rel, err := latestRelease(c)
	if err != nil {
		return "", err
	}
	if !newer(rel.Tag, version) {
		return "", fmt.Errorf("already on the latest version (%s)", version)
	}
	name := assetName(runtime.GOOS, runtime.GOARCH)
	i := 0
	for i < len(rel.Assets) && rel.Assets[i].Name != name {
		i++
	}
	if i == len(rel.Assets) {
		return "", fmt.Errorf("%s has no build for %s/%s", rel.Tag, runtime.GOOS, runtime.GOARCH)
	}
	asset := rel.Assets[i]
	want, ok := strings.CutPrefix(asset.Digest, "sha256:")
	if !ok {
		return "", fmt.Errorf("%s has no sha256 digest to verify against", asset.Name)
	}

	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".watchparty-gui-update-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())

	c, cancel := context.WithTimeout(c, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, asset.URL, nil)
	if err != nil {
		tmp.Close()
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		tmp.Close()
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		tmp.Close()
		return "", fmt.Errorf("download: %s", res.Status)
	}
	sum := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, sum), res.Body)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return "", errors.New("the download does not match its published sha256, not installing it")
	}
	err = os.Chmod(tmp.Name(), 0o755)
	if err != nil {
		return "", err
	}
	err = replaceExe(tmp.Name(), exe)
	if err != nil {
		return "", err
	}
	return rel.Tag, exec.Command(exe).Start()
}
