package main

import (
	"image"
	"image/color"
	"math"
	"math/rand/v2"
	"time"

	"gioui.org/f32"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"

	"github.com/mhashemm/watchparty/gui/art"
)

// view is the window: the page's layout (body, main, .card, .cat, .ground) and what it
// remembers between frames. Screens fill the card (card).
type view struct {
	th     *theme
	cat    *art.Scene
	ground *art.Scene
	// start is when the animations began, as the page's load did
	start time.Time
	mood  art.Mood
	list  widget.List
	bg    background
	// hearts float up from the cat when it is clicked
	catTap widget.Clickable
	hearts []heart
	rnd    *rand.Rand
	// card lays out what the card holds
	card layout.Widget
}

type heart struct {
	at time.Time
	x  float32 // 40-60% across main
}

func newView(th *theme, now time.Time) *view {
	v := &view{th: th, cat: art.Cat(), ground: art.Ground(), start: now, rnd: rand.New(rand.NewPCG(uint64(now.UnixNano()), 1))}
	v.list.Axis = layout.Vertical
	return v
}

// t is the animations' clock, in seconds.
func (v *view) t(gtx C) float64 { return gtx.Now.Sub(v.start).Seconds() }

// frame lays out the whole window. It asks for the next frame: the cat and the turtles move.
func (v *view) frame(gtx C) {
	v.bg.paint(gtx)
	v.list.Layout(gtx, 1, func(gtx C, _ int) D { return v.page(gtx) })
	v.paintGround(gtx)
	gtx.Execute(op.InvalidateCmd{})
}

// page is main: 520dp wide at most, the card 190dp down with Fosdo2 over its top edge.
func (v *view) page(gtx C) D {
	if v.catTap.Clicked(gtx) {
		v.hearts = append(v.hearts, heart{gtx.Now, .4 + .2*v.rnd.Float32()})
	}
	W := gtx.Constraints.Max.X
	mainW := min(gtx.Dp(520), W-gtx.Dp(32))
	x, top := (W-mainW)/2, gtx.Dp(190)

	cg := gtx
	cg.Constraints = layout.Constraints{Min: image.Pt(mainW, 0), Max: image.Pt(mainW, 1<<24)}
	m := op.Record(gtx.Ops)
	cd := v.th.box(cg, boxStyle{bg: art.Paper, border: art.Bark, borderW: 2, radius: 22, shadow: 6, shadowColor: art.Bark, inset: layout.Inset{Top: 34, Left: 28, Right: 28, Bottom: 28}, wide: true}, func(gtx C) D {
		if v.card == nil {
			return D{}
		}
		return v.card(gtx)
	})
	card := m.Stop()
	t := op.Offset(image.Pt(x, top)).Push(gtx.Ops)
	card.Add(gtx.Ops)
	t.Pop()

	v.paintCat(gtx, W)
	v.paintHearts(gtx, x, mainW)
	return D{Size: image.Pt(W, top+cd.Size.Y+gtx.Dp(150))}
}

// paintCat draws Fosdo2 180dp wide, centred, 9dp from the top, and its z's while it sleeps.
func (v *view) paintCat(gtx C, W int) {
	px := float64(gtx.Metric.PxPerDp)
	w := 180 * px
	left, top := float64(W)/2-w/2, 9*px
	base := art.Translate(left, top).Mul(art.Scale(w/200, w/200))
	paintArt(gtx.Ops, v.cat.Frame(v.t(gtx), v.mood, base))
	if !v.mood.Awake {
		for _, z := range art.Zzz(v.t(gtx)) {
			if z.O <= 0 {
				continue
			}
			p := base.Apply(art.Pt{X: z.X, Y: z.Y})
			c := art.Bark
			c.A = uint8(255 * z.O)
			// the SVG's y is the baseline; 16px Gluten's ascent is about 14
			o := op.Offset(image.Pt(int(p.X), int(p.Y-14*px))).Push(gtx.Ops)
			v.th.text(gtx, textStyle{font: displayFont, size: unit.Sp(16 * w / 200 / px), color: c, lineHeight: 1}, "z")
			o.Pop()
		}
	}
	o := op.Offset(image.Pt(int(left), int(top))).Push(gtx.Ops)
	cg := gtx
	cg.Constraints = layout.Exact(image.Pt(int(w), int(w*220/200)))
	v.catTap.Layout(cg, func(gtx C) D {
		pointer.CursorPointer.Add(gtx.Ops)
		return D{Size: gtx.Constraints.Min}
	})
	o.Pop()
}

// heartFor is how long a heart floats (.heart's float animation, ease-out)
const heartFor = 1500 * time.Millisecond

// paintHearts draws each ♥ rising 60dp and growing from .6 to 1.2 as it fades.
func (v *view) paintHearts(gtx C, x, mainW int) {
	kept := v.hearts[:0]
	for _, h := range v.hearts {
		p := float64(gtx.Now.Sub(h.at)) / float64(heartFor)
		if p >= 1 {
			continue
		}
		kept = append(kept, h)
		e := cssEaseOut(p)
		scale := float32(.6 + .6*e)
		c := art.Berry
		c.A = uint8(255 * (1 - e))
		cx := float32(x) + h.x*float32(mainW)
		y := float32(gtx.Dp(190-120)) - float32(gtx.Dp(60))*float32(e)
		size := gtx.Sp(22)
		tr := op.Affine(f32.Affine2D{}.Scale(f32.Pt(0, 0), f32.Pt(scale, scale)).Offset(f32.Pt(cx-float32(size)*scale/2, y))).Push(gtx.Ops)
		v.th.text(gtx, textStyle{font: bodyFont, size: 22, color: c, lineHeight: 1}, "♥")
		tr.Pop()
	}
	v.hearts = kept
}

