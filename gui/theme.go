package main

import (
	"image"
	"image/color"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"

	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/io/clipboard"
	gioevent "gioui.org/io/event"
	"gioui.org/io/key"
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

// The window's look, as CSS rules once had it: each block is named after the rule it was.

type (
	C = layout.Context
	D = layout.Dimensions
)

// Colours named only here, beside art's palette.
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
	// lines caches each style's line box
	lines map[glyphKey]lineBox
}

type glyphKey struct {
	font       font.Font
	px         float32
	lineHeight float32
}

// textStyle is a CSS font and color: lineHeight is CSS's line-height, a multiple of size, 0
// for normal.
type textStyle struct {
	font       font.Font
	size       unit.Sp
	color      color.NRGBA
	align      text.Alignment
	lineHeight float32
	maxLines   int
	// anywhere breaks a line between any two characters (word-break: break-all)
	anywhere bool
	// strut, for inline text in a block of another size (a <small>), is the block's style:
	// each line is the block's line box, the text on its baseline
	strut *textStyle
}

var (
	tBody    = textStyle{font: bodyFont, size: 16, color: art.Forest, lineHeight: 1.5}
	tBold    = textStyle{font: boldFont, size: 16, color: art.Forest, lineHeight: 1.5}
	tHint    = textStyle{font: bodyFont, size: 14, color: art.InkSoft, lineHeight: 1.5}
	tSmall   = textStyle{font: bodyFont, size: 13.3, color: art.InkSoft, lineHeight: 1.5}
	tTitle   = textStyle{font: displayFont, size: 33.6, color: art.Forest, align: text.Middle, lineHeight: 1.1}
	tHeading = textStyle{font: displayFont, size: 20, color: art.Bark, lineHeight: 1.2}
	tLede    = textStyle{font: bodyFont, size: 16, color: art.InkSoft, align: text.Middle, lineHeight: 1.5}
	// a button's: its font shorthand sets line-height back to normal
	tButton = textStyle{font: boldFont, size: 16, color: art.Bark}
)

func (s textStyle) with(c color.NRGBA) textStyle { s.color = c; return s }

// colorOp is a recorded paint.ColorOp, the material text widgets take.
func colorOp(gtx C, c color.NRGBA) op.CallOp {
	c = shade(gtx, c)
	m := op.Record(gtx.Ops)
	paint.ColorOp{Color: c}.Add(gtx.Ops)
	return m.Stop()
}

// ink is the material for text: c painted once here, then the widget's own PaintOp after
// its material paints a second coat, inkAgain opaque. Gio blends in linear light, so a
// glyph's anti-aliased edges came out lighter than Chromium draws them (17% less ink in
// regular text, 9% in bold); a second full coat (an edge covering c covers 1-(1-c)², near
// the 1-(1-c)^2.2 that blending in sRGB gives) overshot by 10-16%.
func ink(gtx C, c color.NRGBA) op.CallOp {
	c = shade(gtx, c)
	m := op.Record(gtx.Ops)
	paint.ColorOp{Color: c}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	again := c
	again.A = uint8(float32(c.A)*inkAgain + .5)
	paint.ColorOp{Color: again}.Add(gtx.Ops)
	return m.Stop()
}

// inkAgain is how opaque text's second coat is: measured against the page's text, .6 puts
// regular, bold and display text within 5% of Chromium's ink (TestRender's scenes)
const inkAgain = .6

// text lays out s in st, wrapping at the constraint's width, each line in CSS's line box:
// line-height tall (normal: the font's ascent and descent, each rounded, as Chromium has it),
// the baseline half the leading plus the ascent down. Gio's own lines stop at the glyphs.
func (th *theme) text(gtx C, st textStyle, s string) D {
	if st.align == text.Middle {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
	} else {
		gtx.Constraints.Min.X = 0
	}
	lb := th.lineBox(gtx, st)
	return lb.place(gtx, func(gtx C, size unit.Sp, lineHeight unit.Sp) D {
		l := widget.Label{Alignment: st.align, MaxLines: st.maxLines, LineHeight: lineHeight, LineHeightScale: 1}
		if st.anywhere {
			l.WrapPolicy = text.WrapGraphemes
		}
		return l.Layout(gtx, th.shaper, st.font, size, s, ink(gtx, st.color))
	}, st.size)
}

