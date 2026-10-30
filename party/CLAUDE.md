# party/CLAUDE.md

`party` is one watch-party node, from start to finish. `Run(ctx, Config)` starts mpv,
connects it to the other peers, blocks until the party ends, and cleans up. It has
exactly two callers, and each is a complete program on its own:

- `main.go`, the CLI: flags → `Config` → `Run`. The flag names and defaults live there.
- `gui/main.go`, the GUI: form → `Config` → `Run`, in-process, once per party.

Nothing here parses flags, touches globals, or exits the process. `Run` is called many
times in one GUI process, so it must not leave state behind.

## Files

| File | Contents |
|---|---|
| `party.go` | `Config`, `Run`, `runMesh` (P2P setup), `DefaultPort`, `errStopped`, `notReady` |
| `ws.go` | `relayConn`: the relay transport (dial, reconnect loop, write pump) |
| `ws_test.go` | `relayConn` against a stub `httptest` websocket handler |

`ws_test.go` covers the relay client's decisions: 403/400 end the party, 401 keeps
retrying, a dropped connection redials with the same session and both connections'
frames reach `incoming`, the secret rides in a header, `Broadcast` sends the envelope
and stops with the party. `Run` and `runMesh` need mpv and a network, so they are
covered end to end: `e2e.py` drives the CLI through them, and the GUI's checks drive
them in-process (see `gui/CLAUDE.md`).

## `Run`, step by step

1. `c, cancel := context.WithCancel(parent)`. **`parent` belongs to the caller**: the
   CLI's `signal.NotifyContext`, or the GUI's per-party context. **`c` is Run's own.**
   Everything inside is handed `c` and `cancel`, never `parent`.
2. The mpv socket name is `mpv.SocketPrefix + "mpv" + rand.Text()`, unique per `Run`,
   so parties started in the same second (e2e nodes, two GUIs) never share one.
3. Two channels are created, each with a 1024 buffer:
   - `outgoing`: mpv → the transport
   - `incoming`: the transport → mpv
4. **Choosing a transport:** `strings.TrimSpace(cfg.Relay) != ""` selects the relay,
   and anything else is the P2P mesh. An empty `Relay` therefore silently means "try
   UPNP". That is why the GUI refuses an empty relay address in relay mode instead of
   passing it through.
   - **Relay:** fill in a missing hostname, room or secret (`rand.Text()`), build a
     `relayConn` with a fresh `session`, then start `Connect` and `Broadcast`.
     `defer conn.Close()`.
   - **Mesh:** `runMesh` returns a cleanup closure, and `Run` defers it. On error,
     `runMesh` has already undone everything it did.
5. Both transports log `your address to share is …`. **This exact string is
   load-bearing:** the GUI's invite box, the GUI's history (`gui/store.go`, which takes
   the real room and secret from it) and `e2e.py` all parse it.
6. Start mpv: `--save-position-on-quit --pause --input-ipc-server=<sock> <file>`,
   with the user's `MpvFlags` first. `mpv.Init(cmd)` sets the Windows process
   attributes. **`defer cmd.Cancel()` only after `Start` succeeds.** The default
   `Cancel` is `Process.Kill()`, which nil-panics on a process that never started.
7. `cmd.Wait()` runs in a goroutine that reports on `exited`. `mpv.Dial` is retried
   every 50ms until the socket answers (~40ms after start). If mpv exits first, `Run`
   returns `notReady`: mpv's real exit status ("exit status 2" for a bad file, 1 for
   a bad flag), or `nil` for exit 0 (the user closed it). If `c` ends, `stopReason`.
8. `mpv.New(c, onWatchEnd, conn, outgoing)` registers the two observers, starts the
   reader, and **waits until the file is loaded** (the first `time-pos` with data).
   If mpv hangs up before that, `New` returns `mpv.ErrClosed` at once and `Run`
   returns `notReady(<-exited)`. The reader doesn't cancel the party before the
   movie is ready: cancelling would kill the exiting mpv, and its real exit status
   would read as `signal: killed`. Then `ProcessIncomingEvents(incoming)` starts in
   a goroutine (events that arrived meanwhile wait in the 1024 buffer), and `init
   done. now you can start watching` is logged. That line is
   also load-bearing for the GUI.
9. Wait on `exited`, then pick a return value (see the next section).

Deferred cleanup runs in LIFO order:

1. delete the socket file
2. `cmd.Cancel` (mpv is usually already dead)
3. the transport: `conn.Close()` for the relay, or for the mesh `/bye` →
   `http.Server.Shutdown` (5s) → delete the UPNP mapping
4. `cancel()`

## Return values: three outcomes, kept apart on purpose

| Outcome | Returns |
|---|---|
| The caller cancelled `parent` (Ctrl-C, GUI Stop, GUI exit) | `nil` |
| **The user quit mpv** | `nil` |
| Something inside cancelled `c` (relay rejection, mesh listener failed to bind) | `errStopped` ("the party stopped unexpectedly") |
| Setup failed (mpv binary missing, UPNP, no LAN IP) | that error |
| mpv exited before the movie was ready (bad file, bad flag) | `mpv exited before the movie was ready: exit status N` |
| mpv's own `Wait` error with nothing cancelled | that error |

**The mpv-quit case is subtle, and it was a real bug.** When mpv quits, it closes its
IPC socket first. `watch()` sees EOF and calls the cancel it was given.
`exec.CommandContext` then **kills the mpv that is already exiting**, so `Wait`
reports `signal: killed`. Read naively, a normal quit looks exactly like an internal
failure: the CLI exited 1 and the GUI showed a false "Diagnosis".

