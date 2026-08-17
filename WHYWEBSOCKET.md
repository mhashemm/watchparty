# Why a websocket relay

Design record for the relay transport added on 2026-08-17 (commit `66d6453`,
13 files, +1511/-137).

`CLAUDE.md` documents *how* to work in this repo. This document is the *why*: the
problems that forced each decision, the alternatives rejected, and the things that
broke on the way. Read it before changing the relay, the rate limiter, or the echo
suppression in `mpv/`, because all three look simpler than they are.

---

## 1. The problem

watchparty is peer-to-peer. Every node is an HTTP server and a client to every
other node, and reachability comes from a UPNP port mapping (`main.go`, the
`else` branch of `runMesh`).

That fails for a large fraction of real users:

- **carrier-grade NAT** — the ISP shares one public IP across many customers, so
  there is no port to forward
- **UPNP disabled or absent** on the router, which is common and often
  non-negotiable on ISP-supplied hardware
- **double NAT** — a router behind another router; the mapping lands on the wrong
  device

None of these are fixable from inside the app. A user in that situation cannot
host *or* join, so the tool is simply unusable for them.

**The fix: an optional relay.** One websocket server that both sides dial *out*
to. Outbound TCP works everywhere, so nobody needs inbound reachability. P2P is
untouched and remains the default; the relay is opt-in via `-relay`.

### Why websocket rather than the obvious alternatives

- **Long-poll / SSE + POST** — SSE is one-directional, so it needs a second
  channel for the upstream half, and long-poll reconnect churn is worse than one
  persistent socket for events that arrive in bursts seconds apart.
- **Raw TCP** — works, but dies at every corporate proxy and can't be put behind
  caddy/nginx for TLS without extra plumbing. Websocket is HTTP on the wire until
  it isn't, so it traverses what HTTP traverses.
- **WebRTC data channels** — the "correct" P2P answer, and it would preserve true
  peer-to-peer through NAT via ICE. Rejected: it needs a signalling server *and* a
  TURN server for the cases ICE can't solve, which is strictly more infrastructure
  than the relay we are trying to avoid building. The relay is the TURN server,
  minus everything else.

### Why `github.com/coder/websocket` v1.8.15

Zero dependencies, context-first API (`conn.Read(ctx)`, `conn.Write(ctx, …)`),
which matches how the rest of this codebase already threads `main`'s
`signal.NotifyContext` through everything. It is the only new dependency the whole
feature adds.

---

## 2. Shape of the thing

```
node A ──┐                        ┌── mpv (unix socket / named pipe)
         ├── ws ── relay ── ws ───┤
node B ──┘                        └── mpv
```

The relay is a **separate `package main` binary** under `relay/`, built with
`go build ./relay`. It is not in the release matrix (`publisher.yml` builds `.`);
it is deployed by hand or as a container, because it is server infrastructure, not
something an end user downloads.

### The relay reuses nothing from `server/`

This was a deliberate call, and it is the single biggest simplification in the
feature. `server/` exists to solve problems the relay does not have:

| `server/` machinery | Why relay mode drops it |
|---|---|
| `counter` / staleness checks | The relay never echoes a frame back to its sender, so there is no self-loop to break |
| 3-strike eviction in `broadcast` | One connection, not N. It is up or it is down |
| `/hi` peer gossip, the `peer` map | The relay *is* the membership list |
| UPNP, `myAddress` | Nothing listens locally |

Consequence: `server/server.go` and `server/server_test.go` were **not modified at
all**. No regression risk on the P2P path, and no churn in `broadcast`'s
`func(context.Context, *peer, uint64) error` signature that `server_test.go` pins.

The alternative — generalising `server/` behind a transport interface — would have
meant an interface with two implementations that share almost no semantics, plus
rewriting the tests that pin the existing one. More code, more risk, less clarity.

### Dumb fan-out

The relay forwards opaque bytes and never parses playback data. The **node** builds
the envelope:

```go
type message struct {
	Hostname string          `json:"hostname"`
	Event    json.RawMessage `json:"event"` // raw mpv JSON, untouched end to end
}
```