// place lays out text w gives (at size, lines lineHeight apart) in this line box: shifted
// so the baseline is where CSS has it, as tall as its lines' boxes.
func (lb lineBox) place(gtx C, w func(gtx C, size, lineHeight unit.Sp) D, size unit.Sp) D {
	m := op.Record(gtx.Ops)
	d := exact(gtx, size, func(gtx C, size unit.Sp, k float32) D {
		return w(gtx, size, unit.Sp(lb.exact*k/gtx.Metric.PxPerSp))
	})
	call := m.Stop()
	o := op.Offset(image.Pt(0, lb.shift)).Push(gtx.Ops)
	call.Add(gtx.Ops)
	o.Pop()
	// Gio's height is the first line's glyphs and a line height for each other line
	d.Size.Y += lb.height - lb.glyphs
	d.Baseline = lb.below
	return d
}

// exact lays out text at size even when that is a fraction of a pixel, which Gio rounds to a
// whole one (17.6 shaped as 18 is 2% wider than Chromium has it): k times larger, then scaled
// back down; glyphs are outlines, so nothing blurs. w multiplies any pixel length by k.
func exact(gtx C, size unit.Sp, w func(gtx C, size unit.Sp, k float32) D) D {
	px := float32(size) * gtx.Metric.PxPerSp
	if px == float32(math.Round(float64(px))) {
		return w(gtx, size, 1)
	}
	const zoom = 8
	big := float32(math.Round(float64(px * zoom)))
	k := big / px
	g := gtx
	g.Constraints.Min.X = int(float32(gtx.Constraints.Min.X) * k)
	g.Constraints.Max.X = int(float32(gtx.Constraints.Max.X) * k)
	g.Constraints.Min.Y = 0
	g.Constraints.Max.Y = 1 << 24
	m := op.Record(gtx.Ops)
	d := w(g, unit.Sp(big/gtx.Metric.PxPerSp), k)
	call := m.Stop()
	t := op.Affine(f32.Affine2D{}.Scale(f32.Point{}, f32.Pt(1/k, 1/k))).Push(gtx.Ops)
	call.Add(gtx.Ops)
	t.Pop()
	return D{
		Size:     image.Pt(int(math.Ceil(float64(float32(d.Size.X)/k))), int(math.Round(float64(float32(d.Size.Y)/k)))),
		Baseline: int(math.Round(float64(float32(d.Baseline) / k))),
	}
}

// lineBox is where Gio's glyphs go in a CSS line box: shift them down by shift; the box is
// height tall, its baseline below from its bottom; glyphs is Gio's own one-line height.
type lineBox struct {
	height, shift, below, glyphs int
	// exact is the line height unrounded: lines after the first are this far apart, as
	// Chromium keeps the fraction (14.4px at 1.4 is 20.16)
	exact float32
}

// fontMetrics are the ascent and descent of each bundled family, in em (hhea; no line gap).
var fontMetrics = map[font.Typeface][2]float64{
	bodyFont.Typeface:    {.95, .29},
	displayFont.Typeface: {.645, .2375},
}

func (th *theme) lineBox(gtx C, st textStyle) lineBox {
	px := float64(st.size) * float64(gtx.Metric.PxPerSp)
	k := glyphKey{st.font, float32(px), st.lineHeight}
	if lb, ok := th.lines[k]; ok {
		return lb
	}
	if th.lines == nil {
		th.lines = map[glyphKey]lineBox{}
	}
	m := op.Record(gtx.Ops)
	g := gtx
	g.Constraints = layout.Constraints{Max: image.Pt(1<<20, 1<<20)}
	d := exact(g, st.size, func(gtx C, size unit.Sp, _ float32) D {
		return widget.Label{MaxLines: 1}.Layout(gtx, th.shaper, st.font, size, "Hg", op.CallOp{})
	})
	m.Stop()
	gioAscent := d.Size.Y - d.Baseline
	am, ok := fontMetrics[st.font.Typeface]
	if !ok {
		am = [2]float64{float64(gioAscent) / px, float64(d.Baseline) / px}
	}
	asc, desc := int(math.Round(am[0]*px)), int(math.Round(am[1]*px))
	h, exact := asc+desc, float32(asc+desc)
	if st.lineHeight > 0 {
		exact = float32(px * float64(st.lineHeight))
		h = int(math.Round(float64(exact)))
	}
	top := (h - asc - desc) / 2
	lb := lineBox{height: h, shift: top + asc - gioAscent, below: h - top - asc, glyphs: d.Size.Y, exact: exact}
	if st.strut != nil {
		sb := th.lineBox(gtx, *st.strut)
		baseline := max(sb.height-sb.below, top+asc)
		lb.below = max(sb.below, h-top-asc)
		lb.height = baseline + lb.below
		lb.shift = baseline - gioAscent
		lb.exact = float32(lb.height)
	}
	if st.strut == nil {
		th.lines[k] = lb
	}
	return lb
}

