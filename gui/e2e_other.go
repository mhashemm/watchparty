//go:build e2e && !linux

package main

// residentMB is read only on Linux.
func residentMB([]byte) float64 { return -1 }
