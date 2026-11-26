package main

import (
	"image"
	"image/color"

	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/x/stroke"

	"github.com/mhashemm/watchparty/gui/art"
)

// The window's look is app.css's: these blocks are its rules, named after them.

type (
	C = layout.Context
	D = layout.Dimensions
)

// Colors app.css names inline, beside art's palette.
var (
	segOn       = art.Hex("#eef3df") // a chosen option, the open feed group
	hoverBg     = art.Hex("#f3eedf") // a button under the mouse
	placeholder = art.Hex("#757575") // Chromium's placeholder grey
	checkEdge   = art.Hex("#767676") // Chromium's unticked checkbox
	badBg       = art.Hex("#f6e4e0") // .msg.bad
	selection   = art.Hex("#b6d48f")
)

type theme struct {
	shaper *text.Shaper
	clock  clock
	// glyphs caches each style's glyph height (ascent and descent) in pixels, for leading
	glyphs map[glyphKey]int
}

type glyphKey struct {
	font font.Font
	size int
}

// textStyle is a CSS font and color: lineHeight is CSS's line-height, a multiple of size.
type textStyle struct {
	font       font.Font
	size       unit.Sp
	color      color.NRGBA
	align      text.Alignment
	lineHeight float32
	maxLines   int
}

var (
	tBody    = textStyle{font: bodyFont, size: 16, color: art.Forest, lineHeight: 1.5}
	tBold    = textStyle{font: boldFont, size: 16, color: art.Forest, lineHeight: 1.5}
	tHint    = textStyle{font: bodyFont, size: 14, color: art.InkSoft, lineHeight: 1.5}
	tSmall   = textStyle{font: bodyFont, size: 13.3, color: art.InkSoft, lineHeight: 1.5}
	tTitle   = textStyle{font: displayFont, size: 33.6, color: art.Forest, align: text.Middle, lineHeight: 1.1}
	tHeading = textStyle{font: displayFont, size: 20, color: art.Bark, lineHeight: 1.2}
	tLede    = textStyle{font: bodyFont, size: 16, color: art.InkSoft, align: text.Middle, lineHeight: 1.5}
)

func (s textStyle) with(c color.NRGBA) textStyle { s.color = c; return s }

// colorOp is a recorded paint.ColorOp, the material text widgets take.
func colorOp(gtx C, c color.NRGBA) op.CallOp {
	m := op.Record(gtx.Ops)
	paint.ColorOp{Color: c}.Add(gtx.Ops)
	return m.Stop()
}

// text lays out s in st, wrapping at the constraint's width. Each line gets CSS's line box:
// Gio's lines are line-height apart, but its first and last stop at the glyphs, so half
// the leading goes above and below as CSS puts it.
func (th *theme) text(gtx C, st textStyle, s string) D {
	// Gio scales LineHeight by 1.2 unless the scale is set: CSS's line-height is the whole of it
	l := widget.Label{Alignment: st.align, MaxLines: st.maxLines, LineHeightScale: 1}
	if st.lineHeight > 0 {
		l.LineHeight = st.size * unit.Sp(st.lineHeight)
	}
	if st.align == text.Middle {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
	} else {
		gtx.Constraints.Min.X = 0
	}
	half := 0
	if st.lineHeight > 0 {
		half = (gtx.Sp(l.LineHeight) - th.glyphHeight(gtx, st)) / 2
	}
	m := op.Record(gtx.Ops)
	d := l.Layout(gtx, th.shaper, st.font, st.size, s, colorOp(gtx, st.color))
	call := m.Stop()
	o := op.Offset(image.Pt(0, half)).Push(gtx.Ops)
	call.Add(gtx.Ops)
	o.Pop()
	d.Size.Y += 2 * half
	d.Baseline += half
	return d
}

