# mpv/CLAUDE.md

`mpv` is the bridge between one running mpv process and the party. It speaks mpv's JSON
IPC over a unix socket (or a Windows named pipe), turns the player's own pause and
seek changes into broadcastable events, and applies remote ones without bouncing
them back.

Reference: **`MPV.md`** in this folder covers the commands, properties and events we
use, and how mpv actually behaves (probed against real mpv). Upstream:
<https://mpv.io/manual/master/#json-ipc>, <https://mpv.io/manual/master/#properties>

## Files

| File | Contents |
|---|---|
| `mpv.go` | `Client`, `connection.do`, `watch`, `handleEvent`, `isEcho`, `ProcessIncomingEvents`, `pause`/`sync`/`showText`, `New` |
| `conn_unix.go` (`!windows`) | `SocketPrefix = "/tmp/"`, unix-socket dial, no-op `Init` |
| `conn_windows.go` | `SocketPrefix = \\.\pipe\`, `go-winio` pipe dial, and `Init` setting `HideWindow` + `CREATE_NEW_PROCESS_GROUP \| CREATE_NEW_CONSOLE` on the mpv `exec.Cmd` |
| `MPV.md` | the slice of mpv we use, and its observed behaviour |
| `mpv_test.go` | `watch()` and `handleEvent` behaviour through a byte-dripping reader, `do()` replies over a `net.Pipe`, shutdown |

## Public surface

- `New(ctx, cancel, socket, outgoing) (*Client, error)` is called once mpv is up.
  - It dials, starts the `watch()` goroutine, then sends `observe_property_string`
    for `pause` (id 1) and `time-pos` (id 2).
  - **`watch` must be running before those `do()` calls**, because only `watch`
    delivers replies.
  - When `watch` returns (EOF when mpv quits, or a read error), it closes the conn
    and calls `cancel`. This is how the party learns that mpv is gone. `party.Run`
    wraps that cancel to tell "mpv quit" apart from other stops (see
    `party/CLAUDE.md`).
  - If an observe fails, `New` returns the error. The party ends and mpv is killed,
    which ends `watch`.
  - `ctx` is the party's context, kept on the `Client`. `watch`'s send to `outgoing`
    and `ProcessIncomingEvents`' receive both select on it, so neither outlives the
    party. The channels are never closed.
- `(*Client).ProcessIncomingEvents(incoming)` is the only consumer of remote events. It
  runs in one goroutine until the party ends.
- `Init(cmd)` must be called on the mpv `exec.Cmd` before `Start`.
- `SocketPrefix` is used to build the socket or pipe name.
- `IncomingMessage{HostName, Event}` is what both transports push. `Event` is raw mpv
  JSON.
- `Event`, `Request` and `Response` are the wire types. `server` and `relay` also use
  `Event` to synthesize `show-text` notices.

## Request/response: `connection.do`

- `do(args...)` registers a one-slot reply channel under a random `request_id` in
  `connection.pending`, writes `{"command": args, "request_id": id}\n` and **logs
  `do: sent request: <json>`**.
  - That log line is load-bearing: `e2e.py`'s `osd()`, the GUI's party feed and the
    GUI's history (`gui/store.go`'s `notice()`) recover every on-screen notice by
    parsing it for `["show-text", text, …]`.
  - Then it waits on its own channel, with a 5s timeout that returns `errTimeout`.
  - A response with `error != "success"` becomes an error.
- Each request has its own channel, so `do()` is safe from any number of goroutines.
  (A single shared channel once let concurrent calls steal each other's replies;
  `TestConcurrentRequestsGetTheirOwnResponse` covers it.)
- **`watch()` is the only reader of the socket.** Never read `conn.scanner` anywhere
  else.
  - A line with `request_id > 0` and a non-empty `error` is a response. `respond`
    hands it to the waiting request with a non-blocking send. An id nobody waits for
    (a request that already timed out) is logged as `Watch: dropped response`.
  - A request id of 0 is never treated as a response. Hand-written test commands
    without an id are harmless.
  - A line with an `event` field goes to `handleEvent`.

## What gets broadcast: `handleEvent`

This runs under `c.mu`, called from `watch()`, and returns `nil` (broadcast) or one of
three sentinels.

| Situation | Result |
|---|---|
| Not a `property-change` (seek, file-loaded, …) | `errDoNotSend` |
| `isEcho`: the value is one this node just applied from a peer | `errEcho` |
| `pause` with the same value as the current state | `errNoChange` |
| `pause` that changed | state updated (and the `time-pos` marker cleared on unpause), then broadcast |
| `time-pos` while **playing** | `errDoNotSend`. Positions only sync while paused |
| `time-pos` while paused | broadcast. This is a seek |
| any other property | `errDoNotSend` |
| **the first** `property-change` per property | state updated, but `errDoNotSend` |

On `nil`, `watch` sends **`bytes.Clone(scanner.Bytes())`** to `outgoing` and logs
`Watch: broadcasted event: …`. The clone is mandatory: `Scan()` reuses that buffer,
and the consumer reads it later. `TestWatchDoesNotAliasScannerBuffer` reproduces the
corruption only because `dripReader` returns about 7 bytes per `Read` against a
128-byte buffer. With the whole input in one fill, the bug hides.

### Why the first change is swallowed (`seen`)

`observe_property_string` makes mpv emit the current value immediately. A fresh node
is paused, so that first `time-pos` passes the "only while paused" rule and would be
broadcast as `time-pos=0`, rewinding everyone. The state is still recorded.
`TestInitialPropertyChangeIsNotBroadcast` covers this.

### Echo suppression (`applied` / `isEcho`)

When a peer's change is applied, mpv reports it back as a `property-change`. Without
suppression, b would rebroadcast a's pause to a, and in relay mode, which has no
counters, that loops forever.

- **`pause()` and `sync()` set `applied[prop] = value` before the IPC call.** mpv's
  echo can arrive before `do()` returns, so setting the marker afterwards is a race.
- `pause` matches exactly and **deletes** the marker on match.
- `time-pos` matches as a float within **±0.5s** and **keeps** the marker, updating it
  to the echoed value. A seek's echo comes back once or twice and up to a frame off
  (`123.456` → `123.456000`, then `123.458000`), and further off under a user's
  `hr-seek=no` (see `MPV.md`). The marker is cleared when the node unpauses.
  - Known limit (`ponytail:` on `timePosTolerance`): a deliberate seek of under 0.5s
    from the applied position is also swallowed.
- `TestAppliedValueIsNotRebroadcast` and `TestAppliedPauseIsNotRebroadcast` cover
  both halves, including that a genuine seek and a later genuine pause still go out.

### `seek` is commented out on purpose

The `case seek:` branch in `handleEvent` is disabled. Syncing works as pause → seek
(the `time-pos` change while paused) → peers apply it → resume. Re-enabling `seek`
changes that interaction. The `seek` const then looks unused; leave it.

## Applying remote events: `ProcessIncomingEvents`

Each `IncomingMessage.Event` is unmarshalled and dispatched by `Name`:

- **`pause`** → `pause(event, host)`.
  - A no-op if the node is already in that state. mpv emits no `property-change` for
    a no-op set, so no marker is left dangling.
  - Otherwise it marks `applied`, sends `set_property pause`, updates `c.paused`, and
    shows `paused by <host>` / `resumed by <host>`.
- **`time-pos`** → `sync(event, host)`.
  - Ignored unless this node is paused, and ignored for an empty value.
  - Otherwise it marks `applied`, sends `set_property time-pos`, and shows `synced by
    <host>`.
- **`show-text`** → `showText(event.Data)`. These are the join/leave/connect notices
  that `server` and `relay` synthesize.
- Errors are logged as `ProcessIncomingEvents: … [event=…] [host=…]` and skipped.

**Never hold `c.mu` across `do()`.** `do()` waits for `watch()`, and `watch()` needs
`c.mu` for `handleEvent`, so holding it turns every call into a 5s timeout. `pause()`
and `sync()` lock, mutate, unlock, and only then do IPC.

## Windows

- The pipe name is `\\.\pipe\mpv<random>`, dialed with `go-winio`.
- `Init` gives mpv `CREATE_NEW_CONSOLE` (hidden) and its own process group. mpv's
  video window is a normal window and is unaffected. Whether that holds when the
  parent is the `-H=windowsgui` GUI has not been verified on Windows yet.

## Testing notes

- Tests build a `Client` by hand (`newTestClient`) and drive `watch()` through
  `feed()`, or over a `net.Pipe` with a fake mpv on the other end. They never touch
  a real socket.
- mpv emits nothing for a no-op set, so tests that count events must strictly
  alternate values, never repeat them.
- When fixing a buffer or concurrency bug here, reintroduce it and watch the test fail
  before trusting the fix.
- Run with `go test -race ./mpv`. Every invariant above is a concurrency one.
