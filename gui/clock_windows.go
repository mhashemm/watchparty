package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// systemClock is the user's short time format (LOCALE_SSHORTTIME), as Windows shows it.
func systemClock() clock {
	const localeShortTime = 0x79
	buf := make([]uint16, 80)
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetLocaleInfoEx")
	n, _, _ := proc.Call(0, localeShortTime, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return clock{}
	}
	return clock{hour12: windows12(windows.UTF16ToString(buf[:n]))}
}
