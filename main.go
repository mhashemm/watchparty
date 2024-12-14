package main

import (
	"context"
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
)

func main() {
	c, cancel := signal.NotifyContext(context.TODO(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	filePath := flag.String("file", "", "file path to play")
	cooldown := flag.Int("cooldown", 5, "cooldown for mpv to open")
	socket := flag.String("socket", "mpv", "name of the socket")
	port := flag.Int("port", 6969, "running port")
	publicPort := flag.Int("pport", 6969, "public port")
	addrs := flag.String("addrs", "", "comma seprated list of addresses to connect to")
	mpvPath := flag.String("mpv", "mpv", "mpv path")
	flag.Parse()
	mpvSocket := mpv.SocketPrefix + *socket + strconv.FormatInt(time.Now().Unix(), 10)

	_, err := upnp.AddPortMapping(upnp.AddPortMappingRequest{
		NewProtocol:               "TCP",
		NewRemoteHost:             struct{}{},
		NewExternalPort:           *publicPort,
		NewInternalPort:           *port,
		NewEnabled:                1,
		NewPortMappingDescription: "watchparty",
		NewLeaseDuration:          0,
	})
	if err != nil {
		panic(err)
	}
	defer upnp.DeletePortMapping(upnp.DeletePortMappingRequest{NewExternalPort: *publicPort, NewProtocol: "TCP"})

	externalIp, err := upnp.GetExternalIPAddress()
	if err != nil {
		panic(err)
	}
	publicIp := externalIp.NewExternalIPAddress
	publicAddress := fmt.Sprintf("%s:%d", publicIp, *publicPort)
	log.Printf("your public address to share is %s\n", publicAddress)

	incoming, outgoing := make(chan []byte, 1024), make(chan []byte, 1024)
	defer close(incoming)
	defer close(outgoing)
	ser := server.New(c, incoming, publicAddress)
	mux := http.NewServeMux()
	mux.HandleFunc("/init", ser.Init)
	mux.HandleFunc("/event", ser.Event)
	s := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", *port),
		Handler: mux,
		BaseContext: func(l net.Listener) context.Context {
			return c
		},
	}
	defer s.Shutdown(c)

	go func() {
		err := s.ListenAndServe()
		if err != nil {
			cancel()
			log.Println(err)
		}
	}()
	go ser.Broadcast(outgoing)

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

	cmd := exec.CommandContext(c, strings.TrimSpace(*mpvPath), "--pause", "--input-ipc-server="+strings.TrimSpace(mpvSocket), strings.TrimSpace(*filePath))
	defer cmd.Cancel()

	err = cmd.Start()
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
