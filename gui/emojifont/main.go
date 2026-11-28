// Command emojifont writes the window's emoji font: the reactions, Twemoji's art as mpv draws
// them (mpv/emoji.json), as a colour bitmap font. The window then draws those emoji in text
// (chat, names, what is being typed) as the reactions look; others come from the system.
//
//	go run ./emojifont fonts/Twemoji-reactions.ttf
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"sort"

	"golang.org/x/image/vector"

	"github.com/mhashemm/watchparty/gui/art"
	"github.com/mhashemm/watchparty/mpv"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: emojifont out.ttf")
		os.Exit(2)
	}
	ttf, err := build()
	if err == nil {
		err = os.WriteFile(os.Args[1], ttf, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "emojifont:", err)
		os.Exit(1)
	}
}

// The font's one strike and its metrics. Lines keep the height Noto Color Emoji gave them,
// the font this replaced: its ascent and descent. An emoji is a 1.15 em square, about as big
// as Noto's look, standing as high (its middle .34 em above the baseline), with .08 em either
// side.
const (
	upem             = 2048
	ascent, descent  = 1900, 500
	ppem             = 128 // pixels per em: sharp up to 64 dp text on a 2x screen
	size             = 147 // 1.15 em
	side             = 10
	top              = 117 // pixels from the baseline up to the emoji's top
	advance          = size + 2*side
	twemojiBox       = 288 // emoji.json's drawings span 288 units
	glyphBitmapIndex = 1   // glyph 0 is .notdef, which has no bitmap
	// space is the font's space, the last glyph, without a picture. A space by an emoji is
	// shaped in the emoji's run: Atkinson's width, or "😂 no" has none (or Noto's 1.25 em)
	space = 573 // .28 em
)

// Glyph draws one reaction as a size by size picture.
func Glyph(code string) (*image.RGBA, error) {
	svg := mpv.EmojiSVG(code)
	if svg == "" {
		return nil, fmt.Errorf("no reaction %s", code)
	}
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	k := float64(size) / twemojiBox
	for _, it := range art.ParseIcon(svg).Items(art.FillStyle(color.NRGBA{A: 255}), art.Scale(k, k)) {
		if !it.Fill {
			return nil, fmt.Errorf("%s: a stroke, which emoji.json doesn't have", code)
		}
		r := vector.NewRasterizer(size, size)
		pt := func(p art.Pt) (float32, float32) { return float32(p.X + it.At.X), float32(p.Y + it.At.Y) }
		for _, o := range it.Path {
			switch o.K {
			case art.Move:
				r.MoveTo(pt(o.P[0]))
			case art.Line:
				r.LineTo(pt(o.P[0]))
			case art.Quad:
				x1, y1 := pt(o.P[0])
				x2, y2 := pt(o.P[1])
				r.QuadTo(x1, y1, x2, y2)
			case art.Cube:
				x1, y1 := pt(o.P[0])
				x2, y2 := pt(o.P[1])
				x3, y3 := pt(o.P[2])
				r.CubeTo(x1, y1, x2, y2, x3, y3)
			case art.Close:
				r.ClosePath()
			}
		}
		r.Draw(img, img.Bounds(), image.NewUniform(it.Color), image.Point{})
	}
	return img, nil
}

