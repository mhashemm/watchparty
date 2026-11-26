package main

import (
	"image"
	"math"
	"strings"

	gioevent "gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/x/richtext"

	"github.com/mhashemm/watchparty/gui/art"
)

// tip is a section of "How to use this": a heading, an optional line, and its points, where
// <b>…</b> is bold.
type tip struct {
	title, line string
	points      []string
}

var tips = []tip{
	{title: "Start a party", points: []string{
		"Pick a video and press <b>Start the party</b>.",
		"Send your friends the invite that appears.",
	}},
	{title: "Join a party", points: []string{
		"Paste the invite into <b>Got an invite?</b>, pick the same video, and start.",
		"Everyone needs the same file. A renamed copy is fine; a different encode or cut is refused.",
	}},
	{title: "Watch together", points: []string{
		"Pause, seek, resume and change the speed in mpv as usual, and everyone follows.",
		"There is no host, so anyone can drive.",
	}},
	{title: "Automatic pauses", line: "The party pauses by itself, and anyone can resume, when someone:", points: []string{
		"seeks while playing",
		"changes the speed while playing",
		"runs out of loaded video",
		"joins, and they start where the party is",
		"leaves, unless the video is in its last few seconds",
		"loses the relay, unless the video is in its last few seconds: they stop at once, and everyone stops when they are back",
		"moves to another video in a playlist",
	}},
	{title: "Who's in the party", points: []string{
		"Vitals lists everyone in the party now, and since when. Anyone who was in before you shows as already here.",
	}},
	{title: "Chat", points: []string{
		"Type in <b>Say something</b> and press Enter. It shows in the top-right corner of everyone's mpv for a few seconds, and under the box here. A message holds up to 200 characters.",
		"Your own messages are green, so nobody can pass for you by taking your name.",
		"Messages are never saved: not in your history, not in the Patient chart, and not by the relay, which only passes them on. Use a <b>wss://</b> relay so nobody else on the network can read them. They leave this page when the party ends.",
	}},
	{title: "Reactions", points: []string{
		"Press the smiley beside <b>Send</b>, or move the mouse over mpv and click the smiley in its bottom-right corner. Pick an emoji, scrolling for more: it bursts across everyone's mpv and shows under the box here.",
		"Each person can react once every 2 seconds. Like messages, reactions are never saved.",
		"<b>Reaction picker on mpv</b> in Settings sets when mpv's smiley shows: when the mouse moves, when it moves near its corner, or never.",
		"The emoji art is <b>Twemoji</b> (github.com/jdecked/twemoji, CC-BY 4.0), redrawn as shapes for mpv.",
	}},
	{title: "Slow networks", points: []string{
		"When a link or torrent runs out of loaded video, the party pauses, and everyone sees how many seconds that person has ready, here and on mpv's screen.",
		"Waiting helps, as their player keeps loading while paused. Resume once they have enough.",
		"A file on your own computer never counts as slow.",
	}},
	{title: "Links", points: []string{
		"File also takes a video's URL. Everyone uses the same URL.",
	}},
	{title: "Playlists", points: []string{
		"Open a video and tick the box under File. It starts on that video, with the rest of its folder before and after it.",
		"Back and next in mpv move everyone, and everyone stops at the start of the new video.",
		"Everyone needs the same videos in the same order. An invite ticks the box for you.",
	}},
	{title: "Torrents", points: []string{
		"File also takes a <b>.torrent</b> or a magnet link. Everyone uses the same one.",
		"It plays while it downloads. Several videos play as a playlist, and only the current one downloads.",
		"What you watched is deleted when you leave, so the disk needs room until then. You can choose where it goes with <b>Cache folder</b> in Settings.",
		"Other people on the torrent can see your IP address, and you upload to them while you watch.",
	}},
	{title: "Connecting", points: []string{
		"<b>Over the internet</b> goes through a relay, and works from anywhere. Starting a party on a relay needs its create key: add it in Settings. Joining needs none.",
		"<b>Peer-to-Peer</b> connects straight to your friends, with no relay. Untick its Wi-Fi box to reach them through your router instead.",
	}},
}

