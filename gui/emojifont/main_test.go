package main

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"testing"

	"gioui.org/font/opentype"
	"github.com/go-text/typesetting/font"

	"github.com/mhashemm/watchparty/mpv"
)

// The committed font is the reactions as emoji.json draws them: change one, and go generate
// ./gui writes the other. Its pictures are compared within 2/255, not byte for byte, as
// another CPU may round the drawing's arithmetic differently.
func TestTheEmojiFontIsCurrent(t *testing.T) {
	t.Parallel()
	ttf, err := os.ReadFile("../fonts/Twemoji-reactions.ttf")
	if err != nil {
		t.Fatal(err)
	}
	faces, err := opentype.ParseCollection(ttf)
	if err != nil || len(faces) != 1 {
		t.Fatalf("the window can't load it: %v", err)
	}
	face := faces[0].Face.Face()
	codes := mpv.Reactions()
	if n := face.Upem(); n != upem {
		t.Fatalf("units per em %d, want %d: go generate ./gui", n, upem)
	}
	for _, code := range codes {
		var r rune
		for _, c := range code {
			r = r*16 + rune(bytes.IndexByte([]byte("0123456789abcdef"), byte(c)))
		}
		gid, ok := face.NominalGlyph(r)
		if !ok {
			t.Fatalf("%s isn't in the font: go generate ./gui", code)
		}
		bm, ok := face.GlyphData(gid).(font.GlyphBitmap)
		if !ok || bm.Format != font.PNG {
			t.Fatalf("%s has no colour picture", code)
		}
		got, err := png.Decode(bytes.NewReader(bm.Data))
		if err != nil {
			t.Fatal(err)
		}
		want, err := Glyph(code)
		if err != nil {
			t.Fatal(err)
		}
		if d := differs(got, want); d > 2 {
			t.Fatalf("%s's picture is off by up to %d/255 from emoji.json's art: go generate ./gui", code, d)
		}
	}
}

// differs is the largest difference of any channel of any pixel, or 255 if the sizes differ.
func differs(a, b image.Image) int {
	if a.Bounds() != b.Bounds() {
		return 255
	}
	worst := 0
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			r1, g1, b1, a1 := a.At(x, y).RGBA()
			r2, g2, b2, a2 := b.At(x, y).RGBA()
			for _, d := range []int{int(r1>>8) - int(r2>>8), int(g1>>8) - int(g2>>8), int(b1>>8) - int(b2>>8), int(a1>>8) - int(a2>>8)} {
				worst = max(worst, d, -d)
			}
		}
	}
	return worst
}
