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
  line starts (`pauseParty`) waits in `later` until that line's broadcast is out.
- **So an action that waits more than once stops once this node has `moved`** (`adopt`,
  `answer`): the person's change went out newer.
- **`New` waits for `opened`**: the first `time-pos` with data, every observe taken and the script's name, or the close.
  - Before that, a hang-up returns `ErrClosed` and cancels nothing, so `party.Run` reads mpv's
    real exit status. Cancelling would kill the exiting mpv and turn "exit status 2" into
    "signal: killed". **A hang-up doesn't prove mpv is exiting:** the socket has dropped with
    mpv alive and well, so that wait has to be bounded.
  - After, the close calls `cancel`, which `party.Run` wraps to tell a quit from other stops.
- `TestWatchDoesNotAliasScannerBuffer` catches a missing `bytes.Clone` only because
  `dripReader` returns ~7 bytes per `Read`; with the whole input in one fill the bug hides.

## What gets broadcast (`handleEvent`)

Only `pause` and the person's seek **while paused** go out, as the script says them:

- **A change with no `data`** (property unavailable: before load, while quitting) is
  ignored entirely and doesn't count as the first change. Otherwise `seen` would
  swallow it and let the real first value through.
- **The first change per property is swallowed (`seen`).** An observe makes mpv send the
  current value at once; a fresh node is paused, so it would broadcast `time-pos=0` and
  rewind everyone. The state is still recorded.
- **`c.pos` is where mpv is:** each `time-pos` while playing, the script's snapshot once stopped
  (a playing second can be reported after the stop, `[R6c]`). It is what every pause carries (`at`).
- **While paused only the leader seeks or moves** (`led`): a follower's seek or move stays on its
  screen, told once a state ("only X can seek until resumed"); `held`/`Position` keep the party's
  entry and second (`partyEntry`, `since`). Its resume starts everyone from its screen; on another
  file the party stops there instead, as everyone must load it. Only the person's own pause, seek
  or move leads (`handleEvent`), not mpv's move at a file's end (`ranOut`): else `free` (`asked`).
- **If `pauseParty` set a reason, a pause is followed by a `time-pos` and a `pause-reason`**
  (`pauseExtras`), even when the pause was an echo, so a seek that raced a peer's pause isn't lost.

### One go per state (`goTo`, `flying`)

Applying a peer's change makes mpv report it back, which can't say whose it is (`ORDER.md`): so a
state goes to the script as one go, applied whole (`watchparty.lua`), or a change loops over the relay.

- **Hold before the IPC call** (`commit` sets the state, `goTo` the seek's mark): the ack can come
  before `do` returns. So `still`, mpv stopped before, is read before `commit`.
- **While a go is on its way, what the script says is nobody's**, nor are mpv's positions: it is
  from before the go, which overwrites it, or from the go. A key lost so is lost everywhere.
- **The ack is weighed, never matched** (`landed`): what differs from what this node holds is the
  person's, after the go, and goes out; a second off where mpv stood, with no seek sent, is their
  seek just before it. Another file than the one it headed to is their move, from where mpv stopped.
- A seek the script says with this node's id, within **±0.1s** of its second (a frame at
  ≥10fps, `--hr-seek=yes`), is ours; those sent before it were folded in. Any other is the
  person's: going out, it ends every mark; a follower's stays on its screen (`positionChanged`).
- **A stop from playing always seeks its second**: mpv plays on until the stop lands.
- **A key a go undid before this node read it** loses to that change anyway: only shown (`overruled`),
  when the ack says `over` (each go names the last script message read) and didn't make it theirs.

### Seeking and buffering while playing pause the party (`pauseParty`)

A seek while playing, or a node waiting on its cache, leaves the others out of step, so it pauses everyone with a reason:

- **`seek`**: a seek that isn't ours while playing (`seeking`). mpv folds seeks, so one carrying
  this node's id off its second is the person's too.
- **`buffering`**: `paused-for-cache` turns `yes` while playing.
- **`still buffering`**: a local resume while `stalled` is caught and not broadcast. A **peer's**
  never reaches mpv: the go stays stopped, and `refuseResume` sends pause, position and reason.

`pauseParty` sends the stop after the line that started it (`later`); its ack's `pause=yes` isn't
what the node holds, so it goes out as this node's own, then the extras; peers get pause before position (P2P
`broadcast` waits per event, the relay keeps a FIFO per member). e2e `[R6b]`, `[R11f]`.

## Applying remote events (`apply`)

**`incoming` is read from `New`, not from the load.** A slow load (a URL, a slow disk) must
never make a peer wait: a blocked P2P `/event` counts as a strike, and a blocked relay read
fills the relay's 64-frame buffer and drops the member. While `opening`, `hear` keeps the
notices in order and, of every `pause` and `time-pos`, only the newest **by stamp**: each
says the party's whole state, and an older one applied after would be answered as behind.
Then `join` seeks where a continued party stopped (`resumeAt`, a go like a peer's seek, so
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
  stamp, entry, the stop; `adopt` then sends one go; one mpv refuses is unmarked by `release`,
  and the node `announce`s unless mpv is gone). Taken apart, it holds the party's stamp on its own entry or second, a tie it can win;
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
   **One that was playing answers by stopping:** it pauses its own mpv (its ack's pause isn't
   what it holds, so it goes out as its own pause, which says where); a `time-pos` beside it
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
(`party/CLAUDE.md`), so an index means the same file to all. A move goes out raw, and while
playing also pauses the party with a reason: nobody starts the next one until someone resumes.

- A peer's index is bounds-checked and only ever sets `playlist-pos`.
- **The script opens another entry stopped and applies the rest once it starts** (its first
  `playback-restart`): a seek before it lands on the old file, or is dropped (`MPV.md`). The ack
  waits for that, `moveLimit` not 5s (a slow stream takes longer; mpv gives up on a hung one),
  or at the `end-file` of a file mpv can't open (not at another's load on the way); what comes
  meanwhile waits, a go for that file with it. `adopt` skips an entry it is on or already headed to: every member answers a
  joiner, and two nodes often reach the end together.
- **Only the script's `start` moves `current`** (`started`), labelled with whose it is: the go's
  that set the entry, `auto` (mpv's own at a file's end, or the first), else the person's.
  `playlist-pos` isn't observed: its reports overtake each other and fold (`MPV.md`).
- **A go names the entry it is for** (`goTo`): one a key left that this node hasn't read is
  only stopped, and the ack's (or refusal's) entry sends the move out. A seek there would go out
  as this node's second on the key's file, and the party would follow it there.
- **A switch resets `c.pos` to 0 and drops the marks for other files** (`commit`, `clear`). The
  old position would go out with the pause, and a seek queued for the old file never lands.
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
  answering and the script ack late: without it, breaking a guard passed every test.
- **What nodes must agree on: `party_test.go`**, whole parties through every order a seed
  picks. A new message or rule gets a check there.
- Never a real socket. mpv emits nothing for a no-op set, so a test counting events alternates values.
