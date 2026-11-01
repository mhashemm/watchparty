# party/CLAUDE.md

One watch-party node, start to finish: `Run(ctx, Config)` starts mpv, joins the other
peers, blocks until the party ends and cleans up. Its two callers, `main.go` (flags) and
`gui/main.go` (form, once per party, in-process), are complete programs. Nothing here
parses flags, touches globals or exits. `Run` and `runMesh` need mpv and a network, so
they are tested by `e2e.py` and the GUI recipes, not unit tests.

## Same file, or no party (`fingerprint`)

SHA-256 of the byte size plus eight 1MB samples spread from the first byte to the last:
a renamed or moved copy matches, a 20GB file is instant, a different encode, cut or
release doesn't. An edit that keeps the size and misses every sample slips through,
accepted. A `-file` containing `://` is a URL, fingerprinted by its text, so nothing is
fetched. A joiner that differs gets 409 from the peer it bootstraps off (`/hi`) or from
the relay (against the room creator's) and its party ends. Older versions send no
fingerprint and can't join.

## `Run`'s traps

- **`parent` belongs to the caller** (CLI signal context, GUI per-party context). Run
  derives its own `c`, and hands everything `c` and `cancel`, never `parent`.
- The mpv socket name carries `rand.Text()`, so parties started in the same second
  (e2e nodes, two GUIs) never share one.
- **An empty `Relay` means P2P, which means UPNP.** That is why the GUI refuses an
  empty relay in relay mode instead of passing it through. A missing `Secret` (and in
  relay mode a missing room or hostname) is generated.
- mpv's command line: the user's `MpvFlags` first, then the pins, so they beat both
  the flags and `mpv.conf` (`mpv/MPV.md` says why each is pinned).
- **`defer cmd.Cancel()` only after `Start` succeeds:** the default `Cancel` is
  `Process.Kill()`, which nil-panics on a process that never started.
- **mpv's stdout and stderr go to one `lineLog`**, logged as `mpv: …`. It is the only
  place mpv says why it failed ("Failed to recognize file format."). One writer for
  both means exec copies in one goroutine (no lock), and `Wait` finishes the copy before
  `exited` fires. **The reason is kept out of the returned error:** the GUI reads "no
  such file" next to "mpv" as "mpv isn't installed", and mpv's "Cannot open file …: No
  such file or directory" would match it.
- `mpv.Dial` is retried every 50ms until the socket answers or mpv exits.

## Return values: kept apart on purpose

| Outcome | Returns |
|---|---|
| The caller cancelled `parent` (Ctrl-C, GUI Stop or exit) | `nil` |
| **The user quit mpv** | `nil` |
| Something inside cancelled `c` (relay rejection, failed bind) | `errStopped` |
| Setup failed (unreadable file, no mpv, UPNP, no LAN IP) | that error |
| P2P bootstrap plays another file / refuses the secret | `server.ErrDifferentFile` / `ErrWrongSecret` |
| mpv exited before the movie was ready | `mpv exited before the movie was ready: exit status N`; the reason is in the `mpv:` lines just before |
| mpv's own `Wait` error, nothing cancelled | that error |

**The mpv-quit case was a real bug.** Quitting closes mpv's socket first, `watch()`
sees EOF and cancels, and `exec.CommandContext` then kills the already-exiting mpv, so
`Wait` says `signal: killed`: a normal quit looked like a failure (CLI exit 1, a false
GUI "Diagnosis"). The cancel passed to `mpv.New` records `mpvQuit` when `c.Err() ==
nil`, i.e. mpv went away before anything else cancelled. In every other case `c` is
cancelled first and only then does the EOF arrive. An mpv crash also reads as a quit,
accepted: Windows exit codes can't tell them apart anyway.

e2e `[R15b]` quits mpv and requires exit 0 and "has left"; `[R14]` requires exit 0 on
SIGTERM. Nothing asserts a rejected node's exit code: check by hand that a wrong
`-secret` still exits 1.

## `runMesh` (P2P)

- `-local` uses the LAN IP. Otherwise UPNP maps `PublicPort` → `Port` with a 24h lease,
  and retries with 0 (permanent) when the router answers `ErrOnlyPermanentLease`.
- **It returns its cleanup instead of deferring it**, so `/bye` → `http.Server.Shutdown`
  → `DeletePortMapping` run when `Run` ends, not when `runMesh` returns. On an error it
  runs the cleanup collected so far, so a half-done setup never leaks a router mapping.
- Any `ListenAndServe` error cancels `c`: harmless at shutdown, ends the party on a
  failed bind.
- A failed `-addrs` peer is logged, not fatal, except `ErrDifferentFile` and
  `ErrWrongSecret`.
- An `EvictAfter` of zero (the GUI) means `DefaultEvictAfter`.

## `ws.go`: the relay client

- `dial` puts room, hostname, session and file in the query, and the secret and create
  key in **headers** (`room-secret`, `create-key`), since proxies log query strings. A
  trailing slash on the address is trimmed, so a pasted `wss://host/` still works.
- How the node reacts to each relay status:

  | Status | Relay meaning | Node |
  |---|---|---|
  | 400 | missing room or hostname | gives up (`errRejected`) |
  | 401 | room not open, no create key sent | retries: "room X is not open yet, waiting for someone with -createKey" |
  | 403 | wrong secret or create key | gives up, logs `rejected by relay`. A wrong secret never becomes right, and retrying burns the relay's rate limit |
  | 409 | not the room creator's file | gives up, `errRejected` wrapping `server.ErrDifferentFile` |
  | 429, 503, other | rate limit, caps | retries |

- **`Connect` is the supervisor**: dial, `set(conn)`, block in `read`, redial on error.
  Backoff doubles 1s → 30s and resets after a good dial; the 1s is `r.retry` so tests
  use 1ms.
- **`Broadcast` is the only writer for the whole party, across reconnects.** One per
  connection would leave two goroutines reading `outgoing` during a redial, stealing
  frames. It copies the conn pointer under `r.mu` and **writes without the lock**, or
  `set()` would wedge behind a stalled 10s write. With no conn it drops the event; the
  next pause resyncs.
- **`session`** is one `rand.Text()` per `Run`: the relay swaps a reconnecting member in
  silently instead of announcing a second join. A new GUI party is a new member.

## Goroutine lifetime

Every goroutine a party starts ends with `c`. `outgoing` and `incoming` are **never
closed**: their writers (`watch`, `read`, server handlers, `notify`) can outlive the
readers, and a send on a closed channel panics. A GUI that ran three parties is back to
its baseline goroutine count.
