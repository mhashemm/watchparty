package main

import (
	"context"
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

	party    *partyTab
	settings settings
	// now is the latest party, as the app last saw it
	now *partyNow
	tab string
	// tabs are Party, History and Settings; updateDot marks Settings when an update is out
	tabs      [3]widget.Clickable
	updateDot bool
	tip       widget.Clickable
	tipOpen   bool
	tips      tipsState

	history historyTab
	// parties is the saved history, oldest first
	parties []session
	// dialog is the one open, over everything
	dialog *modal

	prefs *settingsTab
	about about

	// app is what the window drives; nil draws only (TestRender)
	app backend
	// wake asks for a frame from another goroutine
	wake func()
	// results are what async work leaves for the window's goroutine to apply
	results chan func(gtx C)
	// run, from and saidFrom are how far the window has read the app's news
	run, from, saidFrom int
	// started is the form the latest party started from: its plan for the steps
	started form
	// toolsAsked counts About's checks, so a slow earlier one doesn't overwrite a later one
	toolsAsked int
	// still: the OS asks for reduced motion, so the scenes rest and the window draws only on
	// changes
	still bool
	// dark: the OS's appearance is dark, so the window paints in darkPalette
	dark bool
	// pace spaces the scenes' frames by what one costs here
	pace pacer
	// values is every frame's gtx.Values: the palette and the path cache
	values map[string]any
	// focused: the window has the keyboard; frozen, when the scenes' time stopped (zero
	// while they move)
	focused bool
	frozen  time.Time
	// after runs once this frame is drawn (the e2e control socket's answers)
	after []func()
}

// backend is the app as the window sees it. pick, saveChart, install, latest and tools wait
// on the OS or the network, so the window calls them through async.
type backend interface {
	start(f form) error
	stop()
	chat(text string) error
	react(code string) error
	pick(field string) (string, error)
	saveChart() (string, error)
	saveSettings(s settings) error
	clearHistory() error
	install() (string, error)
	restart() error
	latest(ctx context.Context) (string, error)
	tools(ctx context.Context) []tool
	news(run, from, saidFrom int) news
	state() appState
}

var tabNames = [3]string{"party", "history", "settings"}

// body is the card: the title, the lede, the tabs and the tab shown.
func (v *view) body(gtx C) D {
	for i := range v.tabs {
		if v.tabs[i].Clicked(gtx) {
			v.tab = tabNames[i]
			if v.tab == "history" {
				v.reload()
			}
		}
	}
	if v.tip.Clicked(gtx) {
		v.tipOpen = !v.tipOpen
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(v.title),
		layout.Rigid(func(gtx C) D {
			return v.lede(gtx, "Pick a video and start the party. When anyone pauses or seeks, everyone's player follows.")
		}),
		layout.Rigid(v.tabRow),
		layout.Rigid(func(gtx C) D {
			switch v.tab {
			case "history":
				return v.historyTab(gtx)
			case "settings":
				return v.settingsTab(gtx)
			}
			p := v.now
			if p == nil {
				p = &partyNow{}
			}
			return v.partyTab(gtx, p)
		}),
	)
}

// tabRow is .tabs: three pills, centred, 6dp apart, the one shown in moss.
func (v *view) tabRow(gtx C) D {
	label := [3]string{"Party", "History", "Settings"}
	var kids []layout.FlexChild
	for i := range v.tabs {
		if i > 0 {
			kids = append(kids, layout.Rigid(layout.Spacer{Width: 6}.Layout))
		}
		kids = append(kids, layout.Rigid(func(gtx C) D {
			s := bTab
			if tabNames[i] == v.tab || v.tab == "" && i == 0 {
				s = bTabOn
			}
			d := v.th.button(gtx, &v.tabs[i], s, false, label[i])
			if i == 2 && v.updateDot {
				// .dot: 8dp berry, 6dp after the word, its middle a little over the text's
				r := gtx.Dp(8)
				x, y := d.Size.X-gtx.Dp(16)-r+gtx.Dp(2), d.Size.Y/2-r/2-gtx.Dp(1)
				rrect(gtx, image.Rect(x, y, x+r, y+r), r/2, art.Berry)
			}
			return d
		}))
	}
	return layout.Inset{Top: -6, Bottom: 24}.Layout(gtx, func(gtx C) D {
		return layout.Center.Layout(gtx, func(gtx C) D { return layout.Flex{}.Layout(gtx, kids...) })
	})
}

