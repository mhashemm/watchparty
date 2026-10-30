//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"runtime"
)

const exeSuffix = ""

var browsers = []string{"google-chrome", "chromium", "chromium-browser", "microsoft-edge", "brave-browser"}

func openURL(url string) *exec.Cmd {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", url)
	}
	return exec.Command("xdg-open", url)
}

func pickFile() (string, error) {
	if runtime.GOOS == "darwin" {
		return dialog("osascript", "-e", "POSIX path of (choose file)")
	}
	path, err := dialog("zenity", "--file-selection")
	if errors.Is(err, exec.ErrNotFound) {
		return dialog("kdialog", "--getopenfilename")
	}
	return path, err
}

func hide(_ *exec.Cmd) {}