`json.RawMessage` preserves the repo's existing "raw mpv JSON bytes end to end"
invariant — the relay is as ignorant of mpv as `server/` is.

This struct is **declared twice**, once in `relay/main.go` and once in `ws.go`,
because two `package main`s cannot import each other. Four duplicated lines beat
introducing a third package that exists only to share them. Both copies carry a
comment saying so.

The relay does know one mpv detail: it synthesizes join/leave notices as
`show-text` events, reusing the exact shape `Server.notify` already uses. So
`relay/main.go` imports `mpv` — legal, since a `package main` may import other
packages; only the reverse is impossible.

### Rooms

No config file, no database, no registry. **The first joiner's secret defines the
room's secret**; later joiners must match it (`crypto/subtle.ConstantTimeCompare`).
The room is deleted when its last member leaves. Everything is in memory.

Join is `GET /ws?room=<code>&hostname=<name>` with the secret in a header — *not*
the query string, which reverse proxies write to access logs.

`-room` and `-secret` default to `crypto/rand.Text()` (~130 bits of base32), so the
lazy path is also the secure path.

### The joiner has to learn the room, not just the reverse

Initially the relay only broadcast "X has joined" to *existing* members. That means
the **last node to join learns about nobody** — it joined last, so no one joins
after it, so it receives no notices at all. Caught while reasoning about the `[R1]`
e2e case, before it ever ran.

Fixed by making `add()` return the current roster and sending the joiner one
`connected to X` notice per existing member — mirroring exactly what P2P's
`AddAddress` produces, so `e2e.py`'s `known()` helper works unchanged across both
transports. Pinned by `TestJoinerLearnsWhoIsAlreadyThere`.

---

## 3. The echo problem, and deleting the `slave` role

This started as a question from the user:

> a produced a message and went to b and c. then the action got applied on b and
> then b produced a message that went to a and c. is there a way to ignore this
> message on a because it's the same event

That is the a→b→a rebound, and it turned out to be the more interesting half of
the whole session.

### What was there before

A `role == slave` field. A node that applied a remote pause became a "slave" and
stopped broadcasting **all** of its own events until it un-paused. Nodes started
with `-addrs` began as slaves.

The user was explicit that this was never a topology:

> startAsSlave is a hack i implemented to partially avoid the echo problem, it
> doesn't mean that the watchparty runs as master/slave mode. in fact every peer
> can control the player.

### Why it had to go

**It overshot.** Muting the *peer* instead of the *echo* also swallowed that
peer's own deliberate actions. Concretely: b applies a's pause, becomes a slave,
and now b's user seeks — and nobody follows, because `errSlave` drops it. The old
`e2e.py [5b]` asserted that as correct behaviour. It directly contradicts "every
peer can control the player."

**It only half-worked.** `pause` was covered twice over (`errNoChange`, then
`errSlave`), but `time-pos` was not: `sync()` applied a remote seek and marked
nothing, and `handleEvent`'s `timePos` case did no value comparison at all — only
`!c.paused → errDoNotSend` and the role check. So a node that was paused and *not*
a slave rebroadcast the exact seek it had just applied. That is the reported bug.

**Relay mode has no counters to fall back on.** In P2P the counter check is a
second line of defence. The relay has none, so echo suppression is the *only* thing
between it and an infinite loop. A half-working guard was not good enough.

### The replacement: suppress the echo, not the peer

Two maps on `mpv.Client`:

```go
applied map[string]string // property -> value last applied from a remote event
seen    map[string]bool   // property -> initial property-change already swallowed
```

**`applied`** is the rebound guard. `pause()` and `sync()` record
`c.applied[name] = event.Data` **before** the IPC call; `handleEvent` checks it at
the top and returns `errEcho` on a match.

The ordering is load-bearing and is the same hazard `CLAUDE.md` already documented
for `role`: mpv echoes the change back as an event, and `watch()` has to *already*
see the marker when it arrives. Setting it after `do()` returns is a race that
reads perfectly fine.

