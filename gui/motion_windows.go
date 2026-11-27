package main

import (
	"context"
	"unsafe"

	"golang.org/x/sys/windows"
)

// spiGetClientAreaAnimation is SPI_GETCLIENTAREAANIMATION: Settings' "Animation effects"
const spiGetClientAreaAnimation = 0x1042

var systemParametersInfo = windows.NewLazySystemDLL("user32.dll").NewProc("SystemParametersInfoW")

// reduceMotion is Windows' animation effects turned off, which browsers read as
// prefers-reduced-motion too.
func reduceMotion() bool {
	var on int32 // a BOOL: four bytes
	r, _, _ := systemParametersInfo.Call(spiGetClientAreaAnimation, 0, uintptr(unsafe.Pointer(&on)), 0)
	return r != 0 && on == 0
}

// watchMotion calls set with reduceMotion now and whenever it changes, until ctx ends: read
// every motionEvery, as the window's messages (WM_SETTINGCHANGE) are Gio's.
func watchMotion(ctx context.Context, set func(still bool)) {
	pollMotion(ctx, reduceMotion, set)
}
