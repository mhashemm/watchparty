package main

import (
	"encoding/binary"
	"flag"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gioui.org/font/opentype"
	"gioui.org/gpu/headless"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"

	"github.com/mhashemm/watchparty/gui/art"
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

var renderTo = flag.String("render", "", "draw the window's screens into this folder as PNGs at 560×940 (gui/DESIGN.md)")

// TestRender draws each screen without a window, for looking at (gui/DESIGN.md). It needs a
// GPU or Mesa's software one, and runs only when asked: go test ./gui -run TestRender -render <dir>.
func TestRender(t *testing.T) {
	t.Parallel()
	if *renderTo == "" {
		t.Skip("draws screens only with -render <dir>")
	}
	th, err := newShaper()
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range renders {
		img, err := renderScreen(&theme{shaper: th, clock: clock{}}, s)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		f, err := os.Create(filepath.Join(*renderTo, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		png.Encode(f, img)
		f.Close()
	}
}

// aRender is a screen to draw: how to set up the view, and how far into its animations
// (27s puts the turtles where the page's reduced motion keeps them).
type aRender struct {
	at    time.Duration
	mood  art.Mood
	setup func(v *view)
}

var renders = map[string]aRender{
	"frame": {at: 27 * time.Second, setup: func(v *view) {
		v.card = func(gtx C) D {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(v.title),
				layout.Rigid(func(gtx C) D {
					return v.lede(gtx, "Pick a video and start the party. When anyone pauses or seeks, everyone's player follows.")
				}),
			)
		}
	}},
}

func renderScreen(th *theme, s aRender) (*image.RGBA, error) {
	size := image.Pt(560, 940)
	w, err := headless.NewWindow(size.X, size.Y)
	if err != nil {
		return nil, err
	}
	defer w.Release()
	start := time.Date(2026, 11, 26, 21, 4, 0, 0, time.Local)
	v := newView(th, start)
	v.mood = s.mood
	if s.setup != nil {
		s.setup(v)
	}
	var ops op.Ops
	// two frames: the first one lays out what the second one scrolls and measures
	for range 2 {
		ops.Reset()
		gtx := layout.Context{Ops: &ops, Now: start.Add(s.at), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}, Constraints: layout.Exact(size)}
		v.frame(gtx)
	}
	if err := w.Frame(&ops); err != nil {
		return nil, err
	}
	img := image.NewRGBA(image.Rectangle{Max: size})
	return img, w.Screenshot(img)
}
