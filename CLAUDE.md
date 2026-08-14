# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
go build ./...
go test ./...
go test -race ./...                        # the invariants below are all concurrency ones
go test ./mpv -run '^TestName$' -v         # single test
go mod tidy                                # CI runs this before building
./e2e.py                                   # end-to-end suite, real mpv, ~6 min
./e2e.py --file movie.mkv                  # ...against a real file
./e2e.py --nodes 3                         # fewer nodes, faster
```

`e2e.py` builds the binary, generates a test file with ffmpeg and runs five
headless nodes on this machine, driving them through mpv's IPC socket. No node is
given the full peer list — A is the root, the rest bootstrap off an earlier node
— so the mesh has to close itself through `/hi` peer lists. It covers what the Go
tests cannot: mesh formation, propagation from any node, slave suppression, peer
eviction and strike reset, `/bye`, and the header checks. Needs `mpv` on PATH and
a `-local`-reachable interface.

Nodes are staggered ~1.6s apart on startup: the socket is named
`/tmp/mpv<unixtime>`, so two started in the same second would collide on one path.

Release builds (`.github/workflows/publisher.yml`, on tag push) cross-compile via a `strategy.matrix` for linux/windows/darwin, amd64+arm64. It builds `.` rather than `main.go`: naming the file works today only because `main` is a single file, and it fails outright when run from outside the module directory. No linter is configured.

Manual run (two terminals / two machines):

```sh
./watchparty -local -file movie.mkv -port 6969
./watchparty -local -file movie.mkv -port 6970 -addrs 192.168.1.5:6969
```

## Architecture

Peer-to-peer mpv sync. Every node is both an HTTP server and a client to every other node — no central host. `main.go` wires three pieces together:

1. **Address discovery** — `-local` advertises the LAN IP; otherwise UPNP adds a port mapping and the external IP is advertised (mapping deleted on exit). Peers are keyed by the address the remote advertises in `hit-me-up`, not the string you passed to `-addrs`. Under `-local` that is the LAN IP, so `-addrs 127.0.0.1:6969` completes the `/hi` handshake and then has every event rejected with `does not exists`. Always use the address the node prints.
2. **`server/`** — HTTP peer mesh on `/hi` (handshake), `/event` (playback event), `/bye` (disconnect). All POST, all identity carried in headers: `hit-me-up` (address), `counter`, `hostname`.
3. **`mpv/`** — mpv JSON IPC client over a unix socket (`/tmp/mpv<unixtime>`) or Windows named pipe (`\\.\pipe\...`), selected by build tag in `conn_unix.go` / `conn_windows.go`.

Two channels bridge them, both created in `main.go`:

- `outgoing chan []byte` — `mpv.Client.watch()` → `server.BroadcastEvents` → POST `/event` to every peer.
- `incoming chan mpv.IncomingMessage` — server handlers → `mpv.Client.ProcessIncomingEvents` → IPC commands back into mpv.

Event payloads travel as **raw mpv JSON bytes end to end**; the server never parses them beyond transport metadata. The server does construct synthetic `show-text` events (join/leave/connect notices) and pushes them onto `incoming` directly.

### Loop prevention

Two independent mechanisms, both needed:

- **Counters** — each node has a monotonic `counter` bumped on every broadcast; each peer entry stores the last counter seen from that peer. `Server.Event` drops anything with `counter <= peer.Counter`. `Shutdown` sends `math.MaxUint64` so the `/bye` always wins.

`Shutdown` must pass `context.WithoutCancel(s.c)` to `broadcast`. `s.c` is `main`'s `signal.NotifyContext`, which SIGINT/SIGTERM has already cancelled by the time the deferred `ser.Shutdown()` runs — deriving the 10s timeout from it makes every `/bye` fail instantly with "terminated signal received", and peers only lose the leaver via the 3-strike eviction.
- **`role == slave`** — a node that applied a remote pause becomes a slave and stops broadcasting its own events (`errSlave` in `handleEvent`) until it un-pauses, at which point the role clears. Nodes started with `-addrs` begin as slaves.

Only the node that *generates* an event broadcasts — slaves applying a remote pause do not. So a peer discovers a dead peer only when it drives something itself, which is why eviction is per-node and not simultaneous across the mesh.

### Sync semantics (deliberately narrow)

Only `pause` and `time-pos` are observed (`observe_property_string`) and only `property-change` events are forwarded — everything else returns `errDoNotSend`. `time-pos` is only forwarded and only applied **while paused**, so seeking works as: pause → seek → peers sync position → resume. The `seek` event case in `handleEvent` is commented out on purpose; don't re-enable it without understanding that interaction. This makes the `seek` const look unused to linters — leave it.

### mpv IPC request/response

`connection.do()` writes a request with a random `request_id` straight to the socket and blocks on `resCh` until a matching id arrives (5s timeout). The single reader is `watch()`, which demultiplexes: lines with a `request_id` go to `resCh`, lines with an `event` field go to `handleEvent`. There is one reader goroutine — never read `conn.scanner` elsewhere.

`resCh` is shared, and `do()` discards responses whose id doesn't match — so two concurrent `do()` calls steal each other's replies. That holds today only because every caller is `ProcessIncomingEvents` (one goroutine) or `New` before it starts. Fan `do()` out to more goroutines and you need a per-request channel first.

`main.go` sleeps `-cooldown` seconds (default 5) before dialing, because mpv creates the socket lazily after start.

### Concurrency invariants

Each of these has already caused a real bug — they are easy to reintroduce because the code reads fine locally:

- **Never hold `c.mu` across `conn.do()`.** `do()` waits on `resCh`, which only `watch()` fills, and `watch()` needs `c.mu` for `handleEvent`. Holding it guarantees the 5s timeout instead of a response. `pause()` and `sync()` both take the lock, mutate, release, *then* do IPC.
- **In `pause()`, `role` must be set before the IPC call.** mpv echoes the change back as an event, and `watch()` needs to see `slave` to suppress rebroadcasting it.
- **`broadcast()` must not hold `s.mu` across the fan-out.** It snapshots the peers under the lock, releases, then waits — an earlier version held `RLock` until `wg.Wait()` returned, so one blocking send wedged every handler behind the next writer. Eviction re-takes the lock afterwards and re-checks `s.addresses[p.address] == p`, so a peer that reconnected via `/hi` in the meantime isn't dropped.
- **Synthetic OSD events go through `Server.notify`, which is `select`/`default`.** Notices are cosmetic and nothing drains `incoming` until `ProcessIncomingEvents` starts, so dropping them is correct. Real playback events in `Server.Event` keep a blocking send, bounded by `req.Context()`.
- **Never `log.Fatal` / `os.Exit`.** It skips `main`'s defers, which leaks the UPNP port mapping on the router and skips the `/bye` broadcast. Failing goroutines call the `cancel` passed down from `main` instead (same pattern as the `ListenAndServe` goroutine).
- **`bytes.Clone` anything from `scanner.Bytes()`** before putting it on a channel; `Scan()` overwrites that buffer and the consumer reads it later.

### Conventions

The code carries almost no comments — match that. Explanatory comments get removed in review; put the reasoning in the commit message or here instead.

Tests: `server/server_test.go` drives `broadcast` directly with a stub `f` — no HTTP server, no fixtures. `mpv/mpv_test.go` feeds `watch()` through a `dripReader` that returns ~7 bytes per `Read` with a small `sc.Buffer`, because the scanner-aliasing bug does **not** reproduce when the whole input fits one buffer fill. When fixing a concurrency/buffer bug here, reintroduce it and confirm the test fails before trusting it.

mpv emits no `property-change` for a no-op set, so `set_property pause yes` when already paused produces nothing. Tests that count events must strictly alternate rather than repeat a value.

`e2e.py` recovers each node's OSD notices by parsing its `do: sent request:` log line. That format is load-bearing for the suite — change it and update `osd()` in the same commit.

## Known issues

Confirmed by reading the code, not yet fixed:

- [ ] Real playback events still block on `incoming` during the `-cooldown` window, since `ProcessIncomingEvents` only starts after it — the 1024 buffer is what carries the gap. `Server.Event` bails out on `req.Context()` rather than wedging, but nothing is applied until mpv is up.
- [ ] `/bye` and `/event` trust the `hit-me-up`/`counter` headers. The counter check stops replays and unknown addresses, but a peer can still spoof another peer's address. Needs signing to go further, which is more than a LAN tool wants.

## Reference

- mpv JSON IPC: https://mpv.io/manual/master/#json-ipc
- mpv properties: https://mpv.io/manual/master/#properties