// cssEaseOut is CSS's ease-out, cubic-bezier(0, 0, .58, 1).
var cssEaseOut = art.Bezier(0, 0, .58, 1)

// paintGround is the fixed strip: the fade into the page, the soil, mushrooms and turtles.
func (v *view) paintGround(gtx C) {
	W, H := gtx.Constraints.Max.X, gtx.Constraints.Max.Y
	fade := gtx.Dp(90)
	clear := art.Lichen
	clear.A = 0
	r := clip.Rect{Min: image.Pt(0, H-fade), Max: image.Pt(W, H)}.Push(gtx.Ops)
	paint.LinearGradientOp{Stop1: f32.Pt(0, float32(H-fade)), Color1: clear, Stop2: f32.Pt(0, float32(H-fade)+float32(fade)*.75), Color2: art.Lichen}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	r.Pop()
	paint.FillShape(gtx.Ops, art.Soil, clip.Rect{Min: image.Pt(0, H-gtx.Dp(22)), Max: image.Pt(W, H)}.Op())
	px := float64(gtx.Metric.PxPerDp)
	base := art.Translate(float64(W)/2-280*px, float64(H)-130*px).Mul(art.Scale(px, px))
	paintArt(gtx.Ops, v.ground.Frame(v.t(gtx), v.mood, base))
}

// background is body's: lichen under three soft glows (radial-gradients, fixed to the
// window). Gio paints no radial gradients, so it is an image, made again when the window
// changes size, at half resolution and scaled up smooth.
type background struct {
	size image.Point
	img  paint.ImageOp
}

func (b *background) paint(gtx C) {
	size := gtx.Constraints.Max
	if size != b.size {
		b.size, b.img = size, paint.NewImageOp(glows(size.X/2+1, size.Y/2+1))
		b.img.Filter = paint.FilterLinear
	}
	s := float32(size.X) / float32(size.X/2+1)
	defer op.Affine(f32.Affine2D{}.Scale(f32.Pt(0, 0), f32.Pt(s, float32(size.Y)/float32(size.Y/2+1)))).Push(gtx.Ops).Pop()
	b.img.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
}

// glows paints body's background at w×h: each glow is an ellipse whose colour fades to
// nothing at stop of its radius, the first listed on top, as CSS layers them.
func glows(w, h int) *image.RGBA {
	type glow struct {
		cx, cy, rx, ry float64 // fractions of the window
		c              color.NRGBA
		alpha, stop    float64
	}
	layers := []glow{ // bottom first
		{.5, 1, .8, .3, art.Moss, .22, .75},
		{.92, .3, .45, .3, color.NRGBA{255, 255, 238, 255}, .4, .7},
		{.12, .06, .6, .35, color.NRGBA{255, 255, 238, 255}, .6, .7},
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			r, g, b := float64(art.Lichen.R), float64(art.Lichen.G), float64(art.Lichen.B)
			for _, l := range layers {
				dx := (float64(x)/float64(w) - l.cx) / l.rx
				dy := (float64(y)/float64(h) - l.cy) / l.ry
				d := math.Sqrt(dx*dx + dy*dy)
				if d >= l.stop {
					continue
				}
				a := l.alpha * (1 - d/l.stop)
				r = r*(1-a) + float64(l.c.R)*a
				g = g*(1-a) + float64(l.c.G)*a
				b = b*(1-a) + float64(l.c.B)*a
			}
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = uint8(r+.5), uint8(g+.5), uint8(b+.5), 255
		}
	}
	return img
}

// title is the card's heading: the mushroom, then "Hahas and Hehes Watch Party" in Gluten,
// centred and wrapped as the page wraps it.
func (v *view) title(gtx C) D {
	line := func(mush bool, s string) layout.FlexChild {
		return layout.Rigid(func(gtx C) D {
			return layout.Center.Layout(gtx, func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						if !mush {
							return D{}
						}
						return iconWith(gtx, art.Mushroom, art.FillStyle(art.Porcini), 30, 8)
					}),
					layout.Rigid(func(gtx C) D {
						st := tTitle
						st.align = text.Start
						return v.th.text(gtx, st, s)
					}),
				)
			})
		})
	}
	return layout.Inset{Bottom: 6}.Layout(gtx, func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
			line(true, "Hahas and Hehes"),
			line(false, "Watch Party"),
		)
	})
}

// ch is CSS's ch at 16sp in Atkinson: the width of its "0", .648 em
const ch = 16 * .648

// lede is the line under the title, 40ch wide at most.
func (v *view) lede(gtx C, s string) D {
	return layout.Inset{Bottom: 24}.Layout(gtx, func(gtx C) D {
		gtx.Constraints.Max.X = min(gtx.Constraints.Max.X, gtx.Sp(40*ch))
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		return layout.Center.Layout(gtx, func(gtx C) D { return v.th.text(gtx, tLede, s) })
	})
}
