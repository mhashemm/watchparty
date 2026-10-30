//go:build windows

package mpv

import (
	"context"
	"net"
	"os/exec"
	"syscall"

	win "github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const SocketPrefix = "\\\\.\\pipe\\"

func Dial(c context.Context, socket string) (net.Conn, error) {
	return win.DialPipeContext(c, socket)
}

func Init(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NEW_CONSOLE,
	}
}
