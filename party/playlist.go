package party

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// VideoExts is what counts as a video, in a folder and in the GUI's pickers.
var VideoExts = []string{".mkv", ".mp4", ".m4v", ".webm", ".avi", ".mov", ".wmv", ".flv", ".mpg", ".mpeg", ".ts", ".m2ts", ".mts", ".vob", ".ogv", ".3gp", ".rmvb"}

// media resolves -file: one file or URL, or a folder played as a playlist. A folder's
// identity is its videos in order, so everyone's next episode is the same one.
func media(path string) (files []string, playlist bool, id string, err error) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		id, err = fingerprint(path)
		return []string{path}, false, id, err
	}
	files, err = videos(path)
	if err != nil {
		return nil, false, "", err
	}
	h := sha256.New()
	for _, f := range files {
		sum, err := fingerprint(f)
		if err != nil {
			return nil, false, "", err
		}
		h.Write([]byte(sum))
	}
	return files, true, hex.EncodeToString(h.Sum(nil)), nil
}

func videos(dir string) ([]string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := []string{}
	for _, e := range entries {
		if !e.IsDir() && slices.Contains(VideoExts, strings.ToLower(filepath.Ext(e.Name()))) {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no videos in %s", dir)
	}
	slices.SortStableFunc(files, naturalCompare)
	return files, nil
}

// naturalCompare orders runs of digits by value, so episode 2 comes before episode 10.
func naturalCompare(a, b string) int {
	a, b = strings.ToLower(a), strings.ToLower(b)
	for a != "" && b != "" {
		x, y := leadingDigits(a), leadingDigits(b)
		if x == "" || y == "" {
			if a[0] != b[0] {
				return cmp.Compare(a[0], b[0])
			}
			a, b = a[1:], b[1:]
			continue
		}
		a, b = a[len(x):], b[len(y):]
		x, y = strings.TrimLeft(x, "0"), strings.TrimLeft(y, "0")
		if c := cmp.Or(cmp.Compare(len(x), len(y)), cmp.Compare(x, y)); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}

func leadingDigits(s string) string {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return s[:n]
}
