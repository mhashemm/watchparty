package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// spiGetClientAreaAnimation is SPI_GETCLIENTAREAANIMATION: Settings' "Animation effects"
const spiGetClientAreaAnimation = 0x1042

var systemParametersInfo = windows.NewLazySystemDLL("user32.dll").NewProc("SystemParametersInfoW")

// reduceMotion is Windows' animation effects turned off, which browsers read as
// prefers-reduced-motion too.
func reduceMotion() bool {
	var on int32
	r, _, _ := systemParametersInfo.Call(spiGetClientAreaAnimation, 0, uintptr(unsafe.Pointer(&on)), 0)
	return r != 0 && on == 0
}
