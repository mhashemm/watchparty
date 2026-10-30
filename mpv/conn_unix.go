//go:build !windows

package mpv

import (
	"context"
	"net"
	"os/exec"
)

const SocketPrefix = "/tmp/"

func Dial(c context.Context, socket string) (net.Conn, error) {
	dialer := &net.Dialer{}
	return dialer.DialContext(c, "unix", socket)
}

func Init(_ *exec.Cmd) {}
