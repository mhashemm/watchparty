//go:build !windows

package mpv

import (
	"context"
	"net"
)

const SocketPrefix = "/tmp/"

func newConnection(c context.Context, socket string) (*connection, error) {
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(c, "unix", socket)
	if err != nil {
		return nil, err
	}

	return &connection{
		conn: conn,
	}, nil
}
