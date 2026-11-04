package party

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func testConfig(t *testing.T) *torrent.ClientConfig {
	t.Helper()
	cfg := torrentConfig()
	cfg.NoDHT, cfg.DisableTrackers, cfg.DisablePEX, cfg.DisableUTP, cfg.DisableIPv6 = true, true, true, true, true
	cfg.DisableAcceptRateLimiting = true
	return cfg
}

// seed serves a folder holding a small sample and a larger movie, and returns a magnet
// link that names the seeder, and a .torrent that doesn't.
func seed(t *testing.T) (magnet, file string, movie []byte) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "Show")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	movie = []byte(strings.Repeat("the movie ", 200_000))
	for name, content := range map[string][]byte{"sample.mkv": []byte("short"), "movie.mkv": movie} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info := metainfo.Info{PieceLength: 1 << 18}
	if err := info.BuildFromFilePath(dir); err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{}
	var err error
	if mi.InfoBytes, err = bencode.Marshal(info); err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(root, "show.torrent")
	out, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err = mi.Write(out); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig(t)
	cfg.Seed = true
	cfg.DefaultStorage = storage.NewFile(root)
	client, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	seeded, err := client.AddTorrent(&mi)
	if err != nil {
		t.Fatal(err)
	}
	seeded.VerifyData()
	m, err := mi.MagnetV2()
	if err != nil {
		t.Fatal(err)
	}
	m.Params.Add("x.pe", "127.0.0.1:"+portOf(client))
	return m.String(), file, movie
}

func portOf(client *torrent.Client) string {
	addr := client.ListenAddrs()[0].String()
	return addr[strings.LastIndex(addr, ":")+1:]
}

func openOrFail(t *testing.T, target string) *torrentStream {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := openTorrent(c, target, t.TempDir(), testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stream.close)
	return stream
}

// mpv reads a stream with range requests from wherever it needs: a seek, or the index
// at the end of the file. Only the largest file is served: a pack's sample isn't the movie.
func TestMagnetServesTheLargestFileWithRanges(t *testing.T) {
	t.Parallel()
	magnet, _, movie := seed(t)
	stream := openOrFail(t, magnet)

	req, _ := http.NewRequest(http.MethodGet, stream.url, nil)
	req.Header.Set("Range", "bytes=1000000-1000008")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusPartialContent || string(body) != string(movie[1000000:1000009]) {
		t.Fatalf("got %d %q", res.StatusCode, body)
	}
	if !strings.HasSuffix(stream.url, "/movie.mkv") {
		t.Fatalf("mpv would show %q", stream.url)
	}
}

// The stream's address is a local port and a random token, so identity can't come from it.
func TestEveryWayToNameATorrentHasOneIdentity(t *testing.T) {
	t.Parallel()
	magnet, file, _ := seed(t)
	byMagnet := openOrFail(t, magnet)
	byFile := openOrFail(t, file)
	if byMagnet.id != byFile.id || byMagnet.url == byFile.url {
		t.Fatalf("magnet %s and .torrent %s", byMagnet.id, byFile.id)
	}
	other := openOrFail(t, magnet)
	if other.id != byMagnet.id {
		t.Fatal("a second opening of the same magnet got another identity")
	}
}

// Only a request for the exact route is served, so nothing else on this machine can
// read the stream without the random token.
func TestStreamNeedsItsToken(t *testing.T) {
	t.Parallel()
	magnet, _, _ := seed(t)
	stream := openOrFail(t, magnet)
	res, err := http.Get(stream.url[:strings.LastIndex(stream.url, "/")+1] + "other.mkv")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d", res.StatusCode)
	}
	if !strings.HasPrefix(stream.url, "http://127.0.0.1:") {
		t.Fatalf("not on loopback: %s", stream.url)
	}
}

// A dead magnet can wait forever. Stop has to end it, and nothing stays on disk.
func TestStopWhileFindingPeersLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	cfg, cache := testConfig(t), t.TempDir()
	c, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := openTorrent(c, "magnet:?xt=urn:btih:"+strings.Repeat("ab", 20), cache, cfg)
		done <- err
	}()
	dirs := filepath.Join(cache, "torrent-*")
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if found, _ := filepath.Glob(dirs); len(found) == 1 {
			break
		}
		if time.Now().After(end) {
			t.Fatal("no download folder appeared")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("kept waiting after Stop")
	}
	if found, _ := filepath.Glob(dirs); len(found) != 0 {
		t.Fatalf("left %q behind", found)
	}
}

func TestClosingAStreamRemovesItsDownload(t *testing.T) {
	t.Parallel()
	magnet, _, _ := seed(t)
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cache := t.TempDir()
	stream, err := openTorrent(c, magnet, cache, testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	dirs := filepath.Join(cache, "torrent-*")
	if found, _ := filepath.Glob(dirs); len(found) != 1 {
		t.Fatalf("found %q", found)
	}
	stream.close()
	if found, _ := filepath.Glob(dirs); len(found) != 0 {
		t.Fatalf("left %q behind", found)
	}
	if _, err = http.Get(stream.url); err == nil {
		t.Fatal("still serving after close")
	}
}

func TestIsTorrent(t *testing.T) {
	t.Parallel()
	for target, want := range map[string]bool{
		"magnet:?xt=urn:btih:abc":       true,
		"MAGNET:?xt=urn:btih:abc":       true,
		"/videos/Show.torrent":          true,
		"Show.TORRENT":                  true,
		"/videos/movie.mkv":             false,
		"https://example.com/a.torrent": false,
		"https://example.com/movie.mp4": false,
		"/videos/magnet:movie.mkv":      false,
	} {
		if got := isTorrent(target); got != want {
			t.Errorf("%s: got %v", target, got)
		}
	}
}
