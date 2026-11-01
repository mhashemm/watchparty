# CLAUDE.md

Peer-to-peer mpv sync. This file is always loaded: the overview, how we work, and every
rule or contract that spans packages.

## Where knowledge lives

| What | Where |
|---|---|
| A package's rules and traps | `<pkg>/CLAUDE.md`, loaded when you work in that package |
| How mpv really behaves; why each mpv flag is pinned | `mpv/MPV.md`. Read it before touching anything that reacts to mpv's events |
| The GUI's look | `gui/DESIGN.md`. Read it before any visual change |
| Writing and running `e2e.py` cases | `E2E.md` |
| Why something is the way it is | the commit that did it: `git log -S'<identifier>'`, `git log -L`, `git log -- <file>`. Bodies hold the incident, the rejected alternatives and what was tested |

## Commands

```sh
make -j                                    # cli, gui and relay for this machine, into .bin/
make -j gui linux                          # components × OSes: any of cli gui relay, linux darwin windows
make -j test                               # unit tests (-race) and e2e.py together, ~45s
go test -race -count=30 -cpu=1,4,16 ./...  # stress a changed test before trusting it
go mod tidy                                # run it yourself; CI only does go mod download
make e2e E2E="--mode gui"                  # e2e.py in docker; python e2e_windows.py on Windows
CREATE_KEY=k ./relay                       # :8080, then -relay ws://host:8080 -room r -secret s [-createKey k]
```

CI: `pr.yml` builds every OS, runs `make -j test`, and Windows unit + `e2e_windows.py`;
a tag runs `publisher.yml` (10 files).

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
an event broadcasts it. `applied`/`seeks` drop mpv's echo of a peer's change, `seen`
its first reply to an observe, P2P counters stale requests. Don't bring back a `role`
echo hack: it muted the peer, not the echo.

**Playback is never synced, only paused.** Only `pause` and `time-pos` (while paused)
go over the wire. Anything that would put nodes out of step (a seek while playing,
running out of cache, a mid-party join) pauses the whole party, and someone resumes it
by hand. Every pause carries where it stopped. **Same file, or no party:** a joiner whose
file fingerprint differs is refused with 409.

## Contracts between packages

- **Log lines are an API.** Change one and update every reader in the same commit:

  | Printed by | Line | Read by |
  |---|---|---|
  | party | `your address to share is <invite>` | GUI invite box, `gui/store.go` (real room and secret), e2e |
  | party | `init done` | GUI "You're in", e2e |
  | party | `file fingerprint: <hex>` | e2e |
  | party/ws.go | `relay: connected`, `relay: dial: …` (`… is not open yet …` inside it), `relay: disconnected` | GUI reconnecting and "waiting for the host", e2e |
  | party/ws.go | `rejected by relay`, plus `not the same file` | GUI diagnosis |
  | mpv | `do: sent request: {"command":["show-text",<text>,…` | GUI feed, `gui/store.go`, e2e `osd()` |
  | mpv | `Watch: broadcasted event: <raw mpv JSON>` | GUI feed (your own pause/resume), e2e |
  | server | `skipped event from`, `refused <host>`, `does not exists`, `the party refused your secret` | e2e |

- **Notice texts** "X has joined", "connected to X", "X has left" are sent by both
  `server/` and `relay/`, parsed by `gui/store.go` and e2e's `known()`. Change all or none.
- **The relay envelope** `{"hostname", "event": <raw mpv JSON>}` is declared in both
  `relay/main.go` and `party/ws.go`: importing `party` would pull upnp and server into
  the relay image. Keep them in sync by hand.
- **`party.Run` runs many times in one GUI process:** no globals, no leftover goroutines.

## Concurrency invariants (each caused a real bug; the package docs name where)

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

- **A doc holds only what the code can't say:** a rule with one sentence of why, a trap
  that looks like a simplification, a fact about mpv, browsers or an OS, a contract, or
  a recipe to verify. Never what a function does, inventories, or history: the incident
  goes in the commit body, where the next session will look for it.
- **Each fact lives in one place**, cross-package here, the rest in its package's doc.
- **Update a doc only when a change makes something in it wrong.** Budget: this file
  ≤150 lines, every other doc ≤200. Trim before adding.

## How we work

### Git and releases

- **Never commit on `master`.** Branch first, and check `git status -sb` before
  committing: the user switches branches between requests.
- **Commit each change as soon as it is verified**, without asking, one logical change
  per commit. Subject: `area: what changed and why it matters`. Body: the reasoning,
  the alternatives rejected, what was tested and what wasn't. Push, open PRs and tag
  only when asked; a PR says what was tested and what wasn't.
- **Merge by rebase** (`gh pr merge N --rebase`; `gh pr update-branch N --rebase`);
  `master` requires linear history. Delete merged branches with `git branch -D`.
  Never force-push.
- **Release:** `git tag vX.Y.Z <sha> && git push origin vX.Y.Z` on the last merged
  commit. Verify by its files, not the Actions status: all 10 assets uploaded, the Linux
  GUI's `GET /update` reports the tag, its page loads in headless Chromium and a tab
  click works.

### Approach

- Find the root cause before fixing, and look at the actual environment
  (`XDG_CURRENT_DESKTOP`, how a browser is installed, `busctl introspect`) before
  guessing.
- Prefer a standard API over hard-coding tools (the XDG portal, not zenity). Add a
  dependency only when the alternative isn't a few lines.
- Prove a test catches the bug: reintroduce it, watch it fail, restore, watch it pass.
  When a check fails, suspect the check first.
- **Visual taste is the user's call.** Render options side by side and apply only the
  pick (`gui/DESIGN.md`). Screenshots at the real 560×940 window size.
- **Change only what was asked.** A reverted change stays reverted.
- Keep the product's theme (forest palette, mushrooms, turtles, the cat Fosdo2)
  neutral in docs and commits: never tie it to a person.

### Verifying without touching the user's machine

- `make e2e` runs in a container, Windows code only in CI's `windows` job; say what
  wasn't verified. By hand: `./relay` on a high port, mpv with `--vo=null --ao=null`, GUIs
  driven with `curl` (recipe in `gui/CLAUDE.md`).
- Isolate a run by hand in a `mktemp -d` folder: `XDG_*` and `HOME` inside it, `PATH` with
  only `mpv` and a no-op `xdg-open`, `DBUS_SESSION_BUS_ADDRESS` at a dead path (godbus
  finds the desktop's bus by itself). Never pop UI on the user's screen; leave
  click-throughs to the user with a built binary.
- Clean up by literal path and saved PID or `pkill -x`; **never `pkill -f`** (kills its shell).