// tipButton is the "?" in the card's top-right corner, 14dp in, that opens the tips.
func (v *view) tipButton(gtx C) D {
	sz := gtx.Dp(30)
	gtx.Constraints = layout.Exact(image.Pt(sz, sz))
	return v.tip.Layout(gtx, func(gtx C) D {
		pointer.CursorPointer.Add(gtx.Ops)
		bg := art.Paper
		if v.tip.Hovered() {
			bg = hoverBg
		}
		rrect(gtx, image.Rect(0, 0, sz, sz), sz/2, art.Bark)
		rrect(gtx, image.Rect(0, 0, sz, sz).Inset(gtx.Dp(2)), sz/2, bg)
		if gtx.Focused(&v.tip) {
			ring(gtx, image.Rect(0, 0, sz, sz), sz/2)
		}
		return layout.Center.Layout(gtx, func(gtx C) D {
			return v.th.text(gtx, textStyle{font: displayFont, size: 16, color: art.Bark}, "?")
		})
	})
}

type heart struct {
	at time.Time
	x  float32 // 40-60% across main
}

func newView(th *theme, now time.Time) *view {
	v := &view{th: th, cat: art.Cat(), ground: art.Ground(), start: now, rnd: rand.New(rand.NewPCG(uint64(now.UnixNano()), 1)), party: newPartyTab(), prefs: newSettingsTab(), results: make(chan func(gtx C), 8), wake: func() {}}
	v.list.Axis = layout.Vertical
	return v
}

// t is the animations' clock, in seconds.
func (v *view) t(gtx C) float64 {
	now := gtx.Now
	if !v.frozen.IsZero() {
		now = v.frozen
	}
	return now.Sub(v.start).Seconds()
}

// clock stops the scenes' time when they stop moving and starts it again where it stopped,
// so a turtle carries on from where it was rather than jumping ahead.
func (v *view) clock(gtx C) {
	switch {
	case v.animating() && !v.frozen.IsZero():
		v.start = v.start.Add(gtx.Now.Sub(v.frozen))
		v.frozen = time.Time{}
	case !v.animating() && v.frozen.IsZero():
		v.frozen = gtx.Now
		v.pace.stop()
	}
}

// frame lays out the whole window. It asks for the next frame: the cat and the turtles move.
func (v *view) frame(gtx C) {
	v.sync(gtx)
	v.clock(gtx)
	gtx = v.painted(gtx)
	v.closeTips(gtx)
	v.bg.paint(gtx)
	v.list.Layout(gtx, 1, func(gtx C, _ int) D { return v.page(gtx) })
	v.paintGround(gtx)
	v.paintDialog(gtx)
	v.watchPresses(gtx)
	switch {
	case len(v.hearts) > 0: // they follow a click: smooth whatever it costs
		v.pace.frame(gtx.Now)
		gtx.Execute(op.InvalidateCmd{At: gtx.Now.Add(frameGap)})
	case v.animating():
		v.pace.frame(gtx.Now)
		gtx.Execute(op.InvalidateCmd{At: gtx.Now.Add(v.pace.next())})
	}
	gtx.Values[pathsKey].(*pathCache).next()
	for _, f := range v.after {
		f()
	}
	v.after = v.after[:0]
}

// frameGap is how often the window draws while something moves: 30 fps, half of what
// software rendering (no GPU) would otherwise spend
const frameGap = time.Second / 30

// moving is whether the next frame differs from this one by itself: the scenes animate, or
// hearts float up. Everything else that changes with time asks for its own frame.
func (v *view) moving() bool {
	return v.animating() || len(v.hearts) > 0
}

// animating is whether the scenes move: only while the window has the focus, as a frame costs
// the whole window (gui/CLAUDE.md), and not when the OS asks for reduced motion.
func (v *view) animating() bool { return v.focused && !v.still }

// scene is the mood the scenes are drawn in this frame.
func (v *view) scene() art.Mood {
	m := v.mood
	m.Still = v.still
	return m
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
			return v.body(gtx)
		}
		return v.card(gtx)
	})
	card := m.Stop()
	t := op.Offset(image.Pt(x, top)).Push(gtx.Ops)
	card.Add(gtx.Ops)
	tb := op.Offset(image.Pt(mainW-gtx.Dp(14+30), gtx.Dp(14))).Push(gtx.Ops)
	v.tipButton(gtx)
	tb.Pop()
	t.Pop()

	v.paintCat(gtx, W)
	v.paintHearts(gtx, x, mainW)
	v.paintTips(gtx, x, mainW)
	return D{Size: image.Pt(W, top+cd.Size.Y+gtx.Dp(150))}
}

