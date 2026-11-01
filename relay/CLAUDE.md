# relay/CLAUDE.md

The websocket relay, a separate `package main` for people UPNP can't help (carrier NAT,
locked routers, double NAT). Every node dials **out** to it. It runs on a public VPS,
so half of this file is about abuse. The client is `party/ws.go`, which also holds the
table of how a node reacts to each status. This package never imports `party`.

```sh
go test -race ./relay
docker build -f relay/Dockerfile -t watchparty-relay .     # from the REPO ROOT
[ADDR=:8080] [CREATE_KEY=k] [TRUST_PROXY=true] [ROOMS_PER_HOUR=10] [MAX_ROOMS=10000] [MAX_CONNS=5000] ./relay
```

**Env vars only, no flags.** An empty value means the default; one that doesn't parse
is fatal at startup. The `Dockerfile` lists every one in its `ENV`, so a new variable
goes there too. Not in the release; deployed by hand or as the image.

## Why this shape

- **Websocket**, because it is HTTP until it isn't: it passes proxies and sits behind
  caddy/nginx for TLS. SSE needs a second channel upstream, raw TCP dies at corporate
  proxies, and WebRTC needs a signalling server *and* TURN, more than this relay.
  `coder/websocket`: no dependencies, context-first.
- **Dumb fan-out.** Frames are forwarded as opaque bytes and never sent back to their
  sender, so the relay needs no counters and never parses playback. It only synthesizes
  notices, as `show-text` events (so it imports `mpv`).
- **Rooms are memory only.** The first joiner's secret and file define the room; it is
  deleted when its last member leaves. No accounts, no persistence, no playback state.
- **Accepted:** `hostname` is whatever a member claims, so anyone with the secret can
  impersonate one, as in P2P. Volumetric floods must be absorbed upstream; the relay
  only bounds what reaches its handler.

## Protocol

- `GET /ws?room=&hostname=&session=&file=` upgrades. The secret is in the
  **`room-secret`** header, never the query (proxies log it); the optional create key
  in **`create-key`**. `file` isn't secret.
- Notices: "X has joined" and "X has left" to the room, and "connected to Y" to the
  joiner once per member already there, so the last joiner learns who is in the room.

## Request handling, cheapest rejection first

So a flood is refused before anything is allocated, and nobody who fails is upgraded:

1. **Connection cap** → 503. `conns.Add(1)` then check: a `Load()` check let a whole
   burst pass before any incremented.
2. Missing `room` or `hostname` → 400.
3. **`admit`**, under `r.mu`:
   - Existing room: constant-time secret compare. Wrong → 403, and it costs rate-limit
     quota (429 once over). Right secret but another `file` → 409, free, and only after
     the secret, so outsiders can't probe what a room plays.
   - New room with `CREATE_KEY` set and no key sent → 401, free: a guest who arrived
     before the host, and the node keeps retrying.
   - New room: rate limit (429) → create-key compare (403, after the quota is spent, so
     guessing keys costs) → `MAX_ROOMS` (503) → create.
4. `websocket.Accept`. On failure, `leave(name, nil)` reaps a room `admit` just made,
   announcing nothing.
5. **`add`** registers the member only once `m.conn` is set, or `broadcast` could reach
   a nil conn.
   - Room gone (its last member left between this `admit` and `add`): close with
     `StatusTryAgainLater`, and the node redials through `admit`. Before, the joiner
     stayed connected to nothing.
   - **Same non-empty `session` already there:** swap it in and `CloseNow` the old one,
     with no "has joined", and the old one's `leave` sends no "has left". Keyed on
     session, not hostname: two machines with one hostname kicked each other out.
   - **Notices are queued inside `add`, under the same `r.mu`**, non-blocking, so
     "has joined" and the joiner's "connected to" list describe the same instant.
     Broadcast after `add` returned, a member joining in between got both, and a
     reconnect could be told it had joined itself.
6. Read loop with a **4096-byte** read limit (mpv lines are tiny; the 32KB default lets
   one member push junk to everyone).

## Fan-out and writes

