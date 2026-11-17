# mpv/CLAUDE.md

The bridge between one mpv process and the party, over JSON IPC. What mpv actually sends,
and why each flag is pinned, is in **`MPV.md`**: read it before changing anything here.

Security: the wire carries raw mpv JSON, but `apply` dispatches only the allow-list below,
never a peer's frame straight to the IPC; `SECURITY.md` (repo root) says why and holds the trust model.

## The loop

- **One goroutine owns a node's state** (`watch`, then `serve`). The rest only hand it inputs
  (mpv's lines and close, `do()`'s timer and writer, `incoming`, read first), taken one at a
  time in the order they came. No lock, no goroutine touching `Client`'s fields: an outcome
  would hang on which ready channel a `select` picks.
- **`do()` takes inputs while it waits**: mpv's lines are handled, other replies kept by id,
  a transport message queued while a peer's change is in flight (`busy`). A command an mpv
  line starts (`pauseParty`, a held seek) waits in `later` until that line's broadcast is out.
- **So an action that waits more than once stops once this node has `moved`** (`adopt`,
  `answer`, `resume`): the person's change went out newer. A play already sent is undone
  (`playLate`, `converge`); why no other late step is, `playLate` says. A mark that steers how
  mpv's lines are read is set at the step, never ahead (`peerSeek`, in `seekReq`).
- **`New` waits for `opened`**: the first `time-pos` with data and every observe taken, or the close.
  - Before that, a hang-up returns `ErrClosed` and cancels nothing, so `party.Run` reads mpv's
    real exit status. Cancelling would kill the exiting mpv and turn "exit status 2" into
    "signal: killed". **A hang-up doesn't prove mpv is exiting:** the socket has dropped with
    mpv alive and well, so that wait has to be bounded.
  - After, the close calls `cancel`, which `party.Run` wraps to tell a quit from other stops.
- `TestWatchDoesNotAliasScannerBuffer` catches a missing `bytes.Clone` only because
  `dripReader` returns ~7 bytes per `Read`; with the whole input in one fill the bug hides.

## What gets broadcast (`handleEvent`)

Only `property-change` of `pause`, and of `time-pos` **while paused** (a seek), go out:

- **A change with no `data`** (property unavailable: before load, while quitting) is
  ignored entirely and doesn't count as the first change. Otherwise `seen` would
  swallow it and let the real first value through.
- **The first change per property is swallowed (`seen`).** An observe makes mpv send the
  current value at once; a fresh node is paused, so it would broadcast `time-pos=0` and
  rewind everyone. The state is still recorded.
