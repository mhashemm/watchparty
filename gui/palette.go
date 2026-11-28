package main

import (
	"image/color"
	"maps"

	"github.com/mhashemm/watchparty/gui/art"
)

// palette turns the light palette's colours into another's, by RGB: every colour the window
// paints passes through shade, so a palette needs no other code. Fosdo2's, the emoji's, and
// any colour it doesn't name stay as they are.
type palette map[[3]uint8]color.NRGBA

// paletteKey is where a frame's palette is, in gtx.Values; none is the light one.
const paletteKey = "palette"

// glowLight is the cream of the background's two upper glows
var glowLight = color.NRGBA{255, 255, 238, 255}

func rgb(c color.NRGBA) [3]uint8 { return [3]uint8{c.R, c.G, c.B} }

// darkPalette is "soil at night", painted while the OS's appearance is dark (DESIGN.md).
// Moss turns lighter and white darker, so the text and ticks on moss turn dark.
var darkPalette = palette{
	rgb(art.Lichen):         art.Hex("#1c1712"),
	rgb(art.Paper):          art.Hex("#28211a"),
	rgb(art.Forest):         art.Hex("#d9cfbc"),
	rgb(art.InkSoft):        art.Hex("#a89a85"),
	rgb(art.Bark):           art.Hex("#d0ab7e"),
	rgb(art.Soil):           art.Hex("#dbbf98"),
	rgb(art.Moss):           art.Hex("#93c06c"),
	rgb(art.MossDeep):       art.Hex("#74a050"),
	rgb(art.Rule):           art.Hex("#45392c"),
	rgb(art.Porcini):        art.Hex("#e09a5c"),
	rgb(art.Berry):          art.Hex("#e5807a"),
	rgb(art.Ochre):          art.Hex("#ecc468"),
	rgb(art.White):          art.Hex("#1f1913"),
	rgb(segOn):              art.Hex("#303a24"),
	rgb(paleBg):             art.Hex("#31281f"),
	rgb(stripBg):            art.Hex("#221c16"),
	rgb(barBg):              art.Hex("#352c22"),
	rgb(trailGrey):          art.Hex("#4a3f33"),
	rgb(art.Hex("#5d8a3f")): art.Hex("#a8d283"), // the buffering bar's stripes
	rgb(art.Hex("#b97638")): art.Hex("#e8a96e"), // the buffered-ahead bar's stripes
	rgb(placeholder):        art.Hex("#958a7c"),
	rgb(checkEdge):          art.Hex("#8a7f71"),
	rgb(badBg):              art.Hex("#3d2421"),
	rgb(selection):          art.Hex("#4f5f33"),
	rgb(art.Hex("#f6e9e4")): art.Hex("#33241f"), // the End button under the mouse
	rgb(glowLight):          art.Hex("#3a2f22"),
}

// painted is gtx in the palette the OS's appearance asks for, with the window's path cache.
func (v *view) painted(gtx C) C {
	if v.values == nil {
		c, icons := newPathCache(), map[string]*art.Icon{}
		v.values = map[string]any{pathsKey: c, iconsKey: icons}
		v.values[drawnKey] = map[string]any{pathsKey: c, iconsKey: icons}
	}
	v.values[pathsKey].(*pathCache).begin()
	if v.dark {
		v.values[paletteKey] = darkPalette
	} else {
		delete(v.values, paletteKey)
	}
	gtx.Values = v.values
	return gtx
}

func palOf(gtx C) palette {
	p, _ := gtx.Values[paletteKey].(palette)
	return p
}

// shade is c in the frame's palette, keeping c's own opacity.
func shade(gtx C, c color.NRGBA) color.NRGBA {
	d, ok := palOf(gtx)[rgb(c)]
	if !ok {
		return c
	}
	d.A = uint8((uint16(d.A)*uint16(c.A) + 127) / 255)
	return d
}

func samePalette(a, b palette) bool { return maps.Equal(a, b) }

func sameRGB(a, b color.NRGBA) bool { return a.R == b.R && a.G == b.G && a.B == b.B }

// shadow is a shadow's or the dialog backdrop's colour: c in the light palette, black at c's
// opacity in any other, since a dark page's shadows can't be lighter than it.
func shadow(gtx C, c color.NRGBA) color.NRGBA {
	if palOf(gtx) == nil {
		return c
	}
	return color.NRGBA{A: c.A}
}

// asDrawn is gtx for a drawing (Fosdo2, the ground, an icon's own colours): none of its
// colours follow the palette.
func asDrawn(gtx C) C {
	if palOf(gtx) == nil {
		return gtx
	}
	if drawn, ok := gtx.Values[drawnKey].(map[string]any); ok {
		gtx.Values = drawn
		return gtx
	}
	vals := maps.Clone(gtx.Values)
	delete(vals, paletteKey)
	gtx.Values = vals
	return gtx
}

// drawnKey is where painted keeps asDrawn's values, the palette left out, in gtx.Values
const drawnKey = "drawn"
