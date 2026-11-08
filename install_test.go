//go:build !windows

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// install.sh as a Mac runs it: piped into bash, macOS's sh, as `curl ... | sh` does, with fake
// uname, curl and brew, and Homebrew's prefixes and the system folders moved under a temp root
func TestInstallShOnMacOS(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("needs bash")
	}
	src, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	type setup struct {
		name           string
		brew           string // "" for none, else its exit code
		brewBin, shell []string
	}
	cases := []setup{
		{name: "no brew"},
		{name: "brew fails", brew: "1"},
		{name: "brew works", brew: "0", brewBin: []string{"mpv"}, shell: []string{"yt-dlp"}},
	}
	roots := map[string]string{}
	write := func(path, body string) {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range cases {
		root := t.TempDir()
		roots[c.name] = root
		write(root+"/bin/uname", "#!/bin/sh\ncase $1 in -s) echo Darwin;; -m) echo arm64;; esac\n")
		write(root+"/bin/curl", "#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = -o ] && echo app > \"$2\"; shift; done\n")
		if c.brew != "" {
			write(root+"/opt/homebrew/bin/brew", "#!/bin/sh\necho \"$* $(cat | wc -c | tr -d ' ')\" >> "+root+"/brew.log\nexit "+c.brew+"\n")
		}
		for _, tool := range c.brewBin {
			write(root+"/opt/homebrew/bin/"+tool, "#!/bin/sh\n")
		}
		for _, tool := range c.shell {
			write(root+"/shell/"+tool, "#!/bin/sh\n")
		}
		os.MkdirAll(root+"/usr/local/bin", 0o755)
		os.MkdirAll(root+"/home", 0o755)
	}
	// only now: a parallel test forking while a script is still open for writing hands the
	// child that fd, and exec of the script fails with "text file busy" (golang/go#22315)
	t.Parallel()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root := roots[c.name]
			script := strings.NewReplacer(
				"/opt/homebrew", root+"/opt/homebrew",
				"/usr/local", root+"/usr/local",
				":/usr/bin:/bin:/usr/sbin:/sbin", "",
			).Replace(string(src))
			cmd := exec.Command(bash, "--posix")
			cmd.Stdin = strings.NewReader(script)
			cmd.Env = []string{"HOME=" + root + "/home", "PATH=" + root + "/bin:" + root + "/shell:/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			app := root + "/home/Applications/Watchparty.app"
			_, statErr := os.Stat(app)
			log, _ := os.ReadFile(root + "/brew.log")
			switch c.name {
			case "no brew":
				if err == nil || !bytes.Contains(out, []byte("Nothing was installed")) || statErr == nil {
					t.Errorf("want exit 1, the message and no app; got %v, app %v:\n%s", err, statErr == nil, out)
				}
			case "brew fails":
				if err == nil || !bytes.Contains(out, []byte("brew install mpv failed")) || statErr == nil {
					t.Errorf("want exit 1 at mpv and no app; got %v, app %v:\n%s", err, statErr == nil, out)
				}
			case "brew works":
				if err != nil || statErr != nil {
					t.Fatalf("want the app installed; got %v:\n%s", err, out)
				}
				if got, want := string(log), "install ffmpeg 0\ninstall yt-dlp 0\n"; got != want {
					t.Errorf("brew was asked for %q, want %q: mpv is in brew's bin, yt-dlp only on the terminal's PATH, and brew must read no stdin", got, want)
				}
				plist, _ := os.ReadFile(app + "/Contents/Info.plist")
				if !bytes.Contains(plist, []byte("<string>"+root+"/opt/homebrew/bin:"+root+"/usr/local/bin</string>")) {
					t.Errorf("Info.plist's PATH isn't the one the tools were checked with:\n%s", plist)
				}
			}
		})
	}
}
