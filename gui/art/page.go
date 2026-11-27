package art

import (
	_ "embed"
	"fmt"
	"math"
)

// The palette: the theme's colours and the ones its style rules name.
var (
	Lichen   = Hex("#dfe6c8")
	Paper    = Hex("#fbfaf1")
	Forest   = Hex("#23402b")
	Moss     = Hex("#4f7a34")
	MossDeep = Hex("#3d6128")
	Bark     = Hex("#6e4b30")
	Soil     = Hex("#4a3220")
	Porcini  = Hex("#a8642a")
	Ochre    = Hex("#e0b452")
	Berry    = Hex("#9a3a35")
	InkSoft  = Hex("#55644f")
	Rule     = Hex("#c9cfae") // --line: borders and dividers
	White    = Hex("#ffffff")
)

func r(set func(*Style), classes ...string) rule { return rule{classes, set} }

func fill(c string) func(*Style) {
	return func(s *Style) { s.Fill, s.HasFill = Hex(c), true }
}

func stroke(c string, w float64) func(*Style) {
	return func(s *Style) { s.Stroke, s.HasStroke, s.Width = Hex(c), true, w }
}

func all(fs ...func(*Style)) func(*Style) {
	return func(s *Style) {
		for _, f := range fs {
			f(s)
		}
	}
}

func round(s *Style)                  { s.Round = true }
func noFill(s *Style)                 { s.HasFill = false }
func strokeOp(o float64) func(*Style) { return func(s *Style) { s.StrokeOp = o } }
func opacity(o float64) func(*Style)  { return func(s *Style) { s.Opacity = o } }
func useHazel(s *Style)               { s.hazel = true }

const fur, furDark, furLight = "#8b7763", "#3d3229", "#d6c4a7"

// catRules style cat.svg by its classes, CSS-like: later rules win.
var catRules = []rule{
	r(fill(fur), "fur"),
	r(all(stroke(furDark, 1), strokeOp(.35)), "ruff"),
	r(all(stroke(furDark, 3), round, noFill), "stripe"),
	r(fill("#e4b4a0"), "ear"),
	r(all(fill("#ee9f98"), opacity(.75)), "cheek"),
	r(all(fill("#f7f2e8"), stroke(furDark, 1.5)), "paw"),
	r(all(stroke("#b9ad9c", 1.5), round), "toes"),
	r(all(stroke("#23402b", 2), round, noFill), "line"),
	r(all(useHazel, stroke("#4a3428", 1.3)), "eye"),
	r(all(stroke(furDark, 1.2), strokeOp(.45), round), "o"),
	r(all(stroke(furDark, 1.4), strokeOp(.5), round), "seam"),
	r(fill("#ef8f8c"), "tongue"),
	r(all(stroke(fur, 5), round), "head"),
	r(fill("#1f1a14"), "pupil"),
	r(fill(furLight), "light"),
	r(all(stroke(furLight, 1.6), round, noFill), "wisp"),
	r(all(stroke(furDark, 2), round, noFill), "tuft"),
	r(fill("#ffffff"), "glint"),
	r(all(fill("#c98a79"), stroke(furDark, 1)), "nose"),
	r(all(stroke("#fbfaf1", 1.4), round, opacity(.9)), "whisker"),
	r(all(stroke("#3d6128", 3.5), noFill, round), "steth"),
	r(all(fill("#dfe3e6"), stroke("#7d8790", 1.5)), "bell"),
	r(fill("#b9c1c7"), "bell-in"),
}

// groundRules style ground.svg (.ground, .turtle) the same way.
var groundRules = []rule{
	r(fill("#4a3220"), "soil"),
	r(fill("#4f7a34"), "grass"),
	r(all(fill("#f1ead6"), stroke("#6e4b30", 1.5)), "stem"),
	r(all(stroke("#6e4b30", 1), opacity(.5), noFill), "gill"),
	r(all(fill("#a8642a"), stroke("#4a3220", 1.5)), "cap"),
	r(fill("#9a3a35"), "cap", "red"),
	r(fill("#d8c49a"), "cap", "cream"),
	r(fill("#f6efdc"), "spot"),
	r(all(fill("#4f7a34"), stroke("#23402b", 1.5)), "shell"),
	r(all(fill("#6b8f45"), stroke("#23402b", 1.3)), "rim"),
	r(all(stroke("#23402b", 1.3), noFill), "scute"),
	r(all(fill("#9cb36b"), stroke("#23402b", 1.3)), "skin"),
	r(fill("#23402b"), "eye"),
	r(all(fill("#e9a99a"), opacity(.7)), "cheek"),
}

var (
	//go:embed cat.svg
	catSVG string
	//go:embed ground.svg
	groundSVG string
)

// Cat is Fosdo2, viewBox 0 0 200 220.
func Cat() *Scene { return &Scene{root: parse(catSVG), rules: catRules, anim: catAnim} }

// Ground is the strip along the window's bottom, viewBox 0 0 560 130.
func Ground() *Scene { return &Scene{root: parse(groundSVG), rules: groundRules, anim: groundAnim} }

