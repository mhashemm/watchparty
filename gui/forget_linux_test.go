package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Pages forgotten are no longer in memory, and still read back what they held: a binary's
// embedded font, mapped from its file, comes back from the file if it's touched again.
func TestForgottenPagesLeaveMemory(t *testing.T) {
	t.Parallel()
	page := os.Getpagesize()
	want := make([]byte, 64*page)
	for i := range want {
		want[i] = byte(i * 7)
	}
	path := filepath.Join(t.TempDir(), "font")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := unix.Mmap(int(f.Fd()), 0, len(want), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(m)
	b := m[page/2 : len(m)-page/2] // not page aligned, as an embedded font isn't
	resident := func() int { return mappedKB(uintptr(unsafe.Pointer(&m[0]))) * 1024 / page }
	sum := 0
	for i := 0; i < len(b); i += page / 2 { // read it all into memory
		sum += int(b[i])
	}
	runtime.KeepAlive(sum)
	if resident() == 0 {
		t.Fatal("nothing in memory after reading it all")
	}
	forget(b)
	if n := resident(); n > 2 { // the two pages b only partly covers may stay
		t.Fatalf("%d pages still in memory after forget", n)
	}
	for i, v := range b {
		if v != want[page/2+i] {
			t.Fatalf("byte %d reads %d after forget, the file holds %d", i, v, want[page/2+i])
		}
	}
}
