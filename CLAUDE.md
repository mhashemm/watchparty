# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
go build ./...
go test ./...
go test ./mpv -run '^TestName$' -v         # single test
go mod tidy                                # CI runs this before building
```

Release builds (`.github/workflows/publisher.yml`, on tag push) cross-compile `main.go` for linux/windows/darwin, amd64+arm64. No linter is configured.

Manual run (two terminals / two machines):

```sh
./watchparty -local -file movie.mkv -port 6969
./watchparty -local -file movie.mkv -port 6970 -addrs 192.168.1.5:6969
```

## Architecture

Peer-to-peer mpv sync. Every node is both an HTTP server and a client to every other node — no central host. `main.go` wires three pieces together:

1. **Address discovery** — `-local`/`-oblivious` advertises the LAN IP; otherwise UPNP adds a port mapping and the external IP is advertised (mapping deleted on exit).
2. **`server/`** — HTTP peer mesh on `/hi` (handshake), `/event` (playback event), `/bye` (disconnect). All POST, all identity carried in headers: `hit-me-up` (address), `counter`, `hostname`.
3. **`mpv/`** — mpv JSON IPC client over a unix socket (`/tmp/mpv<unixtime>`) or Windows named pipe (`\\.\pipe\...`), selected by build tag in `conn_unix.go` / `conn_windows.go`.

Two channels bridge them, both created in `main.go`:

- `outgoing chan []byte` — `mpv.Client.watch()` → `server.BroadcastEvents` → POST `/event` to every peer.
- `incoming chan types.IncomingMessage` — server handlers → `mpv.Client.ProcessIncomingEvents` → IPC commands back into mpv.

Event payloads travel as **raw mpv JSON bytes end to end**; the server never parses them beyond transport metadata. The server does construct synthetic `show-text` events (join/leave/connect notices) and pushes them onto `incoming` directly.

### Loop prevention

Two independent mechanisms, both needed:

- **Counters** — each node has a monotonic `counter` bumped on every broadcast; each peer entry stores the last counter seen from that peer. `Server.Event` drops anything with `counter <= peer.Counter`. `Shutdown` sends `math.MaxUint64` so the `/bye` always wins.
- **`role == slave`** — a node that applied a remote pause becomes a slave and stops broadcasting its own events (`errSlave` in `handleEvent`) until it un-pauses, at which point the role clears. Nodes started with `-addrs` begin as slaves.

### Sync semantics (deliberately narrow)

Only `pause` and `time-pos` are observed (`observe_property_string`) and only `property-change` events are forwarded — everything else returns `errDoNotSend`. `time-pos` is only forwarded and only applied **while paused**, so seeking works as: pause → seek → peers sync position → resume. The `seek` event case in `handleEvent` is commented out on purpose; don't re-enable it without understanding that interaction. This makes the `seek` const look unused to linters — leave it.

### mpv IPC request/response

`connection.do()` writes a request with a random `request_id` and blocks on `resCh` until a matching id arrives (5s timeout). The single reader is `watch()`, which demultiplexes: lines with a `request_id` go to `resCh`, lines with an `event` field go to `handleEvent`. There is one reader goroutine — never read `conn.scanner` elsewhere.

`main.go` sleeps `-cooldown` seconds (default 5) before dialing, because mpv creates the socket lazily after start.

### Concurrency invariants

Each of these has already caused a real bug — they are easy to reintroduce because the code reads fine locally:

- **Never hold `c.mu` across `conn.do()`.** `do()` waits on `resCh`, which only `watch()` fills, and `watch()` needs `c.mu` for `handleEvent`. Holding it guarantees the 5s timeout instead of a response. `pause()` and `sync()` both take the lock, mutate, release, *then* do IPC.
- **In `pause()`, `role` must be set before the IPC call.** mpv echoes the change back as an event, and `watch()` needs to see `slave` to suppress rebroadcasting it.
- **Nothing inside `broadcast()`'s per-peer goroutines may block.** `broadcast` holds `s.mu.RLock` until `wg.Wait()` returns (LIFO defers), so a blocking channel send there wedges every handler behind the next writer. Sends to `incoming` from that path must be `select`/`default`.
- **Never `log.Fatal` / `os.Exit`.** It skips `main`'s defers, which leaks the UPNP port mapping on the router and skips the `/bye` broadcast. Failing goroutines call the `cancel` passed down from `main` instead (same pattern as the `ListenAndServe` goroutine).
- **`bytes.Clone` anything from `scanner.Bytes()`** before putting it on a channel; `Scan()` overwrites that buffer and the consumer reads it later.

### Conventions

The code carries almost no comments — match that. Explanatory comments get removed in review; put the reasoning in the commit message or here instead.

Tests: `mpv/mpv_test.go` feeds `watch()` through a `dripReader` that returns ~7 bytes per `Read` with a small `sc.Buffer`, because the scanner-aliasing bug does **not** reproduce when the whole input fits one buffer fill. When fixing a concurrency/buffer bug here, reintroduce it and confirm the test fails before trusting it.

## Known issues

Confirmed by reading the code, not yet fixed:

- [ ] `BroadcastEvents` spawns a goroutine per event, so counters arrive out of order and `Server.Event` drops valid events via `counter <= peer.Counter`. Serializing the send loop fixes it.
- [ ] `Server.Event` sends on `incoming` under `RLock`; `AddAddress` does it under the full `Lock` *and* makes 30s HTTP requests while holding it.
- [ ] Nothing drains `incoming` until `ProcessIncomingEvents` starts, which is after the `-cooldown` sleep — the whole handshake window relies on the 1024 buffer.
- [ ] Peers are never removed after failed requests, so every broadcast keeps paying their 10s timeout, and a down peer emits an OSD error notice per event.
- [ ] `close(incoming)`/`close(outgoing)` in `main` run while `watch()` and the HTTP handlers may still be sending — panics on send to a closed channel.
- [ ] `os.Stat`/`os.Remove` on `mpvSocket` is meaningless for a Windows named pipe; guard by build tag.
- [ ] `/bye` checks no counter and no identity — any caller can evict a peer.
- [ ] `AddAddress` has `defer cancel()` inside a loop; contexts pile up until it returns.

## Reference

- mpv JSON IPC: https://mpv.io/manual/master/#json-ipc
- mpv properties: https://mpv.io/manual/master/#properties
