// Command fakebrowser stands in for chrome.exe in e2e_windows.py, which puts it where
// the GUI looks for Chrome. It logs its arguments, one launch per line, then runs the
// real browser headless with a debugging port. With no E2E_BROWSER it exits at once,
// like a browser handing the page to one already running.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func main() {
	f, err := os.OpenFile(os.Getenv("E2E_BROWSER_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		fmt.Fprintln(f, strings.Join(os.Args[1:], " "))
		f.Close()
	}
	real := os.Getenv("E2E_BROWSER")
	if real == "" {
		return
	}
	cmd := exec.Command(real, append([]string{"--headless=new", "--remote-debugging-port=0"}, os.Args[1:]...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
		os.Exit(exit.ExitCode())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