type stop struct{ at, v float64 }

// Bezier is CSS's cubic-bezier() timing function.
func Bezier(x1, y1, x2, y2 float64) func(float64) float64 {
	return func(x float64) float64 {
		t := x
		for range 8 { // Newton's method on x(t) = x
			dx := 3*(1-t)*(1-t)*x1 + 6*(1-t)*t*(x2-x1) + 3*t*t*(1-x2)
			if math.Abs(dx) < 1e-9 {
				break
			}
			t -= (3*(1-t)*(1-t)*t*x1 + 3*(1-t)*t*t*x2 + t*t*t - x) / dx
			t = math.Max(0, math.Min(1, t))
		}
		return 3*(1-t)*(1-t)*t*y1 + 3*(1-t)*t*t*y2 + t*t*t
	}
}

var (
	ease      = Bezier(.25, .1, .25, 1)
	easeInOut = Bezier(.42, 0, .58, 1)
	linear    = func(x float64) float64 { return x }
)

// key is a CSS @keyframes animation's value t seconds in: dur long, starting after delay,
// running back and forth when alternate, timed by fn between each pair of stops.
func key(t, dur, delay float64, alternate bool, fn func(float64) float64, stops ...stop) float64 {
	t -= delay
	if t < 0 {
		return stops[0].v
	}
	n := math.Floor(t / dur)
	p := t/dur - n
	if alternate && int(n)%2 == 1 {
		p = 1 - p
	}
	for i := 1; i < len(stops); i++ {
		if p <= stops[i].at {
			a, b := stops[i-1], stops[i]
			if b.at == a.at {
				return b.v
			}
			return a.v + (b.v-a.v)*fn((p-a.at)/(b.at-a.at))
		}
	}
	return stops[len(stops)-1].v
}

// lickShow is the lick's opacity (@keyframes lick-show): every 16s, the cat licks a paw.
func lickShow(t float64) float64 {
	return key(t, 16, 0, false, ease, stop{0, 0}, stop{.79, 0}, stop{.81, 1}, stop{.97, 1}, stop{.99, 0}, stop{1, 0})
}

func catAnim(e *elem, t float64, m Mood) (Mat, float64, bool) {
	if m.Still {
		return catAtRest(e, m)
	}
	switch {
	case e.has("tail"):
		dur := 3.2
		if !m.Awake {
			dur = 7
		}
		return About(Rotate(key(t, dur, 0, true, easeInOut, stop{0, -5}, stop{1, 7})), 136, 186), 1, true
	case e.has("head-move") && m.Awake:
		deg := key(t, 16, 0, false, easeInOut, stop{0, 0}, stop{.78, 0}, stop{.82, 5}, stop{.96, 5}, stop{.99, 0}, stop{1, 0})
		return About(Rotate(deg), 100, 120), 1, true
	case e.has("eyes-open"):
		if !m.Awake {
			return Identity, 0, false
		}
		// blink: scaleY about the eyes' centre
		s := key(t, 6, 0, false, ease, stop{0, 1}, stop{.93, 1}, stop{.95, .1}, stop{1, 1})
		o := 1 - lickShow(t)
		return About(Scale(1, s), 80, 72), o, o > 0
	case e.has("eyes-closed"):
		if !m.Awake {
			return Identity, 1, true
		}
		o := lickShow(t)
		return Identity, o, o > 0
	case e.has("paw-lick"):
		if !m.Awake {
			return Identity, 0, false
		}
		y := key(t, 16, 0, false, ease, stop{0, 0}, stop{.81, 0}, stop{.835, -3}, stop{.86, 0}, stop{.885, -3}, stop{.91, 0}, stop{.935, -3}, stop{.96, 0}, stop{1, 0})
		o := lickShow(t)
		return Translate(0, y), o, o > 0
	case e.has("lick"): // the tongue
		if !m.Awake {
			return Identity, 0, false
		}
		o := lickShow(t)
		return Identity, o, o > 0
	case e.has("paw-rest") && m.Awake:
		o := 1 - lickShow(t)
		return Identity, o, o > 0
	case e.has("zzz"):
		return Identity, 0, false // drawn as text by the window (Zzz)
	}
	return Identity, 1, true
}

// catAtRest is the cat with reduced motion: eyes open awake and closed asleep, the paw down,
// nothing moving.
func catAtRest(e *elem, m Mood) (Mat, float64, bool) {
	switch {
	case e.has("eyes-open"):
		return Identity, 1, m.Awake
	case e.has("eyes-closed"):
		return Identity, 1, !m.Awake
	case e.has("paw-lick"), e.has("lick"), e.has("zzz"):
		return Identity, 0, false
	}
	return Identity, 1, true
}

// ZzzStill is the z's with reduced motion: where they rest, a little faded.
func ZzzStill() [3]struct{ X, Y, O float64 } {
	var z [3]struct{ X, Y, O float64 }
	for i, p := range [3]Pt{{150, 40}, {162, 26}, {174, 12}} {
		z[i].X, z[i].Y, z[i].O = p.X, p.Y, .8
	}
	return z
}