// selectable is text that can be selected and copied, like the page's code and status.
func (th *theme) selectable(gtx C, sel *widget.Selectable, st textStyle, s string) D {
	sel.Alignment = st.align
	if st.anywhere {
		sel.WrapPolicy = text.WrapGraphemes
	}
	if sel.Text() != s {
		sel.SetText(s)
	}
	gtx.Constraints.Min.X = 0
	return th.lineBox(gtx, st).place(gtx, func(gtx C, size, lineHeight unit.Sp) D {
		sel.LineHeight, sel.LineHeightScale = lineHeight, 1
		return sel.Layout(gtx, th.shaper, st.font, size, ink(gtx, st.color), colorOp(gtx, selection))
	}, st.size)
}

// baselineRow lays out ws left to right on one baseline, as a line of inline boxes: as tall
// as the most any reaches above it plus the most any reaches below (Flex counts only the
// tallest).
func baselineRow(gtx C, ws ...layout.Widget) D {
	type laid struct {
		call op.CallOp
		d    D
	}
	var row []laid
	x, above, below := 0, 0, 0
	for _, w := range ws {
		g := gtx
		g.Constraints.Min = image.Point{}
		g.Constraints.Max.X = max(0, gtx.Constraints.Max.X-x)
		m := op.Record(gtx.Ops)
		d := w(g)
		row = append(row, laid{m.Stop(), d})
		x += d.Size.X
		above, below = max(above, d.Size.Y-d.Baseline), max(below, d.Baseline)
	}
	x = 0
	for _, l := range row {
		o := op.Offset(image.Pt(x, above-(l.d.Size.Y-l.d.Baseline))).Push(gtx.Ops)
		l.call.Add(gtx.Ops)
		o.Pop()
		x += l.d.Size.X
	}
	return D{Size: image.Pt(x, above+below), Baseline: below}
}

// fill is d as wide as the space it was given: a flexed child that takes up its share.
func fill(gtx C, d D) D {
	d.Size.X = max(d.Size.X, gtx.Constraints.Max.X)
	return d
}

// rrect fills a rounded rectangle.
func rrect(gtx C, r image.Rectangle, radius int, c color.NRGBA) {
	radius = max(min(radius, r.Dx()/2, r.Dy()/2), 0)
	if radius == 0 { // a rectangle: Gio's fast path, no outline to keep
		paint.FillShape(gtx.Ops, shade(gtx, c), clip.Rect(r).Op())
		return
	}
	key := newShapeKey('r').i(r.Min.X).i(r.Min.Y).i(r.Max.X).i(r.Max.Y).i(radius).sum()
	paint.FillShape(gtx.Ops, shade(gtx, c), shape(gtx, key, func(ops *op.Ops) clip.Op { return clip.UniformRRect(r, radius).Op(ops) }))
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
	// middle: stretched taller than its content (Min.Y), the content goes in the middle, as a
	// button's does; otherwise it stays at the top
	middle bool
}

