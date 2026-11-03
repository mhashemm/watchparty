package update

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
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var ReleaseURL = "https://api.github.com/repos/mhashemm/watchparty/releases/latest"

type release struct {
	Tag    string `json:"tag_name"`
	Assets []struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

func fetch(c context.Context) (release, error) {
	c, cancel := context.WithTimeout(c, 15*time.Second)
	defer cancel()
	rel := release{}
	req, err := http.NewRequestWithContext(c, http.MethodGet, ReleaseURL, nil)
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

func Latest(c context.Context) (string, error) {
	rel, err := fetch(c)
	return rel.Tag, err
}

func Newer(latest, current string) bool {
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

func assetName(base, goos, goarch string) string {
	switch goos {
	case "windows":
		if goarch == "amd64" {
			return base + ".exe"
		}
	case "darwin":
		return base + "_mac_" + goarch
	case "linux":
		return base + "_" + goarch
	}
	return ""
}

func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func Install(c context.Context, exe, version, base string) (string, error) {
	rel, err := fetch(c)
	if err != nil {
		return "", err
	}
	if !Newer(rel.Tag, version) {
		return "", fmt.Errorf("already on the latest version (%s)", version)
	}
	name := assetName(base, runtime.GOOS, runtime.GOARCH)
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

	tmp, err := os.CreateTemp(filepath.Dir(exe), ".watchparty-update-*")
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
	return rel.Tag, nil
}
