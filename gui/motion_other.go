//go:build !windows && !darwin

package main

import (
	"time"

	"github.com/godbus/dbus/v5"
)

// reduceMotion is whether the desktop asks apps not to animate, read through the XDG portal's
// Settings: GNOME's (and GTK's) enable-animations, or KDE's animation speed at 0. No portal,
// or neither key, means no.
func reduceMotion() bool {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return false
	}
	defer conn.Close()
	read := func(namespace, key string) (any, bool) {
		var v dbus.Variant
		call := conn.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop").
			Call("org.freedesktop.portal.Settings.Read", 0, namespace, key)
		if call.Err != nil || call.Store(&v) != nil {
			return nil, false
		}
		// Read wraps the value in a second variant
		if inner, ok := v.Value().(dbus.Variant); ok {
			v = inner
		}
		return v.Value(), true
	}
	done := make(chan bool, 1)
	go func() {
		if v, ok := read("org.gnome.desktop.interface", "enable-animations"); ok {
			if on, ok := v.(bool); ok {
				done <- !on
				return
			}
		}
		if v, ok := read("org.kde.kdeglobals.KDE", "AnimationDurationFactor"); ok {
			if f, ok := v.(float64); ok {
				done <- f == 0
				return
			}
		}
		done <- false
	}()
	// a portal that never answers must not hold the window's start
	select {
	case still := <-done:
		return still
	case <-time.After(3 * time.Second):
		return false
	}
}
