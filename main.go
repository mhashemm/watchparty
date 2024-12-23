package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mhashemm/upnp"
	"github.com/mhashemm/watchparty/mpv"
	"github.com/mhashemm/watchparty/server"
)

func main() {
	c, cancel := signal.NotifyContext(context.TODO(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	filePath := flag.String("file", "", "file path to play")
	cooldown := flag.Int("cooldown", 5, "cooldown in seconds for mpv to init the server")
	port := flag.Int("port", 6969, "local port")
	publicPort := flag.Int("pport", 6969, "public port")
	addrs := flag.String("addrs", "", "comma seprated list of addresses to connect to")
	mpvPath := flag.String("mpv", "mpv", "mpv path")
	mpvFlags := flag.String("mpvFlags", "", "any extra flags to pass to mpv")
	local := flag.Bool("local", false, "run on local network")
	update := flag.Bool("update", false, "update to latest version")
	flag.Parse()

	if *update {
		err := _update(c)
		if err != nil {
			panic(err)
		}
		return
	}

	mpvSocket := mpv.SocketPrefix + "mpv" + strconv.FormatInt(time.Now().Unix(), 10)

	address := ""
	if *local {
		address = fmt.Sprintf("%s:%d", upnp.GetLocalIPAddr(), *port)
	} else {
		upnpClient, err := upnp.New()
		if err != nil {
			panic(err)
		}

		_, err = upnpClient.AddPortMapping(upnp.AddPortMappingRequest{
			NewProtocol:               "TCP",
			NewExternalPort:           *publicPort,
			NewInternalPort:           *port,
			NewEnabled:                1,
			NewPortMappingDescription: "watchparty",
			NewLeaseDuration:          86400,
		})
		if err != nil {
			panic(err)
		}
		defer upnpClient.DeletePortMapping(upnp.DeletePortMappingRequest{NewExternalPort: *publicPort, NewProtocol: "TCP"})

		externalIp, err := upnpClient.GetExternalIPAddress()
		if err != nil {
			panic(err)
		}
		publicIp := externalIp.NewExternalIPAddress
		address = fmt.Sprintf("%s:%d", publicIp, *publicPort)
	}

	incoming, outgoing := make(chan []byte, 1024), make(chan []byte, 1024)
	defer close(incoming)
	defer close(outgoing)
	ser := server.New(c, incoming, address)
	mux := http.NewServeMux()
	mux.HandleFunc("/hi", ser.Hi)
	mux.HandleFunc("/bye", ser.Bye)
	mux.HandleFunc("/event", ser.Event)
	s := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", *port),
		Handler: mux,
	}
	defer func() {
		shutdownc, shutdowncancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdowncancel()
		s.Shutdown(shutdownc)
	}()
	defer ser.Shutdown()

	go func() {
		err := s.ListenAndServe()
		if err != nil {
			cancel()
			log.Println(err)
		}
	}()
	go ser.BroadcastEvents(outgoing)

	addresses := strings.Split(*addrs, ",")
	for _, addr := range addresses {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		err := ser.AddAddress(addr)
		if err != nil {
			log.Printf("%s: %s\n", addr, err)
		}
	}

	log.Printf("your address to share is %s\n", address)

	cmd := exec.CommandContext(c, strings.TrimSpace(*mpvPath), *mpvFlags, "--save-position-on-quit", "--pause", "--input-ipc-server="+strings.TrimSpace(mpvSocket), strings.TrimSpace(*filePath))
	defer cmd.Cancel()

	err := cmd.Start()
	if err != nil {
		panic(err)
	}

	select {
	case <-c.Done():
		log.Println(c.Err())
		return
	case <-time.After(time.Duration(*cooldown) * time.Second):
	}

	_, err = os.Stat(mpvSocket)
	if os.IsNotExist(err) {
		panic(err)
	}

	client, err := mpv.New(c, mpvSocket, outgoing)
	if err != nil {
		panic(err)
	}

	go func() {
		err := client.Watch()
		if err != nil {
			cancel()
			log.Println(err)
		}
	}()

	err = client.Observe()
	if err != nil {
		panic(err)
	}

	go client.ProccessIncomingEvents(incoming)

	log.Println("init done. now you can start watching")

	err = cmd.Wait()
	if err != nil {
		log.Println(err)
	}
}

func _update(c context.Context) error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return err
	}
	tmpPath := exePath + ".tmp"

	req, err := http.NewRequestWithContext(c, http.MethodGet, ExecutableUrl, nil)
	if err != nil {
		return err
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	defer res.Body.Close()

	file, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE, 0700)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(file, res.Body)
	if err != nil {
		file.Close()
		os.Remove(tmpPath)
		return err
	}

	return os.Rename(tmpPath, exePath)
}
