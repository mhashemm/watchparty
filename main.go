package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mhashemm/upnp"
	"github.com/mhashemm/watchparty/mpv"
	"github.com/mhashemm/watchparty/server"
	"github.com/mhashemm/watchparty/types"
)

func main() {
	c, cancel := signal.NotifyContext(context.TODO(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	filePath := flag.String("file", "", "file path to play")
	cooldown := flag.Int("cooldown", 5, "cooldown in seconds for mpv to init the server")
	port := flag.Int("port", 6969, "local port")
	publicPort := flag.Int("pport", 6969, "public port")
	addrs := flag.String("addrs", "", "comma separated list of addresses to connect to")
	mpvPath := flag.String("mpv", "mpv", "mpv path")
	mpvFlags := flag.String("mpvFlags", "", "any extra flags to pass to mpv")
	local := flag.Bool("local", false, "run on local network")
	hostname := flag.String("hostname", "", "set your hostname")
	oblivious := flag.Bool("oblivious", false, "intended for people who don't know nothing about nothing and just copy past the command")
	flag.Parse()
	mpvSocket := mpv.SocketPrefix + "mpv" + strconv.FormatInt(time.Now().Unix(), 10)

	address := ""
	if *local || *oblivious {
		localIP := upnp.GetLocalIPAddr()
		if localIP == "" {
			panic("cannot determine local ip; are you connected to a network?")
		}
		address = fmt.Sprintf("%s:%d", localIP, *port)
	} else {
		upnpClient, err := upnp.New()
		if err != nil {
			panic(err)
		}

		mapping := upnp.AddPortMappingRequest{
			NewProtocol:               "TCP",
			NewExternalPort:           *publicPort,
			NewInternalPort:           *port,
			NewEnabled:                1,
			NewPortMappingDescription: "watchparty",
			NewLeaseDuration:          86400,
		}
		_, err = upnpClient.AddPortMapping(mapping)
		var upnpErr *upnp.Error
		if errors.As(err, &upnpErr) && upnpErr.Code == upnp.ErrOnlyPermanentLease {
			mapping.NewLeaseDuration = 0
			_, err = upnpClient.AddPortMapping(mapping)
		}
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

	incoming, outgoing := make(chan types.IncomingMessage, 1024), make(chan []byte, 1024)
	defer close(incoming)
	defer close(outgoing)
	ser := server.New(c, incoming, address, *hostname)
	mux := http.NewServeMux()
	mux.HandleFunc("/hi", ser.Hi)
	mux.HandleFunc("/bye", ser.Bye)
	mux.HandleFunc("/event", ser.Event)
	s := &http.Server{
		Addr:        fmt.Sprintf("0.0.0.0:%d", *port),
		Handler:     mux,
		BaseContext: func(_ net.Listener) context.Context { return c },
	}
	defer func() {
		shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		s.Shutdown(shutdownContext)
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

	addresses := []string{}
	if strings.TrimSpace(*addrs) != "" {
		addresses = strings.Split(*addrs, ",")
	}
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

	args := append(strings.Fields(*mpvFlags), "--save-position-on-quit", "--pause", "--input-ipc-server="+strings.TrimSpace(mpvSocket), strings.TrimSpace(*filePath))
	cmd := exec.CommandContext(c, strings.TrimSpace(*mpvPath), args...)
	defer cmd.Cancel()

	mpv.Init(cmd)

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
	defer os.Remove(mpvSocket)

	client, err := mpv.New(c, cancel, mpvSocket, outgoing, len(addresses) > 0)
	if err != nil {
		panic(err)
	}

	go client.ProcessIncomingEvents(incoming)

	log.Println("init done. now you can start watching")

	err = cmd.Wait()
	if err != nil {
		log.Println(err)
	}
}
