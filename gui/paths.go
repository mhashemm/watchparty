package main

import (
	"math"

	"gioui.org/op"
	"gioui.org/op/clip"
)

// pathCache keeps the outlines of shapes drawn frame after frame in an op list that outlives
// the frame. Gio's renderer caches a path's vertices by its op list and that list's version,
// which every frame's Reset bumps, so a path built in the frame's ops is outlined and built
// again each frame (gui/CLAUDE.md); one kept here is built once. A shape is kept from the
// second frame in a row that draws it, so one that changes every frame (a turtle walking, the
// heartbeat's dash) never is. Only the drawing is cached: layout still runs every frame, so
// every widget still gets its input.
type pathCache struct {
	ops  op.Ops
	kept map[uint64]clip.Op
	// used: kept shapes drawn this frame; last, now: shapes drawn but not kept, in the frame
	// before and in this one
	used, last, now map[uint64]bool
	// dead is how many kept shapes were dropped, their paths still in ops; stale, that ops
	// starts over before the next frame draws
	dead  int
	stale bool
}

func newPathCache() *pathCache {
	return &pathCache{kept: map[uint64]clip.Op{}, used: map[uint64]bool{}, last: map[uint64]bool{}, now: map[uint64]bool{}}
}

// pathsKey is where the window's pathCache is, in gtx.Values
const pathsKey = "paths"

// shape is the clip op for the shape key names: kept, or built by build into the frame's ops.
func shape(gtx C, key uint64, build func(ops *op.Ops) clip.Op) clip.Op {
	c, _ := gtx.Values[pathsKey].(*pathCache)
	if c == nil {
		return build(gtx.Ops)
	}
	if o, ok := c.kept[key]; ok {
		c.used[key] = true
		return o
	}
	if c.last[key] {
		o := build(&c.ops)
		c.kept[key], c.used[key] = o, true
		return o
	}
	c.now[key] = true
	return build(gtx.Ops)
}

// next ends a frame: a kept shape it didn't draw is dropped, and once ops holds more dropped
// shapes than kept ones it is marked to start over.
func (c *pathCache) next() {
	for k := range c.kept {
		if !c.used[k] {
			delete(c.kept, k)
			c.dead++
		}
	}
	clear(c.used)
	c.last, c.now = c.now, c.last
	clear(c.now)
	if c.dead > 64 && c.dead > len(c.kept) {
		c.stale = true
	}
}

// begin starts a frame. ops starts over only here, not in next: the frame just laid out
// refers to it until Gio has drawn it and routed its input, after the layout returns.
func (c *pathCache) begin() {
	if c.stale {
		c.ops.Reset()
		clear(c.kept)
		c.dead, c.stale = 0, false
	}
}

// shapeKey hashes what a shape is drawn from: 64-bit FNV-1a, a value, so making one
// allocates nothing (a maphash.Hash did, every shape every frame).
type shapeKey uint64

func newShapeKey(kind byte) shapeKey { return shapeKey(14695981039346656037).mix(uint64(kind)) }

func (k shapeKey) mix(v uint64) shapeKey {
	for range 8 {
		k ^= shapeKey(v & 0xff)
		k *= 1099511628211
		v >>= 8
	}
	return k
}

func (k shapeKey) f(v float64) shapeKey { return k.mix(math.Float64bits(v)) }

func (k shapeKey) i(v int) shapeKey { return k.f(float64(v)) }

func (k shapeKey) sum() uint64 { return uint64(k) }
