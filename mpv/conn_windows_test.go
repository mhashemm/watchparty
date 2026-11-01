package mpv

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// mpv.com is a console wrapper that starts mpv.exe. Killing it at the end of a party
// leaves mpv.exe playing and holding mpv's output pipe, so Run never returns.
func TestInitRunsMpvExeInsteadOfTheComWrapper(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	com := filepath.Join(dir, "mpv.com")
	os.WriteFile(com, nil, 0o755)

	cmd := exec.Command(com)
	Init(cmd)
	if cmd.Path != com {
		t.Fatalf("no mpv.exe next to it, want %s kept, got %s", com, cmd.Path)
	}

	exe := filepath.Join(dir, "mpv.exe")
	os.WriteFile(exe, nil, 0o755)
	cmd = exec.Command(com)
	Init(cmd)
	if cmd.Path != exe {
		t.Fatalf("want %s, got %s", exe, cmd.Path)
	}
}