// build is the whole font file.
func build() ([]byte, error) {
	codes := mpv.Reactions()
	if len(codes) == 0 || len(codes) > 255 {
		return nil, errors.New("no reactions, or too many for one byte")
	}
	type entry struct {
		r   rune
		gid uint16
	}
	var cmap []entry
	var pngs [][]byte
	for i, code := range codes {
		var r rune
		if _, err := fmt.Sscanf(code, "%x", &r); err != nil || fmt.Sprintf("%x", r) != code {
			return nil, fmt.Errorf("reaction %q isn't one code point", code)
		}
		img, err := Glyph(code)
		if err != nil {
			return nil, err
		}
		var b bytes.Buffer
		if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&b, img); err != nil {
			return nil, err
		}
		pngs = append(pngs, b.Bytes())
		cmap = append(cmap, entry{r, uint16(i + glyphBitmapIndex)})
	}
	glyphs := len(codes) + 2
	cmap = append(cmap, entry{' ', uint16(glyphs - 1)})
	sort.Slice(cmap, func(i, j int) bool { return cmap[i].r < cmap[j].r })
	unitsAdvance := advance * upem / ppem

	be := binary.BigEndian
	w := func(b *bytes.Buffer, vs ...any) {
		for _, v := range vs {
			binary.Write(b, be, v)
		}
	}
	tables := map[string][]byte{}

	var head bytes.Buffer
	w(&head, uint32(0x00010000), uint32(0x00010000), uint32(0), uint32(0x5F0F3CF5), uint16(0x000B), uint16(upem),
		int64(0), int64(0), int16(0), int16(-descent), int16(unitsAdvance), int16(ascent),
		uint16(0), uint16(8), int16(2), int16(0), int16(0))
	tables["head"] = head.Bytes()

	var hhea bytes.Buffer
	w(&hhea, uint32(0x00010000), int16(ascent), int16(-descent), int16(0), uint16(unitsAdvance), int16(0), int16(0),
		int16(unitsAdvance), int16(1), int16(0), int16(0), int64(0), int16(0), uint16(glyphs))
	tables["hhea"] = hhea.Bytes()

	var maxp bytes.Buffer
	w(&maxp, uint32(0x00005000), uint16(glyphs)) // version 0.5: no outlines
	tables["maxp"] = maxp.Bytes()

	var hmtx bytes.Buffer
	w(&hmtx, uint16(0), int16(0)) // .notdef
	for range codes {
		w(&hmtx, uint16(unitsAdvance), int16(side*upem/ppem))
	}
	w(&hmtx, uint16(space), int16(0))
	tables["hmtx"] = hmtx.Bytes()

	var cm bytes.Buffer // one format 12 subtable, Windows' all-of-Unicode encoding
	w(&cm, uint16(0), uint16(1), uint16(3), uint16(10), uint32(12))
	w(&cm, uint16(12), uint16(0), uint32(16+12*len(cmap)), uint32(0), uint32(len(cmap)))
	for _, e := range cmap {
		w(&cm, uint32(e.r), uint32(e.r), uint32(e.gid))
	}
	tables["cmap"] = cm.Bytes()

	// CBDT: each glyph's small metrics and PNG (format 17)
	var cbdt bytes.Buffer
	w(&cbdt, uint16(3), uint16(0))
	dataStart := uint32(cbdt.Len())
	offsets := []uint32{0}
	for _, p := range pngs {
		w(&cbdt, uint8(size), uint8(size), int8(side), int8(top), uint8(advance), uint32(len(p)))
		cbdt.Write(p)
		offsets = append(offsets, uint32(cbdt.Len())-dataStart)
	}
	tables["CBDT"] = cbdt.Bytes()

	// CBLC: one strike, one index subtable (format 1) over glyphs 1..n
	var sub bytes.Buffer
	w(&sub, uint16(1), uint16(17), dataStart)
	for _, o := range offsets {
		w(&sub, o)
	}
	first, last := uint16(glyphBitmapIndex), uint16(len(codes))
	var cblc bytes.Buffer
	const headerSize, bitmapSize, arraySize = 8, 48, 8
	w(&cblc, uint16(3), uint16(0), uint32(1))
	lineMetrics := func() {
		w(&cblc, int8(top), int8(top-size), uint8(advance), int8(0), int8(0), int8(0), int8(0), int8(0), int8(0), int8(0), int8(0), int8(0))
	}
	w(&cblc, uint32(headerSize+bitmapSize), uint32(arraySize+sub.Len()), uint32(1), uint32(0))
	lineMetrics() // horizontal
	lineMetrics() // vertical, unused
	w(&cblc, first, last, uint8(ppem), uint8(ppem), uint8(32), int8(1))
	w(&cblc, first, last, uint32(arraySize))
	cblc.Write(sub.Bytes())
	tables["CBLC"] = cblc.Bytes()

	return sfnt(tables), nil
}

// sfnt lays tables out as an OpenType file: the directory, then each table 4-byte aligned,
// with their checksums and head's adjustment.
func sfnt(tables map[string][]byte) []byte {
	tags := make([]string, 0, len(tables))
	for t := range tables {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	n := len(tags)
	pow := 1
	for pow*2 <= n {
		pow *= 2
	}
	var out bytes.Buffer
	be := binary.BigEndian
	binary.Write(&out, be, []uint16{0x0001, 0x0000, uint16(n), uint16(pow * 16), uint16(log2(pow)), uint16(n*16 - pow*16)})
	off := 12 + 16*n
	var body bytes.Buffer
	headAt := 0
	for _, t := range tags {
		b := tables[t]
		if t == "head" {
			headAt = off
		}
		out.WriteString(t)
		binary.Write(&out, be, []uint32{checksum(b), uint32(off), uint32(len(b))})
		body.Write(b)
		for body.Len()%4 != 0 {
			body.WriteByte(0)
		}
		off = 12 + 16*n + body.Len()
	}
	out.Write(body.Bytes())
	file := out.Bytes()
	be.PutUint32(file[headAt+8:], 0xB1B0AFBA-checksum(file))
	return file
}

func checksum(b []byte) uint32 {
	var sum uint32
	for i := 0; i < len(b); i += 4 {
		var word [4]byte
		copy(word[:], b[i:])
		sum += binary.BigEndian.Uint32(word[:])
	}
	return sum
}

func log2(n int) int {
	k := 0
	for n > 1 {
		n /= 2
		k++
	}
	return k
}
