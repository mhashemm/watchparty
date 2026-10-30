# server/CLAUDE.md

`server` is the P2P transport: a full HTTP mesh in which every node is both a server
and a client to every other node, with no host. It is used only when `party.Config.Relay`
is empty. Relay mode reuses nothing from here, on purpose (see `relay/CLAUDE.md`).

## Wire protocol

Every endpoint is `POST`, and identity travels in headers:

| Header | Meaning |
|---|---|
| `hit-me-up` | the sender's advertised `ip:port`, which is its key in everyone's peer map |
| `counter` | the sender's monotonic broadcast counter (decimal `uint64`) |
| `hostname` | the display name, shown in "paused by X" |

| Endpoint | Body | Handler |
|---|---|---|
| `/hi` | none → responds with a JSON map `{address: {"counter": "N", "hostname": "…"}}` of the peers this node already knows, **not including the caller**, plus its own `counter`/`hostname` in response headers | registers or replaces the caller, logs `connected to <host> with ip <addr>`, and shows "X has joined" |
| `/event` | raw mpv JSON | counter check, then a **blocking** push onto `incoming`, bounded by `req.Context()` (503 if that expires) |
| `/bye` | none | counter check, removes the peer, and shows "X has left" |

Peers are keyed by the address the remote **advertises** in `hit-me-up`, not the
string passed to `-addrs`. Under `-local` that is the LAN IP, so `-addrs
127.0.0.1:6969` completes the handshake and then has every event rejected with `<addr>:
does not exists`. Always use the address the node prints.

**The notice texts are parsed elsewhere.** The GUI's party history reads "X has
joined" and "connected to X" to record who was at a party. The relay sends the same
two, so keep the wording identical in both.

## Joining: `AddAddress`

1. `addr` may be written `ip:port|hostname`, which overrides the hostname the peer
   reports. This syntax is not documented anywhere else.
2. `/hi` goes to the bootstrap peer. Its answer is its own counter and hostname plus
   its peer map.
3. Every peer in that map not already known (excluding itself and the bootstrap) also
   gets a `/hi`. That is the gossip that closes the mesh: no node needs the full list.
   For these gossiped peers, the stored `Counter`/`Hostname` come from the
   **bootstrap's map**, not from the peer's own `/hi` response.
4. All of them are stored under `s.mu`, and each gets "connected to X".

`Hi` **replaces** an existing entry unconditionally and stores whatever `counter` the
caller sent. That is what lets a restarted node, whose counter is back at 0, rejoin.
The counter check only guards `/event` and `/bye`.

## Ordering and staleness: counters

- `broadcast` increments `s.counter` once per call and sends that value to every peer.
- `Event` drops anything with `counter <= peer.Counter`, logging `skipped event from
  X` and answering 204, and otherwise stores the new counter. That stops replays and
  out-of-order deliveries from the 10s fan-out.
- `Shutdown` sends `/bye` with **`math.MaxUint64`**, so a leave always beats any event
  still in flight.
- `Bye` does the same staleness check, then deletes the peer.
- The relay has no counters and doesn't need them: it never echoes a frame back to
  its sender.

## Fan-out: `broadcast(parent, f)`

1. Under `s.mu`, bump the counter and **snapshot** the peer list, then release the
   lock.
2. Fan out `f(ctx, peer, counter)` to every peer in parallel, under a 10s timeout
   derived from `parent`, and `wg.Wait()`.
3. Per peer, under `p.mu`: a success resets `fails`; a failure increments it. At
   `maxFails` (3) the peer is evicted, but **only if `s.addresses[p.address]` is still
   that same `*peer`**. A peer that reconnected through `/hi` meanwhile has a new entry
   and must not be dropped. Eviction shows "dropped X".

Invariants, each of which has caused a real bug:

- **Never hold `s.mu` across the fan-out.** An earlier version held `RLock` until
  `wg.Wait()` returned, so one blocking send wedged every handler behind the next
  writer.
  - Note: the comment on `TestBroadcastDoesNotBlockOnFullIncoming` still describes
    that old RLock behaviour. The test is still valid: it proves `broadcast` returns
    while `incoming` is full.
- **Synthetic notices go through `notify`, which is `select`/`default`.** Notices are
  cosmetic, and nothing drains `incoming` until `ProcessIncomingEvents` starts after
  the cooldown, so dropping them is correct. Real events in `Event` keep the blocking
  send.
- **`Shutdown` must use `context.WithoutCancel(s.c)`.** `s.c` is the party's context,
  which is already cancelled when the deferred `Shutdown` runs. A timeout derived from
  it makes every `/bye` fail instantly, and peers only lose the leaver through the
  3-strike eviction.
- **Only the node that generated an event broadcasts it.** Applying a remote change
  never re-emits it, so a node discovers a dead peer only when it drives something
  itself. Eviction is therefore per-node and not simultaneous across the mesh.

## Construction

`New(ctx, incoming, myAddress, hostname)`:

- An empty hostname falls back to `os.Hostname()`.
- The `http.Client` has no timeout of its own. Every call is bounded by a context:
  30s for `/hi`, 10s for a broadcast.
- `ctx` is the party's `c`, also used as the HTTP server's `BaseContext` in
  `party.runMesh`.

## Tests (`server_test.go`)

The tests drive `broadcast` directly with a stub `f`. There is no HTTP server and no
fixtures.

- `TestBroadcastDoesNotBlockOnFullIncoming`: an unbuffered, undrained `incoming` plus
  a failing peer must not block `broadcast` (`notify` must not block).
- `TestBroadcastEvictsPeerAfterMaxFails`: exactly `maxFails` consecutive failures
  evict, and any success in between resets the count.

Mesh formation, propagation, `/bye`, eviction and the header checks are covered for
real by `e2e.py --mode p2p`.

## Known issues

- **Spoofable identity.** `/event` and `/bye` trust `hit-me-up` and `counter`. The
  counter stops replays and unknown addresses, but a peer can still claim another
  peer's address. Fixing that needs signing, which is more than a LAN tool wants
  (`ponytail:` in `Bye`).
- **Response bodies.** `request()` never closes the response body on a non-2xx status,
  and `broadcast`'s callers discard the `*http.Response` without closing it. A 2xx
  `/event` or `/bye` answers 204 with an empty body, so in practice this only leaks
  on error responses.
- **Events during the cooldown.** Real playback events block on `incoming` until
  `ProcessIncomingEvents` starts. The 1024 buffer carries that gap, and `Event` gives
  up on `req.Context()` rather than wedging.
- **Fixed strike count.** `maxFails` is a constant (`ponytail:`). Make it a flag if
  flaky Wi-Fi starts evicting people mid-movie.
