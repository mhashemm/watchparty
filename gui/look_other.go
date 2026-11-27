//go:build !windows && !darwin

package main

import (
	"context"
	"slices"

	"github.com/godbus/dbus/v5"
)

// The desktop's look, through the XDG portal's Settings. Reduced motion: GNOME's (and GTK's)
// enable-animations off, or KDE's animation speed at 0. Dark: the portal's own color-scheme,
// 1 (prefer dark), which GNOME, KDE and COSMIC all set. No portal, or no key, means neither.
var (
	animationsKey = [2]string{"org.gnome.desktop.interface", "enable-animations"}
	kdeSpeedKey   = [2]string{"org.kde.kdeglobals.KDE", "AnimationDurationFactor"}
	schemeKey     = [2]string{"org.freedesktop.appearance", "color-scheme"}
	lookKeys      = [][2]string{animationsKey, kdeSpeedKey, schemeKey}
)

// watchLook calls set with the desktop's look, now and whenever the portal says it changed,
// until ctx ends.
func watchLook(ctx context.Context, set func(look)) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		set(look{})
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
	for _, k := range lookKeys {
		var v dbus.Variant
		if settings.CallWithContext(ctx, "org.freedesktop.portal.Settings.Read", 0, k[0], k[1]).Store(&v) == nil {
			values[k] = unwrap(v)
		}
	}
	last := lookFrom(values)
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
			k := [2]string{ns, key}
			if !slices.Contains(lookKeys, k) {
				continue
			}
			values[k] = unwrap(v)
			if l := lookFrom(values); l != last {
				last = l
				set(l)
			}
		}
	}
}

// unwrap is a setting's value: Read wraps it in a second variant, SettingChanged doesn't.
func unwrap(v dbus.Variant) any {
	if inner, ok := v.Value().(dbus.Variant); ok {
		return inner.Value()
	}
	return v.Value()
}

// number is a numeric setting's value, whatever D-Bus type it came as.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case uint32:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// lookFrom is the look the values ask for.
func lookFrom(values map[[2]string]any) look {
	var l look
	if on, ok := values[animationsKey].(bool); ok && !on {
		l.still = true
	}
	if f, ok := number(values[kdeSpeedKey]); ok && f == 0 {
		l.still = true
	}
	if s, ok := number(values[schemeKey]); ok && s == 1 {
		l.dark = true
	}
	return l
}
