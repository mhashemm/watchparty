package main

import "os/exec"

// systemClock is how macOS writes the time now, which follows the user's settings
// (AppleICUForce24HourTime and the region's format).
func systemClock() clock {
	out, err := exec.Command("osascript", "-e", "time string of (current date)").Output()
	if err != nil {
		return clock{}
	}
	return clock{hour12: shown12(string(out))}
}