// glyphHeight is a style's one-line height in Gio: the font's ascent plus descent.
func (th *theme) glyphHeight(gtx C, st textStyle) int {
	k := glyphKey{st.font, gtx.Sp(st.size)}
	if h, ok := th.glyphs[k]; ok {
		return h
	}
	if th.glyphs == nil {
		th.glyphs = map[glyphKey]int{}
	}
	m := op.Record(gtx.Ops)
	g := gtx
	g.Constraints = layout.Constraints{Max: image.Pt(1<<20, 1<<20)}
	d := widget.Label{MaxLines: 1}.Layout(g, th.shaper, st.font, st.size, "Hg", op.CallOp{})
	m.Stop()
	th.glyphs[k] = d.Size.Y
	return d.Size.Y
}

// selectable is text that can be selected and copied, like the page's code and status.
func (th *theme) selectable(gtx C, sel *widget.Selectable, st textStyle, s string) D {
	sel.Alignment = st.align
	sel.LineHeight, sel.LineHeightScale = st.size*unit.Sp(st.lineHeight), 1
	if sel.Text() != s {
		sel.SetText(s)
	}
	gtx.Constraints.Min.X = 0
	return sel.Layout(gtx, th.shaper, st.font, st.size, colorOp(gtx, st.color), colorOp(gtx, selection))
}

// rrect fills a rounded rectangle.
func rrect(gtx C, r image.Rectangle, radius int, c color.NRGBA) {
	radius = min(radius, r.Dx()/2, r.Dy()/2)
	paint.FillShape(gtx.Ops, c, clip.UniformRRect(r, max(radius, 0)).Op(gtx.Ops))
}

// boxStyle is a bordered, rounded block: a card, a field, a button.
type boxStyle struct {
	bg, border color.NRGBA
	borderW    unit.Dp
	radius     unit.Dp
	// shadow is a hard drop shadow (box-shadow: 0 <shadow> 0 <shadowColor>)
	shadow      unit.Dp
	shadowColor color.NRGBA
	inset       layout.Inset
	// wide fills the width it is given
	wide bool
	// ring is a focus outline: 3dp of ochre 2dp outside the border (:focus-visible)
	ring bool
}

func (th *theme) box(gtx C, s boxStyle, w layout.Widget) D {
	if s.wide {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
	}
	m := op.Record(gtx.Ops)
	d := s.inset.Layout(gtx, func(gtx C) D {
		if s.wide {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
		}
		return w(gtx)
	})
	content := m.Stop()
	size := d.Size
	r, bw, sh := gtx.Dp(s.radius), gtx.Dp(s.borderW), gtx.Dp(s.shadow)
	if s.ring {
		ring(gtx, image.Rectangle{Max: size}, r)
	}
	if sh > 0 {
		rrect(gtx, image.Rect(0, sh, size.X, size.Y+sh), r, s.shadowColor)
	}
	if bw > 0 {
		rrect(gtx, image.Rectangle{Max: size}, r, s.border)
	}
	rrect(gtx, image.Rect(bw, bw, size.X-bw, size.Y-bw), r-bw, s.bg)
	content.Add(gtx.Ops)
	return D{Size: image.Pt(size.X, size.Y+sh), Baseline: d.Baseline}
}

// buttonStyle is a <button>: its box, its text, and the box's background under the mouse.
type buttonStyle struct {
	box   boxStyle
	text  textStyle
	hover color.NRGBA
	// icon is an art SVG drawn in the text's color before the label
	icon     string
	iconSize unit.Dp
	iconGap  unit.Dp
	center   bool
}

