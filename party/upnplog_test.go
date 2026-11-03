package party

import (
	"bytes"
	"log"
	"log/slog"
	"testing"
)

func TestUpnpLoggerFormatsInfoAndDropsDebug(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := slog.New(upnpLogger{l: log.New(&buf, "", 0)})

	l.Debug("soap request", "body", "<xml/>")
	if buf.Len() != 0 {
		t.Fatalf("debug should be dropped, got %q", buf.String())
	}

	l.Info("port mapping added", "external", 6969, "protocol", "TCP")
	if got, want := buf.String(), "upnp: port mapping added external=6969 protocol=TCP\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
