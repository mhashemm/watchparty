package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/mhashemm/watchparty/party"
	"github.com/mhashemm/watchparty/update"
)

var version = "dev"

const help = `watchparty keeps everyone's mpv in step while you watch the same video.

Start a party
  On one network     watchparty -local -file video.mkv
  Through a relay    watchparty -relay wss://relay.example.com -file video.mkv
  Through a router   watchparty -file video.mkv
  It prints an invite. Send it to your friends.

Join a party
  Add the invite to the command, and pick the same video:
    watchparty -relay wss://relay.example.com -room ROOM -secret SECRET -file video.mkv
  Everyone needs the same file. A renamed copy is fine; a different encode or cut is refused.

Watch together
  Pause, seek and resume in mpv as usual, and everyone follows. There is no host, so anyone
  can drive.

Automatic pauses
  The party pauses by itself, and anyone can resume, when someone:
    seeks while playing
    runs out of loaded video
    joins, and they start where the party is
    leaves, unless the video is in its last few seconds
    moves to another video in a playlist

Links
  -file also takes a video's URL. Everyone uses the same URL.

Playlists
  -playlist starts on -file, with the other videos in its folder before and after it. Back
  and next in mpv move everyone, and everyone stops at the start of the new video. Everyone
  needs the same videos in the same order. The invite includes -playlist.

Torrents
  -file also takes a .torrent or a magnet link. Everyone uses the same one. It plays while it
  downloads. Several videos play as a playlist, and only the current one downloads. What you
  watched is deleted when you leave, so the disk needs room until then. It goes in your system's
  cache folder, or -cacheDir to use another. Other people on the torrent can see your IP address,
  and you upload to them while you watch.

Relays
  Opening a party on a relay needs its create key: add -createKey KEY. Joining needs none.

Flags
`

func main() {
	cfg := party.Config{}
	flag.StringVar(&cfg.File, "file", "", "video file, URL, .torrent or magnet link to play")
	flag.BoolVar(&cfg.Playlist, "playlist", false, "also load the other videos in the file's folder, starting on this one")
	flag.IntVar(&cfg.Port, "port", party.DefaultPort, "local port")
	flag.IntVar(&cfg.PublicPort, "pport", party.DefaultPort, "public port")
	flag.StringVar(&cfg.Addrs, "addrs", "", "comma separated list of addresses to connect to")
	flag.DurationVar(&cfg.EvictAfter, "evictAfter", party.DefaultEvictAfter, "drop a peer after 3 failed sends in a row, once it has been failing this long")
	flag.StringVar(&cfg.Mpv, "mpv", "mpv", "mpv path")
	flag.StringVar(&cfg.MpvFlags, "mpvFlags", "", "any extra flags to pass to mpv")
	flag.BoolVar(&cfg.Local, "local", false, "run on local network")
	flag.StringVar(&cfg.Hostname, "hostname", "", "set your hostname")
	flag.StringVar(&cfg.Relay, "relay", "", "relay websocket url to use instead of a direct peer mesh")
	flag.StringVar(&cfg.Room, "room", "", "relay room code (generated if empty)")
	flag.StringVar(&cfg.Secret, "secret", "", "party secret, for the relay room or the direct mesh (generated if empty)")
	flag.StringVar(&cfg.CacheDir, "cacheDir", "", "folder for torrent downloads, instead of your system's cache folder")
	flag.StringVar(&cfg.CreateKey, "createKey", "", "relay key needed to create a room, if the relay requires one")
	showVersion := flag.Bool("version", false, "print the version and exit")
	doUpdate := flag.Bool("update", false, "update to the latest release and exit")
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), help)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	if *doUpdate {
		if err := selfUpdate(); err != nil {
			log.Println(err)
			os.Exit(1)
		}
		return
	}

	c, cancel := signal.NotifyContext(context.TODO(), syscall.SIGTERM, syscall.SIGINT)
	err := party.Run(c, cfg)
	cancel()
	if err != nil {
		log.Println(err)
		os.Exit(1)
	}
}

func selfUpdate() error {
	if version == "dev" {
		return errors.New("development builds do not update themselves")
	}
	exe, err := update.Executable()
	if err != nil {
		return err
	}
	c, cancel := signal.NotifyContext(context.TODO(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	tag, err := update.Install(c, exe, version, "watchparty")
	if errors.Is(err, update.ErrUpToDate) {
		fmt.Println(err)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("updated to %s; re-run watchparty to use it\n", tag)
	return nil
}
