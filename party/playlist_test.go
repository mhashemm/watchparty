package party

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestNaturalOrder(t *testing.T) {
	t.Parallel()
	want := []string{"E1.mkv", "e2.mkv", "E02.x.mkv", "E10.mkv", "Show S2 E1.mkv", "Show S10 E1.mkv", "z"}
	got := []string{"z", "Show S10 E1.mkv", "E10.mkv", "E02.x.mkv", "Show S2 E1.mkv", "e2.mkv", "E1.mkv"}
	slices.SortStableFunc(got, naturalCompare)
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestFolderHoldsOnlyItsVideos(t *testing.T) {
	t.Parallel()
	dir := writeFiles(t, map[string]string{"e10.mkv": "c", "e2.MP4": "b", "e1.webm": "a", "e1.srt": "s", "notes.txt": "n"})
	if err := os.Mkdir(filepath.Join(dir, "extras.mkv"), 0o755); err != nil {
		t.Fatal(err)
	}
	files, playlist, _, err := media(dir)
	if err != nil || !playlist {
		t.Fatal(playlist, err)
	}
	names := []string{}
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}
	if want := []string{"e1.webm", "e2.MP4", "e10.mkv"}; !slices.Equal(names, want) {
		t.Fatalf("got %q, want %q", names, want)
	}
}

func TestFolderWithNoVideosIsAnError(t *testing.T) {
	t.Parallel()
	dir := writeFiles(t, map[string]string{"notes.txt": "n"})
	if _, _, _, err := media(dir); err == nil || !strings.Contains(err.Error(), "no videos") {
		t.Fatalf("got %v, want it to say there are no videos", err)
	}
}

// Each player moves on by itself at the end of an episode. If their orders differed,
// each would go to its own next one, and then follow the other's.
func TestFolderIdentityIsItsVideosInOrder(t *testing.T) {
	t.Parallel()
	id := func(files map[string]string) string {
		t.Helper()
		_, _, id, err := media(writeFiles(t, files))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	base := id(map[string]string{"e1.mkv": "a", "e2.mkv": "b"})
	if got := id(map[string]string{"x1.mkv": "a", "x2.mkv": "b", "cover.txt": "x"}); got != base {
		t.Fatal("a renamed copy in the same order should match")
	}
	if got := id(map[string]string{"e1.mkv": "b", "e2.mkv": "a"}); got == base {
		t.Fatal("the same videos in another order should not match")
	}
	if got := id(map[string]string{"e1.mkv": "a"}); got == base {
		t.Fatal("a folder missing an episode should not match")
	}
	if got := id(map[string]string{"e1.mkv": "a", "e2.mkv": "other"}); got == base {
		t.Fatal("a different episode should not match")
	}
}

// One file and a URL behave as they always have, so older parties keep matching.
func TestSingleFileAndURLAreNotPlaylists(t *testing.T) {
	t.Parallel()
	path := filepath.Join(writeFiles(t, map[string]string{"m.mkv": "a"}), "m.mkv")
	for _, target := range []string{path, "https://example.com/m.mkv"} {
		files, playlist, id, err := media(target)
		want, _ := fingerprint(target)
		if err != nil || playlist || !slices.Equal(files, []string{target}) || id != want {
			t.Fatalf("%s: got %q %v %s %v", target, files, playlist, id, err)
		}
	}
}
