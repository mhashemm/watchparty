package main

import (
	"image"
	"testing"
	"time"

	"gioui.org/gpu/headless"
	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
)

// BenchmarkFrame is what one animation frame costs in the real 560×940 window, mid-party:
// laying the window out (frame) and drawing it (Gio's GPU path; Mesa's llvmpipe in the
// container). Run: EGL_PLATFORM=surfaceless go test ./gui -run - -bench Frame
func BenchmarkFrame(b *testing.B) {
	sh, err := newShaper()
	if err != nil {
		b.Fatal(err)
	}
	for _, name := range []string{"party-form", "party-in"} {
		b.Run(name, func(b *testing.B) {
			s := scenes()[name]
			s.height = 0
			size := s.size()
			win, err := headless.NewWindow(size.X, size.Y)
			if err != nil {
				b.Skip("no headless GPU:", err)
			}
			defer win.Release()
			v := s.view(&theme{shaper: sh, clock: clock{hour12: true}})
			v.focused = true // animating
			var ops op.Ops
			var router input.Router
			px := image.NewRGBA(image.Rect(0, 0, 1, 1))
			var lay, draw time.Duration
			now := t0.Add(s.at)
			cpu0 := processCPU()
			b.ResetTimer()
			for b.Loop() {
				now = now.Add(frameGap)
				ops.Reset()
				gtx := layout.Context{Ops: &ops, Now: now, Metric: unit.Metric{PxPerDp: 1, PxPerSp: 1}, Constraints: layout.Exact(size), Source: router.Source()}
				t := time.Now()
				v.frame(gtx)
				router.Frame(&ops)
				lay += time.Since(t)
				t = time.Now()
				if err := win.Frame(&ops); err != nil {
					b.Fatal(err)
				}
				win.Screenshot(px) // waits for the GPU
				draw += time.Since(t)
			}
			n := float64(b.N)
			if cpu := processCPU() - cpu0; cpu > 0 {
				b.ReportMetric(float64(cpu.Microseconds())/n/1000, "cpu-ms/frame")
			}
			b.ReportMetric(float64(lay.Microseconds())/n/1000, "layout-ms/frame")
			b.ReportMetric(float64(draw.Microseconds())/n/1000, "draw-ms/frame")
		})
	}
}