var (
	tTip        = textStyle{font: bodyFont, size: 14.4, color: art.Forest, lineHeight: 1.4}
	tTipHeading = textStyle{font: displayFont, size: 16, color: art.Soil}
)

// tipsState is what the tips popover remembers: its scroll and each point's text state.
type tipsState struct {
	list  widget.List
	texts map[string]*richtext.InteractiveText
	// area is where it was drawn, in the window: a click anywhere else closes it
	area image.Rectangle
}

// paintTips draws "How to use this" over the card, 52dp down, 14dp in from main's sides, at
// most 600dp tall or the window less 340dp, scrolling inside.
func (v *view) paintTips(gtx C, mainX, mainW int) {
	if !v.tipOpen {
		return
	}
	s := &v.tips
	s.list.Axis = layout.Vertical
	if s.texts == nil {
		s.texts = map[string]*richtext.InteractiveText{}
	}
	w := mainW - gtx.Dp(28)
	maxH := min(gtx.Dp(600), gtx.Constraints.Max.Y-gtx.Dp(340))
	g := gtx
	g.Constraints = layout.Constraints{Min: image.Pt(w, 0), Max: image.Pt(w, maxH)}
	box := boxStyle{bg: art.Paper, border: art.Bark, borderW: 2, radius: 16, shadow: 6, shadowColor: art.Bark, wide: true}
	o := op.Offset(image.Pt(mainX+gtx.Dp(14), gtx.Dp(190+52))).Push(gtx.Ops)
	m := op.Record(gtx.Ops)
	d := v.th.box(g, box, func(gtx C) D {
		// clipped to the rounded inside, as overflow: auto clips it
		gtx.Constraints.Max.Y = maxH - gtx.Dp(4)
		return s.list.Layout(gtx, 1, func(gtx C, _ int) D {
			return layout.Inset{Top: 16, Bottom: 8, Left: 16, Right: 16}.Layout(gtx, v.tipText)
		})
	})
	call := m.Stop()
	around(gtx, box, d.Size, false, call)
	// it takes the clicks and the wheel over it, not the card under it
	blockInput(gtx, d.Size)
	o.Pop()
	s.area = image.Rectangle{Max: d.Size}.Add(image.Pt(mainX+gtx.Dp(14), gtx.Dp(190+52)))
}

// tipText is the tips' sections. Their margins collapse as CSS's do: h2 14dp above (none for
// the first) and 4 under, points 3 apart, a list's 8 under folding into the next h2's 14.
// They stack at fractional heights, as Chromium's do: a 20.16px line each is 20 rounded, and
// two dozen points would drift 4px up.
func (v *view) tipText(gtx C) D {
	lb := v.th.lineBox(gtx, tTip)
	// exactly is how tall text that laid out h tall is: whole lines of lb.exact
	exactly := func(h int) float32 {
		n := 1 + int(math.Round(float64(float32(h-lb.height)/lb.exact)))
		return float32(n) * lb.exact
	}
	var y float32
	put := func(gap float32, w layout.Widget, tip bool) {
		y += gap
		o := op.Offset(image.Pt(0, int(math.Round(float64(y))))).Push(gtx.Ops)
		d := w(gtx)
		o.Pop()
		if tip {
			y += exactly(d.Size.Y)
		} else {
			y += float32(d.Size.Y)
		}
	}
	for i, t := range tips {
		gap := float32(gtx.Dp(14))
		if i == 0 {
			gap = 0
		}
		put(gap, func(gtx C) D { return v.th.text(gtx, tTipHeading, t.title) }, false)
		gap = float32(gtx.Dp(4))
		if t.line != "" {
			put(gap, func(gtx C) D { return v.th.text(gtx, tTip, t.line) }, true)
		}
		for j, p := range t.points {
			if j > 0 {
				gap = float32(gtx.Dp(3))
			}
			put(gap, func(gtx C) D { return v.bullet(gtx, p) }, true)
		}
	}
	y += float32(gtx.Dp(8))
	return D{Size: image.Pt(gtx.Constraints.Max.X, int(math.Ceil(float64(y))))}
}

