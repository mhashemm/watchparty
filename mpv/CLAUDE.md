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
| `mpv.go` | `Client`, `connection.do`, `ErrClosed`, `watch`, `handleEvent`, `isEcho`, `ProcessIncomingEvents`, `pause`/`sync`/`showText`, `New` |
| `conn_unix.go` (`!windows`) | `SocketPrefix = "/tmp/"`, `Dial` (unix socket), no-op `Init` |
| `conn_windows.go` | `SocketPrefix = \\.\pipe\`, `Dial` (`go-winio` pipe), and `Init` setting `HideWindow` + `CREATE_NEW_PROCESS_GROUP \| CREATE_NEW_CONSOLE` on the mpv `exec.Cmd` |
| `MPV.md` | the slice of mpv we use, and its observed behaviour |
| `mpv_test.go` | `watch()` and `handleEvent` behaviour through a byte-dripping reader, `do()` replies over a `net.Pipe`, shutdown, what is applied after a slow load (`TestIncomingWhileLoadingKeepsOnlyTheLatest`) |

## Public surface

- `Dial(ctx, socket) (net.Conn, error)` opens the unix socket or the Windows pipe. It
  starts nothing, so `party.Run` retries it until mpv has created the socket.
- `New(ctx, cancel, conn, outgoing, incoming) (*Client, error)` starts
  `ProcessIncomingEvents(incoming)` and the `watch()` goroutine, sends `observe_property_string` for `pause` (id 1), `time-pos` (id 2) and
  `paused-for-cache` (id 3), then
  **waits for `ready`**: closed by `handleEvent` on the first `time-pos` that has
  data, i.e. once the file is loaded.
  - **`watch` must be running before those `do()` calls**, because only `watch`
    delivers replies.
  - When `watch` returns (EOF when mpv quits, or a read error), it closes the conn,
    closes `connection.closed`, and **calls `cancel` only if `ready` was reached**.
    This is how the party learns that mpv is gone. `party.Run` wraps that cancel to
    tell "mpv quit" apart from other stops (see `party/CLAUDE.md`).
  - Before `ready`, a hang-up makes `New` (and any `do()` in flight) return
    **`ErrClosed`** at once, and nothing is cancelled. `party.Run` then reads mpv's
    real exit status; cancelling would have killed mpv and turned it into `signal:
    killed`.
  - If an observe fails for another reason, `New` returns that error.
  - `ctx` is the party's context, kept on the `Client`. `watch`'s send to `outgoing`
    and `ProcessIncomingEvents`' receive both select on it, so neither outlives the
    party. The channels are never closed.
- `(*Client).ProcessIncomingEvents(incoming)` is the only consumer of remote events.
  `New` starts it, and it runs in one goroutine until the party ends.
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

This runs under `c.mu`, called from `watch()`. It returns `nil` (broadcast the event)
or one of three sentinels, plus `extra` frames that `watch` sends after the event,
or on their own when the event itself isn't sent.

| Situation | Result |
|---|---|
| `seek` while playing | `seeking` set, `errDoNotSend` |
| Not a `property-change` (any other `seek`, file-loaded, …) | `errDoNotSend` |
| A `property-change` with no `data` (property unavailable: before the file loads, while quitting) | `errDoNotSend`, and it does **not** count as the first change for `seen` |
| `isEcho`: the value is one this node just applied from a peer | `errEcho` |
| `pause` with the same value as the current state | `errNoChange` |
| `pause` that changed | state updated (and `seeks` cleared on unpause), then broadcast |
| `time-pos` while **playing** | `errDoNotSend`. If `seeking`, it is where the seek landed: `pauseParty("seek")` |
| `paused-for-cache` | `stalled` updated; `yes` while playing calls `pauseParty("buffering")`. `errDoNotSend` |
| `pause` → `no` (not an echo) while `stalled` | `pauseParty("still buffering")`, `errDoNotSend`, and `c.paused` stays true: the resume is not passed on |
| `time-pos` while paused | broadcast. This is a seek |
| any other property | `errDoNotSend` |
| **the first** `property-change` per property | state updated, but `errDoNotSend` |
| `pause` → `yes`, echo or not | `extra` from `pauseExtras`: if `pauseParty` set a `reason`, a `time-pos` of `c.pos` (also appended to `seeks`, so mpv's second value of the same seek is an echo), then the `pause-reason` |

Every `time-pos` with data also updates `c.pos`, echo or not.

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

- **`pause()` sets `applied[pause]` and `sync()` appends to `seeks` before the IPC
  call.** mpv's echo can arrive before `do()` returns, so marking afterwards is a race.
- `pause` matches exactly and **deletes** the marker on match.
- `time-pos` is checked against **`seeks`, every seek applied since the last unpause**,
  oldest first, as floats within **±0.1s** (`timePosTolerance`).
  - On a match at entry *i*, the entries before it are dropped and entry *i* becomes
    the echoed value. mpv handles seeks in order, so a later echo means it is done
    with the earlier ones. A user's own seek back to one of them still goes out.
  - It used to be a single marker. A node that applied 30 and then 40 saw mpv's late
    echo of 30 after the marker had moved, and broadcast it as its own seek: the party
    jumped back to 30 (seen in CI in `[R11d]`).
  - A seek's echo comes back once or twice and up to a frame off (`123.456` →
    `123.456000`, then `123.458000`, see `MPV.md`), which is why the entry is kept and
    updated. `--hr-seek=yes` is pinned, so a frame is the most it can be off, and 0.1s
    covers anything from 10fps up. A deliberate seek of 0.1s or more is not an echo.
  - Cleared when the node unpauses, capped at `maxSeeks` (16).
- `TestAppliedValueIsNotRebroadcast`, `TestLateEchoOfAnEarlierSeekIsNotRebroadcast`,
  `TestSmallSeekIsNotAnEcho` and `TestAppliedPauseIsNotRebroadcast` cover it,
  including that a genuine seek and a later genuine pause still go out.

### Seeking and buffering while playing pause the party (`pauseParty`)

Positions only sync while paused, so a seek while playing would leave every peer
behind, and so would a node waiting on its cache. Instead the node pauses the party:

1. A trigger calls `pauseParty(reason)` under `c.mu`:
   - **`"seek"`**: the `seek` event sets `seeking`, and the next `time-pos` is where
     it landed. `sync()` only seeks while paused, so a `seek` while playing is always
     this node's own.
   - **`"buffering"`**: `paused-for-cache` (observe id 3) changes to `yes` while
     playing.
   - **`"still buffering"`**: a resume while `stalled`. A local one is caught in
     `handleEvent` and not broadcast. A peer's is caught in `pause()` after it has
     applied it, since its `pause=no` is an echo that `handleEvent` drops.
3. `pauseParty` sets `reason` and sends `set_property pause true` **in a goroutine**,
   without marking `applied`. `do()` waits for a reply that only `watch()` reads, so
   calling it from `handleEvent` deadlocks. The old commented-out `case seek:
   s.pauseReq(true)` did exactly that. On success it shows "paused for everyone:
   <reason>". On failure it clears `reason`.
4. mpv reports `pause=yes`, broadcast as this node's own pause, followed by
   `pauseExtras`: the position (`c.pos`, so any frames played before the pause took
   effect count), then `{"name":"pause-reason","data":"seek"}`. Peers get pause first,
   then position: P2P `broadcast` waits for each healthy peer before the next event,
   and the relay keeps one FIFO per member. A peer's `apply` shows a `pause-reason` as
   "paused for <host>: <reason>".
5. If a peer's pause lands first, that `pause=yes` is an echo, and the extras still go
   out on their own, so the seek isn't lost.

`TestSeekWhilePlayingPausesTheParty`, `TestPlaybackIsNotASeek`,
`TestBufferingPausesTheParty` and `TestRemoteResumeWhileBufferingPausesAgain` cover
it, through `liveClient`, a `watch()` over a `net.Pipe` with a fake mpv. `e2e.py`
`[R6b]` and `[R11f]` (a stream served at half its bitrate to one node) cover it against
real mpv.

## Applying remote events: `ProcessIncomingEvents`

**It drains `incoming` from the moment `New` is called, not from `ready`.** A file can
take a long time to load (a URL stream, a slow disk), and a peer must never wait on
it. A P2P `/event` blocks on `incoming` and the sender counts the timeout as a strike.
A relay `read` that blocks stops reading the websocket, and the relay drops the
member once its 64-frame buffer fills. Until `ready`:

- only the latest `pause` and the latest `time-pos` are kept; the steps a later one
  overrides are discarded
- everything else (the notices) is kept in order

On `ready` it applies `time-pos` first, then `pause`, then the notices, and then
carries on applying each event as it arrives. Position goes first because mpv starts
`--pause`d, so the seek lands before any resume. Five seeks during a load become one
seek and one "synced by", where the old replay showed all five. `e2e.py` `[R11d]` covers this
against a stream whose HTTP server holds each request for 3s.

Each event, then, is `apply`d: its `IncomingMessage.Event` is unmarshalled and
dispatched by `Name`:

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
