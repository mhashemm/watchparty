package update

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// busyFor is how long a rename waits out another process holding the file.
const busyFor = 2 * time.Second

func replaceExe(src, dst string) error {
	old := dst + ".old"
	os.Remove(old)
	err := rename(dst, old)
	if err != nil {
		return err
	}
	err = rename(src, dst)
	if err != nil {
		rename(old, dst)
	}
	return err
}

// rename retries while another process holds the file without sharing it: an antivirus
// scanning a file just written or started holds it for a moment.
func rename(from, to string) error {
	deadline := time.Now().Add(busyFor)
	for {
		err := os.Rename(from, to)
		busy := errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
		if !busy || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}