// paintCat draws Fosdo2 180dp wide, centred, 9dp from the top, and its z's while it sleeps.
func (v *view) paintCat(gtx C, W int) {
	px := float64(gtx.Metric.PxPerDp)
	w := 180 * px
	left, top := float64(W)/2-w/2, 9*px
	base := art.Translate(left, top).Mul(art.Scale(w/200, w/200))
	paintArt(asDrawn(gtx), v.cat.Frame(v.t(gtx), v.scene(), base))
	if !v.mood.Awake {
		zs := art.Zzz(v.t(gtx))
		if v.still {
			zs = art.ZzzStill()
		}
		for _, z := range zs {
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
	paint.LinearGradientOp{Stop1: f32.Pt(0, float32(H-fade)), Color1: shade(gtx, clear), Stop2: f32.Pt(0, float32(H-fade)+float32(fade)*.75), Color2: shade(gtx, art.Lichen)}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	r.Pop()
	paint.FillShape(gtx.Ops, art.Soil, clip.Rect{Min: image.Pt(0, H-gtx.Dp(22)), Max: image.Pt(W, H)}.Op())
	px := float64(gtx.Metric.PxPerDp)
	at := func(x float64) art.Mat { return art.Translate(x, float64(H)-130*px).Mul(art.Scale(px, px)) }
	// wider than the strip: the soil stretches from the left edge, the turtles stroll the
	// whole width, and the plants repeat every 560 from the middle out
	m, middle := v.scene(), float64(W)/2-280*px
	m.Width = max(float64(W)/px, 560)
	left := min(0, middle)
	n := int(math.Ceil((m.Width/2 - 280) / 560))
	draw := func(l art.Layer, x float64) {
		m.Layer = l
		paintArt(asDrawn(gtx), v.ground.Frame(v.t(gtx), m, at(x)))
	}
	draw(art.SoilLayer, left)
	for k := -n; k <= n; k++ {
		draw(art.PlantLayer, middle+float64(k)*560*px)
	}
	draw(art.TurtleLayer, left)
}

// background is body's: lichen under three soft glows (radial-gradients, fixed to the
// window). Gio paints no radial gradients, so it is an image, made again when the window
// changes size, at half resolution and scaled up smooth.
type background struct {
	size image.Point
	pal  palette
	img  paint.ImageOp
}

func (b *background) paint(gtx C) {
	size, pal := gtx.Constraints.Max, palOf(gtx)
	if size != b.size || !samePalette(pal, b.pal) {
		b.size, b.pal, b.img = size, pal, paint.NewImageOp(glows(gtx, size.X/2+1, size.Y/2+1))
		b.img.Filter = paint.FilterLinear
	}
	s := float32(size.X) / float32(size.X/2+1)
	defer op.Affine(f32.Affine2D{}.Scale(f32.Pt(0, 0), f32.Pt(s, float32(size.Y)/float32(size.Y/2+1)))).Push(gtx.Ops).Pop()
	b.img.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
}

// glows paints body's background at w×h: each glow is an ellipse whose colour fades to
// nothing at stop of its radius, the first listed on top, as CSS layers them.
func glows(gtx C, w, h int) *image.RGBA {
	type glow struct {
		cx, cy, rx, ry float64 // fractions of the window
		c              color.NRGBA
		alpha, stop    float64
	}
	layers := []glow{ // bottom first
		{.5, 1, .8, .3, shade(gtx, art.Moss), .22, .75},
		{.92, .3, .45, .3, shade(gtx, glowLight), .4, .7},
		{.12, .06, .6, .35, shade(gtx, glowLight), .6, .7},
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			bg := shade(gtx, art.Lichen)
			r, g, b := float64(bg.R), float64(bg.G), float64(bg.B)
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
				// on the baseline, as inline boxes are: the mushroom 3dp under it (vertical-align)
				return baselineRow(gtx,
					func(gtx C) D {
						if !mush {
							return D{}
						}
						d := iconWith(gtx, art.Mushroom, art.FillStyle(art.Porcini), 30, 8)
						d.Baseline = gtx.Dp(3)
						return d
					},
					func(gtx C) D {
						st := tTitle
						st.align = text.Start
						return v.th.text(gtx, st, s)
					},
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
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		return layout.N.Layout(gtx, func(gtx C) D {
			gtx.Constraints.Max.X = min(gtx.Constraints.Max.X, gtx.Sp(40*ch))
			return v.th.text(gtx, tLede, s)
		})
	})
}
