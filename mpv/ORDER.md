# mpv/ORDER.md

What mpv guarantees about the order of what it says, why plain JSON IPC can't tell the
person's change from ours, and the script route built on it. From mpv's source (the v0.37.0
tag; the IPC, client and seek paths are unchanged through master as of 2026-11-15) and from
probes on 0.37.0 (this machine) and 0.40.0 (the e2e image), each with mpv pinned to a core
with three busy loops, the person's action fired -20 to +20ms around ours (2026-11-17/18).
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
- `[R6c]` (a playing `time-pos` after our `pause=yes`) was never reproduced: 0 of 340 rounds on
  0.37.0 in either observe order, then 0 of 300 on each version.

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
| `landed` | `playback-restart` after a `seek` | ours if that seek carried ours |
| `ready` | `playback-restart` with no `seek` (a load) | |
| `loaded` | `file-loaded` | ours if the load carried our move, `eof` for mpv's own move at a file's end |
| `held` | our seek while our move loads; sent at its `file-loaded` | ours |
| `end` | `end-file` (lost at close, above) | the reason |

Rules the probes checked: the last snapshot is mpv's state; a `landed` or `loaded` with our id
at our target is ours, any other is the person's; a stopped snapshot's position is exactly where
mpv stopped (the gap was 0.000000s in every round, so it is what a pause carries).

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

## Re-running

```sh
cd mpv/probe && ./videos.sh                      # ffmpeg; the videos stay untracked
./run.sh 0 probe.py echo:150 r6c:150 pause:140 check:420 repause:280
./run.sh 0 probe2.py startup:60 pause:280 seekp:420 playing:280 moves:280 buffering:40 ending:60
```

`run.sh` pins mpv to the core given, beside three busy loops; its header says how to run the
same on 0.40.0 in the e2e image. Each experiment prints its error counts, then every shape it
saw. Never on the user's screen: mpv runs `--vo=null --ao=null`.