// Zzz is where the sleeping cat's three z's are (viewBox units, the text's baseline) and
// how opaque, t seconds in (@keyframes snooze).
func Zzz(t float64) [3]struct{ X, Y, O float64 } {
	var z [3]struct{ X, Y, O float64 }
	at := [3]Pt{{150, 40}, {162, 26}, {174, 12}}
	for i := range z {
		tt := t - 1.2*float64(i)
		o := key(tt, 3.6, 0, false, easeInOut, stop{0, 0}, stop{.3, .9}, stop{1, 0})
		p := key(tt, 3.6, 0, false, easeInOut, stop{0, 0}, stop{1, 1})
		z[i].X, z[i].Y, z[i].O = at[i].X+6*p, at[i].Y+6-16*p, o
	}
	return z
}

func groundAnim(e *elem, t float64, m Mood) (Mat, float64, bool) {
	if m.Still {
		// parked mid-ground, legs still
		switch {
		case e.has("turtle") && e.has("baby"):
			return Translate(196, 0), 1, true
		case e.has("turtle"):
			return Translate(230, 0), 1, true
		}
		return Identity, 1, true
	}
	switch {
	case e.has("turtle"):
		return Translate(key(t, 60, 0, false, linear, stop{0, -90}, stop{1, 620}), 0), 1, true
	case e.has("body") && m.Walking:
		return Translate(0, key(t, .3, 0, true, easeInOut, stop{0, 0}, stop{1, -1.5})), 1, true
	case e.has("leg"):
		dur, delay := .7, 0.0
		if m.Walking {
			dur = .3
		}
		if e.has("b") {
			delay = .35
		}
		return Translate(0, key(t, dur, delay, true, easeInOut, stop{0, 0}, stop{1, -2})), 1, true
	}
	return Identity, 1, true
}

// The page's small icons, in their viewBoxes: 24 units, the mushrooms 32.
const (
	Paw     = `<svg viewBox="0 0 24 24"><ellipse cx="12" cy="16" rx="5.5" ry="4.5"/><circle cx="5" cy="10" r="2.3"/><circle cx="9.3" cy="5.5" r="2.3"/><circle cx="14.7" cy="5.5" r="2.3"/><circle cx="19" cy="10" r="2.3"/></svg>`
	Smiley  = `<svg viewBox="0 0 24 24"><path d="M12 2A10 10 0 1 1 12 22A10 10 0 1 1 12 2ZM12 4A8 8 0 1 0 12 20A8 8 0 1 0 12 4Z"/><circle cx="8.5" cy="9.5" r="1.5"/><circle cx="15.5" cy="9.5" r="1.5"/><path d="M7 13.5A5 5 0 0 0 17 13.5L15 13.5A3 3 0 0 1 9 13.5Z"/></svg>`
	Back    = `<svg viewBox="0 0 24 24"><path d="M15 5 8 12l7 7"/></svg>`
	Forward = `<svg viewBox="0 0 24 24"><path d="M9 5l7 7-7 7"/></svg>`
	ECG     = `<svg viewBox="0 0 64 20"><path d="M0 11 H18 L22 4 L27 17 L31 2 L35 11 H64"/></svg>`
	// Mushroom is the title's and the feed's; TrailMushroom's cap is currentColor
	Mushroom      = `<svg viewBox="0 0 32 32"><path d="M12 18h8l1 11h-10z" fill="#f1ead6" stroke="#6e4b30" stroke-width="1.5"/><path d="M3 18C5 5 27 5 29 18z" fill="#a8642a" stroke="#4a3220" stroke-width="1.5"/><circle cx="11" cy="12" r="2" fill="#f6efdc"/><circle cx="19" cy="9.5" r="1.6" fill="#f6efdc"/></svg>`
	TrailMushroom = `<svg viewBox="0 0 32 32"><path d="M12 18h8l1 11h-10z" fill="#f1ead6" stroke="#6e4b30" stroke-width="1.5"/><path d="M3 18C5 5 27 5 29 18z" fill="currentColor" stroke="#4a3220" stroke-width="1.5"/><circle cx="11" cy="12" r="2" fill="#f6efdc"/><circle cx="19" cy="9.5" r="1.6" fill="#f6efdc"/></svg>`
)

// shellColors are the members' shells, in turn.
var shellColors = []string{"#7d9a4a", "#a8642a", "#c8a245", "#9a3a35"}

// Shell is member i's turtle shell (viewBox 0 0 24 24).
func Shell(i int) string {
	return fmt.Sprintf(`<svg viewBox="0 0 24 24"><path d="M2 17 C2 4 22 4 22 17 Z" fill="%s" stroke="#4a3220" stroke-width="1.4"/><path d="M7 16.5 L9 10 H15 L17 16.5 M12 10 V16.5" stroke="#4a3220" stroke-width="1.1" fill="none"/><rect x="1" y="16" width="22" height="3" rx="1.5" fill="#6e4b30"/></svg>`, shellColors[i%len(shellColors)])
}