Matching differs per property, and the `time-pos` case is the subtle one:

- **`pause`** — exact string compare, entry deleted on match. `pause()`
  early-returns on a no-op, so no stale marker can linger.
- **`time-pos`** — compared as floats with a **±0.5s tolerance**, and the marker is
  *kept*, refreshed to the echoed value.

  Exact compare is wrong here because **mpv snaps a seek to a keyframe**. Applying
  `77.000000` can echo back `77.041000`; an exact compare misses, the node
  rebroadcasts `77.041`, the next peer applies it and snaps again, and the position
  walks forward one hop per peer. Keeping the marker also absorbs a seek that emits
  more than one `property-change`. It is cleared when the node un-pauses, which is
  a clean boundary since `time-pos` is only ever forwarded while paused.

**`seen`** swallows the *first* `property-change` per property. This is the job
`startAsSlave` was actually doing, and nobody had written down why:
`observe_property_string` makes mpv fire one `property-change` per property the
instant it is registered. A fresh node has `c.paused == true`, so that initial
`time-pos` sails past the `!c.paused → errDoNotSend` guard and gets broadcast as
`time-pos=0` — **rewinding the entire party to the start**. `applied` cannot help;
nothing has been applied yet.

`seen` is strictly better than `startAsSlave` was: it covers the *first* node too,
whose initial `time-pos=0` broadcast was pointless noise. State is still updated
from that first event; only the broadcast is suppressed.

### Why not an origin ID in the envelope

The obvious alternative: stamp each message with the hostname that originally
generated it and drop anything you originated. Rejected because the node would have
to correlate "this echo came from that remote event" in order to know which origin
to re-stamp — which is the same correlation problem, plus a wire-format change,
plus a rule every transport has to honour. `applied` *is* that correlation, done
locally in a few lines, and it works identically for P2P and relay.

### What was deleted

`role`, the `slave` const, `errSlave`, and the `startAsSlave` parameter. So:

- `mpv.New(c, cancel, socket, outgoing)` lost its last argument
- `main.go` no longer computes `len(addresses) > 0`
- **the relay never needed to report a member count on join** — an entire piece of
  the originally planned protocol evaporated

The fix lives in `mpv/`, so **both transports got it**. The bug the user could not
solve in P2P is fixed in P2P.

---

## 4. Reconnect

A dropped relay must not end the movie. The node runs:

- a **supervisor goroutine** that owns the reconnect loop: dial → publish the conn
  → run the read pump until it errors → back off → redial, until ctx is done.
  Backoff doubles 1s → 30s and resets on every successful dial.
- **exactly one write pump**, long-lived, spanning reconnects. It must *not* be
  per-connection: two of them briefly overlapping during a redial would both
  `range outgoing` and steal frames from each other.
- `relayConn.mu` guards only the conn *pointer*, which the supervisor swaps. The
  write pump snapshots it and releases the lock **before** writing — holding it
  across a 10s write would wedge the supervisor's `set()`. Same shape as the
  existing "never hold `c.mu` across `conn.do()`" invariant.

A failed write logs and drops that frame rather than killing the pump; the
supervisor is already redialling and the party resyncs on the next event.

**403 and 400 are fatal** (`errRejected`). Everything else retries. A wrong secret
never becomes right, so retrying it forever is noise that also burns the relay's
rate limiter — the node logs the status and cancels instead.

Accepted consequence: a reconnect is a fresh join, so the room is told "X has
joined" again and the stale member lingers until the 30s ping reaps it. Harmless
under dumb fan-out, and documented rather than fixed.

---

## 5. Hardening, because this runs on a public VPS

The user was explicit:

> i'll deploy this in a public vps and a lot of spam bots may ddos us so we need a
> memory efficient way to rate limit to not overwhelm the server

The room secret is the only auth, so the caps below are load-bearing, not
decoration.

### The rate limiter is a count-min sketch, not a map

**This is the decision most likely to be "simplified" back into a bug.**

