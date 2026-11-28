package main

import (
	"strings"
	"time"
	"unicode"
)

// clock is how the window writes a time of day: as the computer does, 12 or 24 hours
// (hour12, one per OS), the way the page's toLocaleTimeString did.
type clock struct{ hour12 bool }

func (c clock) format(t time.Time) string {
	if c.hour12 {
		return t.Format("3:04 PM")
	}
	return t.Format("15:04")
}

// posix12 reads a POSIX locale's time format (`locale t_fmt`, or t_fmt_ampm's use): 12 hours
// if it shows the hour as %I, %l or %r.
func posix12(tfmt string) bool {
	return strings.Contains(tfmt, "%I") || strings.Contains(tfmt, "%l") || strings.Contains(tfmt, "%r")
}

// shown12 reads a time as the OS wrote it ("9:04:05 PM", "21:04:05"): 12 hours if it has a
// day period after the digits.
func shown12(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool {
		return !unicode.IsDigit(r) && r != ':' && r != '.' && !unicode.IsSpace(r)
	}) >= 0
}

// windows12 reads a Windows short-time picture (LOCALE_SSHORTTIME): "h:mm tt" or "HH:mm".
func windows12(picture string) bool {
	quoted := false
	for _, r := range picture {
		switch {
		case r == '\'':
			quoted = !quoted
		case !quoted && r == 'h':
			return true
		}
	}
	return false
}
