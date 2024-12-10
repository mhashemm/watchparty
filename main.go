package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func main() {
	c, cancel := signal.NotifyContext(context.TODO(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	filePath := flag.String("file", "", "file path to play")
	cooldown := flag.Int("cooldown", 5, "cooldown for mpv to open")
	// ips := flag.String("ips", "", "comma seprated list of IPs to connect to")
	flag.Parse()
	mpvSocket := "/tmp/mpvsocket"
	if strings.Contains(runtime.GOOS, "windows") {
		mpvSocket = "\\\\.\\pipe\\mpvsocket"
	}

	cmd := exec.CommandContext(c, "mpv", "--input-ipc-server="+mpvSocket, *filePath)
	defer cmd.Cancel()
	// cmd.Stdin = os.Stdin
	// cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Start()
	if err != nil {
		panic(err)
	}

	time.Sleep(time.Duration(*cooldown) * time.Second)

	_, err = os.Stat(mpvSocket)
	if os.IsNotExist(err) {
		panic(err)
	}
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(c, "unix", mpvSocket)
	if err != nil {
		panic(err)
	}
	responseScanner := bufio.NewScanner(conn)

	// { "command": ["set_property", "pause", true] }
	// req := `{ "command": ["observe_property_string", 1, "pause"] }` + "\n"
	// req := `{ "command": ["observe_property_string", 1, "playback-time"] }` + "\n"
	req := `{ "command": ["get_property", "filename"] }` + "\n"
	_, err = conn.Write([]byte(req))

	go func() {
		for responseScanner.Scan() {
			fmt.Println(responseScanner.Text())
		}
	}()

	err = cmd.Wait()
	fmt.Println(err)
}
