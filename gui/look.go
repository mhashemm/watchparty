package main

import (
	"context"
	"time"
)

// look is what the OS asks of how apps look: reduced motion, and a dark appearance.
type look struct{ still, dark bool }

// lookEvery is how often Windows and macOS are asked: a cheap call or two
const lookEvery = 2 * time.Second

// pollLook calls set with read now and whenever it changes, until ctx ends.
func pollLook(ctx context.Context, read func() look, set func(look)) {
	last := read()
	set(last)
	t := time.NewTicker(lookEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if l := read(); l != last {
				last = l
				set(l)
			}
		}
	}
}
