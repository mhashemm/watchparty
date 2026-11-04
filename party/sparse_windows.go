package party

import (
	"os"

	"golang.org/x/sys/windows"
)

// sparse keeps Windows from writing zeros up to wherever mpv seeks, like a video's index at
// the end of the file. A file system without sparse files just uses the space.
func sparse(f *os.File) {
	var returned uint32
	windows.DeviceIoControl(windows.Handle(f.Fd()), windows.FSCTL_SET_SPARSE, nil, 0, nil, 0, &returned, nil)
}
