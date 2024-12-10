package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func connect(path string) *http.Client {
	client := http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				conn, err := net.Dial("unix", path)
				if err != nil {
					panic(err)
				}
				return conn, nil
			},
		},
	}
	return &client
}

func main() {
	c, cancel := signal.NotifyContext(context.TODO(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	// filePath := flag.String("file", "", "file path to play")
	// ips := flag.String("ips", "", "comma seprated list of IPs to connect to")
	mpvSocket := "/tmp/mpvsocket.sock"
	if strings.Contains(runtime.GOOS, "windows") {
		mpvSocket = "\\\\.\\pipe\\mpvsocket"
	}

	cmd := exec.CommandContext(c, "mpv", "--input-ipc-server="+mpvSocket, "/mnt/hdd/Movies/meat.mp4")
	defer cmd.Cancel()
	// cmd.Stdout = os.Stdout
	// cmd.Stderr = os.Stderr
	// output, _ := cmd.StdoutPipe()
	req := `{ "command": ["get_property", "playback-time"] }`
	go func() {
		time.Sleep(10 * time.Second)
		client := connect(mpvSocket)
		client.Do(&http.Request{})

	}()

	err := cmd.Run()
	if err != nil {
		panic(err)
	}
	time.Sleep(time.Second)
}
