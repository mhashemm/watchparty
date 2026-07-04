//go:build windows

package mpv

import (
	"context"
	"os/exec"
	"syscall"

	win "github.com/Microsoft/go-winio"
)

const SocketPrefix = "\\\\.\\pipe\\"

func newConnection(c context.Context, socket string) (*connection, error) {
	conn, err := win.DialPipeContext(c, socket)
	if err != nil {
		return nil, err
	}
	return &connection{
		conn: conn,
	}, nil
}

func Init(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}
