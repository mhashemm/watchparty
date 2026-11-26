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
func paintArt(ops *op.Ops, items []art.Item, dash ...float32) {
	pt := func(p art.Pt) f32.Point { return f32.Pt(float32(p.X), float32(p.Y)) }
	for _, it := range items {
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
			paint.FillShape(ops, it.Color, clip.Outline{Path: p.End()}.Op())
			continue
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
		paint.FillShape(ops, it.Color, s.Op(ops))
	}
}
