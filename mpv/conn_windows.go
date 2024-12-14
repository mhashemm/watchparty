//go:build windows

package mpv

import (
	"bufio"
	"context"
	"net"
	"sync"
)

const SocketPrefix = "\\\\.\\pipe\\"

type connection struct {
	scanner *bufio.Scanner
	mu      sync.Mutex
}

func newConnection(c context.Context, socket string) (*connection, error) {
	dialer := &net.Dialer{}
	cunt, err := dialer.DialContext(c, "unix", socket)
	if err != nil {
		return nil, err
	}

	return &connection{
		scanner: bufio.NewScanner(cunt),
	}, nil
}

func (c *connection) request(req []byte) error {
	return nil
}
