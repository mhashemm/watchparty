# server/CLAUDE.md

The P2P transport: an HTTP mesh where every node is server and client to every other,
with no host. Used only when `Config.Relay` is empty. The relay reuses nothing from
here on purpose: counters, strikes and gossip solve problems a relay doesn't have, and a
shared transport interface would join two implementations with almost no common
semantics.

## Wire protocol

Every endpoint is `POST`; identity travels in headers: `hit-me-up` (the sender's
advertised `ip:port`, its key in every peer map), `counter` (monotonic `uint64`),
`hostname`, `file` (fingerprint, checked only by `/hi`), `secret`.

- **`Guard` wraps the whole mux.** A wrong or missing secret (constant-time compare) is
  403 and logs `refused <addr>: wrong secret` before any handler runs. Without it,
  `hit-me-up` is just a claim, and anyone reaching the port could send `/event` or
  `/bye` as a known peer with a high counter. Bodies are capped at 64KB (an mpv event is
  under 200 bytes). Members hold the secret and are trusted, so the peer map has no cap.
- `/hi`: 409 for another file, else registers or **replaces** the caller (so a
  restarted node with its counter back at 0 can rejoin) and answers with the peers it
  knows, not including the caller, plus its own counter and hostname in headers.
- `/event`: counter check, then a **blocking** push onto `incoming` bounded by the
  request context (503 when that expires).
- `/bye`: counter check, then removes the peer.

**Peers are keyed by the address they advertise**, not the string passed to `-addrs`.
Under `-local` that is the LAN IP, so `-addrs 127.0.0.1:6969` completes the handshake
and then every event is rejected with `does not exists`. Use the printed address.

## Joining (`AddAddress`)

- `addr` may be `ip:port|hostname`, overriding the hostname the peer reports. Not
  documented anywhere else.
- `/hi` goes to the bootstrap peer, then to every peer in its answer not yet known:
  that gossip closes the mesh, so nobody needs the full list. Gossiped peers' counter
  and hostname come from the bootstrap's map.
- **Each peer is stored as soon as its own `/hi` succeeds**, not after the loop. It
  already has us and may send an event (a `joined`) while we greet the others; stored at
  the end, that event was refused with `does not exists` and counted as a strike.
  `TestPeerIsKnownAsSoonAsItAnswersHi`, e2e `[14]`.
- Only the bootstrap's 409/403 counts (`ErrDifferentFile`, `ErrWrongSecret`): every
  gossiped peer already passed the same check.

## Counters

`broadcast` sends one incremented counter to every peer. `/event` and `/bye` drop
anything with `counter <= peer.Counter` (logs `skipped event from X`, answers 204), which
stops replays and out-of-order deliveries from the parallel fan-out. `Shutdown` sends
`/bye` with `math.MaxUint64`, so a leave beats any event still in flight.

## Fan-out and eviction (`broadcast`)

- Snapshot the peers under `s.mu`, **release it**, then send to all in parallel, each
  with a 10s timeout, and wait. Holding `s.mu` across the wait let one blocked send
  wedge every handler.
- A peer is evicted ("dropped X", once) after **3 failures in a row and** failing for
  `evictAfter` (5 minutes; e2e passes 1ms). Three strikes alone dropped people on flaky
  Wi-Fi: a burst of pauses fails three sends in seconds. Eviction checks that
  `s.addresses[p.address]` is still the same `*peer`, so one that reconnected through
  `/hi` meanwhile isn't dropped.
- **`detach`** (`BroadcastEvents` only): a peer already at 3 failures is sent to in a
  goroutine nobody waits for, bounded by the party context. Otherwise a dead peer's 10s
  timeout delayed this node's next event for the whole 5 minutes. A detached send still
  records its result, and counters drop anything it delivers late. `Shutdown` doesn't
  detach: every `/bye` is waited for.
- Only the node that generated an event broadcasts it, so a node notices a dead peer
  only when it drives something itself: eviction is per node, not simultaneous.

## Traps

- **Notices go through `notify`, a non-blocking send.** `AddAddress` runs before mpv has
  started, and nothing drains `incoming` until mpv's socket answers. Notices are
  cosmetic, so dropping them is right; real events keep the blocking send.
- **`Shutdown` uses `context.WithoutCancel(s.c)`.** `s.c` is already cancelled when the
  deferred `Shutdown` runs, so a derived timeout failed every `/bye` instantly and peers
  only lost the leaver through eviction.
- **Close every response body**, error or not (`request`, `post`). An open body keeps
  its connection out of the pool, one per event.
- The `http.Client` has no timeout; every call has a context: 30s for `/hi`, 10s per
  broadcast.

## Tests

`broadcast` is driven directly with a stub send function: no HTTP server, no fixtures.
Handlers are called through `httptest` recorders. Mesh formation, propagation, `/bye`,
eviction and the header checks run for real in `e2e.py --mode p2p`.
