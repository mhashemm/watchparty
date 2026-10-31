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
| `file` | the sender's file fingerprint (see the root `CLAUDE.md`); only `/hi` checks it |
| `secret` | the party's secret. `Guard` checks it on every endpoint |

**`Guard` wraps the whole mux.** A missing or wrong `secret` (constant-time compare) is
403 and logs `refused <addr>: wrong secret`, before any handler runs. It also caps every
body at `maxBody` (64KB; an mpv event is under 200 bytes), and `/event` answers 413 past
it. Without the secret, `hit-me-up` is just a claim: anyone reaching the port could send
`/event` or `/bye` as a known peer with a high counter. Members, who hold the secret,
are trusted as much as relay members are. The peer map has no size cap, since only a
member can grow it.

| Endpoint | Body | Handler |
|---|---|---|
| `/hi` | none → 409 if `file` isn't this node's fingerprint (logs `refused X with ip …: not the same file`), otherwise responds with a JSON map `{address: {"counter": "N", "hostname": "…"}}` of the peers this node already knows, **not including the caller**, plus its own `counter`/`hostname` in response headers | registers or replaces the caller, logs `connected to <host> with ip <addr>`, and shows "X has joined" |
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
4. Each one is stored under `s.mu` (`add`), and gets "connected to X", **as soon as
   its own `/hi` succeeds**, not after the whole loop. A peer that has answered our
   `/hi` already has us in its table and can send an event right away (a `joined`,
   say), while we are still greeting the others. When peers were stored only at the
   end, that event was refused with 400 "does not exists", dropped, and counted as a
   strike. `e2e.py` `[14]` caught it in the startup log, and
   `TestPeerIsKnownAsSoonAsItAnswersHi` covers it.

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

## Fan-out: `broadcast(parent, detach, f)`

1. Under `s.mu`, bump the counter and **snapshot** the peer list, then release the
   lock.
2. Fan out `f(ctx, peer, counter)` to every peer in parallel, each under its own 10s
   timeout derived from `parent`, and `wg.Wait()`. With `detach` (`BroadcastEvents`),
   a peer already at `maxFails` is sent to in a goroutine nobody waits for. Its
   context comes from `s.c`, so it ends with the party. `Shutdown` doesn't detach, so
   every `/bye` is waited for.
3. `record`, per peer, under `p.mu`:
   - A success resets `fails` and clears `failingSince`.
   - A failure increments `fails`, and the first one in a row sets `failingSince`.
   - The peer is evicted at `maxFails` (3) **and** once it has been failing for
     `evictAfter`, **only if `s.addresses[p.address]` is still that same `*peer`**. A
     peer that reconnected through `/hi` meanwhile has a new entry and must not be
     dropped. Eviction shows "dropped X", once.

Why both conditions: three strikes alone dropped people on flaky Wi-Fi, because a
burst of pauses fails three sends within seconds. `evictAfter` is
`party.DefaultEvictAfter` (5 minutes, the `-evictAfter` flag), and `e2e.py` passes
`1ms` so its strike cases don't wait. Why `detach`: a dead peer behind a 10s timeout
held up this node's *next* event by 10s. That used to stop after 3 events, and with
the 5-minute rule it would last 5 minutes. A detached send still records its strike
or its success, and the counters drop anything it delivers late. Eviction is checked
only when a send fails, so a dead peer goes on the first event this node sends
after the 5 minutes.

Invariants, each of which has caused a real bug:

- **Never hold `s.mu` across the fan-out.** Holding it until `wg.Wait()` returns lets
  one blocking send wedge every handler behind the next writer.
- **Synthetic notices go through `notify`, which is `select`/`default`.** Notices are
  cosmetic, and nothing drains `incoming` until `ProcessIncomingEvents` starts once
  mpv's socket answers (`AddAddress` runs before mpv has even started), so dropping
  them is correct. Real events in `Event` keep the blocking send, bounded by
  `req.Context()`. From then on `incoming` is drained at once, even while mpv is
  still loading the file (see `mpv/CLAUDE.md`).
- **`Shutdown` must use `context.WithoutCancel(s.c)`.** `s.c` is the party's context,
  which is already cancelled when the deferred `Shutdown` runs. A timeout derived from
  it makes every `/bye` fail instantly, and peers only lose the leaver through the
  3-strike eviction.
- **`BroadcastEvents` selects on `s.c`**, so it returns with the party. `outgoing`
  is never closed (see `party/CLAUDE.md`).
- **Only the node that generated an event broadcasts it.** Applying a remote change
  never re-emits it, so a node discovers a dead peer only when it drives something
  itself. Eviction is therefore per-node and not simultaneous across the mesh.

`request` turns a 409 into `ErrDifferentFile` and a 403 into `ErrWrongSecret`, which
`party.runMesh` treats as fatal for the joiner. Only the bootstrap's answer counts: every peer the mesh gossips
already passed the same check against the same fingerprint.

`request` closes the body of every non-2xx response before returning its error, and
`post` (used by `/event` and `/bye`) closes it on success too; only `hi` reads one. An
open body keeps its connection out of the pool, one per event.

## Construction

`New(ctx, incoming, myAddress, hostname, file, secret, evictAfter)`:

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
- `TestBroadcastEvictsPeerAfterMaxFails`: with a negligible `evictAfter`, exactly
  `maxFails` consecutive failures evict, and any success in between resets the count.
- `TestFailingPeerIsKeptUntilEvictAfter`: with an hour, `maxFails + 2` failures keep
  the peer; moving `failingSince` back two hours, the next failure evicts.
- `TestSuspectPeerDoesNotHoldUpBroadcast`: with `detach`, a peer at `maxFails` whose
  send blocks doesn't hold `broadcast` up, and a healthy peer is still waited for.
- `TestBroadcastEventsStopsWithTheParty`: cancelling the context ends
  `BroadcastEvents`.
- `TestResponseBodiesAreClosed`: a stub `RoundTripper` records `Close` on 204, 500,
  409 and 403 answers to `post`.
- `TestGuardNeedsTheSecretAndCapsTheBody`: through `Guard`, no or a wrong secret is 403
  on all three endpoints and reaches no handler, an oversized event is 413, and the
  right secret gets through. `TestWrongSecretIsItsOwnError`: a 403 is `ErrWrongSecret`.
- `TestHiRefusesADifferentFile`: `Hi` called directly with an `httptest` recorder; a
  different or missing `file` gets 409 and never becomes a peer.

Mesh formation, propagation, `/bye`, eviction and the header checks are covered for
real by `e2e.py --mode p2p`.