func (th *theme) box(gtx C, s boxStyle, w layout.Widget) D {
	// as wide as its content, unless wide: a flex column hands its children its own minimum.
	// A minimum height stretches the box, not what is in it
	gtx.Constraints.Min.X = 0
	minY := gtx.Constraints.Min.Y
	gtx.Constraints.Min.Y = 0
	if s.wide {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
	}
	// the border is outside the padding, as CSS's border-box has it
	in := s.inset
	edge := unit.Dp(float32(borderPx(gtx, s.borderW)) / gtx.Metric.PxPerDp)
	in.Top, in.Bottom, in.Left, in.Right = in.Top+edge, in.Bottom+edge, in.Left+edge, in.Right+edge
	m := op.Record(gtx.Ops)
	d := in.Layout(gtx, func(gtx C) D {
		if s.wide {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
		}
		return w(gtx)
	})
	content := m.Stop()
	size := d.Size
	if extra := minY - size.Y; extra > 0 {
		size.Y += extra
		down := 0
		if s.middle {
			down = extra / 2
		}
		c := op.Record(gtx.Ops)
		o := op.Offset(image.Pt(0, down)).Push(gtx.Ops)
		content.Add(gtx.Ops)
		o.Pop()
		content = c.Stop()
	}
	r, bw, sh := gtx.Dp(s.radius), borderPx(gtx, s.borderW), gtx.Dp(s.shadow)
	if s.ring {
		ring(gtx, image.Rectangle{Max: size}, r)
	}
	if sh > 0 {
		rrect(gtx, image.Rect(0, sh, size.X, size.Y+sh), r, shadow(gtx, s.shadowColor))
	}
	if bw > 0 {
		rrect(gtx, image.Rectangle{Max: size}, r, s.border)
	}
	rrect(gtx, image.Rect(bw, bw, size.X-bw, size.Y-bw), r-bw, s.bg)
	content.Add(gtx.Ops)
	// a box-shadow takes no room: what follows may sit on it
	return D{Size: size, Baseline: d.Baseline}
}

// borderPx is a border width in whole pixels, as Chromium draws one: rounded down, at least 1.
func borderPx(gtx C, w unit.Dp) int {
	if w <= 0 {
		return 0
	}
	return max(1, int(float32(w)*gtx.Metric.PxPerDp))
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
	bPlain   = buttonStyle{box: boxStyle{bg: art.White, border: art.Bark, borderW: 2, radius: 12, inset: layout.Inset{Top: 10, Bottom: 10, Left: 14, Right: 14}}, text: tButton.with(art.Bark), hover: hoverBg}
	bPrimary = buttonStyle{box: boxStyle{bg: art.Moss, border: art.MossDeep, borderW: 2, radius: 12, inset: layout.Inset{Top: 10, Bottom: 10, Left: 14, Right: 14}}, text: tButton.with(art.White), hover: art.MossDeep}
	bSmall   = buttonStyle{box: boxStyle{bg: art.White, border: art.Bark, borderW: 2, radius: 10, inset: layout.Inset{Top: 4, Bottom: 4, Left: 10, Right: 10}}, text: textStyle{font: boldFont, size: 14, color: art.Bark}, hover: hoverBg}
	bTab     = buttonStyle{box: boxStyle{bg: art.White, border: art.Rule, borderW: 2, radius: 999, inset: layout.Inset{Top: 6, Bottom: 6, Left: 16, Right: 16}}, text: textStyle{font: boldFont, size: 14.4, color: art.Bark}, hover: hoverBg}
	bTabOn   = buttonStyle{box: boxStyle{bg: segOn, border: art.Moss, borderW: 2, radius: 999, inset: layout.Inset{Top: 6, Bottom: 6, Left: 16, Right: 16}}, text: textStyle{font: boldFont, size: 14.4, color: art.Forest}, hover: segOn}
	bGo      = buttonStyle{box: boxStyle{bg: art.Moss, border: art.MossDeep, borderW: 2, radius: 12, shadow: 4, shadowColor: art.MossDeep, inset: layout.UniformInset(14), wide: true}, text: textStyle{font: displayFont, size: 20.8, color: art.White}, hover: art.MossDeep, icon: art.Paw, iconSize: 22, iconGap: 10, center: true}
	bStop    = buttonStyle{box: boxStyle{bg: art.Paper, border: art.Berry, borderW: 2, radius: 12, shadow: 4, shadowColor: art.Berry, inset: layout.UniformInset(14), wide: true}, text: textStyle{font: displayFont, size: 20.8, color: art.Berry}, hover: art.Hex("#f6e9e4"), icon: art.Paw, iconSize: 22, iconGap: 10, center: true}
)

