//go:build e2e

package main

import "unsafe"

// residentMB is how much of the memory mapping holding b is in this process's memory: the
// Rss of its line in /proc/self/smaps.
func residentMB(b []byte) float64 {
	kb := mappedKB(uintptr(unsafe.Pointer(unsafe.SliceData(b))))
	return float64(kb) / 1e3
}
