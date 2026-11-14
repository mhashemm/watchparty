package update

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Another process, an antivirus scanning a file just written or started, can hold the
// running .exe for a moment without sharing it: the swap waits it out.
func TestReplaceExeWaitsOutAFileHeldForAMoment(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "new.exe"), filepath.Join(dir, "app.exe")
	if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		t.Fatal(err)
	}
	// shared for reading only, so it can't be renamed while open
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		windows.CloseHandle(h)
	}()
	if err := replaceExe(src, dst); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{dst: "new", dst + ".old": "old"} {
		if b, err := os.ReadFile(path); err != nil || string(b) != want {
			t.Fatalf("%s holds %q (%v), want %q", path, b, err, want)
		}
	}
}
