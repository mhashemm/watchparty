package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework AppKit
#import <AppKit/AppKit.h>

static int reduceMotion(void) {
	@autoreleasepool {
		return [[NSWorkspace sharedWorkspace] accessibilityDisplayShouldReduceMotion] ? 1 : 0;
	}
}
*/
import "C"

import "context"

// reduceMotion is System Settings' Accessibility > Display > Reduce motion.
func reduceMotion() bool { return C.reduceMotion() == 1 }

// watchMotion calls set with reduceMotion now and whenever it changes, until ctx ends.
func watchMotion(ctx context.Context, set func(still bool)) {
	pollMotion(ctx, reduceMotion, set)
}
