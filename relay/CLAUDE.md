# relay/CLAUDE.md

`relay` is the websocket relay server, a separate `package main` binary for people
whose UPNP never works (carrier NAT, locked-down routers). Every node dials **out** to
it, so nobody needs to be reachable from outside. It runs on a public VPS, which is why
half of this file is about abuse.

The client side is `party/ws.go`. This package never imports `party`.

```sh
go build ./relay
go test -race ./relay
docker build -f relay/Dockerfile -t watchparty-relay .     # from the REPO ROOT
[ADDR=:8080] [CREATE_KEY=k] [TRUST_PROXY=true] [ROOMS_PER_HOUR=10] [MAX_ROOMS=10000] [MAX_CONNS=5000] ./relay
```

It takes **no flags, only env vars**, read by `env()` in `main.go`. An empty value
means the default, and a value that doesn't parse is fatal at startup. The
`Dockerfile` lists every one of them in its `ENV`, so a new variable goes there too.

It is **not** in `publisher.yml`'s release matrix. It is deployed by hand or as the
Docker image.

## Protocol

- `GET /ws?room=<name>&hostname=<display name>&session=<random>` is upgraded to a
  websocket.
- The secret goes in the **`room-secret`** header, never the query string, which
  proxies log. The optional create key goes in **`create-key`**.
- Frames are text messages of the form `{"hostname": "…", "event": <raw mpv JSON>}`.
  The relay forwards them **as opaque bytes**. It never parses playback data, and it
  never sends a frame back to its sender.
- Notices it synthesizes use the same envelope with a `show-text` event:
  - "X has joined" goes to the room.
  - "connected to Y" goes to the joiner, once per member already there.
  - "X has left" goes to the room.

  The GUI's party history parses "X has joined" and "connected to Y" to record who
  was there. `server/` sends the same texts in P2P mode, so change both or neither.
- **`message` is declared again in `party/ws.go`.** This package is `package main`,
  so it can't be imported. Importing `party` from here would pull upnp, server and
  mpv into the relay image. Keep the two structs in sync by hand.

## Request handling, in cost order

`handle` rejects as cheaply as possible, so a flood is refused before anything is
allocated:

1. **Connection cap.** `conns.Add(1) > maxConns` → 503. It reserves first and checks
   after: with a `Load()` check, a burst would all pass before any of them
   incremented.
2. **`room` and `hostname` are required** → 400. The node treats 400 as fatal
   (`errRejected`) and stops retrying.
3. **`admit`**, under `r.mu`:
   - **Existing room:** a constant-time secret compare. If it's wrong, the attempt
     costs rate-limit quota (429 once over the limit), otherwise 403.
   - **New room, `CREATE_KEY` set and no key sent** → 401, which costs no quota. This
     is a guest who arrived before the host; the node keeps retrying.
   - **New room:** rate limit (429) → create-key compare (403, after the quota was
     spent, so guessing keys costs) → `maxRooms` (503) → create the room with this
     caller's secret.
4. **`websocket.Accept`.** If it fails, `leave(name, nil)` reaps the room `admit` may
   have just created.
5. **`add`** registers the member only now, once `m.conn` is set, so `broadcast` can
   never reach a nil conn.
   - If a member with the same non-empty `session` is already there, it is swapped
     out and `CloseNow`ed. The room gets no new "has joined", and the stale member's
     `leave` sends no "has left", because it is no longer present.
   - This is keyed on session, not hostname: two machines that share a hostname would
     otherwise keep kicking each other out. An empty session (an old client) is never
     deduped.
6. The writer goroutine starts, the notices go out, then it reads in a loop. Each
   frame (read limit **4096 bytes**) is passed to `broadcast`.

| Status | Meaning | Node reaction (`party/ws.go`) |
|---|---|---|
| 400 | missing room or hostname | gives up |
| 401 | room not open, no create key sent | retries |
| 403 | wrong secret or wrong create key | gives up, logs `rejected by relay` |
| 429 | rate limit | retries with backoff |
| 503 | `maxConns` / `maxRooms` | retries with backoff |

## Fan-out and writes

- **`broadcast`** snapshots the members under `r.mu`, releases it, then does a
  non-blocking send into each member's **64-frame** `ch`. A full buffer means a dead
  client, not a slow one: the member's conn is `CloseNow`ed. **Never write to N
  sockets from the sender's read goroutine.** One stalled TCP connection must not
  freeze the party.
- **`write`** is one goroutine per member. It drains `ch` with a 10s timeout per
  write, and **pings every 30s**. A party sits idle for hours between pauses, and NAT
  and proxy idle timeouts kill silent connections. Any error ends the writer, and the
  read loop's failure then cleans up.
- **`leave`** deletes the member, deletes the room once it is empty (rooms are
  memory-only), and announces "has left" only for a member it actually removed.

## Rate limiter: a count-min sketch. Do not turn it back into a map

- `limiter` is `[4][16384]uint16` with 4 `maphash` seeds: **128KB, allocated once**,
  with zero allocation per check. On window rollover (1 hour) the whole thing is
  `clear()`ed; nothing is evicted per key.
