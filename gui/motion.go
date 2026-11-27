package main

import (
	"context"
	"time"
)

// motionEvery is how often Windows and macOS are asked about reduced motion: one cheap call
const motionEvery = 2 * time.Second

// pollMotion calls set with read now and whenever it changes, until ctx ends.
func pollMotion(ctx context.Context, read func() bool, set func(still bool)) {
	last := read()
	set(last)
	t := time.NewTicker(motionEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if still := read(); still != last {
				last = still
				set(still)
			}
		}
	}
}
