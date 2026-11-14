# mpv/MPV.md

The part of mpv this project uses, plus what mpv actually does on the wire. The
upstream manual is <https://mpv.io/manual/master/> (sections "JSON IPC",
"Properties", "List of events"). Read this first.

**Where this comes from.** The manual and real mpv 0.37.0 (the dev machine's) driven
over the socket on 2026-10-30, `--vo=null --ao=null --pause`, a 24fps H.264 file with a
keyframe every 10s. `ipc.rst` is unchanged through v0.41.0 apart from `--input-ipc-client`.
"Observed" means seen in those runs; re-probe after an mpv upgrade (recipe at the end).

## How `party.Run` starts mpv

```
mpv <user MpvFlags...> --pause --speed=1 --hr-seek=yes --cache-pause=yes --no-resume-playback --quiet --input-ipc-server=<sock> <file>
```

| Flag | Why |
|---|---|
| `--pause` | Every node starts paused, so nobody plays ahead before the party is wired up. |
| `--speed=1` | **Pinned.** A `speed=1.1` in someone's config would make their player drift away steadily. |
| `--hr-seek=yes` | **Pinned.** Every seek is exact, arrow keys included. Without it, relative seeks snap to a keyframe (see "Seeking"). |
| `--cache-pause=yes` | **Pinned.** When the cache runs dry, mpv waits and reports `paused-for-cache`, which pauses the party. With `no` it plays through stalls with no signal, and the node falls behind: `[R11f]` measured 16.9s without the pin. |
| `--no-resume-playback` | **Pinned.** Every node opens the file at 0. This also skips the whole watch-later entry, which would otherwise restore a saved `speed` over our `--speed=1`. |
| `--quiet` | **Pinned.** No status line. Into a pipe mpv prints it as a new line several times a second (`AV: 00:00:04 / 00:00:30 …`), which buried every other line of the log. Errors still print. |
| `--input-ipc-server=<sock>` | The JSON IPC endpoint. Unix: a socket path (`/tmp/mpv<random>`). Windows: a named pipe (`\\.\pipe\mpv<random>`); mpv adds the `\\.\pipe\` prefix itself if it's missing. |
| `--no-shuffle --playlist-start=<n>` | **Pinned, playlists only.** An index only means the same file to everyone in our order, and it starts on the file that was picked. |
| *(no `--start`)* | **Continuing a party seeks over IPC** once the first file is open. `--start` applies to every later entry, and a file-local `--{ --start=… file --}` (even undone with `file-local-options/start`) comes back on every return to it (probed on 0.37.0). |

**We don't pass `--no-config`, on purpose:** people keep their subtitle languages,
audio device, key bindings and volume. Instead, **the four options that change sync
are pinned on the command line, after the user's `MpvFlags`**. The command line beats
`mpv.conf`, and a later flag beats an earlier one, so the pins win over both.
Verified: a node started with `--speed=2 --hr-seek=no --resume-playback=yes` and a
saved `start=50, speed=1.5` entry came up at 0, speed 1, `hr-seek` yes. e2e runs
every node with those hostile flags plus `--cache-pause=no` (`[2b]`, `[5d]`, `[R11f]`). Anything else in a user's
config still applies, for example `keep-open` (see "Ending").

## The connection

- **The socket exists ~40ms after mpv starts**, long before the file is loaded. `party.Run`
  dials it every 50ms, and `mpv.New` waits for the first `time-pos` with data.
- Each connection is its own client. **Observers belong to the connection:** close
  it and they are gone, and mpv keeps running.
- It is local-only and can run arbitrary commands. Never expose it.

## Wire protocol

- **One JSON object per line**, `\n`-terminated, no raw newline inside. UTF-8. mpv
  can emit invalid UTF-8 in corner cases (odd filenames, tags).
- **Request:** `{"command": [name, args...], "request_id": N}`. Arguments are native
  JSON values. A line not starting with `{` is an `input.conf`-style text command and gets **no reply**.
- **Reply:** `{"request_id": N, "error": "success", "data": ...}`.
  - `request_id` is echoed verbatim and should be an integer; other types are
    deprecated. **A request without one is answered with `request_id: 0`.** That's why
    `watch()` treats only `request_id > 0` as a reply to us.
  - `error` is `"success"` or a reason. Observed: `"property not found"`, and
    `"property unavailable"` per the manual.
  - `data` is absent for `set_property`/`observe_*`, `null` for commands with no
    result (`show-text`, `quit`, `seek`), and the value for `get_property`.
- **Events:** `{"event": name, ...}`. They can arrive between our request and its
  reply. Replies to one connection come in request order, the manual's only guarantee.

## Commands we send

| Command | Notes |
|---|---|
| `["observe_property_string", id, name]` | Values arrive as strings: `"yes"`/`"no"`, `"77.000000"` (6 decimals). Fires once immediately (see "Observing"). |
| `["set_property", name, value]` | A **no-op set emits nothing**: no `property-change`, so no echo to suppress. `time-pos` takes a string, so a peer's `data` is forwarded as is, and is an **absolute seek**. |
| `["show-text", text, ms]` | OSD message for `ms` milliseconds (default `--osd-duration`). Replies `data: null`. |

## Properties we watch

- **`pause`** (RW): `"yes"`/`"no"` via `observe_property_string`.
- **`paused-for-cache`** (RO): `"yes"` while mpv is waiting on its cache, with
  `pause` still `"no"`. At load it comes first with no data, then `"no"`. Under
  `--cache-pause-wait` (default 1s) it clears once that much is cached again, and mpv
  then resumes by itself unless `pause` is set, which is why the node pauses itself.
- **`time-pos`** (RW): the position in seconds. **When unavailable** (no file yet, or quitting)
  **the `property-change` event has no `data` field at all**, which unmarshals to `Data == ""`.
- **`filename`** (RO): percent-decoded for a URL; it can hold a newline, so it is logged as JSON. Probed on 0.37.0. It fires only when the
  name changes: a replay, or a switch between same-named files (a torrent's folders), makes no history entry. Accepted until it's a real problem.

## Events

Seen during a normal run:

| Event | When |
|---|---|
| `property-change` (`id`, `name`, `data`) | An observed property changed, or right after `observe_*`. `id` is the observe id. |
| `start-file`, `file-loaded` | The file is opening, then loaded. |
| `seek` | A seek started. Arrives **before** the matching `time-pos` change. |
| `playback-restart` | Playback (or the paused frame) is ready after a seek or a load. Rapid seeks give one `seek` and one of these (probed on 0.40.0). |
| `end-file` (`reason`) | `"quit"` for a quit, `"eof"` for the end of the file. |

## Observed behaviour, and what our code relies on

### Observing

- `observe_property_string` replies `success`, then **immediately** sends one
  `property-change` with the current value. This is what `seen` swallows.
- **If the file isn't loaded yet**, `time-pos`'s immediate event has no `data`, and
  the real value follows after `file-loaded`: `0.000000` for a fresh file, the saved
  position for a resumed one, so `seen` must swallow that one, not the data-less first
  (`mpv/CLAUDE.md`).

### Seeking (while paused)

- **An absolute seek** (`set_property time-pos`, which `sync()` uses) **is exact**
  (exact even under mpv's default `--hr-seek=default`, and we pin `yes`). `"77.000000"` echoed `77.000000`. A
  position between frames echoes the target first, then the landed frame after
  `playback-restart`: `123.456` → `123.456000`, then `123.458000`. So **an echo can
  come twice and be up to one frame off**. That's why an applied seek in
  `seeks` matches within ±0.1s and is kept, not deleted, on the first match.
- **`time-pos` reports the target at once, before the data is there.** Probed
  paused on a local file: right after `set_property time-pos 50`, `get_property
  time-pos` read `50.0` while `seeking` still read `true` (it was `false` 2s later). On a slow
  stream that lasts until the new range arrives, and a node resumed in that window
  "plays" with `time-pos` stuck at the target, without `paused-for-cache`. A check that
  a seek finished must wait for `seeking` to be `false`.
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
- **End of file:** without `keep-open`, mpv plays to the end, sends `end-file`
  `reason: "eof"`, closes the socket and exits 0. To the party that is the same as
  a quit, so each node's party ends with the video. A user's `keep-open` instead
  pauses on the last frame and broadcasts that pause.

### Playlists (probed 2026-11-04 on 0.37.0 and 0.40.0)

- A switch sends a data-less `time-pos`, `playlist-pos` (0-based; its place around
  `end-file` varies), `file-loaded`, then `time-pos` 0. It always beat `file-loaded`.
- **`pause` carries over**, so a playing node starts the next file playing. No `seek` is sent.
- **Two sets in a row:** the reply beats the report, so the second can go first. Within about 5ms
  mpv may report only the last (0→1→0: `0` or nothing), from 10ms both. It always loads a file.
- A seek past the end while paused stops on the last frame. Resuming there moves on at
  once. After the last file: `end-file` `eof`, then exit 0.

### Resuming

- A user's `save-position-on-quit` writes the position to the watch-later dir
  (`~/.local/state/mpv/watch_later` on Linux), and plain mpv reopens there.
  **`--no-resume-playback` skips that**, so every node opens at 0: a rejoiner opening at
  its saved spot had that swallowed by `seen` and then didn't follow the party.

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

`--watch-later-dir` keeps probes out of the real resume store, `-g 240` makes keyframe
snapping visible, and observers die with the connection.
