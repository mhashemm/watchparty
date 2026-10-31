# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
make -j                                    # cli, gui and relay for this machine, into .bin/
make -j gui linux                          # components × OSes: any of cli gui relay, linux darwin windows
make -j test                               # unit tests (-race) and e2e.py together, ~45s
go build ./...
go test ./...
go test -race ./...                        # the invariants below are all concurrency ones
go test ./mpv -run '^TestName$' -v         # single test
go mod tidy                                # run it yourself; CI only does go mod download
go build ./relay                           # the relay server, a separate binary
go build -o watchparty-gui ./gui           # the GUI; `-o` because ./gui is a directory
docker build -f relay/Dockerfile -t watchparty-relay .   # from the REPO ROOT
./e2e.py                                   # all three phases at once, real mpv, ~45s
./e2e.py --mode p2p | relay | caps         # one phase only
./e2e.py --file movie.mkv                  # ...against a real file
./e2e.py --nodes 3                         # fewer nodes, faster
```

`e2e.py` builds both binaries, generates a test file with ffmpeg and runs five
headless nodes on this machine, driving them through mpv's IPC socket. It has three
phases. By default it runs them at the same time as three copies of itself, each with
its own ports (`--port-base` + 0, 20, 40) and run directory, and prints each one's
output when it ends. That works because relay-mode nodes open no ports. The slowest
phase sets the total, so a new slow case belongs in whichever phase is shortest.

- **`p2p`** — no node is given the full peer list (A is the root, the rest
  bootstrap off an earlier node), so the mesh has to close itself through `/hi`
  peer lists. Covers mesh formation, propagation from any node, echo suppression,
  peer eviction and strike reset, `/bye`, and the header checks.
- **`relay`** — the same nodes against one `./relay` process. Covers fan-out,
  room isolation, the shared secret, reconnect after the relay is killed
  mid-party, and `[R15b]`: closing a node's mpv is a normal end (exit 0, the room
  hears "has left"). Every other case ends nodes with a signal, which is how a
  regression there once slipped through.
- **`caps`** — one fresh relay per case: every abuse cap, `CREATE_KEY`, and
  dropping a client that never answers pings (`[R20]`, ~40s: it waits out the real 30s ping and 10s pong timeout, and sets the total).

`e2e.py` does not drive the GUI. The GUI is covered by `go test ./gui` (the form →
`Config` mapping and the request guard) and by the headless-Chrome checks described
in `gui/CLAUDE.md`. Since it calls the same `party.Run`, the e2e suites cover
everything underneath it.

Needs `mpv` on PATH and a `-local`-reachable interface.

`osd()` returns a **list**, so `.count(x)` is an exact element match, never a
substring search — `osd(n).count("paused by ")` is silently always 0. Use the full
string (`f"paused by {A}"`) or the `pauses()` helper.

**No fixed sleeps for things that happen.** Every wait is `until(cond, ceiling)`, which
polls and returns as soon as the condition holds; `BOOT` and `SETTLE` are ceilings, not
delays. A P2P node is launched once the previous one's port accepts (`listening()`),
since it may bootstrap off it; relay nodes open no port and start together. Only a
check that something did **not** happen sleeps: `QUIET` (1s) after its positive half
holds (no echo, one broadcast per seek, no rewind from a late joiner, the other room
ignored), plus the 4s `time-pos` storm windows. `toggle()` keeps its 0.9s because
"no new strike while healthy" needs the broadcast to have finished. A new check that
asserts an absence needs its own fixed window; anything else polls.

Release builds (`.github/workflows/publisher.yml`, on tag push) cross-compile via a `strategy.matrix` for linux/windows/darwin, amd64+arm64. Each matrix entry builds both the CLI (`out`) and the GUI (`gui`, with `-H=windowsgui` on Windows so no console opens) and uploads both. The GUI build also gets `-X main.version=<tag>`, which is what its self-update compares against (see `gui/CLAUDE.md`). Releases up to v0.0.11 lack it and report `dev`. CI runs `go mod download`, not `go mod tidy`, so an untidy `go.mod` will not be caught there — tidy before you push. It builds `.` rather than `main.go`: naming the file works today only because `main` is a single file, and it fails outright when run from outside the module directory. No linter is configured.

Every pull request runs `.github/workflows/pr.yml`, with the same Go setup as the release:
a matrix job per OS runs `make -j4 <os>` (all three binaries, every release arch), and a
`test` job installs mpv and ffmpeg and runs `make -j test`. It passed locally pinned to 4
cores (`taskset -c 0-3`), the size of a GitHub runner, in 45s.

Manual run (two terminals / two machines):

```sh
./watchparty -local -file movie.mkv -port 6969
./watchparty -local -file movie.mkv -port 6970 -addrs 192.168.1.5:6969
```

Or with the GUI, which does all of the above from a form and needs only mpv:

```sh
go build -o watchparty-gui ./gui && ./watchparty-gui
```

Or through a relay, which is what people behind carrier NAT need:

```sh
CREATE_KEY=k ./relay                            # listens on :8080; omit CREATE_KEY to let anyone open rooms
./watchparty -relay ws://host:8080 -room test -secret hunter2 -createKey k -file movie.mkv   # host
./watchparty -relay ws://host:8080 -room test -secret hunter2 -file movie.mkv                # guests
```

## Architecture

Each package has its own CLAUDE.md with the details: `party/`, `mpv/`, `server/`,
`relay/` and `gui/`. This file keeps the overview and the cross-cutting rules.

Peer-to-peer mpv sync. `party.Run` (`party/party.go`) is one node from start to
finish. Two front ends call it, and each does everything on its own: `main.go`, the
CLI, which only turns flags into a `party.Config`, and `gui/`, which builds the same
`Config` from a form and runs `party.Run` in-process. `Run` wires three pieces:

1. **A transport**, one of two:
   - **P2P mesh** (`server/`): every node is both an HTTP server and a client to every
     other node, with no host. `-local` advertises the LAN IP, otherwise UPNP maps a
     port and the external IP is advertised. Peers are keyed by the address the
     remote advertises in `hit-me-up`, not the string passed to `-addrs`, so always
     use the address the node prints.
   - **Relay** (`-relay <ws url>`, client in `party/ws.go`, server in `relay/`): every
     node dials *out* to one websocket server, so nobody needs inbound reachability.
     No UPNP, no listener, nothing from `server/`. The relay is a separate binary,
     deployed by hand or from `relay/Dockerfile`, and is not in the release matrix.
2. **`mpv/`**: the mpv JSON IPC client, over a unix socket or a Windows named pipe
   (build tags in `conn_unix.go` / `conn_windows.go`). The socket is named
   `mpv<random>` per `Run`.
3. **Two channels**, created per call in `Run`:
   - `outgoing chan []byte`: `mpv.Client.watch()` → the transport → every peer.
   - `incoming chan mpv.IncomingMessage`: the transport → `ProcessIncomingEvents` →
     IPC commands into mpv.

Event payloads travel as **raw mpv JSON bytes end to end**; no transport parses them.
Transports synthesize `show-text` notices (joined, left, connected) and push them
onto `incoming` themselves.

### Loop prevention

**There is no master and no slave.** Every peer can drive the player at any time.
(A `role == slave` echo hack once muted whoever last applied a remote pause, which also
swallowed that peer's own seeks. Don't bring it back.) Three mechanisms keep events
from looping; details in `mpv/CLAUDE.md` and `server/CLAUDE.md`:

- **`applied`** (`mpv/`, both transports): the value last applied from a peer. mpv
  echoes it back as a `property-change`, and `handleEvent` drops it instead of
  rebroadcasting. `time-pos` matches within ±0.5s because a seek's echo can
  land a frame off and come twice (see `mpv/MPV.md`).
- **`seen`** (`mpv/`, both transports): the first `property-change` per property is
  mpv's reply to `observe_property_string`, and is never broadcast. Otherwise a fresh
  node broadcasts `time-pos=0` and rewinds the party.
- **Counters** (P2P only): `/event` and `/bye` drop anything not newer than the last
  counter seen from that peer. The relay never echoes a frame to its sender, so relay
  mode needs no counters, and `applied` is all that stops a loop there.

Only the node that *generates* an event broadcasts it. Applying a remote change never
re-emits it.

### Sync semantics (deliberately narrow)

Only `pause` and `time-pos` are observed, and only `property-change` events are
forwarded. `time-pos` is only forwarded and only applied **while paused**, so seeking
works as: pause → seek → peers sync position → resume. Any paused peer can seek for
the party. The `seek` case in `handleEvent` is commented out on purpose, which makes
the `seek` const look unused; leave it.

### Contracts between packages

- **Log lines are an API.** The GUI's status display (`handle()` in
  `gui/index.html`), its party history (`gui/store.go`) and `e2e.py` all parse
  them. Change any of these and update every reader in the same commit: `your address
  to share is`, `relay: connected`, `relay: dial:`, `relay: disconnected`, `is not
  open yet`, `rejected by relay`, `init done`, and `do: sent request:` (for
  `show-text`). The table is in `gui/CLAUDE.md`.
- **Notice texts are parsed too.** "X has joined" and "connected to X" are how the
  GUI history records who was there. `server/` and `relay/` send the same texts, so
  change both or neither.
- **The relay envelope** `{"hostname":…, "event":<raw mpv JSON>}` is declared in both
  `relay/main.go` and `party/ws.go`. The relay is `package main` and importing
  `party` from it would pull upnp, server and mpv into its image, so keep the two in
  sync by hand.
- **`party.Run` runs many times in one GUI process**, so it must not leave global
  state or goroutines behind. Flags live only in `main.go`.

### Concurrency invariants

Each of these has caused a real bug, and each reads fine locally. The package docs
have the full story.

- **Never hold a lock across a blocking call that needs the same lock to finish.**
  `c.mu` across `conn.do()` (mpv), `relayConn.mu` across `conn.Write` (party), `s.mu`
  across `broadcast`'s fan-out (server).
- **In `pause()` and `sync()`, set `applied` before the IPC call**, or the echo can
  arrive before the marker does.
- **Exactly one relay write pump per party, spanning reconnects.** Two goroutines
  ranging over `outgoing` steal frames from each other.
- **Synthetic notices go through a non-blocking send** (`Server.notify`). Nothing
  drains `incoming` until mpv is up.
- **`Shutdown` uses `context.WithoutCancel`**, because the party's context is already
  cancelled when the deferred `/bye` runs.
- **Every per-party goroutine selects on the party context**, so it ends with `Run`.
  The channels are never closed: their writers outlive the readers, and a send on a
  closed channel panics.
- **Never `panic`, `log.Fatal` or `os.Exit` in `party/`, `mpv/` or `server/`.** They
  skip `Run`'s defers (UPNP mapping, `/bye`) and would take the GUI window down. Return
  errors, or call the `cancel` that `Run` hands down. Only `main.go` exits.
- **`defer cmd.Cancel()` only after `cmd.Start()` succeeds**; it nil-panics otherwise.
- **`bytes.Clone` anything from `scanner.Bytes()`** before it goes on a channel.

### Conventions

The code carries almost no comments — match that. Explanatory comments get removed in review; put the reasoning in the commit message or here instead.

Tests: `server/server_test.go` drives `broadcast` directly with a stub `f` — no HTTP server, no fixtures. `party/ws_test.go` points a `relayConn` at a stub `httptest` websocket handler, not the real relay. `mpv/mpv_test.go` feeds `watch()` through a `dripReader` that returns ~7 bytes per `Read` with a small `sc.Buffer`, because the scanner-aliasing bug does **not** reproduce when the whole input fits one buffer fill. When fixing a concurrency/buffer bug here, reintroduce it and confirm the test fails before trusting it.

Every Go test calls `t.Parallel()` and finishes in milliseconds; the whole suite is bounded by
compile time. Keep it that way:

- **No real-time waits.** A delay the code under test owns is a struct field that the test
  shortens (`relay.ping`/`relay.timeout`, `relayConn.retry`), never a package `var` the test
  reassigns, which races with every other parallel test.
- **Prove an absence with an ordering, not a timeout.** Send a marker after the thing that
  must not happen; a FIFO delivers anything that did happen first (`mark`/`noticesBefore` in
  `relay/main_test.go`). The one exception is `mpv`'s `TestNewWaitsForTheFileToLoad`, whose
  300ms window has nothing to order against.
- **The only serial test is `TestLimiterDoesNotAllocatePerIP`**: `testing.AllocsPerRun`
  panics in a parallel test.
- Stress a changed test before trusting it: `go test -race -count=30
  -cpu=1,4,16 ./...`. Running in parallel exposed two races the serial suite had hidden.

mpv emits no `property-change` for a no-op set, so `set_property pause yes` when already paused produces nothing. Tests that count events must strictly alternate rather than repeat a value.

`e2e.py` recovers each node's OSD notices by parsing its `do: sent request:` log line. That format is load-bearing for the suite — change it and update `osd()` in the same commit. The relay suite likewise greps `relay: connected` and `relay: dial` out of node logs.

Every behaviour change lands with its `CLAUDE.md` note and its `e2e.py` case in the same commit.

## How we work

This is how work on this repo has gone best. Follow it unless asked otherwise.

### Git and releases

- **Never commit on `master`.** Branch first (`gui-portal-picker`, `update-claude-md`,
  …). If you find yourself on `master` with a commit, move it to a branch and reset
  `master` to `origin/master`. Check `git status -sb` before committing: the user
  switches branches between requests (for example after merging a PR on GitHub).
- **Commit each change as soon as it is verified**, without asking. Don't let finished
  work pile up uncommitted. One logical change per commit.
  - The subject is `area: what changed and why it matters` (`party: closing mpv is a
    normal end, not an unexpected stop`). The body has the reasoning, what was tested,
    and anything left unverified, plus the session's attribution trailer.
  - Put the `CLAUDE.md` note for a change in the same commit.
- **Push and open PRs only when asked.** A PR description lists what changed and how
  it was tested, and says plainly what was not exercised.
- **Merge with a merge commit** (`gh pr merge N --merge`), matching the history's
  "Merge pull request #N". Then pull `master` and delete the merged local branch.
- **Releases:** a lightweight tag `vX.Y.Z` on the merge commit on `master` (`git tag
  vX.Y.Z <sha> && git push origin vX.Y.Z`), only when asked. The tag push runs
  `publisher.yml`, which uploads 10 files (CLI and GUI × linux amd64/arm64, darwin
  amd64/arm64, windows amd64).
- **Verify a release by its files, not the Actions status.** The GitHub API has
  returned 502s and stale "queued" job states while the release was already complete.
  1. Wait until all 10 assets are `uploaded`.
  2. Download the Linux GUI.
  3. Run it headless and check `GET /update` reports the new tag.
  4. Load its page in headless Chromium and click a tab. `GET /update` only proves
     the server; a broken page script shipped in three releases that passed step 3.
- History was rewritten once to remove personal wording, which took a
  `--force-with-lease` that the user ran. Never force-push yourself.

### Approaching a problem

- **Find the root cause before fixing.** Several bugs here read fine locally and were
  only understood by tracing them through:
  - `localStorage` "remembering" the name while the per-launch random port made it a
    new origin every time
  - mpv's socket EOF cancelling the context, so a normal quit looked like a failure
  - a file picker that only failed on desktops without zenity or kdialog
- **Look at the actual environment before guessing.** Examples: the desktop
  (`XDG_CURRENT_DESKTOP`, `/etc/os-release`), how a browser is installed (deb, snap or
  flatpak changes what paths it can see), what the portal exposes (`busctl
  introspect`).
- **Prefer a standard API over hard-coding tools.** The Linux picker uses only the XDG
  desktop portal. Hard-coded zenity/kdialog were exactly what broke.
- **Prove a test catches the bug.** Reintroduce the bug, watch the test fail, restore
  the fix, watch it pass (done for `[R15b]`). An existing suite passing is not proof:
  e2e passed with the mpv-quit bug because every case ended nodes with a signal.
- **When a check fails, suspect the check first.** A stand-in browser "failed" because
  its `PATH` had no `sleep`, and a `grep -c` on a binary printed nothing. Fix the test,
  rerun, and only then judge the code.
- **Add a dependency only when the alternative isn't a few lines.** The GUI took
  exactly one, `godbus`, because a hand-written D-Bus client isn't small. It is pure
  Go, so cross-compiling is unaffected.

### Verifying without touching the user's machine

- **Real runs:** `./relay` on a high localhost port, real mpv with `--vo=null
  --ao=null`, and parties driven through the GUI's API with `curl` (see `gui/CLAUDE.md`
  for the token recipe). Two GUIs as host and guest cover sync, the feed, "has left"
  and Stop.
- **Isolation:** each test gets a `mktemp -d` folder.
  - `XDG_CONFIG_HOME=<tmp>` keeps the GUI off the real `~/.config/watchparty`, one
    folder per GUI so two instances don't share a store.
  - A `PATH` with only an `mpv` symlink and a no-op `xdg-open` stops any window
    opening. A stand-in `chromium` script can record its arguments and "close" by
    exiting. Use absolute paths inside stand-ins, because `PATH` is minimal.
- **Never pop UI on the user's screen** (a file dialog, a browser window). Prefer
  read-only checks, and leave the click-through to the user with a ready-built binary
  such as `/tmp/watchparty-gui-test`.
- **Clean up:** kill every started process (`pgrep -x watchparty-gui`, `relay`,
  leftover mpv), and delete temp folders and downloads by their **literal** path. The
  safety check refuses `rm -rf "$(…)"`.
- **Say what was not verified.** Windows-only behaviour compiles in CI but has never
  been run on Windows. Every summary and PR lists it.

### Design and communication

- **Visual taste is the user's call.** Draw options in a sandbox, render them side by
  side (with any reference, and a GIF if it moves), let the user pick, and apply only
  that. The workflow and its tools are in `gui/CLAUDE.md` under "Working on the look".
  Guessing cost many rounds on the mascot's face.
- **Change only what was asked.** "Fix the eyes" means the eyes, not a new face. A
  reverted change stays reverted: don't bring back a rejected layout.
- **Show, don't describe:** give screenshot paths for every visual change, taken at
  the real 560×940 window size.
- **Keep the theme neutral in docs and commits.** The forest palette, mushrooms,
  turtles, the cat mascot Fosdo2 and the medical touches are the product's own
  identity. Never tie them to a particular person.
- **Every package has its own `CLAUDE.md`** (`party/`, `mpv/`, `server/`, `relay/`,
  `gui/`). Detail goes there, and this file keeps the overview and the cross-cutting
  rules. Check a doc against the code when you change either.

## Known issues

Confirmed by reading the code, not yet fixed:

- [ ] **Playback drifts apart and nothing corrects it.** `time-pos` only syncs while
  paused, so different start latency and buffering pull the nodes apart over a long
  film, and they stay apart until someone pauses. Planned fix: a periodic position
  check that corrects only drift past ~2s.
- [ ] `/bye` and `/event` trust the `hit-me-up`/`counter` headers (P2P only). The counter check stops replays and unknown addresses, but a peer can still spoof another peer's address. Needs signing to go further, which is more than a LAN tool wants. Accepted: P2P is for friends and LANs.
- [ ] Relay identity is only as strong as the room secret: the `hostname` is whatever the joiner claims, so anyone who knows the secret can impersonate a member. Same trade as above.

## Reference

- **`mpv/MPV.md`**: the part of mpv we use, plus its observed behaviour on the wire
  (observe timing, seek echoes, quit and end-of-file sequences). Read it before
  touching `mpv/` or anything that depends on mpv's events.
- mpv JSON IPC: https://mpv.io/manual/master/#json-ipc
- mpv properties: https://mpv.io/manual/master/#properties