var (
	bPlain   = buttonStyle{box: boxStyle{bg: art.White, border: art.Bark, borderW: 2, radius: 12, inset: layout.Inset{Top: 10, Bottom: 10, Left: 14, Right: 14}}, text: tBold.with(art.Bark), hover: hoverBg}
	bPrimary = buttonStyle{box: boxStyle{bg: art.Moss, border: art.MossDeep, borderW: 2, radius: 12, inset: layout.Inset{Top: 10, Bottom: 10, Left: 14, Right: 14}}, text: tBold.with(art.White), hover: art.MossDeep}
	bSmall   = buttonStyle{box: boxStyle{bg: art.White, border: art.Bark, borderW: 2, radius: 10, inset: layout.Inset{Top: 4, Bottom: 4, Left: 10, Right: 10}}, text: textStyle{font: boldFont, size: 14, color: art.Bark, lineHeight: 1.5}, hover: hoverBg}
	bTab     = buttonStyle{box: boxStyle{bg: art.White, border: art.Rule, borderW: 2, radius: 999, inset: layout.Inset{Top: 6, Bottom: 6, Left: 16, Right: 16}}, text: textStyle{font: boldFont, size: 14.4, color: art.Bark, lineHeight: 1.5}, hover: hoverBg}
	bTabOn   = buttonStyle{box: boxStyle{bg: segOn, border: art.Moss, borderW: 2, radius: 999, inset: layout.Inset{Top: 6, Bottom: 6, Left: 16, Right: 16}}, text: textStyle{font: boldFont, size: 14.4, color: art.Forest, lineHeight: 1.5}, hover: segOn}
	bGo      = buttonStyle{box: boxStyle{bg: art.Moss, border: art.MossDeep, borderW: 2, radius: 12, shadow: 4, shadowColor: art.MossDeep, inset: layout.UniformInset(14), wide: true}, text: textStyle{font: displayFont, size: 20.8, color: art.White, lineHeight: 1.2}, hover: art.MossDeep, icon: art.Paw, iconSize: 22, iconGap: 10, center: true}
	bStop    = buttonStyle{box: boxStyle{bg: art.Paper, border: art.Berry, borderW: 2, radius: 12, shadow: 4, shadowColor: art.Berry, inset: layout.UniformInset(14), wide: true}, text: textStyle{font: displayFont, size: 20.8, color: art.Berry, lineHeight: 1.2}, hover: art.Hex("#f6e9e4"), icon: art.Paw, iconSize: 22, iconGap: 10, center: true}
)

// button is a clickable box; disabled ones are faded and take no clicks.
func (th *theme) button(gtx C, c *widget.Clickable, s buttonStyle, disabled bool, label string) D {
	if disabled {
		gtx = gtx.Disabled()
		defer paint.PushOpacity(gtx.Ops, .55).Pop()
	} else if c.Hovered() {
		s.box.bg = s.hover
	}
	s.box.ring = gtx.Focused(c) && !disabled
	return c.Layout(gtx, func(gtx C) D {
		if !disabled {
			pointer.CursorPointer.Add(gtx.Ops)
		}
		return th.box(gtx, s.box, func(gtx C) D {
			row := func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						if s.icon == "" {
							return D{}
						}
						return iconWith(gtx, s.icon, art.FillStyle(s.text.color), s.iconSize, s.iconGap)
					}),
					layout.Rigid(func(gtx C) D { return th.text(gtx, s.text, label) }),
				)
			}
			if s.center {
				return layout.Center.Layout(gtx, row)
			}
			return row(gtx)
		})
	})
}

// icon draws an art SVG size dp square.
func icon(gtx C, svg string, st art.Style, size unit.Dp) D {
	return iconWith(gtx, svg, st, size, 0)
}

// iconWith is icon followed by gap dp of space; the SVG's viewBox is 24 units, a mushroom's 32.
func iconWith(gtx C, svg string, st art.Style, size, gap unit.Dp) D {
	px := float64(gtx.Dp(size))
	box := 24.0
	if svg == art.Mushroom || svg == art.TrailMushroom {
		box = 32
	}
	paintArt(gtx.Ops, art.Icon(svg, st, art.Scale(px/box, px/box)))
	return D{Size: image.Pt(gtx.Dp(size+gap), gtx.Dp(size))}
}

