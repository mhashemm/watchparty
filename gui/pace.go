package main

import "time"

// pacer spaces the scenes' frames by what a frame costs on this machine: each one redraws the
// whole window (gui/CLAUDE.md), ~3 ms of CPU on a GPU but ~80 ms on software rendering, so a
// fixed 30 fps is cheap on one and takes cores on the other. It measures the process's CPU
// time, never a frame's wall time: presenting a frame can wait for the display's refresh,
// which would make a fast machine look slow.
type pacer struct {
	// cpu is the process's CPU time so far, all threads; nil paces at frameGap
	cpu func() time.Duration
	// since, cpuSince and frames: the measurement under way; zero when none is
	since    time.Time
	cpuSince time.Duration
	frames   int
	// gap is the pause between the scenes' frames, from the last measurement
	gap time.Duration
}

const (
	// paceBudget is the share of one core the scenes may take
	paceBudget = 0.10
	// paceEvery is how long a measurement runs: OS CPU clocks tick in 1-16 ms, so a few
	// frames' worth is needed for a fair figure
	paceEvery = 2 * time.Second
	// paceSlowest is the longest pause: the scenes still move once a second
	paceSlowest = time.Second
)

// frame counts a frame drawn while the scenes move, and when a measurement has run its time,
// sets the pause to what a frame cost divided by the budget.
func (p *pacer) frame(now time.Time) {
	if p.cpu == nil {
		return
	}
	if p.since.IsZero() {
		p.since, p.cpuSince, p.frames = now, p.cpu(), 0
		return
	}
	p.frames++
	if now.Sub(p.since) < paceEvery {
		return
	}
	cpu := p.cpu()
	perFrame := (cpu - p.cpuSince) / time.Duration(p.frames)
	p.gap = min(max(time.Duration(float64(perFrame)/paceBudget), frameGap), paceSlowest)
	p.since, p.cpuSince, p.frames = now, cpu, 0
}

// stop ends the measurement: the scenes stopped, and CPU spent meanwhile isn't theirs.
func (p *pacer) stop() { p.since = time.Time{} }

// next is the pause before the scenes' next frame.
func (p *pacer) next() time.Duration { return max(p.gap, frameGap) }
