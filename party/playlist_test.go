package party

import (
	"os"
	"path/filepath"
	"slices"
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
	want := []string{"Part 2 a.mkv", "Part 10 a.mkv", "V1.mkv", "v2.mkv", "V02.x.mkv", "V10.mkv", "z"}
	got := []string{"z", "Part 10 a.mkv", "V10.mkv", "V02.x.mkv", "Part 2 a.mkv", "v2.mkv", "V1.mkv"}
	slices.SortStableFunc(got, naturalCompare)
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPlaylistHoldsTheFoldersVideosAndStartsOnTheFile(t *testing.T) {
	t.Parallel()
	dir := writeFiles(t, map[string]string{"e10.mkv": "c", "e2.MP4": "b", "e1.webm": "a", "e1.srt": "s", "notes.txt": "n"})
	if err := os.Mkdir(filepath.Join(dir, "extras.mkv"), 0o755); err != nil {
		t.Fatal(err)
	}
	files, start, playlist, _, err := media(filepath.Join(dir, "e2.MP4"), true)
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
	if start != 1 {
		t.Fatalf("starts on %d, want the file that was picked", start)
	}
}

// Each player moves on by itself at the end of an entry. If their orders differed,
// each would go to its own next one, and then follow the other's. Which file each person
// picked doesn't matter: a joiner goes to the party's entry.
func TestPlaylistIdentityIsTheVideosInOrder(t *testing.T) {
	t.Parallel()
	id := func(pick string, files map[string]string) string {
		t.Helper()
		_, _, _, id, err := media(filepath.Join(writeFiles(t, files), pick), true)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	base := id("e1.mkv", map[string]string{"e1.mkv": "a", "e2.mkv": "b"})
	if got := id("e2.mkv", map[string]string{"e1.mkv": "a", "e2.mkv": "b"}); got != base {
		t.Fatal("picking another entry should still match")
	}
	if got := id("x1.mkv", map[string]string{"x1.mkv": "a", "x2.mkv": "b", "cover.txt": "x"}); got != base {
		t.Fatal("a renamed copy in the same order should match")
	}
	if got := id("e1.mkv", map[string]string{"e1.mkv": "b", "e2.mkv": "a"}); got == base {
		t.Fatal("the same videos in another order should not match")
	}
	if got := id("e1.mkv", map[string]string{"e1.mkv": "a"}); got == base {
		t.Fatal("a folder missing an entry should not match")
	}
	if got := id("e1.mkv", map[string]string{"e1.mkv": "a", "e2.mkv": "other"}); got == base {
		t.Fatal("a different entry should not match")
	}
}

// Without the check mark nothing changes, and neither does a URL or an unlisted file.
func TestWhatPlaysAlone(t *testing.T) {
	t.Parallel()
	dir := writeFiles(t, map[string]string{"m.mkv": "a", "n.mkv": "b", "odd.xyz": "c"})
	for target, siblings := range map[string]bool{
		filepath.Join(dir, "m.mkv"):   false,
		"https://example.com/m.mkv":   true,
		filepath.Join(dir, "odd.xyz"): true,
	} {
		files, _, playlist, id, err := media(target, siblings)
		want, _ := fingerprint(target)
		if err != nil || playlist || !slices.Equal(files, []string{target}) || id != want {
			t.Fatalf("%s: got %q %v %s %v", target, files, playlist, id, err)
		}
	}
}
