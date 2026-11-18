# mpv/ORDER.md

What mpv guarantees about the order of what it says, why plain JSON IPC can't tell the
person's change from ours, and the script route built on it. From mpv's source (the v0.37.0
tag; the IPC, client and seek paths are unchanged through master as of 2026-11-15) and from
probes on 0.37.0 (this machine) and 0.40.0 (the e2e image), each with mpv pinned to a core
with three busy loops, the person's action fired -20 to +20ms around ours (2026-11-17/18). The
shipped script's probes ran on 0.42.0 too (Arch's, the release the installers fetch): 0 errors.
`MPV.md` holds how mpv behaves over the wire; `probe/` re-runs every number here.

## Why plain IPC can't tell who did what

- **No change carries its cause.** Every seek (our `set_property time-pos`, keys, the OSC,
  scripts) goes through `queue_seek` into one slot, `mpctx->seek` (`player/playloop.c`): an
  absolute seek replaces what is queued, a relative one adds to it. `cmd->sender` holds the
  client's name, but only overlays and logs read it. Hooks exist only around load and unload.
- **A property report is a snapshot, not a change.** The core reads the value at the end of
  a loop pass (`send_client_property_changes`) and hands reports out only once the client's
  event queue is empty (`client.h`: "Property changes are coalesced"), round-robin from
  `cur_property_index`, not in change order. A value equal to the last one reported makes
  no report; a value marked changed again is withheld until it is read again.
- **So a set the person undoes at once reports nothing.** Our `pause` set and the person's
  space within one loop pass leave no report at all: 6 to 9 of 140 rounds within 1ms, on
  both versions. A marker waiting for that report then swallows the person's next pause.
  - A `get_property` right after the reply can't tell this from a `yes`, `no` still on the
    way (13 and 11 of 140 rounds).
  - Unobserving and observing again does: mpv drops the pending report and sends the value
    held then (0 wrong in 280 rounds each). Rejected as a design: it patches one property.
- **A sync reply comes before any report our command causes.** One IPC thread per
  connection writes both, and writes a sync reply right after the command runs
  (`ipc-unix.c`, `ipc-win.c` alike): our own echo before our reply, 0 of 170 rounds on 0.37.0,
  then 0 of 150 on each version.
- **An `async:true` reply travels in the event queue**, as a normal reply line, so it is a
  fence in event order: events before it happened before our command ran. `client.h` warns
  async calls may reorder against other calls.
- **A seek's `seek` event comes before its `time-pos` report:** `mp_seek` queues the event,
  then the tick that marks `time-pos` changed. Seeks queued at once fold into one `seek`.
- **JSON IPC offers only `observe_property` and `observe_property_string`**, both value
  reports; there is no format `none`, and the `pause`/`unpause` events are gone. **No other
  protocol exists:** the only endpoints are `--input-ipc-server` and `--input-ipc-client=fd://N`,
  both this JSON.
- `[R6c]` (a playing `time-pos` after our `pause=yes`, seen once in CI) was never reproduced:
  about 1,400 rounds on both versions with e2e's video and the node's flags (2026-11-15), 0 of
  340 on 0.37.0 in either observe order, then 0 of 300 on each version.

### The IPC seek fence (probed, not used)

Our seek sent `async:true`; after its reply, the first `seek` event is the seek that carries
ours, and the `playback-restart` after that is where it landed: at our target means ours,
anywhere else the person's (their relative key added to ours, or their absolute one replaced
it). 0 errors in 420 rounds each, against relative and absolute keys, an absolute seek from a
second connection, a seek to the same second and to where mpv stood. **Trap:** the first
`playback-restart` after the reply can belong to the person's earlier seek.

## Scripts

- **`--script=<file>` loads even with `load-scripts=no`** (`mp_load_scripts` loads the list first).
- **mpv waits for every script's main chunk before it plays the first file**
  (`mp_play_files` → `mp_clients_all_initialized`), so a script misses no event of the file.
- **A script's name is its file name.** A clash appends a number to the later one (`wp2` →
  `wp22`, `mp_new_client`), and `script-message-to wp2` then reaches the other script with
  `success`. Learn the name: a broadcast `script-message`, answered with `mp.get_script_name()`.
- **Lua can observe with format `"none"`** (`player/lua.c`), which fires on every change
  mark, also when the value ends where it was: the silent undo above is seen.
