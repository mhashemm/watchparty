package update

import "os"

func replaceExe(src, dst string) error {
	old := dst + ".old"
	os.Remove(old)
	err := os.Rename(dst, old)
	if err != nil {
		return err
	}
	err = os.Rename(src, dst)
	if err != nil {
		os.Rename(old, dst)
	}
	return err
}
