//go:build !windows && !darwin

package main

import (
	"context"

	"github.com/godbus/dbus/v5"
)

// The desktop's "reduce motion", through the XDG portal's Settings: GNOME's (and GTK's)
// enable-animations off, or KDE's animation speed at 0. No portal, or neither key, means no.
var motionKeys = [][2]string{{"org.gnome.desktop.interface", "enable-animations"}, {"org.kde.kdeglobals.KDE", "AnimationDurationFactor"}}

// watchMotion calls set with whether the desktop asks apps not to animate, now and whenever
// the portal says it changed, until ctx ends.
func watchMotion(ctx context.Context, set func(still bool)) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		set(false)
		return
	}
	defer conn.Close()
	settings := conn.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop")
	// subscribed before the first read, so a change between the two isn't missed
	signals := make(chan *dbus.Signal, 8)
	conn.Signal(signals)
	err = conn.AddMatchSignal(dbus.WithMatchObjectPath("/org/freedesktop/portal/desktop"),
		dbus.WithMatchInterface("org.freedesktop.portal.Settings"), dbus.WithMatchMember("SettingChanged"))
	values := map[[2]string]any{}
	for _, k := range motionKeys {
		var v dbus.Variant
		if settings.CallWithContext(ctx, "org.freedesktop.portal.Settings.Read", 0, k[0], k[1]).Store(&v) == nil {
			values[k] = unwrap(v)
		}
	}
	last := stillFrom(values)
	set(last)
	if err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case sig := <-signals:
			if sig.Name != "org.freedesktop.portal.Settings.SettingChanged" || len(sig.Body) < 3 {
				continue
			}
			ns, _ := sig.Body[0].(string)
			key, _ := sig.Body[1].(string)
			v, _ := sig.Body[2].(dbus.Variant)
			if !isMotionKey(ns, key) {
				continue
			}
			values[[2]string{ns, key}] = unwrap(v)
			if still := stillFrom(values); still != last {
				last = still
				set(still)
			}
		}
	}
}

func isMotionKey(ns, key string) bool {
	for _, k := range motionKeys {
		if k == [2]string{ns, key} {
			return true
		}
	}
	return false
}

// unwrap is a setting's value: Read wraps it in a second variant, SettingChanged doesn't.
func unwrap(v dbus.Variant) any {
	if inner, ok := v.Value().(dbus.Variant); ok {
		return inner.Value()
	}
	return v.Value()
}

// stillFrom is whether the values ask for reduced motion.
func stillFrom(values map[[2]string]any) bool {
	if on, ok := values[motionKeys[0]].(bool); ok && !on {
		return true
	}
	f, ok := values[motionKeys[1]].(float64)
	return ok && f == 0
}
