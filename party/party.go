package party

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mhashemm/upnp"
	"github.com/mhashemm/watchparty/mpv"
	"github.com/mhashemm/watchparty/server"
)

const (
	DefaultPort     = 6969
	DefaultCooldown = 5
)

type Config struct {
	File       string
	Mpv        string
	MpvFlags   string
	Hostname   string
	Cooldown   int
	Relay      string
	Room       string
	Secret     string
	CreateKey  string
	Local      bool
	Port       int
	PublicPort int
	Addrs      string
}

var errStopped = errors.New("the party stopped unexpectedly")

// Run blocks until mpv exits or parent is cancelled. Cancelling parent is a normal
// stop and returns nil; every cleanup (/bye, the UPNP mapping) has run by the time
// it returns.
func Run(parent context.Context, cfg Config) error {
	c, cancel := context.WithCancel(parent)
	defer cancel()
	mpvSocket := mpv.SocketPrefix + "mpv" + rand.Text()

	incoming, outgoing := make(chan mpv.IncomingMessage, 1024), make(chan []byte, 1024)

	if strings.TrimSpace(cfg.Relay) != "" {
		if cfg.Hostname == "" {
			cfg.Hostname, _ = os.Hostname()
		}
		if cfg.Room == "" {
			cfg.Room = rand.Text()
		}
		if cfg.Secret == "" {
			cfg.Secret = rand.Text()
		}
		conn := &relayConn{
			addr:      strings.TrimSpace(cfg.Relay),
			room:      cfg.Room,
			secret:    cfg.Secret,
			createKey: cfg.CreateKey,
			hostname:  cfg.Hostname,
			session:   rand.Text(),
			incoming:  incoming,
		}
		defer conn.Close()
		go conn.Connect(c, cancel)
		go conn.Broadcast(c, outgoing)
		log.Printf("your address to share is -relay %s -room %s -secret %s\n", conn.addr, cfg.Room, cfg.Secret)
	} else {
		cleanup, err := runMesh(c, cancel, incoming, outgoing, cfg)
		if err != nil {
			return err
		}
		defer cleanup()
	}

	args := append(strings.Fields(cfg.MpvFlags), "--save-position-on-quit", "--pause", "--input-ipc-server="+strings.TrimSpace(mpvSocket), strings.TrimSpace(cfg.File))
	cmd := exec.CommandContext(c, strings.TrimSpace(cfg.Mpv), args...)

	mpv.Init(cmd)

	err := cmd.Start()
	if err != nil {
		return err
	}
	defer cmd.Cancel()

	select {
	case <-c.Done():
		log.Println(c.Err())
		return stopReason(parent)
	case <-time.After(time.Duration(cfg.Cooldown) * time.Second):
	}

	defer os.Remove(mpvSocket)

	mpvQuit := atomic.Bool{}
	client, err := mpv.New(c, func() {
		if c.Err() == nil {
			mpvQuit.Store(true)
		}
		cancel()
	}, mpvSocket, outgoing)
	if err != nil {
		return err
	}

	go client.ProcessIncomingEvents(incoming)

	log.Println("init done. now you can start watching")

	err = cmd.Wait()
	if err != nil {
		log.Println(err)
	}
	if mpvQuit.Load() {
		return nil
	}
	if c.Err() != nil {
		return stopReason(parent)
	}
	return err
}

func stopReason(parent context.Context) error {
	if parent.Err() != nil {
		return nil
	}
	return errStopped
}

func runMesh(c context.Context, cancel context.CancelFunc, incoming chan mpv.IncomingMessage, outgoing chan []byte, cfg Config) (func(), error) {
	cleanup := []func(){}
	runCleanup := func() {
		for i := len(cleanup) - 1; i >= 0; i-- {
			cleanup[i]()
		}
	}
	address := ""
	if cfg.Local {
		localIP := upnp.GetLocalIPAddr()
		if localIP == "" {
			return nil, errors.New("cannot determine local ip; are you connected to a network?")
		}
		address = fmt.Sprintf("%s:%d", localIP, cfg.Port)
	} else {
		upnpClient, err := upnp.New()
		if err != nil {
			return nil, fmt.Errorf("upnp: %w", err)
		}

		mapping := upnp.AddPortMappingRequest{
			NewProtocol:               "TCP",
			NewExternalPort:           cfg.PublicPort,
			NewInternalPort:           cfg.Port,
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
			return nil, fmt.Errorf("upnp: %w", err)
		}
		cleanup = append(cleanup, func() {
			upnpClient.DeletePortMapping(upnp.DeletePortMappingRequest{NewExternalPort: cfg.PublicPort, NewProtocol: "TCP"})
		})

		externalIp, err := upnpClient.GetExternalIPAddress()
		if err != nil {
			runCleanup()
			return nil, fmt.Errorf("upnp: %w", err)
		}
		publicIp := externalIp.NewExternalIPAddress
		address = fmt.Sprintf("%s:%d", publicIp, cfg.PublicPort)
	}

	ser := server.New(c, incoming, address, cfg.Hostname)
	mux := http.NewServeMux()
	mux.HandleFunc("/hi", ser.Hi)
	mux.HandleFunc("/bye", ser.Bye)
	mux.HandleFunc("/event", ser.Event)
	s := &http.Server{
		Addr:        fmt.Sprintf("0.0.0.0:%d", cfg.Port),
		Handler:     mux,
		BaseContext: func(_ net.Listener) context.Context { return c },
	}
	cleanup = append(cleanup, func() {
		shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		s.Shutdown(shutdownContext)
	})
	cleanup = append(cleanup, ser.Shutdown)

	go func() {
		err := s.ListenAndServe()
		if err != nil {
			cancel()
			log.Println(err)
		}
	}()
	go ser.BroadcastEvents(outgoing)

	for _, addr := range strings.Split(cfg.Addrs, ",") {
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

	// callers defer this, so it runs in Run and keeps the original LIFO order:
	// /bye first, then the http server, then the upnp mapping
	return runCleanup, nil
}