// button is a clickable box; disabled ones are faded and take no clicks.
func (th *theme) button(gtx C, c *widget.Clickable, s buttonStyle, disabled bool, label string) D {
	return th.pressable(gtx, c, s, disabled, func(gtx C) D {
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
}

// pressable is a clickable box holding w: faded and deaf when disabled, its hover colour
// under the mouse, the ring when focused. The shadow and the ring are drawn around the
// clickable, which clips what it holds to its own size.
func (th *theme) pressable(gtx C, c *widget.Clickable, s buttonStyle, disabled bool, w layout.Widget) D {
	if disabled {
		gtx = gtx.Disabled()
		defer paint.PushOpacity(gtx.Ops, .55).Pop()
	} else if c.Hovered() {
		s.box.bg = s.hover
	}
	focused := gtx.Focused(c) && !disabled
	if focused {
		// :focus-visible: a moss border inside the ring
		s.box.border = art.Moss
	}
	b := s.box
	b.middle, b.shadow, b.ring = true, 0, false
	m := op.Record(gtx.Ops)
	d := c.Layout(gtx, func(gtx C) D {
		if !disabled {
			pointer.CursorPointer.Add(gtx.Ops)
		}
		return th.box(gtx, b, w)
	})
	call := m.Stop()
	around(gtx, s.box, d.Size, focused, call)
	return d
}

// around draws a box's shadow, then call (the box), then its focus ring if focused.
func around(gtx C, s boxStyle, size image.Point, focused bool, call op.CallOp) {
	r := gtx.Dp(s.radius)
	if sh := gtx.Dp(s.shadow); sh > 0 {
		rrect(gtx, image.Rect(0, sh, size.X, size.Y+sh), r, shadow(gtx, s.shadowColor))
	}
	call.Add(gtx.Ops)
	if focused {
		ring(gtx, image.Rectangle{Max: size}, r)
	}
}

// icon draws an art SVG size dp square.
func icon(gtx C, svg string, st art.Style, size unit.Dp) D {
	return iconWith(gtx, svg, st, size, 0)
}

// iconWith is icon followed by gap dp of space, the SVG's viewBox scaled to size.
func iconWith(gtx C, svg string, st art.Style, size, gap unit.Dp) D {
	px := float64(gtx.Dp(size))
	box := viewBox(svg)
	items := art.Icon(svg, st, art.Scale(px/box, px/box))
	// the palette paints the style's colour, CSS's currentColor; the SVG's own (an emoji's,
	// a shell's) stay as drawn
	for i, it := range items {
		if sameRGB(it.Color, st.Fill) && st.HasFill || sameRGB(it.Color, st.Stroke) && st.HasStroke {
			items[i].Color = shade(gtx, it.Color)
		}
	}
	paintArt(asDrawn(gtx), items)
	return D{Size: image.Pt(gtx.Dp(size+gap), gtx.Dp(size))}
}

var reViewBox = regexp.MustCompile(`viewBox="[-\d.]+ [-\d.]+ ([\d.]+)`)

// viewBox is an SVG's width in its own units (its viewBox), 24 if it has none.
func viewBox(svg string) float64 {
	if m := reViewBox.FindStringSubmatch(svg); m != nil {
		if w, err := strconv.ParseFloat(m[1], 64); err == nil && w > 0 {
			return w
		}
	}
	return 24
}

// iconButton is a button showing only an icon, size dp square (.react-open).
func (th *theme) iconButton(gtx C, c *widget.Clickable, s buttonStyle, disabled bool, svg string, st art.Style, size unit.Dp) D {
	return th.pressable(gtx, c, s, disabled, func(gtx C) D { return icon(gtx, svg, st, size) })
}

// focusCmd moves the keyboard focus to tag.
func focusCmd(tag any) key.FocusCmd { return key.FocusCmd{Tag: tag} }

// clipboardWrite puts s on the clipboard.
func clipboardWrite(s string) clipboard.WriteCmd {
	return clipboard.WriteCmd{Type: "application/text", Data: io.NopCloser(strings.NewReader(s))}
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
		// CSS's line box: 24dp a line, the glyphs in its middle
		minY := gtx.Sp(unit.Sp(24 * max(minLines, 1)))
		lb := th.lineBox(gtx, tBody)
		lead := lb.height - lb.glyphs
		gtx.Constraints.Min.Y = 0
		o := op.Offset(image.Pt(0, lb.shift)).Push(gtx.Ops)
		e.LineHeight, e.LineHeightScale = 24, 1
		var d D
		if e.Len() > 0 {
			d = e.Layout(gtx, th.shaper, bodyFont, 16, ink(gtx, art.Forest), colorOp(gtx, selection))
		} else {
			// the hint under an empty editor, which still takes the clicks and the caret
			l := widget.Label{LineHeight: 24, LineHeightScale: 1}
			if minLines <= 1 {
				// one line, cut off at the field's edge as an <input>'s is
				hg := gtx
				hg.Constraints.Max.X = 1 << 20
				cl := clip.Rect{Max: image.Pt(gtx.Constraints.Max.X, 1<<20)}.Push(gtx.Ops)
				d = l.Layout(hg, th.shaper, bodyFont, 16, hint, ink(gtx, placeholder))
				cl.Pop()
			} else {
				d = l.Layout(gtx, th.shaper, bodyFont, 16, hint, ink(gtx, placeholder))
			}
			eg := gtx
			eg.Constraints = layout.Exact(image.Pt(gtx.Constraints.Max.X, max(d.Size.Y, minY-lead)))
			e.Layout(eg, th.shaper, bodyFont, 16, ink(gtx, art.Forest), colorOp(gtx, selection))
		}
		o.Pop()
		d.Size.X = gtx.Constraints.Max.X
		d.Size.Y = max(d.Size.Y+lead, minY)
		return d
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
					paintArt(gtx, []art.Item{{Path: tick, Color: art.White, Width: 2.2 * px, Round: true}})
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
	paint.FillShape(gtx.Ops, shade(gtx, art.Ochre), clip.Stroke{Path: p, Width: float32(w)}.Op())
}

// option is one card of a .seg choice: a bold title over a small grey line, moss when chosen.
func (th *theme) option(gtx C, e *widget.Enum, key, title, sub string) D {
	bg, border := art.White, art.Rule
	if e.Value == key {
		bg, border = segOn, art.Moss
	}
	st := boxStyle{bg: bg, border: border, borderW: 2, radius: 12, inset: layout.Inset{Top: 10, Bottom: 10, Left: 12, Right: 12}, wide: true}
	focused, _ := e.Focused()
	m := op.Record(gtx.Ops)
	d := e.Layout(gtx, key, func(gtx C) D {
		pointer.CursorPointer.Add(gtx.Ops)
		return th.box(gtx, st, func(gtx C) D {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx C) D { return th.text(gtx, tBold, title) }),
				layout.Rigid(func(gtx C) D {
					st := tSmall
					st.strut = &tBody
					return th.text(gtx, st, sub)
				}),
			)
		})
	})
	around(gtx, st, d.Size, focused == key, m.Stop())
	return d
}

