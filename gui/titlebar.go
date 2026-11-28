package main

import (
	"image"

	"gioui.org/f32"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op/paint"
	"gioui.org/widget"
	"gioui.org/x/stroke"

	"github.com/mhashemm/watchparty/gui/art"
)

// titleBar is the window's own title bar, for a desktop that draws none (GNOME on Wayland):
// paper, a bark line under it, the mushroom and the title in Gluten, and round buttons like
// the "?" one.
type titleBar struct {
	deco widget.Decorations
}

// titleBarHeight is how tall the bar is, in dp
const titleBarHeight = 40

var tBarTitle = textStyle{font: displayFont, size: 15, color: art.Forest}

func (v *view) titleBar(gtx C, b *titleBar, title string) D {
	gtx = v.painted(gtx)
	h := gtx.Dp(titleBarHeight)
	W := gtx.Constraints.Max.X
	rrect(gtx, image.Rect(0, 0, W, h), 0, art.Paper)
	rrect(gtx, image.Rect(0, h-gtx.Dp(2), W, h), 0, art.Bark)
	g := gtx
	g.Constraints = layout.Exact(image.Pt(W, h-gtx.Dp(2)))
	layout.Flex{Alignment: layout.Middle}.Layout(g,
		layout.Flexed(1, func(gtx C) D {
			return b.deco.LayoutMove(gtx, func(gtx C) D {
				gtx.Constraints.Min = gtx.Constraints.Max
				return layout.W.Layout(gtx, func(gtx C) D {
					// W keeps the bar's height as the minimum, which would put the text at the top
					gtx.Constraints.Min.Y = 0
					return layout.Inset{Left: 12}.Layout(gtx, func(gtx C) D {
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							// both by their ink, in the bar's middle: the mushroom's sits low in its box,
							// and the capitals sit high in their line box
							layout.Rigid(func(gtx C) D {
								return layout.Inset{Bottom: 2}.Layout(gtx, func(gtx C) D {
									return iconWith(gtx, art.Mushroom, art.FillStyle(art.Porcini), 20, 8)
								})
							}),
							layout.Rigid(func(gtx C) D {
								return layout.Inset{Top: 4}.Layout(gtx, func(gtx C) D { return v.th.text(gtx, tBarTitle, title) })
							}),
						)
					})
				})
			})
		}),
		layout.Rigid(func(gtx C) D { return v.barButton(gtx, b, system.ActionMinimize) }),
		layout.Rigid(func(gtx C) D { return v.barButton(gtx, b, system.ActionMaximize) }),
		layout.Rigid(func(gtx C) D { return v.barButton(gtx, b, system.ActionClose) }),
		layout.Rigid(layout.Spacer{Width: 6}.Layout),
	)
	return D{Size: image.Pt(W, h)}
}

// barButton is a 26dp round button, 4dp either side: a bar for minimize, a square for
// maximize, a cross for close, whose hover is the error red.
func (v *view) barButton(gtx C, b *titleBar, a system.Action) D {
	c := b.deco.Clickable(a)
	return layout.UniformInset(4).Layout(gtx, func(gtx C) D {
		return c.Layout(gtx, func(gtx C) D {
			sz := gtx.Dp(26)
			bg := art.Paper
			if c.Hovered() {
				bg = hoverBg
				if a == system.ActionClose {
					bg = badBg
				}
			}
			rrect(gtx, image.Rect(0, 0, sz, sz), sz/2, art.Bark)
			rrect(gtx, image.Rect(0, 0, sz, sz).Inset(gtx.Dp(2)), sz/2, bg)
			m := float32(sz) / 2
			r := float32(gtx.Dp(5))
			var segs []stroke.Segment
			switch a {
			case system.ActionMinimize:
				segs = []stroke.Segment{stroke.MoveTo(f32.Pt(m-r, m+r*.6)), stroke.LineTo(f32.Pt(m+r, m+r*.6))}
			case system.ActionMaximize:
				segs = []stroke.Segment{stroke.MoveTo(f32.Pt(m-r, m-r)), stroke.LineTo(f32.Pt(m+r, m-r)), stroke.LineTo(f32.Pt(m+r, m+r)), stroke.LineTo(f32.Pt(m-r, m+r)), stroke.LineTo(f32.Pt(m-r, m-r))}
			default:
				segs = []stroke.Segment{stroke.MoveTo(f32.Pt(m-r, m-r)), stroke.LineTo(f32.Pt(m+r, m+r)), stroke.MoveTo(f32.Pt(m+r, m-r)), stroke.LineTo(f32.Pt(m-r, m+r))}
			}
			s := stroke.Stroke{Path: stroke.Path{Segments: segs}, Width: float32(gtx.Dp(2)), Cap: stroke.RoundCap, Join: stroke.RoundJoin}
			paint.FillShape(gtx.Ops, shade(gtx, art.Bark), s.Op(gtx.Ops))
			return D{Size: image.Pt(sz, sz)}
		})
	})
}
