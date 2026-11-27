package main

import (
	"context"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
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

// darkApps is Settings' "Choose your app mode" at Dark: AppsUseLightTheme 0. Missing (before
// Windows 10 1809) means light.
func darkApps() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	return err == nil && v == 0
}

// watchLook calls set with Windows' look now and whenever it changes, until ctx ends: read
// every lookEvery, as the window's messages (WM_SETTINGCHANGE) are Gio's.
func watchLook(ctx context.Context, set func(look)) {
	pollLook(ctx, func() look { return look{still: reduceMotion(), dark: darkApps()} }, set)
}
