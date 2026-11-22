package party

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/anacrolix/torrent"
	"github.com/mhashemm/upnp"
	"github.com/mhashemm/watchparty/mpv"
	"github.com/mhashemm/watchparty/server"
)

const (
	DefaultPort       = 6969
	DefaultEvictAfter = 5 * time.Minute

	sampleSize  = 1 << 20
	sampleCount = 8
)

type Config struct {
	File       string
	Playlist   bool
	CacheDir   string
	Mpv        string
	MpvFlags   string
	Hostname   string
	Relay      string
	Room       string
	Secret     string
	CreateKey  string
	Local      bool
	Port       int
	PublicPort int
	Addrs      string
	EvictAfter time.Duration
	// Entry and Start continue a party where this node stopped: the playlist entry by
	// name, and seconds into it. An entry no longer in the playlist starts as usual.
	Entry string
	Start float64
	// Chat is what this node's person types to the party (the GUI's box), one message each,
	// and React the reactions they pick there. Heard is given each message and reaction mpv
	// shows: neither is ever logged.
	Chat  <-chan string
	React <-chan string
	Heard func(mpv.Said)
	// NoPicker starts mpv without its reaction picker (the GUI's Settings); Picker turns it on
	// or off while the party runs
	NoPicker bool
	Picker   <-chan bool
}

var (
	errStopped = errors.New("the party stopped unexpectedly")
	errHungUp  = errors.New("mpv stopped answering before the video was ready, though it was still running")
	errNobody  = errors.New("nobody answered at the address you were given; check the invite, and that someone is still in the party")
)

const mpvExitGrace = 10 * time.Second

// Run blocks until mpv exits or parent is cancelled. Cancelling parent is a normal
// stop and returns nil; every cleanup (/bye, the UPNP mapping) has run by the time
// it returns.
func Run(parent context.Context, cfg Config) error {
	// a relay's rejection says why it stopped the party (outcome)
	c, stop := context.WithCancelCause(parent)
	cancel := func() { stop(nil) }
	defer cancel()
	incoming, outgoing := make(chan mpv.IncomingMessage, 1024), make(chan []byte, 1024)
	// mpv's client once it has the video
	var playing atomic.Pointer[mpv.Client]

	var v video
	target := strings.TrimSpace(cfg.File)
	if isTorrent(target) {
		cache, err := torrentCache(strings.TrimSpace(cfg.CacheDir))
		if err != nil {
			return err
		}
		stream, err := openSwarm(c, target, cache, &playing)
		if err != nil && parent.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		defer stream.close()
		v = video{playlist: len(stream.urls) > 1, id: stream.id, infohash: stream.infohash}
		for i, u := range stream.urls {
			v.entries = append(v.entries, entry{file: u, name: stream.names[i], size: stream.sizes[i]})
		}
		cfg.Playlist = false
	} else {
		var err error
		if v, err = media(target, cfg.Playlist); err != nil {
			return err
		}
		cfg.Playlist = v.playlist
	}
	log.Printf("file fingerprint: %s\n", v.id)
	startAt := cfg.Start
	if v.playlist && cfg.Entry != "" {
		if i := slices.IndexFunc(v.entries, func(e entry) bool { return e.name == cfg.Entry }); i >= 0 {
			v.start = i
		} else {
			startAt = 0
		}
	}
	// what a joiner refused for its file is told, and sets beside its own
	ours := v.about().header()
	if cfg.Secret == "" {
		cfg.Secret = rand.Text()
	}
	if cfg.Hostname == "" {
		cfg.Hostname, _ = os.Hostname()
	}

	if strings.TrimSpace(cfg.Relay) != "" {
		joinRelay(c, stop, &cfg, incoming, outgoing, v.id, ours)
	} else {
		cleanup, err := runMesh(c, cancel, incoming, outgoing, cfg, v.id, ours)
		if err != nil && parent.Err() != nil {
			return nil
		}
		if err != nil {
			return refusedFile(err, v)
		}
		defer cleanup()
	}
	return refusedFile(runMpv(parent, c, cancel, cfg, v, startAt, incoming, outgoing, &playing), v)
}

// video is what the party plays: its entries, the one it starts on, and the fingerprint
// every member's must match (id). infohash is a torrent's, for a refused joiner (About).
type video struct {
	entries  []entry
	start    int
	playlist bool
	id       string
	infohash string
}

// entry is one video mpv opens: its file or URL, its name, and for a refused joiner its
// size (0 when unknown) and, on disk, its fingerprint.
type entry struct {
	file, name, sum string
	size            int64
}

