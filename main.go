package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/mhashemm/watchparty/party"
)

func main() {
	cfg := party.Config{}
	flag.StringVar(&cfg.File, "file", "", "file path to play")
	flag.IntVar(&cfg.Cooldown, "cooldown", party.DefaultCooldown, "cooldown in seconds for mpv to init the server")
	flag.IntVar(&cfg.Port, "port", party.DefaultPort, "local port")
	flag.IntVar(&cfg.PublicPort, "pport", party.DefaultPort, "public port")
	flag.StringVar(&cfg.Addrs, "addrs", "", "comma separated list of addresses to connect to")
	flag.StringVar(&cfg.Mpv, "mpv", "mpv", "mpv path")
	flag.StringVar(&cfg.MpvFlags, "mpvFlags", "", "any extra flags to pass to mpv")
	flag.BoolVar(&cfg.Local, "local", false, "run on local network")
	flag.StringVar(&cfg.Hostname, "hostname", "", "set your hostname")
	flag.StringVar(&cfg.Relay, "relay", "", "relay websocket url to use instead of a direct peer mesh")
	flag.StringVar(&cfg.Room, "room", "", "relay room code (generated if empty)")
	flag.StringVar(&cfg.Secret, "secret", "", "relay room secret (generated if empty)")
	flag.StringVar(&cfg.CreateKey, "createKey", "", "relay key needed to create a room, if the relay requires one")
	flag.Parse()

	c, cancel := signal.NotifyContext(context.TODO(), syscall.SIGTERM, syscall.SIGINT)
	err := party.Run(c, cfg)
	cancel()
	if err != nil {
		log.Println(err)
		os.Exit(1)
	}
}