The obvious implementation is `map[string]int` keyed by IP, reset each window. It
is wrong for a public server: it allocates one entry per *distinct* source
address, so a botnet with 100k addresses mints 100k entries — roughly 10MB/hour,
growing with the attacker's IP pool. **The rate limiter becomes the
memory-exhaustion attack it exists to prevent.** Capping the map only picks between
failing open (no limit for new IPs) and failing closed (service dead).

So:

```go
const (
	rows = 4
	cols = 16384
)

type limiter struct {
	mu     sync.Mutex
	seeds  [rows]maphash.Seed
	counts [rows][cols]uint16
	window time.Time
}
```

- **128KB, allocated once at startup, forever.** No per-key entries, no TTL, no
  eviction pass, no sweeper goroutine, and **zero allocation per check** — so no GC
  pressure at exactly the moment the server is under load.
- The whole thing is `clear()`ed on window rollover rather than evicted per key.
  Cost: up to 2× the cap across a window boundary. Meaningless at this scale.
- A sketch **over-counts on collision but never under-counts**, so the error is
  one-sided in the safe direction: an attacker cannot collide their way into extra
  quota. The price is an occasional false rejection for a busy shared NAT.
- Seeds are `maphash.MakeSeed()` per process, so collisions cannot be targeted.
- `uint16` saturating at 65535 is plenty against a cap of 10.

`golang.org/x/time/rate` was not used: it is a new dependency, and its per-key form
has exactly the same unbounded-map problem.

Three tests pin the properties that matter — zero allocation after 200k distinct
IPs, never under-counting, and no false rejection at 10k IPs — because each is a
property you would silently lose by "cleaning it up".

### What is counted

Both → **429**, no upgrade:

- **Room creation only.** Joining an existing room is free, or a party of eight
  would exhaust one person's quota. `TestOnlyRoomCreationIsRateLimited` exists
  specifically to pin that distinction.
- **Failed secret attempts.** `rand.Text()` secrets are unguessable, but a
  hand-picked `-secret hunter2` is not, and this is the only lock on the door.

### The other caps

- **`-maxRooms` (10000)** → 503. The per-IP limiter does not bound the *global*
  total: 100k botnet addresses × 10 rooms/hour is a million rooms an hour, all
  within quota.
- **`-maxConns` (5000)** → 503. Bounds goroutines and buffers, and covers the bot
  that creates a room and just holds the socket open answering pongs, which the
  ping reaper will never catch.
- **`conn.SetReadLimit(4096)`.** mpv event lines are tiny; the 32KB default lets a
  member stream junk through the fan-out to everyone else.
- **`http.Server` timeouts.** Go's defaults are *no timeout*, which is free
  slowloris. `ReadHeaderTimeout: 5s`, `IdleTimeout: 60s`, `MaxHeaderBytes: 8192`.

  **`ReadTimeout` is deliberately absent** — it puts an absolute deadline on the
  hijacked connection and would kill every long-lived websocket.

**Rejection order is cost-ordered**: conn cap → rate limit → secret → room cap →
`Accept`. The cheapest, most brutal rejection first, so a flood is refused before
anything is allocated and nobody who fails gets upgraded.

### `-trustProxy` takes the *last* `X-Forwarded-For`, not the first

Behind the reverse proxy that terminates TLS, `r.RemoteAddr` is always the proxy,
so **every user lands in one bucket and the relay bricks itself at room 11**. Hence
the flag.

Last, not first: with a single proxy appending, the last entry is the proxy's own
view of the direct peer and a client cannot forge it. The first entry is entirely
attacker-supplied.

This matters more in the container than on bare metal — every containerized
connection arrives from the docker bridge IP (observed as `172.17.0.1` during
testing), so without the flag the whole world shares one bucket by default.

---

## 6. Container

`relay/Dockerfile`, two stages, built **from the repo root** — the relay is one
package inside the module and needs `go.mod` and `mpv/` beside it:

```sh
docker build -f relay/Dockerfile -t watchparty-relay .
```

Final stage is `scratch` with a static `CGO_ENABLED=0` binary: **6.28MB**, no
shell, no libc, no CA bundle. The relay only listens and never dials out, so certs
would be dead weight.

