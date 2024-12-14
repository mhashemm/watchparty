//go:build !windows

package mpv

import (
	"bufio"
	"context"
	"log"
	"net"
	"sync"
)

const SocketPrefix = "/tmp/"

type connection struct {
	conn    net.Conn
	scanner *bufio.Scanner
	mu      sync.Mutex
}

func newConnection(c context.Context, socket string) (*connection, error) {
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(c, "unix", socket)
	if err != nil {
		return nil, err
	}

	return &connection{
		conn:    conn,
		scanner: bufio.NewScanner(conn),
	}, nil
}

func (c *connection) request(req []byte) error {
	req = append(req, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	log.Printf("%s", req)
	_, err := c.conn.Write(req)
	return err
}