// openSwarm opens a torrent and reports its swarm while the party waits on it.
func openSwarm(c context.Context, target, cache string, playing *atomic.Pointer[mpv.Client]) (*torrentStream, error) {
	return openTorrent(c, target, cache, torrentConfig(), findFor, func(t *torrent.Torrent) {
		stats := func() (int, int64) {
			s := t.Stats()
			return s.ActivePeers, s.BytesReadUsefulData.Int64()
		}
		// until mpv has the video, and then while it buffers
		wanted := func() bool {
			cl := playing.Load()
			return cl == nil || cl.Buffering()
		}
		go swarmReport(c, reportEvery, stats, wanted, func(s string) { log.Println(s) })
	})
}

// joinRelay connects to the relay in the background, creating a room when cfg has none.
func joinRelay(c context.Context, stop context.CancelCauseFunc, cfg *Config, incoming chan mpv.IncomingMessage, outgoing chan []byte, file, video string) {
	creating := cfg.Room == ""
	if creating {
		cfg.Room = rand.Text()
	}
	conn := &relayConn{
		addr:      strings.TrimSpace(cfg.Relay),
		room:      cfg.Room,
		secret:    cfg.Secret,
		createKey: cfg.CreateKey,
		creating:  creating,
		hostname:  cfg.Hostname,
		id:        rand.Text(),
		session:   rand.Text(),
		file:      file,
		video:     video,
		incoming:  incoming,
		retry:     minBackoff,
		wait:      notOpenFor,
		up:        make(chan struct{}, 1),
	}
	go conn.Connect(c, stop)
	go conn.Broadcast(c, outgoing)
	log.Printf("your address to share is -relay %s -room %s -secret %s%s\n", conn.addr, cfg.Room, cfg.Secret, cfg.playlistFlag())
}

// runMpv starts mpv on the video with the sync script, joins it to the party and waits for
// it to exit.
func runMpv(parent, c context.Context, cancel context.CancelFunc, cfg Config, v video, startAt float64, incoming chan mpv.IncomingMessage, outgoing chan []byte, playing *atomic.Pointer[mpv.Client]) error {
	// the sync script, and the reactions': mpv reads them as it starts, and loads them even
	// under the user's load-scripts=no
	scripts, err := os.MkdirTemp("", "watchparty")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scripts)
	script, reactions := filepath.Join(scripts, mpv.ScriptFile), filepath.Join(scripts, mpv.ReactionsFile)
	if err := os.WriteFile(script, mpv.Script, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(reactions, mpv.ReactionsScript(), 0o600); err != nil {
		return err
	}
	mpvSocket := mpv.SocketPrefix + "mpv" + rand.Text()
	args := append(mpvArgs(cfg.MpvFlags), "--pause", "--speed=1", "--hr-seek=yes", "--cache-pause=yes", "--no-resume-playback", "--quiet", "--input-ipc-server="+mpvSocket, "--script="+script, "--script="+reactions)
	if cfg.NoPicker {
		args = append(args, "--script-opts-append="+mpv.PickerOff)
	}
	var names []string
	if v.playlist {
		args = append(args, "--no-shuffle", "--playlist-start="+strconv.Itoa(v.start))
	}
	for _, e := range v.entries {
		args = append(args, e.file)
		if v.playlist {
			names = append(names, e.name)
		}
	}
	cmd := exec.CommandContext(c, strings.TrimSpace(cfg.Mpv), args...)

	mpv.Init(cmd)
	out := &lineLog{l: log.Default()}
	cmd.Stdout, cmd.Stderr = out, out

	if err := cmd.Start(); err != nil {
		// the party stopped before mpv started, as when the relay refuses at once: why it
		// stopped, not exec's "context canceled"
		if c.Err() != nil {
			return outcome(parent, c, false, nil)
		}
		return err
	}
	defer cmd.Cancel()
	defer os.Remove(mpvSocket)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	mpvQuit := atomic.Bool{}
	conn, err := mpv.Dial(c, mpvSocket)
	for err != nil {
		select {
		case <-c.Done():
			log.Println(c.Err())
			return outcome(parent, c, mpvQuit.Load(), nil)
		case err := <-exited:
			return outcome(parent, c, mpvQuit.Load(), notReady(err))
		case <-time.After(50 * time.Millisecond):
		}
		conn, err = mpv.Dial(c, mpvSocket)
	}
	log.Println("mpv is up, loading the video")

	client, err := mpv.New(c, func() {
		if c.Err() == nil {
			mpvQuit.Store(true)
		}
		cancel()
	}, conn, mpv.Config{Outgoing: outgoing, Incoming: incoming, Chat: cfg.Chat, React: cfg.React, Picker: cfg.Picker, Heard: cfg.Heard, Names: names, Start: v.start, At: startAt})
	if err != nil {
		if errors.Is(err, mpv.ErrClosed) && !mpvQuit.Load() && c.Err() == nil {
			err = waitExit(exited, mpvExitGrace)
		}
		return outcome(parent, c, mpvQuit.Load(), err)
	}

	playing.Store(client)
	log.Println("init done. now you can start watching")

	err = <-exited
	logEnd(client, names)
	if err != nil {
		log.Println(err)
	}
	return outcome(parent, c, mpvQuit.Load(), err)
}