// options lays out a .seg: its cards side by side, 8dp apart, all as tall as the tallest.
func (th *theme) options(gtx C, e *widget.Enum, opts ...[3]string) D {
	return th.optionGrid(gtx, e, len(opts), opts...)
}

// optionGrid lays out a .seg's cards in cols columns, 8dp apart both ways, each row as tall
// as its tallest card, as a grid row stretches its items.
func (th *theme) optionGrid(gtx C, e *widget.Enum, cols int, opts ...[3]string) D {
	e.Update(gtx)
	gap := gtx.Dp(8)
	w := (gtx.Constraints.Max.X - gap*(cols-1)) / cols
	card := func(gtx C, o [3]string, minY int) D {
		gtx.Constraints = layout.Constraints{Min: image.Pt(w, minY), Max: image.Pt(w, 1<<24)}
		return th.option(gtx, e, o[0], o[1], o[2])
	}
	y := 0
	for row := 0; row*cols < len(opts); row++ {
		cards := opts[row*cols : min(len(opts), (row+1)*cols)]
		tallest := 0
		for _, o := range cards {
			m := op.Record(gtx.Ops)
			tallest = max(tallest, card(gtx, o, 0).Size.Y)
			m.Stop()
		}
		if row > 0 {
			y += gap
		}
		for i, o := range cards {
			t := op.Offset(image.Pt(i*(w+gap), y)).Push(gtx.Ops)
			card(gtx, o, tallest)
			t.Pop()
		}
		y += tallest
	}
	return D{Size: image.Pt(gtx.Constraints.Max.X, y)}
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
	key := newShapeKey('-').i(width).f(float64(h)).sum()
	paint.FillShape(gtx.Ops, shade(gtx, art.Rule), shape(gtx, key, s.Op))
	return D{Size: image.Pt(width, int(h))}
}

