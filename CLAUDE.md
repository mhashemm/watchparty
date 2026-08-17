# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
go build ./...
go test ./...
go test -race ./...                        # the invariants below are all concurrency ones
go test ./mpv -run '^TestName$' -v         # single test
go mod tidy                                # run it yourself; CI only does go mod download
go build ./relay                           # the relay server, a separate binary
docker build -f relay/Dockerfile -t watchparty-relay .   # from the REPO ROOT
./e2e.py                                   # both suites, real mpv, ~12 min
./e2e.py --mode p2p | --mode relay         # one phase only
./e2e.py --file movie.mkv                  # ...against a real file
./e2e.py --nodes 3                         # fewer nodes, faster
```

`e2e.py` builds both binaries, generates a test file with ffmpeg and runs five
headless nodes on this machine, driving them through mpv's IPC socket. It has two
phases:

- **`p2p`** — no node is given the full peer list (A is the root, the rest
  bootstrap off an earlier node), so the mesh has to close itself through `/hi`
  peer lists. Covers mesh formation, propagation from any node, echo suppression,
  peer eviction and strike reset, `/bye`, and the header checks.
- **`relay`** — the same nodes against one `./relay` process. Covers fan-out,
  room isolation, the shared secret, every abuse cap, and reconnect after the
  relay is killed mid-party.

Needs `mpv` on PATH and a `-local`-reachable interface.

`osd()` returns a **list**, so `.count(x)` is an exact element match, never a
substring search — `osd(n).count("paused by ")` is silently always 0. Use the full
string (`f"paused by {A}"`) or the `pauses()` helper.

Nodes are staggered ~1.6s apart on startup: the socket is named
`/tmp/mpv<unixtime>`, so two started in the same second would collide on one path.

Release builds (`.github/workflows/publisher.yml`, on tag push) cross-compile via a `strategy.matrix` for linux/windows/darwin, amd64+arm64. CI runs `go mod download`, not `go mod tidy`, so an untidy `go.mod` will not be caught there — tidy before you push. It builds `.` rather than `main.go`: naming the file works today only because `main` is a single file, and it fails outright when run from outside the module directory. No linter is configured.

Manual run (two terminals / two machines):

```sh
./watchparty -local -file movie.mkv -port 6969
./watchparty -local -file movie.mkv -port 6970 -addrs 192.168.1.5:6969
```

Or through a relay, which is what people behind carrier NAT need:

```sh
./relay -addr :8080
./watchparty -relay ws://host:8080 -room test -secret hunter2 -file movie.mkv
```

## Architecture

Peer-to-peer mpv sync. Every node is both an HTTP server and a client to every other node — no central host. `main.go` wires three pieces together:

1. **Address discovery** — `-local` advertises the LAN IP; otherwise UPNP adds a port mapping and the external IP is advertised (mapping deleted on exit). Peers are keyed by the address the remote advertises in `hit-me-up`, not the string you passed to `-addrs`. Under `-local` that is the LAN IP, so `-addrs 127.0.0.1:6969` completes the `/hi` handshake and then has every event rejected with `does not exists`. Always use the address the node prints.
2. **`server/`** — HTTP peer mesh on `/hi` (handshake), `/event` (playback event), `/bye` (disconnect). All POST, all identity carried in headers: `hit-me-up` (address), `counter`, `hostname`.
3. **`mpv/`** — mpv JSON IPC client over a unix socket (`/tmp/mpv<unixtime>`) or Windows named pipe (`\\.\pipe\...`), selected by build tag in `conn_unix.go` / `conn_windows.go`.

`-relay <ws url>` selects the relay transport instead of steps 1 and 2: `runMesh` is skipped entirely, so no UPNP, no listener, no `server/`. `runMesh` returns a cleanup closure rather than using `defer` internally, because those defers have to fire in `main`, not when it returns.

Two channels bridge them, both created in `main.go`:

- `outgoing chan []byte` — `mpv.Client.watch()` → `server.BroadcastEvents` → POST `/event` to every peer.
- `incoming chan mpv.IncomingMessage` — server handlers → `mpv.Client.ProcessIncomingEvents` → IPC commands back into mpv.

Event payloads travel as **raw mpv JSON bytes end to end**; the server never parses them beyond transport metadata. The server does construct synthetic `show-text` events (join/leave/connect notices) and pushes them onto `incoming` directly.

### Loop prevention

**There is no master and no slave.** Every peer can drive the player at any time.
A `role == slave` field used to exist as an echo-suppression hack — it muted
whoever last applied a remote pause — and it is gone. It overshot: it also
swallowed that peer's *own* deliberate seeks, so nobody followed them.

Two mechanisms now, in `mpv/mpv.go`, and they apply to both transports:

- **`applied`** (`map[string]string`, property → last value applied from a remote
  event) — the a→b→a rebound guard. b applies a's pause, mpv echoes the change
  back as a `property-change`, and `handleEvent` drops it instead of broadcasting
  it to a. Set in `pause()`/`sync()`, checked at the top of `handleEvent`, returns
  `errEcho`.

  `pause` compares exactly and the entry is deleted on match. **`time-pos`
  compares as floats with a ±0.5s tolerance and keeps the marker**, because mpv
  snaps a seek to a keyframe — applying `77.000000` can echo `77.041000`, which an
  exact compare would miss and rebroadcast, walking the position forward one hop
  per peer. The marker is cleared when the node un-pauses.

- **`seen`** (`map[string]bool`) — swallow the *first* `property-change` per
  property. `observe_property_string` makes mpv fire one immediately on
  registration; a fresh node is paused, so that initial `time-pos` skips the
  `!c.paused → errDoNotSend` guard and would be broadcast as `time-pos=0`,
  rewinding the whole party. State is still updated, only the broadcast is
  suppressed. This is what the old `startAsSlave` argument was really for, and it
  now covers the first node too.

- **Counters** (P2P only, `server/`) — each node has a monotonic `counter` bumped on every broadcast; each peer entry stores the last counter seen from that peer. `Server.Event` drops anything with `counter <= peer.Counter`. `Shutdown` sends `math.MaxUint64` so the `/bye` always wins. Relay mode has no counters and does not need them: the relay never echoes a frame back to its sender, so `applied` is the only thing between it and an infinite loop.

`Shutdown` must pass `context.WithoutCancel(s.c)` to `broadcast`. `s.c` is `main`'s `signal.NotifyContext`, which SIGINT/SIGTERM has already cancelled by the time the deferred `ser.Shutdown()` runs — deriving the 10s timeout from it makes every `/bye` fail instantly with "terminated signal received", and peers only lose the leaver via the 3-strike eviction.

Only the node that *generates* an event broadcasts — a peer applying a remote change does not re-emit it. So a peer discovers a dead peer only when it drives something itself, which is why eviction is per-node and not simultaneous across the mesh.

### Sync semantics (deliberately narrow)

Only `pause` and `time-pos` are observed (`observe_property_string`) and only `property-change` events are forwarded — everything else returns `errDoNotSend`. `time-pos` is only forwarded and only applied **while paused**, so seeking works as: pause → seek → peers sync position → resume. **Any** paused peer can seek for the party, not just whoever drove the pause — that restriction was an artifact of the removed `slave` hack. The `seek` event case in `handleEvent` is commented out on purpose; don't re-enable it without understanding that interaction. This makes the `seek` const look unused to linters — leave it.

### `relay/` (websocket transport)

A separate `package main` binary (`go build ./relay`) for people whose UPNP never
works. Both sides dial *out* to it, so nobody needs inbound reachability. It is
**not** in `publisher.yml`'s release matrix — it is deployed by hand to a server,
or as the image built from `relay/Dockerfile`.

**`relay/Dockerfile` builds from the repo root, not from `relay/`** — the relay is
one package inside the module and needs `go.mod` and `mpv/` next to it:
`docker build -f relay/Dockerfile -t watchparty-relay .`.

It does `COPY . .`, so **`.dockerignore` is load-bearing**: without it `.bin`
(~10MB of an 11MB context) is shipped to the daemon on every build. Only the
builder stage sees any of it — the final stage is `scratch` with just the static
`CGO_ENABLED=0` binary, ~6MB, no shell, no libc, no CA bundle. The relay only
listens and never dials out, so certs would be dead weight; add them only if that
changes.

The container listens on **:80** and runs as **root**. Non-root is not actually a
tradeoff here: Docker 20.10+ sets `net.ipv4.ip_unprivileged_port_start=0` inside
containers, so `USER 65534:65534` binds :80 fine (verified). Under podman or a
hardened host that sysctl may differ, in which case listen on :8080 in-container
and publish `-p 80:8080`.

Relay mode reuses **nothing** from `server/`, on purpose. That package solves
problems the relay does not have: counters (the relay never echoes to the sender),
3-strike eviction (there is one connection, not N), `/hi` gossip (the relay *is*
the membership list), and UPNP (nothing listens locally). `server/` is untouched
by this feature.

- **Dumb fan-out.** The relay forwards opaque bytes and never parses playback
  data. The node builds the envelope: `{"hostname":…, "event":<raw mpv JSON>}`,
  declared identically in `relay/main.go` and `ws.go` because two `package main`s
  cannot import each other.
- **Rooms** are memory-only. The first joiner's secret defines the room's secret
  (`crypto/subtle.ConstantTimeCompare`); the room is deleted when the last member
  leaves. The secret rides in a header, not the query string, which proxies log.
- **The room learns about the joiner and the joiner learns about the room.**
  Without the second half — a `connected to X` notice per existing member — the
  last node in would think it was alone.
- **Fan-out never writes to N sockets from the sender's read goroutine.** Each
  member has a 64-frame buffered channel and its own writer goroutine; the send is
  `select`/`default` and a member 64 frames behind is closed. One stalled TCP
  connection must not freeze the party.
- **30s `conn.Ping`** per member. A party sits idle for hours between pauses, and
  NAT/proxy idle timeouts kill the connection silently without it.
- **No `ReadTimeout` on the `http.Server`**, deliberately: it puts an absolute
  deadline on the hijacked conn and kills every long-lived websocket.
  `ReadHeaderTimeout`/`IdleTimeout`/`MaxHeaderBytes` are set — Go defaults to no
  timeout at all, which is free slowloris.
- **The node gives up on 403/400 (`errRejected`) but retries everything else** with
  1s→30s backoff. A wrong secret never becomes right, and retrying it just burns
  the relay's rate limiter.

### Relay hardening (it runs on a public VPS)

The room secret is the only auth, so the caps below are load-bearing, not
decoration. Handler rejects in cost order — **conn cap → rate limit → secret →
room cap → `Accept`** — so a flood is refused before anything is allocated.

**The rate limiter is a count-min sketch, not a `map[string]int`. Do not
"simplify" it back into a map.** A per-IP map allocates one entry per *distinct*
source address, so a botnet with 100k addresses mints 100k entries and the rate
limiter becomes the memory-exhaustion attack it exists to prevent. Capping the map
only picks between failing open (no limit for new IPs) and failing closed (service
dead). The sketch is `[4][16384]uint16` — **128KB, allocated once, forever**, with
zero allocation per check, and the whole thing is `clear()`ed on window rollover
rather than evicted per key.

A sketch over-counts on collision but never under-counts, so the error is one
sided in the safe direction: an attacker cannot collide their way into extra
quota, and the cost is an occasional false rejection for a busy shared NAT.

Counted against `-roomsPerHour`: **room creation only** (joining an existing room
is free, or a party of eight would exhaust one person's quota) and **failed secret
attempts** (the brute-force floor). `-maxRooms` and `-maxConns` bound the global
totals, which the per-IP limiter cannot.

`-trustProxy` takes the client IP from the **last** `X-Forwarded-For` element, not
the first: with one proxy appending, the last entry is the proxy's own view of the
direct peer and a client cannot forge it. Without the flag, `r.RemoteAddr`. Get
this wrong behind a TLS-terminating proxy and every user lands in one bucket, so
the relay bricks itself at room 11.

### mpv IPC request/response

`connection.do()` writes a request with a random `request_id` straight to the socket and blocks on `resCh` until a matching id arrives (5s timeout). The single reader is `watch()`, which demultiplexes: lines with a `request_id` go to `resCh`, lines with an `event` field go to `handleEvent`. There is one reader goroutine — never read `conn.scanner` elsewhere.

`resCh` is shared, and `do()` discards responses whose id doesn't match — so two concurrent `do()` calls steal each other's replies. That holds today only because every caller is `ProcessIncomingEvents` (one goroutine) or `New` before it starts. Fan `do()` out to more goroutines and you need a per-request channel first.

`main.go` sleeps `-cooldown` seconds (default 5) before dialing, because mpv creates the socket lazily after start.

### Concurrency invariants

Each of these has already caused a real bug — they are easy to reintroduce because the code reads fine locally:

- **Never hold `c.mu` across `conn.do()`.** `do()` waits on `resCh`, which only `watch()` fills, and `watch()` needs `c.mu` for `handleEvent`. Holding it guarantees the 5s timeout instead of a response. `pause()` and `sync()` both take the lock, mutate, release, *then* do IPC.
- **In `pause()` and `sync()`, `applied` must be set before the IPC call.** mpv echoes the change back as an event, and `watch()` has to already see the marker to suppress rebroadcasting it. Setting it after `do()` returns is a race that reads fine.
- **The relay's write pump must not hold `relayConn.mu` across `conn.Write`.** The mutex only guards the pointer, swapped on reconnect; holding it through a 10s write wedges the supervisor's `set()`. Same shape as the `c.mu`/`do()` rule above.
- **Exactly one write pump for the whole process, spanning reconnects.** Restarting it per connection means two goroutines briefly both `range outgoing`, stealing frames from each other.
- **`broadcast()` must not hold `s.mu` across the fan-out.** It snapshots the peers under the lock, releases, then waits — an earlier version held `RLock` until `wg.Wait()` returned, so one blocking send wedged every handler behind the next writer. Eviction re-takes the lock afterwards and re-checks `s.addresses[p.address] == p`, so a peer that reconnected via `/hi` in the meantime isn't dropped.
- **Synthetic OSD events go through `Server.notify`, which is `select`/`default`.** Notices are cosmetic and nothing drains `incoming` until `ProcessIncomingEvents` starts, so dropping them is correct. Real playback events in `Server.Event` keep a blocking send, bounded by `req.Context()`.
- **Never `log.Fatal` / `os.Exit`.** It skips `main`'s defers, which leaks the UPNP port mapping on the router and skips the `/bye` broadcast. Failing goroutines call the `cancel` passed down from `main` instead (same pattern as the `ListenAndServe` goroutine).
- **`bytes.Clone` anything from `scanner.Bytes()`** before putting it on a channel; `Scan()` overwrites that buffer and the consumer reads it later.

### Conventions

The code carries almost no comments — match that. Explanatory comments get removed in review; put the reasoning in the commit message or here instead.

Tests: `server/server_test.go` drives `broadcast` directly with a stub `f` — no HTTP server, no fixtures. `mpv/mpv_test.go` feeds `watch()` through a `dripReader` that returns ~7 bytes per `Read` with a small `sc.Buffer`, because the scanner-aliasing bug does **not** reproduce when the whole input fits one buffer fill. When fixing a concurrency/buffer bug here, reintroduce it and confirm the test fails before trusting it.

mpv emits no `property-change` for a no-op set, so `set_property pause yes` when already paused produces nothing. Tests that count events must strictly alternate rather than repeat a value.

`e2e.py` recovers each node's OSD notices by parsing its `do: sent request:` log line. That format is load-bearing for the suite — change it and update `osd()` in the same commit. The relay suite likewise greps `relay: connected` and `relay: dial` out of node logs.

Every behaviour change lands with its `CLAUDE.md` note and its `e2e.py` case in the same commit.

## Known issues

Confirmed by reading the code, not yet fixed:

- [ ] Real playback events still block on `incoming` during the `-cooldown` window, since `ProcessIncomingEvents` only starts after it — the 1024 buffer is what carries the gap. `Server.Event` bails out on `req.Context()` rather than wedging, but nothing is applied until mpv is up.
- [ ] `/bye` and `/event` trust the `hit-me-up`/`counter` headers (P2P only). The counter check stops replays and unknown addresses, but a peer can still spoof another peer's address. Needs signing to go further, which is more than a LAN tool wants.
- [ ] Relay identity is only as strong as the room secret: the `hostname` is whatever the joiner claims, so anyone who knows the secret can impersonate a member. Same trade as above.
- [ ] A relay reconnect is a fresh join, so the room is told "X has joined" again and the stale member lingers until the 30s ping reaps it.

## Reference

- mpv JSON IPC: https://mpv.io/manual/master/#json-ipc
- mpv properties: https://mpv.io/manual/master/#properties
