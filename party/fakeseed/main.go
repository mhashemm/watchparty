// Command fakeseed seeds os.Args[1] on loopback for e2e.py, with no tracker or DHT, and
// prints a magnet link that names it. Peers find it through that link alone. An optional
// os.Args[2] caps its upload at that many bytes a second, as a slow swarm
// (mpv/probe/slowswarm.py).
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"
)

func main() {
	path := os.Args[1]
	info := metainfo.Info{PieceLength: 1 << 18}
	if err := info.BuildFromFilePath(path); err != nil {
		log.Fatal(err)
	}
	mi := metainfo.MetaInfo{}
	var err error
	if mi.InfoBytes, err = bencode.Marshal(info); err != nil {
		log.Fatal(err)
	}
	cfg := torrent.NewDefaultClientConfig()
	cfg.Seed, cfg.NoDHT, cfg.DisableTrackers, cfg.NoDefaultPortForwarding = true, true, true, true
	cfg.ListenPort = 0
	cfg.DefaultStorage = storage.NewFileWithCompletion(filepath.Dir(path), storage.NewMapPieceCompletion())
	if len(os.Args) > 2 {
		bps, err := strconv.Atoi(os.Args[2])
		if err != nil || bps <= 0 {
			log.Fatalf("upload cap %q: want bytes a second", os.Args[2])
		}
		// the burst must hold a whole request (16KB chunks), or anacrolix panics reserving it
		cfg.UploadRateLimiter = rate.NewLimiter(rate.Limit(bps), 1<<16)
	}
	client, err := torrent.NewClient(cfg)
	if err != nil {
		log.Fatal(err)
	}
	t, err := client.AddTorrent(&mi)
	if err != nil {
		log.Fatal(err)
	}
	t.VerifyDataContext(context.Background())
	addr := client.ListenAddrs()[0].String()
	m, err := mi.MagnetV2()
	if err != nil {
		log.Fatal(err)
	}
	m.Params.Add("x.pe", "127.0.0.1:"+addr[strings.LastIndex(addr, ":")+1:])
	fmt.Println(m.String())
	select {}
}
