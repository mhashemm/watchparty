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

// dark: the global AppleInterfaceStyle, "Dark" while the appearance is dark, Auto included;
// readable from any thread, unlike NSApp's effectiveAppearance.
static int dark(void) {
	@autoreleasepool {
		NSString *s = [[NSUserDefaults standardUserDefaults] stringForKey:@"AppleInterfaceStyle"];
		return [s caseInsensitiveCompare:@"Dark"] == NSOrderedSame ? 1 : 0;
	}
}
*/
import "C"

import "context"

// watchLook calls set with System Settings' Reduce motion (Accessibility > Display) and
// Appearance, now and whenever they change, until ctx ends.
func watchLook(ctx context.Context, set func(look)) {
	pollLook(ctx, func() look { return look{still: C.reduceMotion() == 1, dark: C.dark() == 1} }, set)
}
