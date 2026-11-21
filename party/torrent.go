package party

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	"strconv"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
)

const torrentReadahead = 32 << 20

func isTorrent(target string) bool {
	lower := strings.ToLower(target)
	return strings.HasPrefix(lower, "magnet:") || (strings.HasSuffix(lower, ".torrent") && !strings.Contains(lower, "://"))
}

// findFor is how long a torrent may take to get its metadata: it is tiny, so any peer sends
// it within seconds, and none in this long means nobody is sharing it.
const findFor = 5 * time.Minute

// reportEvery is how often the swarm is reported while the party waits on it
const reportEvery = 2 * time.Second

// swarmReport says how the swarm does every so often, but only while wanted: the party waits on
// it while it finds peers, while mpv opens the video and while mpv buffers. Played smoothly, it
// says nothing, or the log would grow a line every few seconds for hours.
func swarmReport(c context.Context, every time.Duration, stats func() (peers int, read int64), wanted func() bool, say func(string)) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	_, last := stats()
	for {
		select {
		case <-c.Done():
			return
		case <-tick.C:
		}
		peers, read := stats()
		rate := float64(read-last) / every.Seconds()
		last = read
		if wanted() {
			say("torrent: " + swarm(peers, rate))
		}
	}
}

// swarm is "3 peers, 1.2 MB/s"
func swarm(peers int, rate float64) string {
	s := strconv.Itoa(peers) + " peers, "
	if peers == 1 {
		s = "1 peer, "
	}
	switch {
	case rate >= 1e6:
		return s + strconv.FormatFloat(rate/1e6, 'f', 1, 64) + " MB/s"
	case rate >= 1e3:
		return s + strconv.FormatFloat(rate/1e3, 'f', 0, 64) + " KB/s"
	}
	return s + strconv.FormatFloat(rate, 'f', 0, 64) + " B/s"
}

// errNoPeers is a torrent nobody sent the metadata of within findFor. The GUI shows it as
// the diagnosis, so it says findFor in words.
var errNoPeers = errors.New("no peer sent this torrent's details within 5 minutes, so nobody seems to be sharing it")

func torrentConfig() *torrent.ClientConfig {
	cfg := torrent.NewDefaultClientConfig()
	cfg.ListenPort = 0
	cfg.NoDefaultPortForwarding = true
	cfg.DisableWebtorrent = true
	cfg.Slogger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return cfg
}

type torrentStream struct {
	urls  []string
	names []string
	files []*torrent.File
	id    string
	close func()
}

// playable picks what a torrent plays: its videos in natural order, without samples, or
// anything under a tenth of the largest, which keeps a trailer from playing before the main video.
// ponytail: a size guess, so a short video among long ones is dropped too; a
// way to choose files is the upgrade. A torrent with no videos plays its largest file and
// leaves the failing to mpv.
func playable(files []*torrent.File) []*torrent.File {
	largest := slices.MaxFunc(files, func(a, b *torrent.File) int { return cmp.Compare(a.Length(), b.Length()) })
	videos := slices.DeleteFunc(slices.Clone(files), func(f *torrent.File) bool {
		name := strings.ToLower(f.DisplayPath())
		return f.Length() < largest.Length()/10 || strings.Contains(name, "sample") || !slices.Contains(VideoExts, path.Ext(name))
	})
	if len(videos) == 0 {
		return []*torrent.File{largest}
	}
	slices.SortStableFunc(videos, func(a, b *torrent.File) int { return naturalCompare(a.DisplayPath(), b.DisplayPath()) })
	return videos
}

func removeDownload(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("torrent: couldn't remove the download: %s\n", flatten(err.Error()))
	}
}

// DefaultCacheDir is where downloads go unless the user chose a folder.
func DefaultCacheDir() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "watchparty"), nil
}

func torrentCache(chosen string) (string, error) {
	if chosen == "" {
		var err error
		chosen, err = DefaultCacheDir()
		if err != nil {
			return "", err
		}
	}
	return chosen, os.MkdirAll(chosen, 0o700)
}

// openTorrent waits up to find for the torrent's metadata, then serves what it plays to mpv
// on loopback. Nothing downloads but what mpv reads, so only the video playing buffers. It
// downloads into a new folder in cache, which close removes.
func openTorrent(c context.Context, target, cache string, cfg *torrent.ClientConfig, find time.Duration, added func(*torrent.Torrent)) (*torrentStream, error) {
	dir, err := os.MkdirTemp(cache, "torrent-*")
	if err != nil {
		return nil, err
	}
	cfg.DefaultStorage = torrentStore{dir}
	client, err := torrent.NewClient(cfg)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	closers := []func(){func() { removeDownload(dir) }, func() { client.Close() }}
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
	if added != nil {
		added(t)
	}
	timer := time.NewTimer(find)
	defer timer.Stop()
	select {
	case <-t.GotInfo():
	case <-timer.C:
		closeAll()
		return nil, errNoPeers
	case <-c.Done():
		closeAll()
		return nil, c.Err()
	}
	files := playable(t.Files())
	log.Printf("torrent: found %s, %d to play\n", flatten(files[0].DisplayPath()), len(files))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		closeAll()
		return nil, err
	}
	token := rand.Text()
	routes := map[string]*torrent.File{}
	urls, names := make([]string, len(files)), make([]string, len(files))
	identity := sha256.New()
	io.WriteString(identity, t.InfoHash().HexString())
	for i, f := range files {
		name := path.Base(f.DisplayPath())
		names[i] = name
		routes["/"+token+"/"+strconv.Itoa(i)+"/"+name] = f
		urls[i] = "http://" + listener.Addr().String() + "/" + token + "/" + strconv.Itoa(i) + "/" + url.PathEscape(name)
		io.WriteString(identity, "/"+f.Path())
	}
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := routes[r.URL.Path]
		if !ok {
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

	return &torrentStream{urls, names, files, hex.EncodeToString(identity.Sum(nil)), closeAll}, nil
}