The fix is the closure passed to `mpv.New`:

```go
mpvQuit := atomic.Bool{}
mpv.New(c, func() { if c.Err() == nil { mpvQuit.Store(true) }; cancel() }, ...)
```

It records "mpv went away before anything else cancelled the party". In the other
cases (relay rejection, a failed bind), `c` is cancelled *first*, `CommandContext`
then kills mpv, and only then does the socket EOF arrive. By that time `c.Err() !=
nil`, so `mpvQuit` stays false.

A crash of mpv itself (a segfault) also reads as `mpvQuit`, so it returns `nil`. That
is accepted: on Windows a killed process exits with code 1 rather than as a signal,
so exit codes can't separate these cases reliably anyway.

e2e `[R15b]` covers this: it sends `quit` to one node's mpv and requires that node to
exit 0 and the room to hear "has left". SIGTERM must exit 0 (`[R14]`). Nothing in
e2e asserts the *exit code* of a rejected node (`[R11]` checks only the 403 and that it
gives up), so check by hand that a wrong `-secret` still exits 1.

## `runMesh`

1. Pick the address.
   - `Local`: `upnp.GetLocalIPAddr()` + `Port`.
   - Otherwise UPNP: `AddPortMapping` for `PublicPort` → `Port` with a 24h lease. If
     the router answers `ErrOnlyPermanentLease`, it retries with lease 0 (permanent).
     Then `GetExternalIPAddress`.
   - Every failure is returned. A failure after the mapping was added runs the cleanup
     collected so far, so a half-done setup never leaks a mapping on the router.
2. `server.New(c, incoming, address, hostname)`, with `/hi`, `/bye` and `/event` on a
   plain `http.Server` at `0.0.0.0:Port`. `BaseContext` is `c`, so the handlers'
   request contexts die with the party.
3. `ListenAndServe` runs in a goroutine. **Any** error, including `ErrServerClosed` at
   shutdown, calls `cancel()`. At shutdown that is a harmless no-op. On a bind failure
   it ends the party, and `Run` returns `errStopped`.
4. `ser.BroadcastEvents(outgoing)` runs in a goroutine until `c` ends.
5. `AddAddress` is called for each comma-separated `Addrs` entry. A failure is logged,
   not fatal, so one dead bootstrap peer doesn't stop the node.
6. The returned closure runs the cleanup LIFO: `ser.Shutdown` (`/bye` to everyone) →
   `http.Server.Shutdown` → `DeletePortMapping`.

## `ws.go`: the relay client

- **`dial`** connects to `addr + "/ws?room=…&hostname=…&session=…"`, with the secret
  and create key in the `room-secret` / `create-key` **headers**, never in the query
  string (proxies log query strings). A trailing slash on `addr` is trimmed, so an
  invite pasted as `wss://host/` still dials `/ws`.
  - 403 and 400 → `errRejected`: `Connect` logs `rejected by relay` and calls
    `cancel()`. A wrong secret never becomes right, and retrying burns the relay's
    rate limiter.
  - 401 → "room X is not open yet, waiting for someone with -createKey", and it
    retries. This is a guest who arrived before the key holder.
  - Everything else, 429 and 503 included, is retried with backoff.
- **`Connect`** is the supervisor for the whole party.
  - Backoff goes 1s → 30s, doubling, and resets after a successful dial.
  - On each connection it logs `relay: connected to <addr> room <room>`, `set(conn)`s
    it, and blocks in `read`.
  - When `read` fails it logs `relay: disconnected: …` and redials.
  - A failed dial logs `relay: dial: …, retrying in …`.
  - The GUI and `e2e.py` grep all three of these strings.
- **`read`** unmarshals each frame into the `message` envelope and pushes
  `mpv.IncomingMessage{HostName, Event}` onto `incoming`. The send blocks, but it is
  bounded by `c`.
- **`Broadcast`** is **the only writer for the whole party, across every
  reconnect**, and returns when `c` ends. Starting one per connection would leave two
  goroutines reading `outgoing` during a redial, stealing frames from each other. It
  wraps each event as `{"hostname", "event": <raw mpv JSON>}`.
  - It takes `r.mu` only long enough to copy the conn pointer, **never across the 10s
    `Write`**. Otherwise `Connect`'s `set()` would wedge behind a stalled write.
  - With no connection it logs `relay: not connected, dropping event` and moves on.
    The party resyncs on the next pause.
- **`session`** is one `rand.Text()` per `Run`. The relay uses it to swap a
  reconnecting member in silently instead of announcing a second join. In the GUI,
  each party gets a fresh session, which is correct: a new party is a new member.
- **`message`** is declared again in `relay/main.go`. The relay is `package main` and
  can't be imported, and importing `party` the other way would pull upnp, server and
  mpv into the relay image. Keep the two in sync by hand.

## Lifetime of the goroutines

Every goroutine a party starts ends with `c`: `Connect`, `Broadcast`, `read`,
`BroadcastEvents`, `ProcessIncomingEvents`, and `watch` (its `outgoing` send selects
on `c`, and mpv's death ends its read). `outgoing` and `incoming` are **never
closed**: their writers (`watch`, `read`, the server's handlers, `notify`) can still
be running when the readers stop, and a send on a closed channel panics. A GUI that
ran three parties is back to its baseline goroutine count.

## Known issues

- mpv's stdout/stderr are not captured, so a failed start reports only mpv's exit
  status, not its message.
