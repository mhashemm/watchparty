//go:build !windows

package party

import "os"

func sparse(*os.File) {}
