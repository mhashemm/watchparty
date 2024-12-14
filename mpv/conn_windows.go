//go:build windows

package mpv

import (
	"bufio"
	"context"
	"os"
	"sync"
)

const SocketPrefix = "\\\\.\\pipe\\"

type connection struct {
	pipe    string
	scanner *bufio.Scanner
	mu      sync.Mutex
}

func newConnection(_ context.Context, socket string) (*connection, error) {
	file, err := os.OpenFile(socket, os.O_RDONLY, os.ModeNamedPipe)
	if err != nil {
		return nil, err
	}
	return &connection{
		pipe:    socket,
		scanner: bufio.NewScanner(file),
	}, nil
}

func (c *connection) request(req []byte) error {
	req = append(req, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	file, err := os.OpenFile(c.pipe, os.O_WRONLY, os.ModeNamedPipe)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(req)
	return err
}