// bullet is a list point: a disc in the 18dp margin, on the first line, then its text.
func (v *view) bullet(gtx C, text string) D {
	s := &v.tips
	st := s.texts[text]
	if st == nil {
		st = new(richtext.InteractiveText)
		s.texts[text] = st
	}
	lb := v.th.lineBox(gtx, tTip)
	d := layout.Inset{Left: 18}.Layout(gtx, func(gtx C) D { return v.richLine(gtx, st, tTip, text) })
	// Chromium's disc: about a third of an em across, its middle near the x-height's
	r := float32(gtx.Sp(tTip.size)) * .17
	cx, cy := float32(gtx.Dp(18))-float32(gtx.Sp(tTip.size))*.9, float32(lb.height-lb.below)-float32(gtx.Sp(tTip.size))*.27
	disc := clip.Ellipse{Min: image.Pt(int(cx-r), int(cy-r)), Max: image.Pt(int(cx+r+.5), int(cy+r+.5))}
	paint.FillShape(gtx.Ops, tTip.color, disc.Op(gtx.Ops))
	return d
}

// richLine is st's text where <b>…</b> is bold, in CSS's line box like theme.text.
func (v *view) richLine(gtx C, state *richtext.InteractiveText, st textStyle, text string) D {
	var spans []richtext.SpanStyle
	for i, part := range strings.Split(text, "<b>") {
		plain := part
		if i > 0 {
			b, rest, _ := strings.Cut(part, "</b>")
			spans = append(spans, richtext.SpanStyle{Font: boldFont, Color: st.color, Content: b})
			plain = rest
		}
		if plain != "" {
			spans = append(spans, richtext.SpanStyle{Font: st.font, Color: st.color, Content: plain})
		}
	}
	gtx.Constraints.Min.X = 0
	lb := v.th.lineBox(gtx, st)
	d := lb.place(gtx, func(gtx C, size, lineHeight unit.Sp) D {
		for i := range spans {
			spans[i].Size = size
		}
		rt := richtext.Text(state, v.th.shaper, spans...)
		rt.LineHeight, rt.LineHeightScale = lineHeight, 1
		m := op.Record(gtx.Ops)
		d := rt.Layout(gtx)
		call := m.Stop()
		call.Add(gtx.Ops)
		o := paint.PushOpacity(gtx.Ops, inkAgain)
		call.Add(gtx.Ops)
		o.Pop()
		return d
	}, st.size)
	d.Size.Y -= lb.height - lb.glyphs
	return d
}

// closeTips closes the tips on Escape, or on a press anywhere but on them or the "?": every
// press passes through watchPresses' area on its way.
func (v *view) closeTips(gtx C) {
	if !v.tipOpen {
		return
	}
	for {
		ev, ok := gtx.Event(key.Filter{Name: key.NameEscape})
		if !ok {
			break
		}
		if e, ok := ev.(key.Event); ok && e.State == key.Press {
			v.tipOpen = false
		}
	}
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: &v.tips, Kinds: pointer.Press})
		if !ok {
			break
		}
		// the area is in the page's coordinates, which scroll under the window's
		area := v.tips.area.Sub(image.Pt(0, v.list.Position.Offset))
		if e, ok := ev.(pointer.Event); ok && !image.Pt(int(e.Position.X), int(e.Position.Y)).In(area) && !v.tip.Hovered() {
			v.tipOpen = false
		}
	}
}

// watchPresses puts an area over the whole window that sees every press and passes it on, for
// closeTips; it goes on top of everything else.
func (v *view) watchPresses(gtx C) {
	if !v.tipOpen {
		return
	}
	defer clip.Rect{Max: gtx.Constraints.Max}.Push(gtx.Ops).Pop()
	defer pointer.PassOp{}.Push(gtx.Ops).Pop()
	gioevent.Op(gtx.Ops, &v.tips)
}