- **`broadcast`** snapshots members under `r.mu`, releases it, then does a non-blocking
  send into each member's **64-frame** channel. A full channel means a dead client:
  `CloseNow`. Never write to N sockets from the sender's read goroutine; one stalled
  TCP connection would freeze the party.
- **`write`**, one goroutine per member: 10s per write, **a ping every 30s** (a party
  sits idle for hours, and NAT and proxy idle timeouts kill silent connections). Any
  error ends it, and it **`CloseNow`s the conn on its way out**, which fails the read
  loop, which calls `leave`. Without that, a member that stopped answering pings kept
  its read loop and its room forever. The intervals are `r.ping`/`r.timeout` fields so
  tests shorten them.

## Rate limiter: a count-min sketch. Do not turn it back into a map

- `[4][1 << 20]uint16` with 4 `maphash` seeds: **8MB allocated once, zero allocation
  per check**, `clear()`ed each hour instead of evicting per key.
- **A `map[ip]int` is the memory-exhaustion attack it exists to prevent:** 100k botnet
  addresses mint 100k entries, and capping the map only picks failing open or closed.
  `x/time/rate`'s per-key form has the same problem.
- It over-counts on collision and never under-counts, so an attacker can't collide into
  extra quota; the cost is a rare false rejection (~1e-8 at 10k addresses an hour).
  Seeds are per process, so collisions can't be targeted. Counters saturate.
- Counted against `ROOMS_PER_HOUR`: room creations, failed secrets, any create that
  sends a key. Free: joining an existing room (or a party of eight would use up one
  person's quota) and a keyless create while `CREATE_KEY` is set.

## Client IP and HTTP server

- **`TRUST_PROXY` takes the last `X-Forwarded-For` entry**: with one proxy appending,
  that is the proxy's own view of the peer, unforgeable; the first is attacker-supplied.
  Without the flag behind a proxy (or in Docker, where everyone arrives from the bridge
  IP), every user shares one bucket and the relay bricks itself at room 11.
- **No `ReadTimeout`, deliberately:** it puts a deadline on the hijacked conn and kills
  every websocket. `ReadHeaderTimeout` 5s, `IdleTimeout` 60s, `MaxHeaderBytes` 8KB:
  Go's defaults are no timeout, which is free slowloris.
- On SIGINT/SIGTERM, `Shutdown` gets 5s. Hijacked websockets end because `BaseContext`
  is the signal context. No TLS: put a TLS proxy in front and set `TRUST_PROXY=true`.

## Docker and compose

- Build from the repo root: the relay needs `go.mod` and `mpv/`. `COPY . .` makes
  **`.dockerignore` load-bearing** (`.bin` is most of the context).
- `scratch` with a static `CGO_ENABLED=0` binary: no shell, libc or CA bundle, since the
  relay never dials out.
- It listens on **:80** as root. Docker 20.10+ allows unprivileged ports below 1024 in
  containers, so `USER 65534:65534` would also bind :80 (verified). Under podman or a
  hardened host, use `ADDR=:8080` and `-p 80:8080`.
- Compose: `printf 'CREATE_KEY=%s\n' "$(openssl rand -hex 16)" > relay/.env` (also
  `PORT`, `TRUST_PROXY`), then `docker compose -f relay/compose.yaml up -d --build`.
  `relay/.env` must exist and is ignored by git and Docker. `PORT` is the host port,
  read by compose; leave `ADDR` alone there.

## Tests (`main_test.go`)

Real websockets against `httptest`, all parallel.

- **`join` returns only once the relay has registered the member**, not when the
  handshake completes: `add` runs after `Accept`, so a frame sent in between misses the
  newcomer. `serve` records each URL's relay for this.
- **`mark` + `noticesBefore`** prove an absence without a timeout: a member's queue is
  FIFO, so whatever was sent before the marker arrives first. A joiner's own notices
  are queued after `add`, so mark **from** the joiner first, then **to** it.
- End to end: `e2e.py --mode relay` (fan-out, isolation, reconnect through a TCP proxy)
  and `--mode caps` (every cap, `CREATE_KEY`, `[R20]`'s socket that never answers
  pings).
