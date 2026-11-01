# mpv/CLAUDE.md

The bridge between one mpv process and the party: JSON IPC over a unix socket or a
Windows named pipe. It turns the player's own pause and seek changes into broadcast
events, and applies remote ones without bouncing them back. What mpv actually sends,
and why each flag is pinned, is in **`MPV.md`**: read it before changing anything here.

## The connection

- **`watch()` is the only reader of the socket**, and `do()` waits for a reply only
  `watch()` delivers. So `watch` starts before `New`'s observe calls, and
  **`c.mu` is never held across `do()`**: `watch` needs `c.mu` for `handleEvent`, so
  every call would become a 5s timeout. Lock, mutate, unlock, then IPC.
- Only a line with `request_id > 0` and an `error` is a reply; mpv answers a request
  without an id with 0. An id nobody waits for anymore logs `Watch: dropped response`.
- `do()` logs `do: sent request: <json>`. Every on-screen notice is recovered from that
  line (contract table in the root `CLAUDE.md`).
- **`New` waits for `ready`**, the first `time-pos` with data, i.e. the file is loaded.
  - Before `ready`, a hang-up returns `ErrClosed` and cancels nothing, so `party.Run`
    reads mpv's real exit status. Cancelling would kill the exiting mpv and turn
    "exit status 2" into "signal: killed".
  - After `ready`, `watch` ending (EOF on quit) calls `cancel`: that is how the party
    learns mpv is gone. `party.Run` wraps that cancel to tell a quit from other stops.
- `watch` sends **`bytes.Clone(scanner.Bytes())`**: `Scan()` reuses the buffer.
  `TestWatchDoesNotAliasScannerBuffer` catches it only because `dripReader` returns
  ~7 bytes per `Read`; with the whole input in one fill the bug hides.

## What gets broadcast (`handleEvent`)

Only `property-change` of `pause`, and of `time-pos` **while paused** (that is a seek),
go out. The non-obvious rules:

- **A change with no `data`** (property unavailable: before load, while quitting) is
  ignored entirely and doesn't count as the first change. Otherwise `seen` would
  swallow it and let the real first value through.
- **The first change per property is swallowed (`seen`).** An observe makes mpv send the
  current value at once; a fresh node is paused, so it would broadcast `time-pos=0` and
  rewind everyone. The state is still recorded.
- **Every `time-pos` updates `c.pos`, echo or not.** It is what every pause carries.
- **A pause goes out with its position:** `paused-at` = `c.pos`, or, if `pauseParty` set
  a reason, a `time-pos` plus `pause-reason` (`pauseExtras`). Those extras go out even
  when the pause itself was an echo, so a seek that raced a peer's pause isn't lost.
- `errNoChange` for a pause equal to the current state; a resume clears `seeks`,
  `seeking` and `joining`.

### Echo suppression (`applied`, `seeks`)

Applying a peer's change makes mpv report it back. Without suppression b rebroadcasts
a's pause to a, and the relay, which has no counters, loops forever. An origin id in
the envelope was rejected: re-stamping an echo needs the same correlation, plus a wire
change every transport must honour.

- **Mark before the IPC call** (`pause()` sets `applied`, `seekTo()` appends to `seeks`):
  the echo can arrive before `do()` returns.
- `pause` matches exactly and deletes the marker. A no-op set emits nothing, so
  `pause()` returns early on one and leaves no marker dangling.
- `time-pos` matches any entry of `seeks` (every seek applied since the last resume,
  capped at 16) within **±0.1s**. On a match at entry *i*, earlier entries are dropped
  (mpv handles seeks in order) and *i* becomes the echoed value, because one seek can
  echo twice, a frame apart. One marker isn't enough: an earlier seek's echo can land
  after a later seek was applied. 0.1s covers a frame at ≥10fps under the pinned
  `--hr-seek=yes`; a deliberate seek of 0.1s or more still goes out.

### Seeking and buffering while playing pause the party (`pauseParty`)

