package party

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"unicode"
)

type upnpLogger struct{ l *log.Logger }

func (upnpLogger) Enabled(_ context.Context, lvl slog.Level) bool { return lvl >= slog.LevelInfo }
func (h upnpLogger) WithAttrs([]slog.Attr) slog.Handler           { return h }
func (h upnpLogger) WithGroup(string) slog.Handler                { return h }

func (h upnpLogger) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString("upnp: ")
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	h.l.Println(strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return ' '
		}
		return c
	}, b.String()))
	return nil
}
