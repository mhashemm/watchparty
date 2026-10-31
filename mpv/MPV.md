# mpv/MPV.md

The part of mpv this project uses, plus what mpv actually does on the wire. The
upstream manual is <https://mpv.io/manual/master/> (sections "JSON IPC",
"Properties", "List of events"). Read this first, and the manual only for anything
not covered here.

**Where this comes from.** Protocol text: the manual for mpv 0.37.0, the version
installed on the dev machine. `DOCS/man/ipc.rst` is unchanged from v0.37.0 through
master (v0.41.0 is the latest release), apart from a new `--input-ipc-client`
mention, and no interface change since 0.37 touches anything below. Behaviour: real
mpv 0.37.0 driven over the socket on 2026-10-30, with `--vo=null --ao=null --pause`
and a 24fps H.264 file with a keyframe every 10s. "Observed" means seen in those
runs; re-probe after an mpv upgrade (recipe at the end).

## How `party.Run` starts mpv

```
mpv <user MpvFlags...> --pause --speed=1 --hr-seek=yes --no-resume-playback --input-ipc-server=<sock> <file>
```

| Flag | Why |
|---|---|
| `--pause` | Every node starts paused, so nobody plays ahead before the party is wired up. |
| `--speed=1` | **Pinned.** A `speed=1.1` in someone's config would make their player drift away steadily. |
| `--hr-seek=yes` | **Pinned.** Every seek is exact, arrow keys included. Without it, relative seeks snap to a keyframe (see "Seeking"). |
| `--no-resume-playback` | **Pinned.** Every node opens the file at 0. This also skips the whole watch-later entry, which would otherwise restore a saved `speed` over our `--speed=1`. |
| `--input-ipc-server=<sock>` | The JSON IPC endpoint. Unix: a socket path (`/tmp/mpv<random>`). Windows: a named pipe (`\\.\pipe\mpv<random>`); mpv adds the `\\.\pipe\` prefix itself if it's missing. |
| `--vo=null --ao=null` | Tests only (e2e, GUI checks): no window, no sound. |

**We don't pass `--no-config`, on purpose:** people keep their subtitle languages,
audio device, key bindings and volume. Instead, **the three options that change sync
are pinned on the command line, after the user's `MpvFlags`**. The command line beats
`mpv.conf`, and a later flag beats an earlier one, so the pins win over both.
Verified: a node started with `--speed=2 --hr-seek=no --resume-playback=yes` and a
saved `start=50, speed=1.5` entry came up at 0, speed 1, `hr-seek` yes. e2e runs
every node with those hostile flags (`[2b]`, `[5d]`). Anything else in a user's
config still applies, for example `keep-open` (see "Ending").

## The connection

- **The socket exists ~40ms after mpv starts**, long before the file is loaded.
  `party.Run` dials it every 50ms until it answers, and `mpv.New` then waits for the
  first `time-pos` with data, which means the file is loaded. A movie is ready in
  ~0.06s locally. (This replaced a fixed 5s `-cooldown`.)
- Each connection is its own client. **Observers belong to the connection:** close
  it and they are gone, and mpv keeps running.
- It is local-only and explicitly insecure (it can run arbitrary commands). Never
  expose it.

## Wire protocol

- **One JSON object per line**, `\n`-terminated, no raw newline inside. UTF-8. mpv
  can emit invalid UTF-8 in corner cases (odd filenames, tags).
- **Request:** `{"command": [name, args...], "request_id": N}`. Arguments are native
  JSON values. A line not starting with `{` is an `input.conf`-style text command
  and gets **no reply**.
- **Reply:** `{"request_id": N, "error": "success", "data": ...}`.
  - `request_id` is echoed verbatim and should be an integer; other types are
    deprecated. **A request without one is answered with `request_id: 0`.** That's why
    `watch()` treats only `request_id > 0` as a reply to us.
  - `error` is `"success"` or a reason. Observed: `"property not found"`, and
    `"property unavailable"` per the manual.
  - `data` is absent for `set_property`/`observe_*`, `null` for commands with no
    result (`show-text`, `quit`, `seek`), and the value for `get_property`.
- **Events:** `{"event": name, ...}`. They can arrive between our request and its
  reply, so never assume the next line is the reply. Replies to one connection come
  in request order (the manual's only ordering guarantee). The `async` flag exists;
  we don't use it.

## Commands we send

| Command | Sent by | Notes |
|---|---|---|
| `["observe_property_string", id, name]` | `New`, ids 1 = `pause`, 2 = `time-pos` | Values arrive as strings: `"yes"`/`"no"`, `"77.000000"` (6 decimals). Fires once immediately (see "Observing"). |
| `["set_property", "pause", bool]` | `pause()` | A **no-op set emits nothing**: no `property-change`, so no echo to suppress. |
| `["set_property", "time-pos", "<string>"]` | `sync()` | A string is accepted, so the received event's `data` is forwarded as is. This is an **absolute seek**. |
| `["show-text", text, ms]` | `showText()` | OSD message for `ms` milliseconds (default `--osd-duration`). Replies `data: null`. Its log line is how e2e and the GUI read notices. |

Useful when testing by hand (not used by the code): `["get_property", name]`,
`["quit"]`, `["seek", secs, "relative"|"absolute"]`.

## Properties we watch

- **`pause`** (option-property, RW): `"yes"`/`"no"` via `observe_property_string`.
  `core-idle` differs (also true while buffering or seeking). We don't use it.
- **`time-pos`** (RW): the position in seconds. **When unavailable** (no file yet,
  or quitting) **the `property-change` event has no `data` field at all**, which
  unmarshals to `Data == ""`. `playback-time` is the same, clamped to the file's
  range. We don't use it.

## Events

Every event goes through `handleEvent`; anything but `property-change` is
`errDoNotSend`. Seen during a normal run:

| Event | When |
|---|---|
| `property-change` (`id`, `name`, `data`) | An observed property changed, or right after `observe_*`. `id` is the observe id. |
| `start-file`, `file-loaded` | The file is opening, then loaded. |
| `seek` | A seek started. Arrives **before** the matching `time-pos` change. |
| `playback-restart` | Playback (or the paused frame) is ready after a seek or a load. |
| `audio-reconfig`, `video-reconfig` | Output reinitialised. Noise for us. |
| `end-file` (`reason`) | `"quit"` for a quit, `"eof"` for the end of the file. |

## Observed behaviour, and what our code relies on

### Observing

- `observe_property_string` replies `success`, then **immediately** sends one
  `property-change` with the current value. This is what `seen` swallows.
- **If the file isn't loaded yet**, `time-pos`'s immediate event has no `data`, and
  the real value follows after `file-loaded`: `0.000000` for a fresh file, the saved
  position for a resumed one. **`handleEvent` ignores a data-less change entirely**
  (it neither broadcasts it nor counts it for `seen`), so `seen` swallows the real
  value instead. Otherwise that value would be broadcast and rewind (or seek) the
  whole party.

### Seeking (while paused)

- **An absolute seek** (`set_property time-pos`, which `sync()` uses) **is exact**
  (exact even under mpv's default `--hr-seek=default`, and we pin `yes`). `"77.000000"` echoed `77.000000`. A
  position between frames echoes the target first, then the landed frame after
  `playback-restart`: `123.456` → `123.456000`, then `123.458000`. So **an echo can
  come twice and be up to one frame off**. That's why an applied seek in
  `seeks` matches within ±0.1s and is kept, not deleted, on the first match.
- **A relative seek** (arrow keys, `seek N relative`) **is exact too, because we pin
  `--hr-seek=yes`**: +7 from 12 sent exactly one position, `19.000000`. Under mpv's
  default it would snap to a keyframe instead: +10 from 123.458 sent `133.458000`
  and then `140.000000`, the keyframe, so peers seeked twice and landed up to a
  keyframe interval away from where the user aimed.

### Playing and pausing

- **While playing, `time-pos` fires on every frame** (24 times a second at 24fps).
  `handleEvent` drops all of them (`!c.paused`) unless a `seek` came just before.
- **What sends a `seek` while playing** (probed 2026-11-01 with internal and external
  audio and subtitle tracks and two chapters):
  - **Sends one:** relative and absolute seeks, including a −0.5s step; chapter
    skips (forward from 9.29 landed on 10.00, so +0.7s); a `--loop-file` wrap (29.6 →
    0).
  - **Sends none:** switching audio or subtitle track (internal or `--audio-file` /
    `--sub-file`), volume, `audio-delay`, `sub-delay`, `video-reload`,
    `audio-reload`, `frame-step`.
  - So no size threshold is needed: every `seek` while playing is the user's, except
    a loop wrap.
- On unpause, the `pause` → `"no"` event arrives before the first playing
  `time-pos`. On pause, only `pause` → `"yes"` arrives; the position doesn't
  re-fire.

### Ending

- **Quit** (the `quit` command; closing the window is assumed to be the same,
  not verified): the reply, then (if `time-pos` is observed) a **data-less
  `time-pos` change**, then `end-file` with `reason: "quit"`, then socket EOF, exit
  code 0.
  - `handleEvent` drops that data-less change. (Receivers also ignore an empty
    `time-pos` in `sync()`, for peers on older versions that still send it.)
  - The EOF is what ends `watch()` and the party (see `party/CLAUDE.md`, "Return
    values").
- **End of file:** without `keep-open`, mpv plays to the end, sends `end-file`
  `reason: "eof"`, closes the socket and exits 0. To the party that is the same as
  a quit, so each node's party ends with the movie. A user's `keep-open` instead
  pauses on the last frame and broadcasts that pause.

### Resuming

- With `save-position-on-quit` (we don't pass it, but a user's `mpv.conf` may set
  it), a quit writes the position to the watch-later dir
  (`~/.local/state/mpv/watch_later` on Linux, or `--watch-later-dir`), and plain mpv
  opens the file there next time.
- **In a party, `--no-resume-playback` skips that**, so every node opens at 0.
  Without the pin, a rejoining node opened at its saved position. `seen` swallowed
  that, so the party wasn't dragged along, but the node didn't follow the party
  until the next seek or pause either. A node that rejoins mid-party still opens
  at 0, and catches up on the next pause and seek.

## Re-probing

Drive a real mpv the way `watch()` does: Python, a unix socket, no dependencies.

```sh
ffmpeg -f lavfi -i testsrc=size=160x120:rate=24 -t 200 -c:v libx264 -g 240 -pix_fmt yuv420p /tmp/probe.mkv
mpv --vo=null --ao=null --pause --watch-later-dir=/tmp/wl --input-ipc-server=/tmp/probe.sock /tmp/probe.mkv &
python3 -c '
import socket, time
s = socket.socket(socket.AF_UNIX); s.connect("/tmp/probe.sock"); s.settimeout(1)
for c in [b"[\"observe_property_string\",2,\"time-pos\"]", b"[\"set_property\",\"time-pos\",\"77\"]"]:
    s.sendall(b"{\"command\":" + c + b"}\n"); time.sleep(0.5)
print(s.recv(65536).decode())'
```

- `--watch-later-dir` keeps probes out of the real resume store.
- `-g 240` gives a keyframe every 10s, so keyframe snapping is visible.
- Keep the connection open for as long as you want events: observers die with it.
