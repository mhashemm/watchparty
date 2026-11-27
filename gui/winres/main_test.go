package main

import (
	"bytes"
	"os"
	"testing"
)

// The committed resource is icon.ico as it is now: change one, and go generate ./gui writes
// the other.
func TestTheWindowsIconIsCurrent(t *testing.T) {
	t.Parallel()
	ico, err := os.ReadFile("../icon.ico")
	if err != nil {
		t.Fatal(err)
	}
	want, err := syso(ico)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../rsrc_windows_amd64.syso")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("rsrc_windows_amd64.syso is not icon.ico's (%v): run go generate ./gui", err)
	}
}
