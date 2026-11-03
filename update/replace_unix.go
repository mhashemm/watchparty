//go:build !windows

package update

import "os"

func replaceExe(src, dst string) error {
	return os.Rename(src, dst)
}
