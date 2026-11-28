package main

import (
	"image"
	"testing"
	"time"

	"gioui.org/font/opentype"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
	"gioui.org/widget"
)

// TestSpaceAfterEmoji measures it as the window shapes it: a space by a reaction is shaped in
// the emoji font's run, and must be as wide as the text's (Noto Color Emoji's was 1.25 em; a
// font without one gives none).
func TestSpaceAfterEmoji(t *testing.T) {
	t.Parallel()
	sh, err := newShaper()
	if err != nil {
		t.Fatal(err)
	}
	var ops op.Ops
	gtx := layout.Context{Ops: &ops, Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}, Constraints: layout.Constraints{Max: image.Pt(1000, 100)}}
	width := func(s string) int {
		return widget.Label{MaxLines: 1}.Layout(gtx, sh, bodyFont, 16, s, op.CallOp{}).Size.X
	}
	for _, pair := range [][2]string{{"😂 b", "😂b"}, {"😂 😭", "😂😭"}, {"a 😂", "a😂"}} {
		if gap := width(pair[0]) - width(pair[1]); gap < 3 || gap > 6 {
			t.Errorf("%q: a space by an emoji is %dpx at 16px, want Atkinson's 4.5", pair[0], gap)
		}
	}
}

func TestFontsAreTheFamiliesNamed(t *testing.T) {
	t.Parallel()
	for ttf, want := range map[*[]byte]string{&atkinsonRegular: "Atkinson Hyperlegible", &atkinsonBold: "Atkinson Hyperlegible", &glutenBold: "Gluten"} {
		faces, err := opentype.ParseCollection(*ttf)
		if err != nil || len(faces) != 1 || string(faces[0].Font.Typeface) != want {
			t.Errorf("want %q, got %v %v", want, faces, err)
		}
	}
	if _, err := newShaper(); err != nil {
		t.Fatal(err)
	}
}

func TestClock(t *testing.T) {
	t.Parallel()
	for tfmt, want := range map[string]bool{"%r": true, "%I:%M:%S %p": true, "%l:%M %p": true, "%T": false, "%H:%M:%S": false} {
		if posix12(tfmt) != want {
			t.Errorf("posix12(%q) != %v", tfmt, want)
		}
	}
	for shown, want := range map[string]bool{"9:04:05 PM\n": true, "下午9:04:05": true, "21:04:05": false, "21.04.05": false, "": false} {
		if shown12(shown) != want {
			t.Errorf("shown12(%q) != %v", shown, want)
		}
	}
	for pic, want := range map[string]bool{"h:mm tt": true, "HH:mm": false, "H.mm": false, "HH'h'mm": false, "tt h:mm": true} {
		if windows12(pic) != want {
			t.Errorf("windows12(%q) != %v", pic, want)
		}
	}
	at := time.Date(2026, 11, 26, 21, 4, 0, 0, time.UTC)
	if a, b := (clock{hour12: true}).format(at), (clock{}).format(at); a != "9:04 PM" || b != "21:04" {
		t.Fatalf("%q %q", a, b)
	}
}