`COPY . .` makes `.dockerignore` load-bearing — `.bin` alone is ~10MB of an 11MB
context, shipped to the daemon on every build. With it, 267B.

Build flags, measured on this binary:

| build | size |
|---|---|
| plain | 9,084,233 |
| `-trimpath` | 9,069,135 |
| `-ldflags='-s -w'` | 6,291,618 |
| both | 6,279,330 |

- **`-ldflags='-s -w'`** drops the symbol table and DWARF debug info. That is the
  entire 31% size win.
- **`-trimpath`** is not a size flag (15KB). It strips absolute build paths — a
  plain binary embeds `/mnt/ssd/.../relay/main.go`, a trimmed one has
  `github.com/mhashemm/watchparty/relay/main.go`. Buys reproducible builds and
  stops the binary leaking your filesystem layout.
- **Panic traces survive `-s -w`.** Go resolves function names and line numbers
  from its own `pclntab`, not DWARF. Verified on a stripped binary. What you lose
  is debugger support, which is already moot on a `scratch` image.

**Open item: the container runs as root.** `USER 65534:65534` was dropped, seemingly
on the assumption that binding `:80` requires privilege. It does not under Docker
20.10+, which sets `net.ipv4.ip_unprivileged_port_start=0` inside containers —
verified directly (`--user 65534:65534 -addr :80` binds fine, `docker top` shows
uid 65534). Adding the line back is free on this platform. Under podman or a
hardened host where that sysctl differs, listen on `:8080` and publish
`-p 80:8080` instead.

---

## 7. Bugs found while building this

Kept because each one is a trap the next person can fall into.

1. **The 429 path consumed quota but never enforced it.** The bad-secret branch
   called `limiter.allow(...)` and discarded the result, always returning 403. Ten
   brute-force attempts were all answered. Caught by
   `TestSecretGrindingIsRateLimited`.
2. **The connection cap could overshoot.** `Load()`-then-check lets an entire
   flood pass before any of them increments. Fixed to `Add(1)`-then-check, with
   `Add(-1)` on rejection.
3. **`m.conn` was written after the member was already reachable by `broadcast`.**
   A data race and a nil dereference if the send buffer filled in that window.
   Fixed by splitting `admit()` (validate + create room, before the upgrade) from
   `add()` (register, only once the conn is set).
4. **A failed upgrade broadcast a spurious "X has left"** for a member that never
   joined. `leave(name, nil)` now reaps an empty room without announcing anything.
5. **The last joiner learned about nobody** (section 2).
6. **A wrong secret retried forever**, burning the relay's own rate limiter, until
   403/400 were made fatal.
7. **`e2e.py`: `osd()` returns a list, so `.count("paused by ")` is an exact
   element match, not a substring search — silently always 0.** Two echo
   assertions passed vacuously and then reported `0 pause notices` once the state
   was right. The existing suite had only ever used full strings
   (`f"synced by {A}"`), so the trap was invisible. Now there is a `pauses()`
   helper and a note in `CLAUDE.md`.

Note that (1), (5) and (7) were found by *tests and reasoning*, not by the feature
appearing to work — it appeared to work throughout.

---

## 8. What was actually verified

Not "it compiles":

- `go build ./... && go vet ./... && go test -race ./...` — clean.
- **The `mpv` fixes were verified by reintroducing each bug and confirming the new
  tests fail first**, per the repo's existing convention. Both did:
  `TestAppliedValueIsNotRebroadcast` / `TestAppliedPauseIsNotRebroadcast` fail with
  dedup disabled; `TestInitialPropertyChangeIsNotBroadcast` and the aliasing test
  fail with the `seen` guard disabled.
- **`./e2e.py` — both phases, 5 real nodes, real mpv processes: ALL PASS.**
  - p2p `[1]`–`[15]`, including the flipped `[5b]` and the new `[5c]`
  - relay `[R1]`–`[R17]`, including reconnect after `SIGKILL`ing the relay
    mid-party, room isolation, wrong secret, and all three caps
