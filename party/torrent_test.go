package party

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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

// seed serves a folder of files named as given, and returns a magnet link that names the
// seeder, and a .torrent that doesn't.
func seed(t *testing.T, contents map[string][]byte) (magnet, file string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "Show")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range contents {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info := metainfo.Info{PieceLength: 1 << 16}
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
	cfg.DefaultStorage = storage.NewFileWithCompletion(root, storage.NewMapPieceCompletion())
	client, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	seeded, err := client.AddTorrent(&mi)
	if err != nil {
		t.Fatal(err)
	}
	seeded.VerifyDataContext(context.Background())
	m, err := mi.MagnetV2()
	if err != nil {
		t.Fatal(err)
	}
	m.Params.Add("x.pe", "127.0.0.1:"+portOf(client))
	return m.String(), file
}

func repeated(s string, size int) []byte {
	return []byte(strings.Repeat(s, size/len(s)))
}

func oneVideo() map[string][]byte {
	return map[string][]byte{"a.trailer.mkv": []byte("short"), "video.mkv": repeated("the video ", 2_000_000), "video.nfo": []byte("notes"), "video.sample.mkv": repeated("x", 1_000_000)}
}

func severalVideos() map[string][]byte {
	return map[string][]byte{"e10.mkv": repeated("ten ", 2_000_000), "e2.mkv": repeated("two ", 2_000_000), "e1.mkv": repeated("one ", 2_000_000), "sample.mkv": repeated("x", 1000), "cover.jpg": repeated("c", 3_000_000)}
}

func read(t *testing.T, url string, from, to int) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("got %d", res.StatusCode)
	}
	return body
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
// at the end of the file. A video's sample and notes aren't something to play.
func TestOneVideoPlaysAloneWithRanges(t *testing.T) {
	t.Parallel()
	magnet, _ := seed(t, oneVideo())
	stream := openOrFail(t, magnet)

	if len(stream.urls) != 1 || !strings.HasSuffix(stream.urls[0], "/video.mkv") {
		t.Fatalf("plays %q", stream.urls)
	}
	if got := read(t, stream.urls[0], 1000000, 1000008); string(got) != "the video" {
		t.Fatalf("got %q", got)
	}
}

func TestSeveralVideosPlayInOrder(t *testing.T) {
	t.Parallel()
	magnet, _ := seed(t, severalVideos())
	stream := openOrFail(t, magnet)

	want := []string{"/e1.mkv", "/e2.mkv", "/e10.mkv"}
	if len(stream.urls) != len(want) {
		t.Fatalf("plays %q", stream.urls)
	}
	if !slices.Equal(stream.names, []string{"e1.mkv", "e2.mkv", "e10.mkv"}) {
		t.Fatalf("names = %q", stream.names)
	}
	for i, w := range want {
		if !strings.HasSuffix(stream.urls[i], w) {
			t.Fatalf("plays %q, want %q", stream.urls, want)
		}
	}
	if got := read(t, stream.urls[2], 800, 802); string(got) != "ten" {
		t.Fatalf("the third is %q", got)
	}
}

// Only what mpv reads downloads, or a whole torrent would fill the disk behind the first
// video. A file's last piece can be shared with its neighbour, so read the middle.
func TestOnlyThePlayingVideoBuffers(t *testing.T) {
	t.Parallel()
	magnet, _ := seed(t, severalVideos())
	stream := openOrFail(t, magnet)

	read(t, stream.urls[1], 1_000_000, 1_000_100)
	if got := stream.files[1].BytesCompleted(); got == 0 {
		t.Fatal("the playing video has nothing")
	}
	for _, i := range []int{0, 2} {
		if got := stream.files[i].BytesCompleted(); got != 0 {
			t.Fatalf("video %d buffered %d bytes", i, got)
		}
	}
}