// dashedDown is dashed standing up: a 2dp dashed line height tall (border-left).
func dashedDown(gtx C, height int) D {
	w := float32(gtx.Dp(2))
	s := stroke.Stroke{
		Path:   stroke.Path{Segments: []stroke.Segment{stroke.MoveTo(f32.Pt(w/2, 0)), stroke.LineTo(f32.Pt(w/2, float32(height)))}},
		Width:  w,
		Cap:    stroke.FlatCap,
		Dashes: stroke.Dashes{Dashes: []float32{3 * w, 3 * w}},
	}
	key := newShapeKey('|').i(height).f(float64(w)).sum()
	paint.FillShape(gtx.Ops, shade(gtx, art.Rule), shape(gtx, key, s.Op))
	return D{Size: image.Pt(int(w), height)}
}

// blockInput takes every pointer event over size, so nothing under it gets them (a modal's
// backdrop).
func blockInput(gtx C, size image.Point) {
	defer clip.Rect{Max: size}.Push(gtx.Ops).Pop()
	gioevent.Op(gtx.Ops, &blocked)
	for {
		if _, ok := gtx.Event(pointer.Filter{Target: &blocked, Kinds: pointer.Press | pointer.Release | pointer.Scroll | pointer.Move}); !ok {
			break
		}
	}
}

// blocked is the tag of blockInput's area
var blocked int

// rule is a 1dp solid line across the width (a list's divider).
func rule(gtx C) D {
	w, h := gtx.Constraints.Max.X, max(1, gtx.Dp(1))
	paint.FillShape(gtx.Ops, shade(gtx, art.Rule), clip.Rect{Max: image.Pt(w, h)}.Op())
	return D{Size: image.Pt(w, h)}
}

// disclosure is a <details>: its summary after a ▸/▾ marker, and its body when open.
func (th *theme) disclosure(gtx C, open *widget.Bool, summary textStyle, title string, body layout.Widget) D {
	open.Update(gtx)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			return open.Layout(gtx, func(gtx C) D {
				pointer.CursorPointer.Add(gtx.Ops)
				return baselineRow(gtx,
					func(gtx C) D { return th.marker(gtx, open.Value, summary) },
					func(gtx C) D { return th.text(gtx, summary, title) },
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

// marker is a <summary>'s triangle as Chromium draws it: in a square .656em a side whose
// bottom is on the text's baseline (Baseline 0), pointing right when closed and down when
// open, the text 1.07em after it. Lay it out on the summary's baseline (baselineRow).
func (th *theme) marker(gtx C, open bool, st textStyle) D {
	em := float64(gtx.Sp(st.size))
	s := .656 * em
	pt := func(x, y float64) art.Pt { return art.Pt{X: x * s, Y: y * s} }
	p := art.Path{{K: art.Move, P: [3]art.Pt{pt(.1, .07)}}, {K: art.Line, P: [3]art.Pt{pt(.96, .53)}}, {K: art.Line, P: [3]art.Pt{pt(.1, .99)}}, {K: art.Close}}
	if open {
		p = art.Path{{K: art.Move, P: [3]art.Pt{pt(.07, .1)}}, {K: art.Line, P: [3]art.Pt{pt(.93, .1)}}, {K: art.Line, P: [3]art.Pt{pt(.5, .97)}}, {K: art.Close}}
	}
	paintArt(gtx, []art.Item{{Path: p, Fill: true, Color: st.color}})
	return D{Size: image.Pt(int(math.Round(1.07*em)), int(math.Round(s)))}
}
