package main

import (
	_ "embed"
	"encoding/binary"
	"errors"

	"gioui.org/font"
	"gioui.org/font/opentype"
	"gioui.org/text"
)

// The window's fonts, shipped in the binary (OFL, licenses beside them in fonts/). Noto
// Color Emoji is fetched at build time (make fonts), not kept in git: it is 10 MB.
var (
	//go:embed fonts/AtkinsonHyperlegible-Regular.ttf
	atkinsonRegular []byte
	//go:embed fonts/AtkinsonHyperlegible-Bold.ttf
	atkinsonBold []byte
	//go:embed fonts/Gluten-Bold.ttf
	glutenBold []byte
	//go:embed fonts/NotoColorEmoji.ttf
	notoEmoji []byte
)

// The two typefaces. Each falls back to the bundled emoji before any system font, or a
// system font's black-and-white smileys would draw an emoji; other scripts still come from
// the system's fonts. The bundled faces go by names of their own (bundled): a system font of
// the same family would otherwise be picked instead, its emoji space as wide as an emoji.
var (
	bodyFont    = font.Font{Typeface: "wp Atkinson, wp Emoji"}
	boldFont    = font.Font{Typeface: "wp Atkinson, wp Emoji", Weight: font.Bold}
	displayFont = font.Font{Typeface: "wp Gluten, wp Emoji", Weight: font.Bold}
)

// bundled is each embedded font and the family it goes by here.
var bundled = []struct {
	ttf    *[]byte
	family font.Typeface
}{{&atkinsonRegular, "wp Atkinson"}, {&atkinsonBold, "wp Atkinson"}, {&glutenBold, "wp Gluten"}, {&notoEmoji, "wp Emoji"}}

// newShaper is the window's text shaper: the bundled fonts, then the system's.
func newShaper() (*text.Shaper, error) {
	var faces []font.FontFace
	for _, b := range bundled {
		ttf := *b.ttf
		if b.ttf == &notoEmoji {
			ttf = narrowSpace(ttf, 0.28)
		}
		f, err := opentype.ParseCollection(ttf)
		if err != nil {
			return nil, err
		}
		for i := range f {
			f[i].Font.Typeface = b.family
		}
		faces = append(faces, f...)
	}
	// the emoji font is in the heap now, parsed; its 10 MB in the binary, read to get there,
	// needn't stay in memory
	forget(notoEmoji)
	return text.NewShaper(text.WithCollection(faces)), nil
}

// narrowSpace makes a font's space em wide. Noto Color Emoji's is 1.25 em, and a space
// right after an emoji is shaped in the emoji's run, so "😂 no" had a gap four spaces
// wide; 0.28 is Atkinson's space. It changes a copy: the font's advance in hmtx.
func narrowSpace(ttf []byte, em float64) []byte {
	out, err := withAdvance(ttf, ' ', em)
	if err != nil {
		return ttf
	}
	return out
}

func withAdvance(ttf []byte, r rune, em float64) ([]byte, error) {
	tables, offsets := map[string][]byte{}, map[string]int{}
	if len(ttf) < 12 {
		return nil, errors.New("not a font")
	}
	n := int(binary.BigEndian.Uint16(ttf[4:]))
	for i := range n {
		rec := 12 + 16*i
		if rec+16 > len(ttf) {
			return nil, errors.New("short table directory")
		}
		off, size := binary.BigEndian.Uint32(ttf[rec+8:]), binary.BigEndian.Uint32(ttf[rec+12:])
		if uint64(off)+uint64(size) > uint64(len(ttf)) {
			return nil, errors.New("table out of bounds")
		}
		tag := string(ttf[rec : rec+4])
		tables[tag], offsets[tag] = ttf[off:off+size], int(off)
	}
	head, hhea, hmtx, cmap := tables["head"], tables["hhea"], tables["hmtx"], tables["cmap"]
	if len(head) < 20 || len(hhea) < 36 || cmap == nil || hmtx == nil {
		return nil, errors.New("missing tables")
	}
	gid, ok := glyphOf(cmap, r)
	if !ok {
		return nil, errors.New("no glyph")
	}
	metrics := int(binary.BigEndian.Uint16(hhea[34:]))
	if int(gid) >= metrics || 4*int(gid)+2 > len(hmtx) {
		return nil, errors.New("glyph has no advance of its own")
	}
	upm := float64(binary.BigEndian.Uint16(head[18:]))
	out := append([]byte(nil), ttf...)
	binary.BigEndian.PutUint16(out[offsets["hmtx"]+4*int(gid):], uint16(em*upm+0.5))
	return out, nil
}

// glyphOf looks r up in a cmap's format 4 (BMP) or format 12 (all of Unicode) subtable.
func glyphOf(cmap []byte, r rune) (uint16, bool) {
	if len(cmap) < 4 {
		return 0, false
	}
	u16 := func(b []byte, at int) int { return int(binary.BigEndian.Uint16(b[at:])) }
	u32 := func(b []byte, at int) int { return int(binary.BigEndian.Uint32(b[at:])) }
	for i := range u16(cmap, 2) {
		rec := 4 + 8*i
		if rec+8 > len(cmap) {
			return 0, false
		}
		off := u32(cmap, rec+4)
		if off+16 > len(cmap) {
			continue
		}
		sub := cmap[off:]
		switch u16(sub, 0) {
		case 12:
			groups := u32(sub, 12)
			if 16+12*groups > len(sub) {
				continue
			}
			for g := range groups {
				at := 16 + 12*g
				start, end := rune(u32(sub, at)), rune(u32(sub, at+4))
				if r >= start && r <= end {
					return uint16(u32(sub, at+8) + int(r-start)), true
				}
			}
		case 4:
			if r > 0xffff {
				continue
			}
			segs := u16(sub, 6) / 2
			ends, starts := 14, 14+2*segs+2
			deltas, ranges := starts+2*segs, starts+4*segs
			if ranges+2*segs > len(sub) {
				continue
			}
			for s := range segs {
				end, start := rune(u16(sub, ends+2*s)), rune(u16(sub, starts+2*s))
				if r < start || r > end {
					continue
				}
				delta, ro := uint16(u16(sub, deltas+2*s)), u16(sub, ranges+2*s)
				if ro == 0 {
					return uint16(r) + delta, true
				}
				at := ranges + 2*s + ro + 2*int(r-start)
				if at+2 > len(sub) {
					return 0, false
				}
				if g := uint16(u16(sub, at)); g != 0 {
					return g + delta, true
				}
				return 0, false
			}
		}
	}
	return 0, false
}
