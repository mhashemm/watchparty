package main

import (
	"encoding/binary"
	"image"
	"testing"
	"time"

	"gioui.org/font/opentype"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
	"gioui.org/widget"
)

// advance reads glyph r's advance in font units, as withAdvance finds it.
func advance(t *testing.T, ttf []byte, r rune) (int, int) {
	t.Helper()
	tables := map[string][]byte{}
	for i := range int(binary.BigEndian.Uint16(ttf[4:])) {
		rec := 12 + 16*i
		off, size := binary.BigEndian.Uint32(ttf[rec+8:]), binary.BigEndian.Uint32(ttf[rec+12:])
		tables[string(ttf[rec:rec+4])] = ttf[off : off+size]
	}
	gid, ok := glyphOf(tables["cmap"], r)
	if !ok {
		t.Fatalf("no glyph for %q", r)
	}
	return int(binary.BigEndian.Uint16(tables["hmtx"][4*int(gid):])), int(binary.BigEndian.Uint16(tables["head"][18:]))
}

func TestEmojiSpaceIsNarrowed(t *testing.T) {
	t.Parallel()
	before, upm := advance(t, notoEmoji, ' ')
	if before != 2550 || upm != 2048 {
		t.Fatalf("Noto's space was %d/%d: did the font change? (narrowSpace)", before, upm)
	}
	narrow := narrowSpace(notoEmoji, .28)
	if got, _ := advance(t, narrow, ' '); got != 573 {
		t.Fatalf("space after narrowing: %d, want 573 (.28 em)", got)
	}
	if got, _ := advance(t, notoEmoji, ' '); got != 2550 {
		t.Fatal("narrowSpace changed the embedded font itself")
	}
	if a, _ := advance(t, narrow, '#'); a != 2550 {
		t.Fatalf("another glyph changed: %d", a)
	}
	// what isn't a font comes back as it was
	if got := narrowSpace([]byte("nope"), .28); string(got) != "nope" {
		t.Fatal("narrowSpace changed a non-font")
	}
}

// TestSpaceAfterEmoji measures it as the window shapes it: a system Noto Color Emoji, if the
// computer has one, must not stand in for the bundled one, whose space is narrowed.
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
	if gap := width("😂 b") - width("😂b"); gap > 6 {
		t.Fatalf("a space after an emoji is %dpx at 16px, want Atkinson's 4.5", gap)
	}
}

func TestFontsAreTheFamiliesNamed(t *testing.T) {
	t.Parallel()
	for ttf, want := range map[*[]byte]string{&atkinsonRegular: "Atkinson Hyperlegible", &atkinsonBold: "Atkinson Hyperlegible", &glutenBold: "Gluten", &notoEmoji: "Noto Color Emoji"} {
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
