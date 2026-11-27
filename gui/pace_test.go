package main

import (
	"testing"
	"time"
)

// The scenes' frames are spaced so they take a tenth of a core: at 30 fps where a frame is
// cheap, further apart where it isn't, never further than a second.
func TestPacingFollowsWhatAFrameCosts(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		perFrame, want time.Duration
	}{
		{3 * time.Millisecond, frameGap},                // a GPU: 30 fps, as before
		{10 * time.Millisecond, 100 * time.Millisecond}, // 10 fps
		{81 * time.Millisecond, 810 * time.Millisecond}, // llvmpipe
		{300 * time.Millisecond, paceSlowest},
	} {
		var cpu time.Duration
		p := pacer{cpu: func() time.Duration { return cpu }}
		now := time.Unix(100, 0)
		if p.next() != frameGap {
			t.Fatalf("before any measurement: %v, want %v", p.next(), frameGap)
		}
		for now.Before(time.Unix(100, 0).Add(paceEvery + time.Second)) {
			p.frame(now)
			now = now.Add(p.next())
			cpu += c.perFrame
		}
		if got := p.next(); got != c.want {
			t.Errorf("%v a frame: frames %v apart, want %v", c.perFrame, got, c.want)
		}
	}
}

// What the CPU did while the scenes rested isn't counted against their frames.
func TestPacingForgetsTheRest(t *testing.T) {
	t.Parallel()
	var cpu time.Duration
	p := pacer{cpu: func() time.Duration { return cpu }}
	now := time.Unix(100, 0)
	p.frame(now)
	p.stop()
	cpu += time.Minute // a minute of the party's own work, the window unfocused
	now = now.Add(time.Hour)
	for range 100 {
		p.frame(now)
		now = now.Add(p.next())
		cpu += 3 * time.Millisecond
	}
	if got := p.next(); got != frameGap {
		t.Fatalf("3 ms a frame after a rest: frames %v apart, want %v", got, frameGap)
	}
}

// The process's CPU clock goes forward with work on every OS.
func TestProcessCPUAdvances(t *testing.T) {
	t.Parallel()
	start := processCPU()
	x := 0
	for end := time.Now().Add(time.Second); processCPU() == start && time.Now().Before(end); {
		x++
	}
	if processCPU() <= start {
		t.Fatalf("no CPU time counted after %d loops", x)
	}
}