- **A real two-node party driven through mpv's IPC socket**, twice: once against a
  local relay, once against the container. Pause, resume, seek-while-paused from
  *either* node, clean leave, and OSD notices showing exactly one per action — the
  direct evidence that the echo is gone.
- Killed the relay mid-party and watched the backoff (`1s`, `2s`, `4s`) and
  recovery in the node logs, then confirmed a pause still propagated.

### e2e.py restructure

The suite was one flat `try:` block. It is now `p2p_suite()` / `relay_suite()` /
`caps_suite()` behind `--mode {p2p,relay,both}`. Every existing helper — `ipc`,
`get`, `setp`, `toggle`, `log`, `osd`, `known`, `child_mpv_socket`, `kill_mpv_for`,
`stop`, `resolve` — is reused unchanged; `resolve()` still works because relay mode
keeps printing `share is …` on line 1.

Two existing cases changed meaning because the behaviour did:

- **`[5b]` flipped.** Was "a slave's own seek is NOT broadcast" — that was the
  hack's false positive. Now asserts any paused peer's seek reaches the others.
- **`[7]` lost its premise** ("slave role clears after un-pausing") and is now a
  plain round trip driven by a node that had only ever followed.

---

## 9. Deliberately not done

- **Relay TLS** — terminate at caddy/nginx. Building it in means cert management in
  a tool that does not want it.
- **Real auth / accounts / persistence** — rooms are ephemeral by design. Room
  secret + rate limiter + caps is the entire access model.
- **L3/L4 flood protection** — a volumetric attack has to be absorbed upstream (VPS
  provider, Cloudflare, nftables). The relay can only bound what reaches its accept
  loop, which the caps do.
- **Relay in the release matrix** — `publisher.yml` builds `.`. The relay is server
  infrastructure, not an end-user download. One `strategy.matrix` entry if that
  changes.
- **Relay-side state for late joiners** — the relay stays stateless about playback.
  A late joiner is not force-synced; it follows the next event. Making the relay
  authoritative over pause/time-pos would be a new sync model, not a transport.

### Known trade-offs, accepted

- Relay identity is only as strong as the room secret: `hostname` is whatever the
  joiner claims, so anyone with the secret can impersonate a member. Same trade as
  the existing P2P `hit-me-up` header.
- A reconnect re-announces "X has joined" and leaves a stale member until the ping
  reaps it.
- The ±0.5s `time-pos` tolerance also swallows a deliberate sub-second seek.
  Marked with a `ponytail:` comment; tighten only if frame-stepping ever needs to
  sync.
- Count-min collisions can rate-limit a busy shared NAT early. Bump `cols` if it
  shows up in practice.

---

## 10. File map

| File | Role |
|---|---|
| `relay/main.go` | The relay server. Rooms, fan-out, limiter, caps. 352 lines |
| `relay/main_test.go` | Handler + limiter tests, `httptest` + raw `websocket.Dial`. 262 lines |
| `relay/Dockerfile` | Two-stage → `scratch`. Build from the repo root |
| `.dockerignore` | Load-bearing: `COPY . .` would otherwise ship ~10MB |
| `ws.go` | Node-side client. Supervisor, read pump, single write pump. 158 lines |
| `main.go` | `-relay/-room/-secret`; `runMesh()` extracted, returns a cleanup closure |
| `mpv/mpv.go` | `applied` + `seen`; `role`/`slave`/`errSlave`/`startAsSlave` deleted |
| `mpv/mpv_test.go` | Three new cases for echo suppression and the startup guard |
| `e2e.py` | Three suites behind `--mode`; `[R1]`–`[R17]`; `pauses()` helper |
| `CLAUDE.md` | Loop prevention rewritten, relay + hardening sections added |

`runMesh()` returns a cleanup closure rather than using `defer` internally,
because those defers — the UPNP mapping delete and the `/bye` broadcast — have to
fire in `main`, not when the function returns. Getting that wrong would silently
leak a port mapping on the user's router.

`server/` was not touched.