// input is a text field: white, a 2dp border that turns moss with an ochre ring when focused,
// its hint in grey while empty.
func (th *theme) input(gtx C, e *widget.Editor, hint string, radius unit.Dp, minLines int, disabled bool) D {
	border := art.Rule
	focused := gtx.Focused(e) && !disabled
	if focused {
		border = art.Moss
	}
	if disabled {
		gtx = gtx.Disabled()
		defer paint.PushOpacity(gtx.Ops, .55).Pop()
	}
	pad := layout.Inset{Top: 10, Bottom: 10, Left: 12, Right: 12}
	if radius >= 999 {
		pad.Left = 16
	}
	return th.box(gtx, boxStyle{bg: art.White, border: border, borderW: 2, radius: radius, inset: pad, wide: true, ring: focused}, func(gtx C) D {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		if minLines > 1 {
			gtx.Constraints.Min.Y = gtx.Sp(unit.Sp(24 * minLines))
		}
		ed := func(gtx C) D {
			e.LineHeight, e.LineHeightScale = 24, 1
			return e.Layout(gtx, th.shaper, bodyFont, 16, colorOp(gtx, art.Forest), colorOp(gtx, selection))
		}
		if e.Len() > 0 {
			return ed(gtx)
		}
		return layout.Stack{}.Layout(gtx,
			layout.Stacked(func(gtx C) D { return th.text(gtx, tBody.with(placeholder), hint) }),
			layout.Expanded(ed),
		)
	})
}

// check is a checkbox and its label (.check: 18dp, accent-color moss).
func (th *theme) check(gtx C, b *widget.Bool, label string) D {
	b.Update(gtx)
	return b.Layout(gtx, func(gtx C) D {
		pointer.CursorPointer.Add(gtx.Ops)
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				sz := gtx.Dp(18)
				r := image.Rect(0, 0, sz, sz)
				if b.Value {
					rrect(gtx, r, gtx.Dp(3), art.Moss)
					px := float64(sz) / 18
					tick := art.Path{
						{K: art.Move, P: [3]art.Pt{{X: 4 * px, Y: 9.5 * px}}},
						{K: art.Line, P: [3]art.Pt{{X: 7.5 * px, Y: 13 * px}}},
						{K: art.Line, P: [3]art.Pt{{X: 14 * px, Y: 5.5 * px}}},
					}
					paintArt(gtx.Ops, []art.Item{{Path: tick, Color: art.White, Width: 2.2 * px, Round: true}})
				} else {
					rrect(gtx, r, gtx.Dp(3), checkEdge)
					rrect(gtx, r.Inset(max(1, gtx.Dp(1))), gtx.Dp(2), art.White)
				}
				return D{Size: r.Max}
			}),
			layout.Rigid(layout.Spacer{Width: 8}.Layout),
			layout.Rigid(func(gtx C) D { return th.text(gtx, tBody, label) }),
		)
	})
}

// ring draws the ochre focus outline around r: 3dp wide, 2dp outside it (outline-offset).
func ring(gtx C, r image.Rectangle, radius int) {
	o, w := gtx.Dp(2), gtx.Dp(3)
	mid := r.Inset(-o - w/2)
	radius = min(radius+o+w/2, mid.Dx()/2, mid.Dy()/2)
	p := clip.UniformRRect(mid, radius).Path(gtx.Ops)
	paint.FillShape(gtx.Ops, art.Ochre, clip.Stroke{Path: p, Width: float32(w)}.Op())
}

// option is one card of a .seg choice: a bold title over a small grey line, moss when chosen.
func (th *theme) option(gtx C, e *widget.Enum, key, title, sub string) D {
	return e.Layout(gtx, key, func(gtx C) D {
		pointer.CursorPointer.Add(gtx.Ops)
		on := e.Value == key
		bg, border := art.White, art.Rule
		if on {
			bg, border = segOn, art.Moss
		}
		focused, _ := e.Focused()
		return th.box(gtx, boxStyle{bg: bg, border: border, borderW: 2, radius: 12, inset: layout.Inset{Top: 10, Bottom: 10, Left: 12, Right: 12}, wide: true, ring: focused == key}, func(gtx C) D {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx C) D { return th.text(gtx, tBold, title) }),
				layout.Rigid(func(gtx C) D { return th.text(gtx, tSmall, sub) }),
			)
		})
	})
}

