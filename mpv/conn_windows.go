//go:build windows

package mpv

import (
	"bufio"
	"context"
	"log"
	"os"
	"sync"
)

const SocketPrefix = "\\\\.\\pipe\\"

type connection struct {
	file    *os.File
	scanner *bufio.Scanner
	mu      sync.Mutex
}

func newConnection(_ context.Context, socket string) (*connection, error) {
	file, err := os.OpenFile(socket, os.O_RDWR, os.ModeNamedPipe)
	if err != nil {
		return nil, err
	}
	return &connection{
		file:    file,
		scanner: bufio.NewScanner(file),
	}, nil
}

func (c *connection) request(req []byte) error {
	req = append(req, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	log.Printf("%s", req)
	_, err := c.file.Write(req)
	return err
}