- **Every `time-pos` updates `c.pos`, echo or not.** It is what every pause carries (`at`).
- **While paused only the leader seeks** (`led`: stopped on the state of a member's pause or seek):
  a follower's seek stays on its screen, told once a state ("only X can seek until resumed"), and
  `held`/`Position` keep the party's second (`since`); its resume starts everyone from its screen.
  A stop nobody chose (`answer`, `left`, a leader that left) is `free`: the first seek leads.
- **If `pauseParty` set a reason, a pause is followed by a `time-pos` and a `pause-reason`**
  (`pauseExtras`), even when the pause was an echo, so a seek that raced a peer's pause isn't lost.
- **mpv's report of a pause this node applied is when it stops or starts** (`c.paused`),
  not the reply to the set: the next `time-pos` can come first, and while stopped it reads
  as a seek, which stops the party it was resuming with.

### Echo suppression (`applied`, `seeks`)

Applying a peer's change makes mpv report it back; rebroadcast, it loops forever over the relay.

- **Mark before the IPC call** (`commit` and `resume` set `applied`, `mark` appends to `seeks`): the echo can arrive before `do()` returns.
- `pause` matches exactly and deletes the marker; none is set for a pause mpv already has (no event).
- `time-pos` matches any entry of `seeks` (those applied since the last resume, or still landing
  through it, or the person's last seek; capped at 16) within **±0.1s**. On a match at entry *i*,
  earlier entries go (mpv handles seeks in order) and *i* becomes the echoed value: one seek can
  echo twice, a frame apart, and an earlier seek's echo can land after a later one was applied.
  0.1s covers a frame at ≥10fps under `--hr-seek=yes`; a deliberate seek of 0.1s or more goes out.

### Seeking and buffering while playing pause the party (`pauseParty`)

A seek while playing, or a node waiting on its cache, leaves the others out of step, so it pauses everyone with a reason:

- **`seek`**: a `seek` event while playing sets `seeking`; the next `time-pos` is where
  it landed. A peer's seek right before its resume reports after mpv replies to the resume, when
  `c.paused` is false, so until `playback-restart` (`peerSeek`) a `seek` isn't ours. mpv folds seeks.
- **`buffering`**: `paused-for-cache` turns `yes` while playing.
- **`still buffering`**: a local resume while `stalled` is caught and not broadcast. A **peer's**
  never reaches mpv: `resume()` refuses it and sends pause, position and reason itself. Applied
  and re-paused, mpv folds the two sets into no `property-change`.

`pauseParty` sends the pause after the line that started it (`later`), unmarked, so mpv's
`pause=yes` goes out as this node's own, then the extras; peers get pause before position (P2P
`broadcast` waits per event, the relay keeps a FIFO per member). e2e `[R6b]`, `[R11f]`.

## Applying remote events (`apply`)

**`incoming` is read from `New`, not from the load.** A slow load (a URL, a slow disk) must
never make a peer wait: a blocked P2P `/event` counts as a strike, and a blocked relay read
fills the relay's 64-frame buffer and drops the member. While `opening`, `hear` keeps the
notices in order and, of every `pause` and `time-pos`, only the newest **by stamp**: each
says the party's whole state, and an older one applied after would be answered as behind.
Then `join` seeks where a continued party stopped (`resumeAt`, marked like a peer's seek, so
not broadcast), applies that one, announces `joined`, then the notices, all as one message:
what comes meanwhile waits. e2e `[R11d]`.

### One rule for who follows whom (`stamp`)

Every message says the state it was sent from (`tag`, `held`): a `stamp` (a Lamport clock),
`paused`, `at` (where it stopped, or resumes from), in a playlist `entry`; one missing any is
refused (`stateOf`), and `take` weighs it. A move while playing says stopped. A node's own
change takes the next stamp, **written in the step that decided the event goes out**
(`handleEvent`): tagged afterwards, a peer's change adopted in between went out as its own.

- **Newer is taken. The same stamp means made at once, and the safer wins for everyone**
  (`over`): the higher entry, stopped over playing, the earlier second.
- **Older and saying another state means its sender missed something** (`answer`, "catching
  up"): the party stops as for a join and says where it is. Older and the same (`agrees`), or
  sent `again`, only crossed something newer. A tie's loser is told quietly: it may not have heard.
- **A resume reaching a node that plays stops the party** ("out of sync"): newer, it missed
  the pause between; a tie from another stop (`since`, beyond twice the tolerance), or from a
  member the mesh failed to reach (`Missed`) that sent nothing newer since; or the resume it
  took from a member, sent `again` by it: the first came late (`[13d]`). `Resend`: to it alone.
- **A joiner syncs with the party, never the party with it.** `announce` sets the stamp to
  `unheard`, far below any a party holds: nothing it does before it is answered wins, and
  joiners still order among themselves. `Opened` takes 1.
- **A newer state is taken whole, in the step that weighed it, before mpv is told** (`commit`:
  stamp, entry, the stop and the seek marked; `adopt` then tells mpv; a step mpv refuses is
  unmarked by `release`, where a new step adds its undo, and the node `announce`s unless mpv is
  gone). Taken apart, it holds the party's stamp on its own entry or second, a tie it can win;
  so it speaks for the entry it is **heading** to, never mpv's (`held`). A newer resume on
  another entry stops the party: loading, it can't keep up.
- **A stamp, not a vector clock:** one winner, never a merge; changes at once with different
  stamps go by the stamp. Revisit only with a simulation or a party that ends split, and ask.

### Joining mid-party (`joined` → `answer`)

A joiner opens at 0, paused, and would stay behind until the next seek.

1. Once its file has loaded, the joiner broadcasts `joined`, after what it heard while
   loading. So the party pauses only when the joiner can actually play, and a joiner whose
   mpv fails pauses nobody.
2. Every member, in `answer`, shows "paused for X: joining", **then** stops (the GUI's feed
   merges the two into "paused by you: joining", as for catching up) and says where it is.
   **One that was playing answers by stopping:** it pauses its own mpv (not marked
   `applied`, so it goes out as its own pause, which says where); a `time-pos` beside it
   could carry the stamp from before that pause, and read as falling behind. One stopped
   sends a `time-pos`. A joiner only says where it is, quietly: only joiners take it.
3. The joiner takes whatever it hears first, as newer; members stop a moment apart with
   one stamp, so everyone, joiner included, settles on the earliest (`over`).
4. **A dropped relay connection stops nobody by itself:** on `Lost` the node says so and plays
   on; on `Back` it sends its state marked `check`, weighed like any, but a resume in it stops
   the party unless it has the party's stamp: older, it missed a stop; newer, nobody heard it.
   A joiner never answered joins again; not near the end (`back`). `[R12]`, `[R13]`.

### A member leaving (`left`)

The transports' "X has left" notice, sent only for a member that says goodbye or that the
relay loses, pauses the party. Reading the text means an already deployed relay works.
P2P's "dropped X" doesn't pause: it fires at least 5 minutes later, on a peer's next send.

- Each node pauses itself, as its own `free` pause, so a node whose notice was lost still stops;
  they stop a moment apart with one stamp and settle on the earliest (`over`).
- **Not within `endGraceSeconds` of the end.** The first player to reach the end leaves,
  and a pause would hold everyone else just short of it. So `duration` is observed.
- A notice shows first, so the GUI's feed can tell this pause from a person's. `[14]`, `[R14]`, `[R15h]`.

### Playlists (`entries`, `adopt`)

Only with `Config.Playlist` (`MPV.md`, "Playlists"), where everyone's list is identical
(`party/CLAUDE.md`), so an index means the same file to all. `playlist-pos` moving, by hand
or at the end of a file, goes out raw, and while playing also pauses the party with a
reason: nobody starts the next one until someone resumes.

- A peer's index is bounds-checked and only ever sets `playlist-pos`.
- **`commit` sets `loading` before mpv is told.** Two players reach the end together, so
  mpv's own move can land while it stops; set later, `loading` waited for a load already
  done, holding every seek. Setting the entry mpv already reached is a no-op (`MPV.md`).
- **`adopt` pauses before a move**, so the file opens stopped, and marks `applied`, so
  mpv's report isn't sent back. It skips one it is on or already headed to: every member
  answers a joiner, and two nodes often reach the end together.
- **`following` queues moves:** the next can overtake mpv's report of the last (`MPV.md`). A
  report drops entries up to its own; `file-loaded` clears the rest once this node is where it
  headed, as there-and-back can report nothing. mpv's first report of where it started uses up
  nothing: it can land after a peer sent this node elsewhere.
- **Any other report is a key press and clears the queue,** so the party hears every report from there on.
- **A switch resets `c.pos` and `seeks` to 0** (`clear`). The old position would go out with
  the pause, and the new file's `time-pos` 0 would go out as a seek.
- **`loading` holds a seek until the entry it is heading to has loaded** (`heldSeek`), or it
  lands on another file and is lost; another move drops it. **The loaded entry is `start-file`'s
  id, never `current`:** mpv can report the entry after `file-loaded` (`MPV.md`).
- **A pause, seek or resume from another entry that doesn't win** was sent before someone
  moved: it loses on the entry, and its sender is told where the party is (`answer`, which
  stops a playing node): the move may never have reached it.
- Accepted: a seek reaching someone just before the move it depends on is lost, and a
  seek right after a move can lose to a stop on its way. e2e `[16]`, `[R15c]`.

## Windows

- The pipe is `\\.\pipe\mpv<random>`, dialed with `go-winio`.
- `Init` gives mpv `CREATE_NEW_CONSOLE` (hidden) and its own process group, so a
  node's Ctrl-Break never reaches it. Works under the `-H=windowsgui` GUI parent
  (`e2e_windows.py` `[W10]`, `--vo=null` only).
- **`Init` runs the `mpv.exe` next to an `mpv.com`.** A bare `mpv` resolves to the `.com`
  console wrapper (`.COM` precedes `.EXE` in `PATHEXT`), and killing the wrapper leaves
  `mpv.exe` holding the pipe. `e2e_windows.py` `[W8]` runs a party through `mpv.com`.

## Tests

- **One node: `driver_test.go`'s `node`**, a real `New` over a pipe to a fake mpv in a `synctest`
  bubble: `mpvSays`/`peerSays` in, `broadcasts()`/`mpvWasTold()` out; a state is reached as in
  production (`setup`, `takes`), never through a field. **`mpvAnswersLate`** has mpv report before
  answering, the order the echo guards exist for: without it, breaking one passed every test.
- **What nodes must agree on: `party_test.go`**, whole parties through every order a seed
  picks. A new message or rule gets a check there.
- Never a real socket. mpv emits nothing for a no-op set, so a test counting events alternates values.