// logEnd says where the party stopped, which the GUI continues from, or that it finished.
func logEnd(client *mpv.Client, names []string) {
	if client.Finished() {
		log.Println("finished: played to the end")
		return
	}
	entry, at := client.Position()
	if entry < len(names) {
		// the name Config.Entry looks up, from the same list
		name, _ := json.Marshal(names[entry])
		log.Printf("stopped at: %s in %s\n", strconv.FormatFloat(at, 'f', 3, 64), name)
	} else {
		log.Printf("stopped at: %s\n", strconv.FormatFloat(at, 'f', 3, 64))
	}
}

// outcome decides what Run returns from the facts once it ends, never from which channel
// fired first: a Stop kills mpv, so "cancelled" and "mpv exited" both fire, and the cancel is
// why mpv died. The caller cancelling parent and the user quitting mpv are normal ends;
// anything else cancelling c stopped the party; otherwise err, mpv's own reason, stands.
func outcome(parent, c context.Context, mpvQuit bool, err error) error {
	switch {
	case parent.Err() != nil, mpvQuit:
		return nil
	case c.Err() != nil:
		// a relay's rejection, which a cancel without a cause isn't
		if cause := context.Cause(c); !errors.Is(cause, context.Canceled) {
			return cause
		}
		return errStopped
	}
	return err
}

// mpvArgs splits the user's flags on spaces, except inside double quotes, which it
// removes: --sub-file="C:\My Subs\a.srt" is one argument. No escapes: a backslash is a
// path separator on Windows.
func mpvArgs(flags string) []string {
	var args []string
	var arg strings.Builder
	quoted, started := false, false
	for _, r := range flags {
		switch {
		case r == '"':
			quoted, started = !quoted, true
		case unicode.IsSpace(r) && !quoted:
			if started {
				args = append(args, arg.String())
				arg.Reset()
				started = false
			}
		default:
			arg.WriteRune(r)
			started = true
		}
	}
	if started {
		args = append(args, arg.String())
	}
	return args
}

// lineLog logs mpv's output a line at a time, so the reason a file won't play reaches the
// CLI's log and the GUI's chart. exec copies into one writer shared by stdout and stderr
// from a single goroutine, so Write needs no lock.
type lineLog struct {
	l   *log.Logger
	buf []byte
}

func (w *lineLog) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		line, rest, ok := bytes.Cut(w.buf, []byte("\n"))
		if !ok {
			return len(p), nil
		}
		w.l.Printf("mpv: %s\n", line)
		w.buf = rest
	}
}

// notReady reports mpv exiting before the video loaded. Exit 0 is the user closing it, a
// normal end; mpv exits non-zero for a bad file or flag.
func notReady(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("mpv exited before the video was ready: %w", err)
}

func waitExit(exited <-chan error, grace time.Duration) error {
	select {
	case err := <-exited:
		return notReady(err)
	case <-time.After(grace):
		return errHungUp
	}
}

func (cfg Config) playlistFlag() string {
	if cfg.Playlist {
		return " -playlist"
	}
	return ""
}

