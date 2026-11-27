package main

import (
	"os/exec"
	"strings"
)

// reduceMotion is System Settings' Accessibility > Display > Reduce motion.
func reduceMotion() bool {
	out, err := exec.Command("defaults", "read", "com.apple.universalaccess", "reduceMotion").Output()
	return err == nil && strings.TrimSpace(string(out)) == "1"
}
