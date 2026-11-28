//go:build !windows && !darwin

package main

import "testing"

// The portal's values as a desktop sends them: color-scheme a uint32, KDE's speed a double.
func TestLookFrom(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		values map[[2]string]any
		want   look
	}{
		{nil, look{}},
		{map[[2]string]any{schemeKey: uint32(1)}, look{dark: true}},
		{map[[2]string]any{schemeKey: uint32(2)}, look{}},
		{map[[2]string]any{schemeKey: uint32(0)}, look{}},
		{map[[2]string]any{animationsKey: false}, look{still: true}},
		{map[[2]string]any{animationsKey: true, kdeSpeedKey: 1.0}, look{}},
		{map[[2]string]any{kdeSpeedKey: 0.0, schemeKey: uint32(1)}, look{still: true, dark: true}},
	} {
		if got := lookFrom(c.values); got != c.want {
			t.Errorf("%v: %+v, want %+v", c.values, got, c.want)
		}
	}
}