// fingerprint identifies a video by its content alone, so the same file under
// another name, path or timestamp matches, and says its size. A URL is identified by
// itself, with no size.
func fingerprint(path string) (string, int64, error) {
	if strings.Contains(path, "://") {
		sum := sha256.Sum256([]byte(path))
		return hex.EncodeToString(sum[:]), 0, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	size := st.Size()
	h := sha256.New()
	binary.Write(h, binary.BigEndian, size)
	if size <= sampleSize*sampleCount {
		_, err = io.Copy(h, f)
	} else {
		for i := range int64(sampleCount) {
			if _, err = io.Copy(h, io.NewSectionReader(f, i*(size-sampleSize)/(sampleCount-1), sampleSize)); err != nil {
				break
			}
		}
	}
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// runMesh starts this node's side of the P2P mesh: an address others can reach, the
// server, and a hello to each address given. The cleanup it returns runs in reverse: /bye
// first, then the HTTP server, then the UPNP mapping. Callers defer it, so it runs in Run.
func runMesh(c context.Context, cancel context.CancelFunc, incoming chan mpv.IncomingMessage, outgoing chan []byte, cfg Config, file, video string) (func(), error) {
	var cleanup cleanups
	address, err := meshAddress(c, cfg, &cleanup)
	if err == nil {
		var ser *server.Server
		if ser, err = serveMesh(c, cancel, incoming, outgoing, cfg, file, video, address, &cleanup); err == nil {
			err = joinMesh(c, ser, cfg.Addrs, incoming)
		}
	}
	if err != nil {
		cleanup.run()
		return nil, err
	}
	log.Printf("your address to share is -addrs %s -secret %s%s\n", address, cfg.Secret, cfg.playlistFlag())
	return cleanup.run, nil
}

// cleanups run in reverse order of adding.
type cleanups []func()

func (cs *cleanups) add(f func()) {
	*cs = append(*cs, f)
}

func (cs *cleanups) run() {
	for _, f := range slices.Backward(*cs) {
		f()
	}
}

// meshAddress is the address others reach this node at: the LAN address with cfg.Local,
// else the router's public address, with a port the router maps to this node.
func meshAddress(c context.Context, cfg Config, cleanup *cleanups) (string, error) {
	if cfg.Local {
		localIP := upnp.GetLocalIPAddr()
		if localIP == "" {
			return "", errors.New("cannot determine local ip; are you connected to a network?")
		}
		return fmt.Sprintf("%s:%d", localIP, cfg.Port), nil
	}
	log.Println("asking the router for a port")
	upnp.Logger = slog.New(upnpLogger{l: log.Default()})
	upnpClient, err := upnp.NewContext(c)
	if err != nil {
		return "", fmt.Errorf("upnp: %w", err)
	}

	mapping := upnp.AddPortMappingRequest{
		NewProtocol:               "TCP",
		NewExternalPort:           cfg.PublicPort,
		NewInternalPort:           cfg.Port,
		NewEnabled:                1,
		NewPortMappingDescription: "watchparty",
		NewLeaseDuration:          86400,
	}
	if _, err = upnpClient.AddPortMapping(mapping); err != nil {
		return "", fmt.Errorf("upnp: %w", err)
	}
	cleanup.add(func() {
		upnpClient.DeletePortMapping(upnp.DeletePortMappingRequest{NewExternalPort: cfg.PublicPort, NewProtocol: "TCP"})
	})

	externalIp, err := upnpClient.GetExternalIPAddress()
	if err != nil {
		return "", fmt.Errorf("upnp: %w", err)
	}
	publicIp := flatten(externalIp.NewExternalIPAddress)
	if upnp.IPClass(publicIp) != upnp.IPPublic {
		log.Printf("unreachable address: %s can't be reached from outside this network, your connection may be behind carrier NAT; use a relay for friends elsewhere\n", publicIp)
	}
	return fmt.Sprintf("%s:%d", publicIp, cfg.PublicPort), nil
}

// serveMesh starts the HTTP server peers send to, and the broadcast to them.
func serveMesh(c context.Context, cancel context.CancelFunc, incoming chan mpv.IncomingMessage, outgoing chan []byte, cfg Config, file, video, address string, cleanup *cleanups) (*server.Server, error) {
	ser := server.New(c, incoming, address, cfg.Hostname, file, video, cfg.Secret, cmp.Or(cfg.EvictAfter, DefaultEvictAfter))
	mux := http.NewServeMux()
	mux.HandleFunc("/hi", ser.Hi)
	mux.HandleFunc("/bye", ser.Bye)
	mux.HandleFunc("/event", ser.Event)
	s := &http.Server{
		Addr:              fmt.Sprintf("0.0.0.0:%d", cfg.Port),
		Handler:           ser.Guard(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8192,
		BaseContext:       func(_ net.Listener) context.Context { return c },
	}
	cleanup.add(func() {
		shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		s.Shutdown(shutdownContext)
	})
	cleanup.add(ser.Shutdown)

	// listen here, not in the goroutine: a port in use must be the error Run returns. Found
	// later, it only cancelled c, so a joiner said nobody answered and a host just stopped
	l, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return nil, fmt.Errorf("can't take port %d, maybe another program has it; choose another port: %w", cfg.Port, err)
	}
	go func() {
		err := s.Serve(l)
		if err != nil {
			cancel()
			log.Println(err)
		}
	}()
	go ser.BroadcastEvents(outgoing)
	return ser, nil
}

// joinMesh says hello to each address in addrs. Given none, this node started the party, so
// its position is the party's.
func joinMesh(c context.Context, ser *server.Server, addrs string, incoming chan mpv.IncomingMessage) error {
	asked, answered := false, false
	for addr := range strings.SplitSeq(addrs, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		asked = true
		err := ser.AddAddress(addr)
		if errors.Is(err, server.ErrDifferentFile) || errors.Is(err, server.ErrWrongSecret) {
			return err
		}
		if err != nil {
			log.Printf("%s: %s\n", addr, err)
			continue
		}
		answered = true
	}
	// a joiner nobody answered would play alone, told it is in the party
	if asked && !answered {
		return errNobody
	}
	if !asked {
		select {
		case incoming <- mpv.IncomingMessage{Opened: true}:
		case <-c.Done():
		}
	}
	return nil
}