- **`script-message` reaches every client as a `client-message` event**, in order, never
  coalesced. A script's own async command reply is an event in its queue, in order with
  `seek` and `start-file`, so the fence above works inside the script.
- **A script's last messages are lost when mpv closes:** at a quit or the end of the last file
  none arrived on 0.37.0 (30 of 30), all but once on 0.40.0. `end-file` over IPC always arrives.
- **Each client's event queue holds 1000 events**; when full, mpv drops what comes next until
  the client drains it, then sends a queue-overflow event (`client.c`).
- **`frame-step` (`.`) sends no `seek`:** mpv unpauses for one frame, so `pause` is marked
  changed (a `"none"` observer fires) and the position is new. `frame-back-step` (`,`) seeks.
- `paused-for-cache` observed `"none"` fires a few times a second while playing.

## The script design, probed

`probe/wp2.lua`. The node changes mpv only through the script, `script-message-to <name>`
`wp-pause <yes|no> <id>`, `wp-seek <second> <id>`, `wp-move <index> <id>`, and hears mpv
only from it: each message is a snapshot read when it is sent,
`wp <kind> <id> <pause> <time-pos> <playlist-pos> <paused-for-cache>`.

| Kind | Sent at | id |
|---|---|---|
| `hello` | a broadcast `wp-hello` | the script's name |
| `ack` | right after our pause set | ours |
| `pause`, `cache` | every change mark of `pause`, `paused-for-cache` | |
| `began` | every `seek` event | ours if that seek carries ours |
| `landed` | `playback-restart` after a `seek` | ours if that seek carried ours |
| `ready` | `playback-restart` with no `seek` (a load) | |
| `loaded` | `file-loaded` | ours if the load carried our move, `eof` for mpv's own move at a file's end |
| `held` | our seek while our move loads; sent at its `file-loaded` | ours |
| `end` | `end-file` (lost at close, above) | the reason |

Rules the probes checked: the last snapshot is mpv's state; a `landed` or `loaded` with our id
at our target is ours, any other is the person's; a stopped snapshot's position is exactly where
mpv stopped (the gap was 0.000000s in every round, so it is what a pause carries).

**`watchparty.lua` says a seek at its `seek` event (`began`), not at `landed`:** the person's seek
while stopped, then space at once (`[R6c]`), reports the space before the landing, so a landing
reads as a seek made while playing. `began`'s position is where the seek lands (0 off in 80
rounds each); `seekp` and `playing` counted at `began` (`LANDING=began`) gave 0 errors in 420 and
280 rounds on each version. Its `pause` can already hold a space pressed right after it (28 of
80): the node reads the order of messages, never that value.

| What the node does | Rounds each | Errors | Raced against |
|---|---|---|---|
| Pause, resume | 280 | 0 | the space key |
| Seek while paused | 420 | 0 | relative and absolute keys, an absolute seek from another connection, space, a seek to where mpv stood |
| While playing | 280 | 0 | the person's seek and our pause of the party while they act again; a peer's resume from its second (our seek, then our resume) |
| Playlist moves | 280 | 0 | next and prev keys, a seek right after our move, there and back, playing into a file's end |
| Buffering | 40 | 0 | a throttled HTTP source: our pause in a stall, our resume in a stall, the person's space |
| Startup | 60 | 0 | plain, `load-scripts=no`, a name clash: the hello always came |

Seen along the way:

- A seek past the end lands on the last frame, carrying our id but off our target. Peers share
  one file, so their seconds never pass its end.
- Moving away and straight back reloads the file at 0 (`end` `stop`, `loaded` with our id).
- Playing into the last entry's end with `keep-open` pauses on its last frame; nothing loads.
- Not probed: Windows, an mpv built without Lua (`--script` then fails to load it), a real
  remote stream.

### One message per state (`probe/wp3.lua`, built as `watchparty.lua`'s go)

