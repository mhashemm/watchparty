# CLAUDE.md

Peer-to-peer mpv sync. This file is the overview and the cross-package rules. Each
package has its own `CLAUDE.md`, `E2E.md` covers `e2e.py`, and `mpv/MPV.md` covers
mpv's observed behaviour: read it before touching anything that depends on mpv's events.

## Commands

```sh
make -j                                    # cli, gui and relay for this machine, into .bin/
make -j gui linux                          # components × OSes: any of cli gui relay, linux darwin windows
make -j test                               # unit tests (-race) and e2e.py together, ~45s
go test -race ./...
go test ./mpv -run '^TestName$' -v
go test -race -count=30 -cpu=1,4,16 ./...  # stress a changed test before trusting it
go mod tidy                                # run it yourself; CI only does go mod download
docker build -f relay/Dockerfile -t watchparty-relay .   # from the repo root
./e2e.py [--mode p2p|relay|caps] [--file movie.mkv] [--nodes 3]
```

Through a relay by hand:

```sh
CREATE_KEY=k ./relay                       # :8080; omit CREATE_KEY to let anyone open rooms
./watchparty -relay ws://host:8080 -room test -secret s -createKey k -file movie.mkv   # host
./watchparty -relay ws://host:8080 -room test -secret s -file movie.mkv                # guests
```

CI: `pr.yml` builds every OS and runs `make -j test`. A tag push runs `publisher.yml`,
which uploads 10 files (CLI and GUI × linux/darwin amd64+arm64, windows amd64).

## Architecture

`party.Run` is one node from start to finish. The CLI (`main.go`, flags → `Config`)
and the GUI (`gui/`, form → `Config`, in-process) both call it. It wires:

- **A transport:** the P2P HTTP mesh (`server/`, LAN IP or UPNP), or a websocket
  relay (`-relay`, client `party/ws.go`, server `relay/`) for people behind carrier
  NAT. Both require the party's secret on every request.
- **`mpv/`:** the JSON IPC client over a unix socket or Windows named pipe.
- **Two channels:** `outgoing` (mpv → transport) and `incoming` (transport → mpv).
  Events travel as raw mpv JSON; no transport parses them.

**No master, no slave:** any peer can drive the player. Only the node that generates
an event broadcasts it, and three things stop loops: `applied`/`seeks` drop mpv's
echo of a change applied from a peer, `seen` drops mpv's first reply to an observe,
and P2P counters drop stale requests. Don't bring back a `role` echo hack.

**Playback is never synced, only paused.** Only `pause` and `time-pos` (while paused)
go over the wire. Anything that would put nodes out of step (a seek while playing,
running out of cache, a mid-party join) pauses the whole party, and someone resumes it
by hand. Every pause carries where it stopped. Details in `mpv/CLAUDE.md`.

**Same file, or no party:** a joiner whose file fingerprint differs is refused with
409 (`party/CLAUDE.md`).

## Contracts between packages

- **Log lines are an API.** The GUI (`gui/index.html`, `gui/store.go`) and `e2e.py`
  parse them; the table is in `gui/CLAUDE.md`. Change one and update every reader in
  the same commit.
- **Notice texts** ("X has joined", "connected to X") are sent by both `server/` and
  `relay/` and parsed by the GUI history. Change both or neither.
- **The relay envelope** is declared in both `relay/main.go` and `party/ws.go`, since
  importing `party` would bloat the relay image. Keep them in sync by hand.
- **`party.Run` runs many times in one GUI process:** no global state, no leftover
  goroutines. Flags live only in `main.go`.

## Concurrency invariants

Each has caused a real bug; the package docs say where.

- Never hold a lock across a blocking call that needs the same lock to finish.
- Set `applied`/`seeks` before the IPC call, or the echo can beat the marker.
- Exactly one relay write pump per party, spanning reconnects.
- Synthetic notices use a non-blocking send (`Server.notify`).
- `Shutdown` uses `context.WithoutCancel`; the party context is already cancelled.
- Every per-party goroutine selects on the party context. Channels are never closed.
- Never `panic`, `log.Fatal` or `os.Exit` outside `main.go`: they skip `Run`'s defers
  and kill the GUI.
- `defer cmd.Cancel()` only after `cmd.Start()` succeeds.
- `bytes.Clone` anything from `scanner.Bytes()` before it goes on a channel.

## Conventions

- Almost no code comments. Reasoning goes in the commit message.
- Every Go test calls `t.Parallel()` and finishes in milliseconds. A delay the code
  owns is a struct field the test shortens, never a package `var`. Prove an absence
  with an ordering (a marker sent after), not a timeout. The only serial test is
  `TestLimiterDoesNotAllocatePerIP` (`AllocsPerRun` panics in parallel).
- A behaviour change lands with its tests and its `e2e.py` case in the same commit.

### Docs

- **Update a doc only when a change makes something in it wrong.** Reasoning and
  history go in the commit message.
- **A rule gets at most one sentence of why;** the incident behind it goes in `git log`.
- **Size budget:** this file ≤150 lines, each package doc ≤250. Trim before adding.
- No inventories of functions or tests; the names are in the code. Name a test only
  where it proves a specific point.

## How we work

### Git and releases

- **Never commit on `master`.** Branch first, and check `git status -sb` before
  committing: the user switches branches between requests.
- **Commit each change as soon as it is verified**, without asking, one logical change
  per commit. Subject: `area: what changed and why it matters`. Body: reasoning, what
  was tested, what wasn't.
- **Push, open PRs and tag releases only when asked.** A PR says what was tested and
  what wasn't.
- **Merge by rebase** (`gh pr merge N --rebase`; `gh pr update-branch N --rebase`);
  `master` requires linear history. Delete merged branches with `git branch -D`.
  Never force-push.
- **Release:** `git tag vX.Y.Z <sha> && git push origin vX.Y.Z` on the last merged
  commit. Verify by its files, not the Actions status: all 10 assets uploaded, the
  Linux GUI's `GET /update` reports the tag, and its page loads in headless Chromium
  with a working tab click.

### Approaching a problem

- Find the root cause before fixing, and look at the actual environment
  (`XDG_CURRENT_DESKTOP`, how a browser is installed, `busctl introspect`) before
  guessing.
- Prefer a standard API over hard-coding tools (the XDG portal, not zenity).
- Prove a test catches the bug: reintroduce it, watch it fail, restore, watch it pass.
- When a check fails, suspect the check first.
- Add a dependency only when the alternative isn't a few lines.

### Verifying without touching the user's machine

- Real runs: `./relay` on a high localhost port, mpv with `--vo=null --ao=null`, GUIs
  driven with `curl` (recipe in `gui/CLAUDE.md`).
- Isolate each run in a `mktemp -d` folder: `XDG_CONFIG_HOME`, `XDG_DATA_HOME` and
  `XDG_DATA_DIRS` pointed into it, and a `PATH` with only `mpv` and a no-op
  `xdg-open`, so no window opens. Never pop UI on the user's screen; leave
  click-throughs to the user with a built binary.
- Clean up every process and temp folder, by literal path. **Never `pkill -f`:** it
  kills its own shell. Use a saved PID, or `pkill -x` for names of ≤15 characters.
- Say what wasn't verified. Windows-only code compiles in CI but never runs there.

### Design and communication

- **Visual taste is the user's call.** Render options side by side and apply only the
  pick (`gui/CLAUDE.md`, "Working on the look"). Screenshots at the real 560×940 window size.
- **Change only what was asked.** A reverted change stays reverted.
- Keep the product's theme (forest palette, mushrooms, turtles, the cat Fosdo2)
  neutral in docs and commits: never tie it to a person.