- **Why not a map:** a `map[ip]int` allocates one entry per distinct source address.
  A botnet with 100k addresses would mint 100k entries, and the rate limiter would
  become the memory-exhaustion attack it exists to prevent. Capping the map only
  chooses between failing open and failing closed.
- A sketch over-counts on collisions and never under-counts. The error is one-sided
  in the safe direction: the cost is an occasional false rejection for a busy shared
  NAT (`ponytail:` on `limiter`, bump `cols` if that shows up).
- Saturating `uint16` counters can't wrap around to 0.
- What counts against `ROOMS_PER_HOUR`:
  - room creations
  - failed secret attempts
  - any create that sends a key, right or wrong
- What is free:
  - joining an existing room (or a party of eight would exhaust one person's quota)
  - a keyless create while `CREATE_KEY` is set (a guest waiting for the host)

## Client IP: `TRUST_PROXY`

- Without it, the client IP is `r.RemoteAddr`.
- With it, it is the **last** `X-Forwarded-For` element. With one proxy appending, the
  last entry is the proxy's own view of the direct peer, and the client can't forge
  it.
- Getting this wrong behind a TLS-terminating proxy puts every user in one bucket, and
  the relay bricks itself at room 11.

## HTTP server settings

- **No `ReadTimeout`, deliberately.** It puts an absolute deadline on the hijacked conn
  and kills every long-lived websocket.
- `ReadHeaderTimeout` 5s, `IdleTimeout` 60s and `MaxHeaderBytes` 8KB are set. Go's
  defaults are no timeout at all, which is free slowloris.
- On SIGINT/SIGTERM, `Shutdown` gets 5s. Websockets are hijacked, so `Shutdown` doesn't
  track them. They end because `BaseContext` is the signal context, which cancels each
  handler's `req.Context()`.
- There is no TLS. For `wss://`, put a TLS-terminating proxy in front and set
  `TRUST_PROXY=true`.

## Docker (`Dockerfile`)

- **Build from the repo root:** the relay is one package in the module and needs
  `go.mod` and `mpv/` (for `mpv.Event`).
- `COPY . .` makes **`.dockerignore` load-bearing**. Without it, `.bin` (about 10MB of
  an 11MB context) ships to the daemon on every build.
- The final stage is `scratch` with the static `CGO_ENABLED=0` binary, about 6MB: no
  shell, no libc, no CA bundle. The relay only listens and never dials out, so certs
  would be dead weight.
- It listens on **:80** as root. Docker 20.10+ sets
  `net.ipv4.ip_unprivileged_port_start=0` in containers, so `USER 65534:65534` would
  also bind :80 (verified). Under podman or a hardened host, set `ADDR=:8080` inside the
  container and publish `-p 80:8080`.

## Compose (`compose.yaml`)

```sh
printf 'CREATE_KEY=%s\n' "$(openssl rand -hex 16)" > relay/.env   # also PORT, TRUST_PROXY
docker compose -f relay/compose.yaml up -d --build
```

- `env_file: .env` resolves next to the compose file, so it is `relay/.env`, and it
  must exist. It is in `.gitignore` and `.dockerignore`.
- `PORT` is the host port and is read by compose, not the relay. Leave `ADDR` alone
  under compose: the container side of the mapping is fixed at `:80`.
- `context: ..` because the build needs the repo root, same as the plain
  `docker build`.

## Tests (`main_test.go`)

The tests use real websockets against an `httptest` server (`serve`, `join`,
`recvEvent` skips the synthesized notices):

| Test | Covers |
|---|---|
| `TestBroadcastReachesEveryoneButTheSender` | no echo to the sender (the reason relay mode needs no counters) |
| `TestJoinerLearnsWhoIsAlreadyThere` | the "connected to" half of the notices |
| `TestReconnectReplacesStaleMember` | session dedupe, with no second join and no false leave |
| `TestWrongSecretIsRejected` | wrong secrets |
| `TestSecretGrindingIsRateLimited` | grinding secrets costs quota |
| `TestOnlyRoomCreationIsRateLimited` | joins are free |
| `TestCreateKey` | keyless 401 costs no quota, a wrong key costs quota |
| `TestMaxRoomsAndMaxConns` | the global caps |
| `TestLimiterDoesNotAllocatePerIP` | the sketch's fixed memory |
| `TestLimiterNeverUnderCounts` | the sketch's safe-direction error |
| `TestLimiterDoesNotFalselyRejectAtRealisticLoad` | sketch width at realistic load |
| `TestLimiterResetsOnWindowRollover` | the window |

The end-to-end versions (fan-out, room isolation, every cap, reconnect through a TCP
proxy, `CREATE_KEY`) are `e2e.py --mode relay`.

## Known issues

- **Identity is only as strong as the room secret.** `hostname` is whatever the joiner
  claims, so anyone who knows the secret can pose as a member.
- **A narrow race between `admit` and `add`.** If the last member leaves after a
  joiner's `admit` but before its `add`, the room is reaped and `add` finds no room.
  The joiner then stays connected without being registered: it hears nothing, and
  nobody hears it. Its next reconnect would recreate the room.
