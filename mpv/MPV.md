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
mpv <user MpvFlags...> --save-position-on-quit --pause --input-ipc-server=<sock> <file>
```

| Flag | Why |
|---|---|
| `--pause` | Every node starts paused, so nobody plays ahead before the party is wired up. |
| `--save-position-on-quit` | Quitting saves the position. The next start of the same file **resumes there** (see "Resuming"). |
| `--input-ipc-server=<sock>` | The JSON IPC endpoint. Unix: a socket path (`/tmp/mpv<random>`). Windows: a named pipe (`\\.\pipe\mpv<random>`); mpv adds the `\\.\pipe\` prefix itself if it's missing. |
| `--vo=null --ao=null` | Tests only (e2e, GUI checks): no window, no sound. |

**We don't pass `--no-config`, so the user's mpv config applies to the party**:
`mpv.conf`, `input.conf`, scripts, and the watch-later store. A user's `hr-seek=no`
or `keep-open` changes the behaviour described below. The dev machine's own
`mpv.conf` sets `save-position-on-quit`, so resume happens there even without our
flag.

## The connection

- **The socket exists ~40ms after mpv starts**, long before the file is loaded. The
  `-cooldown` (default 5s) is not waiting for the socket. What it buys is that the
  file is loaded when we observe, which `seen` depends on (see "Observing").
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
  position for a resumed one. **`seen` swallows only the empty one, and the next one
  would be broadcast, rewinding (or seeking) the whole party.** Only the cooldown
  being longer than the load time prevents this today: a short `-cooldown` or a slow
  file (network, huge mkv) brings it back. A fix would be to never broadcast or
  count a data-less change. Not done yet.

### Seeking (while paused)

- **An absolute seek** (`set_property time-pos`, which `sync()` uses) **is exact**
  under the default `--hr-seek=default`. `"77.000000"` echoed `77.000000`. A
  position between frames echoes the target first, then the landed frame after
  `playback-restart`: `123.456` → `123.456000`, then `123.458000`. So **an echo can
  come twice and be up to one frame off**. That's why the `time-pos` marker in
  `applied` matches within ±0.5s and is kept, not deleted, on the first match.
- **A relative seek** (arrow keys, `seek N relative`) snaps to a keyframe. +10 from
  123.458 sent `133.458000`, then `140.000000`, the keyframe. Both arrive while
  paused, so the user's node broadcasts **both**, and peers apply both and end on the
  same frame.
- With a user's `hr-seek=no`, absolute seeks snap to keyframes too. The echo can
  then be seconds off and gets rebroadcast once. Peers converge on the keyframe and
  it stops, because applying that value echoes exactly.

### Playing and pausing

- **While playing, `time-pos` fires on every frame** (24 times a second at 24fps).
  `handleEvent` drops all of them (`!c.paused`). Anything that wants to sync
  during playback (drift correction) must throttle, not forward these.
- On unpause, the `pause` → `"no"` event arrives before the first playing
  `time-pos`. On pause, only `pause` → `"yes"` arrives; the position doesn't
  re-fire.

### Ending

- **Quit** (the `quit` command; closing the window is assumed to be the same,
  not verified): the reply, then (if `time-pos` is observed) a **data-less
  `time-pos` change**, then `end-file` with `reason: "quit"`, then socket EOF, exit
  code 0.
  - That data-less change passes `handleEvent` while paused and is broadcast as a
    `time-pos` with no data. Receivers drop it in `sync()` (`event.Data == ""`),
    which is the only reason it's harmless.
  - The EOF is what ends `watch()` and the party (see `party/CLAUDE.md`, "Return
    values").
- **End of file:** without `keep-open`, mpv plays to the end, sends `end-file`
  `reason: "eof"`, closes the socket and exits 0. To the party that is the same as
  a quit, so each node's party ends with the movie. A user's `keep-open` instead
  pauses on the last frame and broadcasts that pause.

### Resuming

- With `save-position-on-quit` (always on in a party), a quit writes the position
  to the watch-later dir (`~/.local/state/mpv/watch_later` on Linux, or
  `--watch-later-dir`). The next start of that file **opens at the saved
  position**, still paused.
- With the default cooldown, the immediate observe event already carries that
  position and `seen` swallows it, so a rejoining node doesn't drag the party to
  where it left off. It also doesn't follow the party until the next seek or pause.

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