The node sends the whole state it adopts, `wp-go <yes|no> <second|-> <entry|-> <id>`; the script
stops, moves (the rest waits for that file's `file-loaded`), seeks, plays if asked, and acks once,
so a person's key lands before or after it, never inside it. Errors counted: mpv ending away from
what was sent with no snapshot after the ack saying so, and a snapshot after the ack that differs
when the person did nothing. 0 of either on both versions, and no late ack:

| Raced against | Rounds each |
|---|---|
| no key, space, a relative and an absolute seek key, ±20ms, from stopped or playing, with and without a seek | 420 |
| next and prev keys (the node heading elsewhere), a seek right after the move, playing into the entry's end | 280 |
| a throttled stream stalled while playing: a stop or a resume while stalled, with or without space | 40 |

A key just before the handler is overwritten by it (mpv ends as sent): heard before the ack in
156 and 163 rounds, never heard in 59 and 59. Unheard, it left no effect anywhere: a seek key
folds into ours, and space's change mark reads the state after the handler.

**The shipped script, as the node sees it** (`probe4.py`: `watchparty.lua` unchanged, its
messages read with the entry and cache from mpv's own reports): the same three, 0 errors on
both versions, overwritten keys alike (never heard in 59 and 54 of 420). Then a move to a stream
that starts 8s late and to a file mpv can't open: acked once the file started, or at the
`end-file` of the broken one (mpv moved on to the next entry; as the last, under `--idle`, it
stays on none, `-1`), 12, 20 and 20 rounds, 0 errors. A seek
at that stream's `file-loaded` was dropped in 5 of 12 rounds (acked, no `seek`, at 0), so a move's
go waits for its first `playback-restart`. A stream that hangs (`hang`: never answers, headers
only, or its start then a stall) never held a go past 81s: mpv gives up on it (`MPV.md`).

**A key the go undoes, flagged** (`over`, 840 rounds each, keys -20..+20ms around the go): the
script numbers its messages, each go says the last one the node read, and the ack says `over` when
mpv's pause had changed since then, a person's seek came since, a seek ran while the go's seek
was queued, or mpv already played when the go was to play it. That last one matters: a go that
seeks isn't one step (stop, seek, then play at the seek's answer), and a space in between was
missed in 6 and 4 rounds before it was checked. Then on 0.37.0 and 0.40.0: every lost key
flagged (50, 83), none flagged with no key (212 each). 2 and 8 flags on a key not lost were all
a seek just before a go that doesn't seek, from stopped, which the ack already sends as theirs.

**Whose file each start is** (`starts`, 490 rounds each; `autos`, 40): the script labels every
`start-file` with the go's id when it opens the entry a go set (a go's set lost to a later one
never opens; one set after the start is still to come), `auto` after an `end-file` `eof` or
`error` and for mpv's first file, else the person's. Raced: next and prev keys -20..+20ms around
our move (the key runs from either entry), a seek after it, a file's end, there and back (a
second go while the first loads), a key alone, playing into an end, a broken entry. 0 wrong
labels, 0 wrong final entries on both versions. A person's start the go then undid is flagged
`over` (0 missed, 0 false). The start's entry is its `playlist_entry_id` - 1: `playlist-pos` can
already name a later set.

**A go racing a key that opens another file** (`away`, 840 rounds each): the node sends a stop
with a second for its file (`on`) as the person presses next -20..+20ms around it, playing or
stopped. When the ack names the key's file, the go never seeked there, and mpv is where the ack
says: 0 errors on all three. The old file's queued seek can still be said after the ack: mpv runs it
as it leaves that file, and the new one opens at 0. 0.42.0 then still says the old file's second
(the seek's, with `playlist-pos` already on the new: 3 of 840 rounds, all with the key 1ms after),
which the node sent with the move, so the script says no position until a set entry starts. A
go's seek begun just before such a key is `refused` instead (once in `moves` on 0.40.0).

## Re-running

```sh
cd mpv/probe && ./videos.sh                      # ffmpeg; the videos stay untracked
./run.sh 0 probe.py echo:150 r6c:150 pause:140 check:420 repause:280
./run.sh 0 probe2.py startup:60 pause:280 seekp:420 playing:280 moves:280 buffering:40 ending:60
LANDING=began ./run.sh 0 probe2.py began:80 seekp:420 playing:280
./run.sh 0 probe3.py go:420 moves:280 buffering:40
./run.sh 0 probe4.py go:420 moves:280 buffering:40 slow:12 broken:20 brokenlast:20  # the shipped script
python3 -I probe4.py hang 2                         # ~10 minutes: mpv's own timeout
./run.sh 0 probe4.py over:840                       # the ack's over flag against lost keys
./run.sh 0 probe4.py starts:490 autos:40            # whose file each start is
./run.sh 0 probe4.py away:840                       # a go for the file a key is leaving
```

`run.sh` pins mpv to the core given, beside three busy loops; its header says how to run the
same on 0.40.0 in the e2e image. Each experiment prints its error counts, then every shape it
saw.
Never on the user's screen: mpv runs `--vo=null --ao=null`.
