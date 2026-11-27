package main

import (
	"image"
	"testing"

	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/unit"
)

// A shape is kept from the second frame in a row that draws it, dropped from the first that
// doesn't, and the op list starts over only before a frame draws.
func TestPathCacheKeepsWhatRepeats(t *testing.T) {
	t.Parallel()
	c := newPathCache()
	var frameOps op.Ops
	gtx := layout.Context{Ops: &frameOps, Values: map[string]any{pathsKey: c}}
	builds := 0
	draw := func(key uint64) {
		shape(gtx, key, func(ops *op.Ops) clip.Op {
			builds++
			return clip.Rect{Max: image.Pt(1, 1)}.Op()
		})
	}
	draw(1)
	c.next()
	draw(1) // the second frame in a row: built once more, now kept
	c.next()
	draw(1)
	c.next()
	if builds != 2 {
		t.Fatalf("drawn three frames in a row: built %d times, want 2", builds)
	}
	c.next() // a frame without it
	draw(1)
	if builds != 3 {
		t.Fatalf("after a frame without it: built %d times, want 3 (dropped, so not kept)", builds)
	}
	for i := range 200 { // churn: 100 shapes kept, then dropped; no frame has begun since
		draw(uint64(1000 + i/2))
		c.next()
	}
	if !c.stale {
		t.Fatal("100 dropped shapes and 1 kept: not marked to start over")
	}
	c.begin()
	if c.stale || len(c.kept) != 0 || c.dead != 0 {
		t.Fatalf("begin left it as it was: stale %v, %d kept", c.stale, len(c.kept))
	}
}

// The frame that marks the cache to start over refers to it until Gio has drawn it and the
// input router has read it, after the layout returns: the cache must still be whole then.
func TestCachedShapesOutliveTheirFrame(t *testing.T) {
	t.Parallel()
	sh, err := newShaper()
	if err != nil {
		t.Fatal(err)
	}
	s := scenes()["party-form"]
	s.height = 0
	v := s.view(&theme{shaper: sh, clock: clock{hour12: true}})
	var ops op.Ops
	var router input.Router
	frame := func() {
		ops.Reset()
		gtx := layout.Context{Ops: &ops, Now: t0, Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}, Constraints: layout.Exact(s.size()), Source: router.Source()}
		v.frame(gtx)
		router.Frame(&ops) // as the window does once frame returns
	}
	frame()
	frame() // the card's shapes are kept now, and this frame's ops refer to them
	c := v.values[pathsKey].(*pathCache)
	if len(c.kept) == 0 {
		t.Fatal("nothing kept after two frames")
	}
	c.dead = 1 << 20 // as after a long animation: the next frame marks it to start over
	frame()
	frame()
}
