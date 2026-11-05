# mpv/CLAUDE.md

The bridge between one mpv process and the party: JSON IPC over a unix socket or a
Windows named pipe. It turns the player's own pause and seek changes into broadcast
events, and applies remote ones without bouncing them back. What mpv actually sends,
and why each flag is pinned, is in **`MPV.md`**: read it before changing anything here.

Security: the wire carries raw mpv JSON, but `apply` must dispatch only the fixed allow-list
below, never write a peer's frame straight to the IPC; `SECURITY.md` (repo root) says why
that line is load-bearing and holds the cross-cutting mindset and trust model.

## The connection

- **`watch()` is the only reader of the socket**, and `do()` waits for a reply only
  `watch()` delivers. So `watch` starts before `New`'s observe calls, and
  **`c.mu` is never held across `do()`**: `watch` needs `c.mu` for `handleEvent`, so
  every call would become a 5s timeout. Lock, mutate, unlock, then IPC.
- Only a line with `request_id > 0` and an `error` is a reply; mpv answers a request
  without an id with 0. An id nobody waits for anymore logs `Watch: dropped response`.
- `do()` logs `do: sent request: <json>`. Every on-screen notice is recovered from that
  line (contract table in the root `CLAUDE.md`).
- **`New` waits for `ready`**, the first `time-pos` with data, i.e. the first file is loaded.
  - Before `ready`, a hang-up returns `ErrClosed` and cancels nothing, so `party.Run`
    reads mpv's real exit status. Cancelling would kill the exiting mpv and turn
    "exit status 2" into "signal: killed". **A hang-up doesn't prove mpv is exiting:** the
    socket has dropped with mpv alive and well, so that wait has to be bounded.
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
  it landed. A peer's seek applied just before its resume can report after the resume, so
  until `playback-restart` (`peerSeek`) a `seek` isn't ours. Not a count: mpv folds seeks.
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
   (it can beat the pauses), then the seek, unless already within 0.1s. Members stop a moment
   apart, so everyone then settles on the earliest stop (`settling`, below).
4. Any resume or seek clears `joining`, so the room's first node stops being a joiner.

e2e `[11c]`, `[R9]`, `[R10b]`, `[R11d2]` (a joiner whose stream takes 3s to load).

### A member leaving (`left`)

The transports' "X has left" notice, sent only for a member that says goodbye or that the
relay loses, pauses the party. Reading the text means an already deployed relay works.
P2P's "dropped X" doesn't pause: it fires at least 5 minutes later, on a peer's next send.

- Each node pauses itself and it goes out as its own pause, so a node whose notice was
  lost still stops. Nodes stop a moment apart, so until the next resume `settling` makes
  `align` follow any earlier `paused-at`: they settle on the earliest stop.
- **Not within `endGraceSeconds` of the end.** The first player to reach the end leaves,
  and a pause would hold everyone else just short of it. So `duration` is observed.
- A notice shows first, so the GUI can tell this pause from a person's in its feed.

e2e `[14]`, `[R14]`, `[R15h]`.

### Playlists (`entries`, `followEntry`)

Only with `Config.Playlist` (`MPV.md`, "Playlists"), where everyone's list is identical
(`party/CLAUDE.md`), so an index means the same file to all. `playlist-pos` moving, by hand
or at the end of a file, goes out raw, and while playing also pauses the party with a
reason: nobody starts the next one until someone resumes.

- A peer's index is bounds-checked and only ever sets `playlist-pos`.
- **`followEntry` checks again after pausing, under the lock that sets `loading`.** Two
  players reach the end together, and its own move can arrive during the pause. Setting
  the same entry then emits nothing, and `loading` waits for a load that already happened.
- **`welcome` always names the entry**, even from a node still `joining`: the room's first
  node has no position to give, and each person can open a different entry.
- **`followEntry` pauses first**, so the file opens stopped, and marks `applied`, so
  mpv's report isn't sent back. It skips one it is on or already headed to: every member
  answers a joiner, and two nodes often reach the end together.
- **Only a node still `joining` follows an answer to a joiner** (a `playlist-pos` with no
  `From`), and moving by hand ends `joining`. Everyone hears every answer, and one sent just
  before two people crossed would otherwise send each to the other's entry, for good.
- **Two joiners who heard each other settle on the higher entry (`joinedWith`).** Each
  answers the other, and following both ways would swap them. An answer from anyone whose
  join it didn't hear is the party's, and is followed, lower or not.
- **Only a matching `playlist-pos` uses up the marker.** mpv's first report of where it
  started can land after a peer already sent this node elsewhere; used up on that, the real
  echo goes out as this node moving the party.
- **A switch resets `c.pos` and `seeks` to 0.** The old position would go out with the
  pause, and the new file's `time-pos` 0 would go out as a seek.
- **`loading` holds a seek until `file-loaded`** (`heldSeek`). Sent right after the
  switch it lands on the old file, which then ends and loses it. A joiner hits this every
  time: `welcome` sends the entry, then `start-pos`.
- **Every message says its entry (`tagged`).** A position or a resume for another
  entry is dropped, against where this node is `heading`, since it was sent before someone
  moved. A pause still counts. A table of who is on what can't do this: a follower doesn't
  announce its move, so the table would go stale and drop its later seeks.
- **Two moves that left the same entry settle on the higher (`movedFrom`, `From`).** Each
  would otherwise follow the other and swap. Dropping them would strand both. Any other
  move is followed, lower or not, so someone who missed one catches up on the next.
- Accepted: a seek reaching someone just before the move it depends on is lost. A node
  that really is on another entry stays paused until the next move, since its resumes
  and everyone else's are dropped.

e2e `[16]`, `[R15c]`.

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
