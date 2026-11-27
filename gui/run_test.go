package main

import (
	"context"
	"image"
	"strings"
	"testing"
	"time"

	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
)

// fakeApp is a backend whose news is whatever the test sets, and which counts the store reads.
type fakeApp struct {
	n     news
	reads int
}

func (f *fakeApp) start(form) error                       { return nil }
func (f *fakeApp) stop()                                  {}
func (f *fakeApp) chat(string) error                      { return nil }
func (f *fakeApp) react(string) error                     { return nil }
func (f *fakeApp) pick(string) (string, error)            { return "", nil }
func (f *fakeApp) saveChart() (string, error)             { return "", nil }
func (f *fakeApp) saveSettings(settings) error            { return nil }
func (f *fakeApp) clearHistory() error                    { return nil }
func (f *fakeApp) install() (string, error)               { return "", nil }
func (f *fakeApp) restart() error                         { return nil }
func (f *fakeApp) latest(context.Context) (string, error) { return "", nil }
func (f *fakeApp) tools(context.Context) []tool           { return nil }
func (f *fakeApp) state() appState                        { f.reads++; return appState{} }
func (f *fakeApp) news(run, from, saidFrom int) news {
	n := f.n
	if run == n.Run {
		n.Lines, n.Said = n.Lines[min(from-n.From, len(n.Lines)):], n.Said[min(saidFrom-n.SaidFrom, len(n.Said)):]
		n.From, n.SaidFrom = from, saidFrom
	}
	return n
}

// The window reads each party from its first line, keeps its chat only while it runs, and
// reads the store again when it ends (its history entry closed).
func TestTheWindowFollowsEachParty(t *testing.T) {
	t.Parallel()
	f := &fakeApp{}
	v := newView(&theme{}, time.Unix(0, 0))
	v.app = f
	gtx := layout.Context{Ops: new(op.Ops), Now: time.Unix(10, 0), Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}}
	v.sync(gtx)
	if v.now != nil {
		t.Fatal("vitals before any party")
	}
	f.n = news{Run: 1, Lines: []string{"2026/11/27 10:00:00 a", "2026/11/27 10:00:01 b"}, Said: []said{{Who: "sam", Text: "hi"}}, Running: true}
	v.sync(gtx)
	v.sync(gtx)
	if p := v.now; p == nil || !p.ran || strings.Join(p.lines, "|") != strings.Join(f.n.Lines, "|") || len(p.said) != 1 {
		t.Fatalf("first party: %+v", v.now)
	}
	reads := f.reads
	f.n.Running = false
	v.sync(gtx)
	if len(v.now.said) != 0 || f.reads != reads+1 {
		t.Fatalf("at its end: %d messages kept, %d store reads", len(v.now.said), f.reads-reads)
	}
	f.n = news{Run: 2, Lines: []string{"2026/11/27 11:00:00 c"}, Running: true}
	v.sync(gtx)
	if strings.Join(v.now.lines, "|") != "2026/11/27 11:00:00 c" {
		t.Fatalf("second party: %q", v.now.lines)
	}
}

// Work run with async is applied on the window's goroutine, in the frame after it finished.
func TestAsyncResultsWaitForAFrame(t *testing.T) {
	t.Parallel()
	v := newView(&theme{}, time.Unix(0, 0))
	woke := make(chan struct{}, 1)
	v.wake = func() { woke <- struct{}{} }
	got := ""
	v.async(func() func(gtx C) { return func(gtx C) { got = "done" } })
	<-woke
	if got != "" {
		t.Fatal("applied before a frame")
	}
	v.sync(layout.Context{Ops: new(op.Ops)})
	if got != "done" {
		t.Fatal("not applied in the frame")
	}
}

// A still window asks for no frame by itself: only hearts floating up, and timed changes (the
// copied label going back) wake it, each when due; moving scenes ask at 30 fps.
func TestTheWindowDrawsOnlyWhatChanges(t *testing.T) {
	t.Parallel()
	sh, err := newShaper()
	if err != nil {
		t.Fatal(err)
	}
	v := newView(&theme{shaper: sh}, time.Unix(0, 0))
	now := time.Unix(100, 0)
	wakeup := func() (time.Duration, bool) {
		var router input.Router
		var ops op.Ops
		gtx := layout.Context{Ops: &ops, Now: now, Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}, Constraints: layout.Exact(image.Pt(560, 940)), Source: router.Source()}
		v.frame(gtx)
		router.Frame(&ops)
		at, ok := router.WakeupTime()
		return at.Sub(now), ok
	}
	if d, ok := wakeup(); !ok || d != frameGap {
		t.Fatalf("moving: next frame in %v (%v), want %v", d, ok, frameGap)
	}
	v.still = true
	if d, ok := wakeup(); ok {
		t.Fatalf("still: asked for a frame in %v", d)
	}
	v.hearts = []heart{{at: now, x: .5}}
	if d, ok := wakeup(); !ok || d != frameGap {
		t.Fatalf("still with a heart: next frame in %v (%v), want %v", d, ok, frameGap)
	}
	v.hearts = nil
	v.now = &partyNow{ran: true, running: true, scr: screen{running: true, invite: "-relay ws://r -room R -secret S"}, w: newWatch(form{})}
	v.party.copyLabel, v.party.copiedAt = "Copied", now.Add(-time.Second)
	if d, ok := wakeup(); !ok || d != time.Second {
		t.Fatalf("copied a second ago: next frame in %v (%v), want 1s", d, ok)
	}
}
