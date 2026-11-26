//go:build !windows && !darwin

package main

import "os/exec"

// systemClock is the time format of the locale (LC_TIME), as `locale t_fmt` prints it.
func systemClock() clock {
	out, err := exec.Command("locale", "t_fmt").Output()
	if err != nil {
		return clock{}
	}
	return clock{hour12: posix12(string(out))}
}