// options lays out a .seg: its cards side by side, 8dp apart, all as tall as the tallest.
func (th *theme) options(gtx C, e *widget.Enum, opts ...[3]string) D {
	e.Update(gtx)
	var kids []layout.FlexChild
	for i, o := range opts {
		if i > 0 {
			kids = append(kids, layout.Rigid(layout.Spacer{Width: 8}.Layout))
		}
		kids = append(kids, layout.Flexed(1, func(gtx C) D { return th.option(gtx, e, o[0], o[1], o[2]) }))
	}
	return layout.Flex{}.Layout(gtx, kids...)
}

// dashed is a horizontal dashed rule, 2dp of the line color (details' border-top).
func dashed(gtx C, width int) D {
	h := float32(gtx.Dp(2))
	s := stroke.Stroke{
		Path:   stroke.Path{Segments: []stroke.Segment{stroke.MoveTo(f32.Pt(0, h/2)), stroke.LineTo(f32.Pt(float32(width), h/2))}},
		Width:  h,
		Cap:    stroke.FlatCap,
		Dashes: stroke.Dashes{Dashes: []float32{3 * h, 3 * h}},
	}
	paint.FillShape(gtx.Ops, art.Rule, s.Op(gtx.Ops))
	return D{Size: image.Pt(width, int(h))}
}

// rule is a 1dp solid line across the width (a list's divider).
func rule(gtx C) D {
	w, h := gtx.Constraints.Max.X, max(1, gtx.Dp(1))
	paint.FillShape(gtx.Ops, art.Rule, clip.Rect{Max: image.Pt(w, h)}.Op())
	return D{Size: image.Pt(w, h)}
}

// disclosure is a <details>: its summary after a ▸/▾ marker, and its body when open.
func (th *theme) disclosure(gtx C, open *widget.Bool, summary textStyle, title string, body layout.Widget) D {
	open.Update(gtx)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			return open.Layout(gtx, func(gtx C) D {
				pointer.CursorPointer.Add(gtx.Ops)
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx C) D { return th.marker(gtx, open.Value, summary) }),
					layout.Rigid(func(gtx C) D { return th.text(gtx, summary, title) }),
				)
			})
		}),
		layout.Rigid(func(gtx C) D {
			if !open.Value {
				return D{}
			}
			return body(gtx)
		}),
	)
}

// marker is a <summary>'s triangle, pointing right when closed and down when open.
func (th *theme) marker(gtx C, open bool, st textStyle) D {
	size := gtx.Sp(st.size)
	s := float64(size) * .35
	w := float64(gtx.Sp(st.size)) * .7
	cx, cy := s, float64(size)*.75
	p := art.Path{
		{K: art.Move, P: [3]art.Pt{{X: cx - s/2, Y: cy - s*.6}}},
		{K: art.Line, P: [3]art.Pt{{X: cx + s*.55, Y: cy}}},
		{K: art.Line, P: [3]art.Pt{{X: cx - s/2, Y: cy + s*.6}}},
		{K: art.Close},
	}
	if open {
		p = art.Path{
			{K: art.Move, P: [3]art.Pt{{X: cx - s*.6, Y: cy - s/2}}},
			{K: art.Line, P: [3]art.Pt{{X: cx + s*.6, Y: cy - s/2}}},
			{K: art.Line, P: [3]art.Pt{{X: cx, Y: cy + s*.55}}},
			{K: art.Close},
		}
	}
	paintArt(gtx.Ops, []art.Item{{Path: p, Fill: true, Color: st.color}})
	return D{Size: image.Pt(int(w)+gtx.Dp(4), size)}
}