// The stream's address is a local port and a random token, so identity can't come from it.
func TestEveryWayToNameATorrentHasOneIdentity(t *testing.T) {
	t.Parallel()
	magnet, file := seed(t, severalVideos())
	byMagnet := openOrFail(t, magnet)
	byFile := openOrFail(t, file)
	if byMagnet.id != byFile.id || byMagnet.urls[0] == byFile.urls[0] {
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
	magnet, _ := seed(t, oneVideo())
	stream := openOrFail(t, magnet)
	url := stream.urls[0]
	res, err := http.Get(url[:strings.LastIndex(url, "/")+1] + "other.mkv")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d", res.StatusCode)
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("not on loopback: %s", url)
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
	magnet, _ := seed(t, oneVideo())
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
	if _, err = http.Get(stream.urls[0]); err == nil {
		t.Fatal("still serving after close")
	}
}

// Nothing on disk is named by the torrent, so a hostile name has nothing to escape from.
func TestTheDownloadTakesNoNamesFromTheTorrent(t *testing.T) {
	t.Parallel()
	magnet, _ := seed(t, severalVideos())
	cache := t.TempDir()
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := openTorrent(c, magnet, cache, testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.close()
	if got := read(t, stream.urls[1], 800, 802); string(got) != "two" {
		t.Fatalf("got %q", got)
	}
	found, _ := filepath.Glob(filepath.Join(cache, "torrent-*", "*"))
	if len(found) != 1 || filepath.Base(found[0]) != "data" {
		t.Fatalf("on disk: %q", found)
	}
}

func TestAChosenCacheFolderIsCreatedAndUsed(t *testing.T) {
	t.Parallel()
	chosen := filepath.Join(t.TempDir(), "new", "folder")
	got, err := torrentCache(chosen)
	if err != nil || got != chosen {
		t.Fatalf("got %q %v", got, err)
	}
	if info, err := os.Stat(chosen); err != nil || !info.IsDir() {
		t.Fatalf("not created: %v", err)
	}
}

// A path that can't be a folder fails before anything starts, and says why.
func TestACacheFolderThatCantBeUsedIsAnError(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := torrentCache(filepath.Join(file, "sub")); err == nil {
		t.Fatal("no error")
	}
}

func TestTheDefaultCacheFolderIsTheSystems(t *testing.T) {
	t.Parallel()
	system, err := os.UserCacheDir()
	if err != nil {
		t.Skip(err)
	}
	if got, err := torrentCache(""); err != nil || got != filepath.Join(system, "watchparty") {
		t.Fatalf("got %q %v", got, err)
	}
	if got, err := DefaultCacheDir(); err != nil || got != filepath.Join(system, "watchparty") {
		t.Fatalf("what the Settings page shows: got %q %v", got, err)
	}
}

// WebRTC peers bring a large parser surface for little gain, as nearly every swarm has
// regular peers. It also means a magnet link's wss:// tracker opens no connection.
func TestWebRTCPeersAreOff(t *testing.T) {
	t.Parallel()
	if !torrentConfig().DisableWebtorrent {
		t.Fatal("WebRTC peers are on")
	}
}

func TestIsTorrent(t *testing.T) {
	t.Parallel()
	for target, want := range map[string]bool{
		"magnet:?xt=urn:btih:abc":       true,
		"MAGNET:?xt=urn:btih:abc":       true,
		"/videos/Show.torrent":          true,
		"Show.TORRENT":                  true,
		"/videos/video.mkv":             false,
		"https://example.com/a.torrent": false,
		"https://example.com/video.mp4": false,
		"/videos/magnet:video.mkv":      false,
	} {
		if got := isTorrent(target); got != want {
			t.Errorf("%s: got %v", target, got)
		}
	}
}

// Leaving mid-download is the normal way a party ends. The client doesn't wait for its
// piece hashers when it closes, so one can finish after everything else has gone. The
// library's own store panicked there, taking the whole app with it, and on Windows left
// the half-downloaded file behind because it was still mapped.
func TestClosingMidDownloadDoesNotCrash(t *testing.T) {
	t.Parallel()
	magnet, _ := seed(t, severalVideos())
	for range 5 {
		cache := t.TempDir()
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		stream, err := openTorrent(c, magnet, cache, testConfig(t))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			res, err := http.Get(stream.urls[0])
			if err == nil {
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
			}
		}()
		time.Sleep(10 * time.Millisecond)
		stream.close()
		<-done
		if left, _ := filepath.Glob(filepath.Join(cache, "torrent-*")); len(left) != 0 {
			t.Fatalf("left %q behind", left)
		}
	}
}

// A .torrent, or metadata from a peer on a magnet link, can nest lists millions of levels
// deep. The decoder used to recurse once per level until the stack limit, which Go can't
// recover from, so one hostile peer took the whole app down.
func TestDeeplyNestedMetadataDoesNotCrash(t *testing.T) {
	t.Parallel()
	const depth = 4_000_000
	crafted := "d4:infod" + strings.Repeat("l", depth) + strings.Repeat("e", depth) + "ee"
	file := filepath.Join(t.TempDir(), "crafted.torrent")
	if err := os.WriteFile(file, []byte(crafted), 0o600); err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if stream, err := openTorrent(c, file, t.TempDir(), testConfig(t)); err == nil {
		stream.close()
		t.Fatal("opened a crafted torrent")
	}
	var info metainfo.Info
	if err := bencode.Unmarshal([]byte(strings.Repeat("l", depth)+strings.Repeat("e", depth)), &info); err == nil {
		t.Fatal("decoded crafted metadata")
	}
}

// A v2 file tree used to be re-parsed at every level, so a small one pinned a core for
// minutes. Peers can send one on a magnet link too.
func TestDeepFileTreeDoesNotHang(t *testing.T) {
	t.Parallel()
	const depth = 30_000
	var tree any = map[string]any{"": map[string]any{"length": 5, "pieces root": strings.Repeat("A", 32)}}
	for range depth {
		tree = map[string]any{"a": tree}
	}
	crafted, err := bencode.Marshal(map[string]any{"meta version": 2, "piece length": 32768, "file tree": tree})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var info metainfo.Info
		bencode.Unmarshal(crafted, &info)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("still parsing after 10s")
	}
}
