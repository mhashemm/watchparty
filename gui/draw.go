package main

import (
	"gioui.org/f32"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/x/stroke"

	"github.com/mhashemm/watchparty/gui/art"
)

// paintArt paints art's items with Gio's own paths: fills as outlines, strokes with
// gioui.org/x/stroke for round caps and joins. dash, when set, dashes every stroke
// (dash on, dash off, starting offset into the pattern), as SVG's stroke-dasharray.
func paintArt(gtx C, items []art.Item, dash ...float32) {
	for _, it := range items {
		at := op.Affine(f32.AffineId().Offset(f32.Pt(float32(it.At.X), float32(it.At.Y)))).Push(gtx.Ops)
		paint.FillShape(gtx.Ops, shade(gtx, it.Color), shape(gtx, itemKey(it, dash), func(ops *op.Ops) clip.Op { return itemShape(ops, it, dash) }))
		at.Pop()
	}
}

// iconsKey is gtx.Values' icons parsed, by their SVG: parsing one is XML, too dear for
// every frame
const iconsKey = "icons"

func parsedIcon(gtx C, svg string) *art.Icon {
	m, _ := gtx.Values[iconsKey].(map[string]*art.Icon)
	if m == nil {
		return art.ParseIcon(svg)
	}
	ic := m[svg]
	if ic == nil {
		ic = art.ParseIcon(svg)
		m[svg] = ic
	}
	return ic
}

// itemKey hashes everything itemShape draws an item from.
func itemKey(it art.Item, dash []float32) uint64 {
	k := newShapeKey('a')
	if it.Fill {
		k = k.i(1)
	}
	if it.Round {
		k = k.i(2)
	}
	k = k.f(it.Width)
	for _, d := range dash {
		k = k.f(float64(d))
	}
	for _, o := range it.Path {
		k = k.i(int(o.K))
		for _, p := range o.P {
			k = k.f(p.X).f(p.Y)
		}
	}
	return k.sum()
}

// itemShape is an item's outline, built in ops: a fill's path, or a stroke's outline.
func itemShape(ops *op.Ops, it art.Item, dash []float32) clip.Op {
	pt := func(p art.Pt) f32.Point { return f32.Pt(float32(p.X), float32(p.Y)) }
	if it.Fill {
		var p clip.Path
		p.Begin(ops)
		for _, o := range it.Path {
			switch o.K {
			case art.Move:
				p.MoveTo(pt(o.P[0]))
			case art.Line:
				p.LineTo(pt(o.P[0]))
			case art.Quad:
				p.QuadTo(pt(o.P[0]), pt(o.P[1]))
			case art.Cube:
				p.CubeTo(pt(o.P[0]), pt(o.P[1]), pt(o.P[2]))
			case art.Close:
				p.Close()
			}
		}
		return clip.Outline{Path: p.End()}.Op()
	}
	var segs []stroke.Segment
	var start art.Pt
	for _, o := range it.Path {
		switch o.K {
		case art.Move:
			start = o.P[0]
			segs = append(segs, stroke.MoveTo(pt(o.P[0])))
		case art.Line:
			segs = append(segs, stroke.LineTo(pt(o.P[0])))
		case art.Quad:
			segs = append(segs, stroke.QuadTo(pt(o.P[0]), pt(o.P[1])))
		case art.Cube:
			segs = append(segs, stroke.CubeTo(pt(o.P[0]), pt(o.P[1]), pt(o.P[2])))
		case art.Close:
			segs = append(segs, stroke.LineTo(pt(start)))
		}
	}
	s := stroke.Stroke{Path: stroke.Path{Segments: segs}, Width: float32(it.Width), Cap: stroke.FlatCap, Join: stroke.BevelJoin}
	if it.Round {
		s.Cap, s.Join = stroke.RoundCap, stroke.RoundJoin
	}
	if len(dash) == 3 {
		s.Dashes = stroke.Dashes{Phase: dash[2], Dashes: []float32{dash[0], dash[1]}}
	}
	return s.Op(ops)
}
