package party

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
)

const torrentReadahead = 32 << 20

func isTorrent(target string) bool {
	lower := strings.ToLower(target)
	return strings.HasPrefix(lower, "magnet:") || (strings.HasSuffix(lower, ".torrent") && !strings.Contains(lower, "://"))
}

func torrentConfig() *torrent.ClientConfig {
	cfg := torrent.NewDefaultClientConfig()
	cfg.ListenPort = 0
	cfg.NoDefaultPortForwarding = true
	cfg.Slogger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return cfg
}

type torrentStream struct {
	url, id string
	close   func()
}

func torrentCache() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	cache = filepath.Join(cache, "watchparty")
	return cache, os.MkdirAll(cache, 0o700)
}

// openTorrent waits for the torrent's metadata, then serves its largest file to mpv on
// loopback. It downloads into a new folder in cache, which close removes.
func openTorrent(c context.Context, target, cache string, cfg *torrent.ClientConfig) (*torrentStream, error) {
	dir, err := os.MkdirTemp(cache, "torrent-*")
	if err != nil {
		return nil, err
	}
	store := storage.NewFileWithCompletion(dir, storage.NewMapPieceCompletion())
	cfg.DefaultStorage = store
	client, err := torrent.NewClient(cfg)
	if err != nil {
		store.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	closers := []func(){func() { os.RemoveAll(dir) }, func() { store.Close() }, func() { client.Close() }}
	closeAll := func() {
		for _, f := range slices.Backward(closers) {
			f()
		}
	}

	var t *torrent.Torrent
	if strings.HasPrefix(strings.ToLower(target), "magnet:") {
		t, err = client.AddMagnet(target)
	} else {
		t, err = client.AddTorrentFromFile(target)
	}
	if err != nil {
		closeAll()
		return nil, err
	}
	log.Println("torrent: finding peers")
	select {
	case <-t.GotInfo():
	case <-c.Done():
		closeAll()
		return nil, c.Err()
	}
	f := slices.MaxFunc(t.Files(), func(a, b *torrent.File) int { return cmp.Compare(a.Length(), b.Length()) })
	log.Printf("torrent: found %s\n", flatten(f.DisplayPath()))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		closeAll()
		return nil, err
	}
	token, name := rand.Text(), path.Base(f.DisplayPath())
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+token+"/"+name {
			http.NotFound(w, r)
			return
		}
		reader := f.NewReader()
		defer reader.Close()
		reader.SetContext(r.Context())
		reader.SetResponsive()
		reader.SetReadahead(torrentReadahead)
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", time.Time{}, reader)
	})}
	go server.Serve(listener)
	closers = append(closers, func() { server.Close() })

	sum := sha256.Sum256([]byte(t.InfoHash().HexString() + "/" + f.Path()))
	return &torrentStream{"http://" + listener.Addr().String() + "/" + token + "/" + url.PathEscape(name), hex.EncodeToString(sum[:]), closeAll}, nil
}