A seek while playing, or a node waiting on its cache, would leave the others out of
step, so that node pauses everyone with a reason:

- **`seek`**: a `seek` event while playing sets `seeking`; the next `time-pos` is where
  it landed. `sync()` only seeks while paused, so a seek while playing is always ours.
- **`buffering`**: `paused-for-cache` turns `yes` while playing.
- **`still buffering`**: a local resume while `stalled` is caught and not broadcast.
  A **peer's** resume while `stalled` never reaches mpv: `pause()` refuses it and sends
  pause, position and reason itself. Applying it and re-pausing doesn't work, since mpv
  folds the two sets into no `property-change`.

`pauseParty` sends the pause **in a goroutine** (it runs under `c.mu` inside
`handleEvent`, and `do()` from there deadlocks) and doesn't mark `applied`, so mpv's
`pause=yes` goes out as this node's own, followed by the extras. Peers get pause before
position: P2P `broadcast` waits per event and the relay keeps a FIFO per member.
e2e `[R6b]`, `[R11f]`.

## Applying remote events (`ProcessIncomingEvents`)

**It drains `incoming` from `New`, not from `ready`.** A slow load (a URL, a slow disk)
must never make a peer wait: a blocked P2P `/event` counts as a strike, and a blocked
relay read lets the relay's 64-frame buffer fill and drop the member. Until `ready` it
keeps only the latest `pause` and `time-pos` plus the notices in order, then applies
position first (mpv starts `--pause`d, so the seek lands before any resume), then
pause, then announces `joined`, then the notices. e2e `[R11d]`.

- `pause` shows "paused by X"/"resumed by X" and records `pausedBy`; our own pause
  clears it.
- `time-pos` is applied only while paused.
- **`paused-at` is followed only from the peer whose pause we applied** (`pausedBy`), so
  two nodes pausing at once don't swap positions, and only if not already within 0.1s
  (`at`). e2e `[R11g]`.

### Joining mid-party (`joined` → `welcome` → `start-pos` → `arrive`)

A joiner opens at 0, paused, and would stay behind until the next seek.

1. Once its file has loaded, the joiner sets `joining` and broadcasts `joined`, after the
   held events so a held resume can't clear it. So the party pauses only when the
   joiner can actually play, and a joiner whose mpv fails pauses nobody. Sent once; a
   relay reconnect doesn't resend it.
2. Every member, in `welcome`, pauses its own mpv if playing (not marked `applied`, so
   it goes out as its own pause), shows "paused for X: joining" either way (another
   member's pause often arrives first), and broadcasts `start-pos` = `c.pos`. A member
   still `joining` doesn't answer: its position isn't the party's.
3. The joiner takes the **first** `start-pos` only: applies a pause as from that peer
   (it can beat the pauses), then the seek, unless already within 0.1s.
4. Any resume or seek clears `joining`, so the room's first node stops being a joiner.

e2e `[11c]`, `[R9]`, `[R10b]`, `[R11d2]` (a joiner whose stream takes 3s to load).

## Windows

- The pipe is `\\.\pipe\mpv<random>`, dialed with `go-winio`.
- `Init` gives mpv `CREATE_NEW_CONSOLE` (hidden) and its own process group, so a
  node's Ctrl-Break never reaches it. Works under the `-H=windowsgui` GUI parent
  (`e2e_windows.py` `[W10]`, `--vo=null` only).
- **`Init` runs the `mpv.exe` next to an `mpv.com`.** A bare `mpv` resolves to the
  `.com` console wrapper (`.COM` precedes `.EXE` in `PATHEXT`); `cmd.Cancel` killed only
  the wrapper, `mpv.exe` kept the pipe open and `Run` never returned. CI's `windows` job
  runs its test, and `e2e_windows.py` `[W8]` runs a party through `mpv.com`.

## Tests

Tests build a `Client` with `newTestClient` and drive `watch()` through `feed()` or a
`net.Pipe` with a fake mpv; never a real socket. mpv emits nothing for a no-op set, so
tests that count events must alternate values.
