# E2E.md

Notes for working on `e2e.py`. What each phase covers is in its docstring; this file
has the rules for writing and running cases. Needs `mpv`, `ffmpeg`, `dbus-daemon`
and Chrome or Chromium on PATH and a `-local`-reachable interface.

## Phases

The six phases run at once as copies of the script, `--port-base` + 20 apart, which works
because relay-mode nodes open no ports. The slowest phase sets the total, so a new slow case
goes in whichever phase is shortest. `caps` can't go below about 45s: `[R20]` waits out the
real 30s ping and 10s pong timeout.

- **`p2p`** nodes run with `-evictAfter 1ms` so strike cases don't wait; `[13b]` runs
  one on the 5-minute default.
- **`relay`**: every case ends nodes with a signal except `[R15b]`, which quits mpv.
  Keep it: the mpv-quit bug passed every other case.
- **`gui`**: each GUI gets its own HOME, XDG folders, TMPDIR and a `PATH` with only
  `mpv` and a failing `xdg-open`. Its one browser is a desktop entry, made the
  `mimeapps.list` default, for a wrapper that adds `--headless=new
  --remote-debugging-port=0`; `attach()` reads the port from the profile's
  `DevToolsActivePort`, so a GUI restarted by self-update is found too.
  - **Every GUI gets `DBUS_SESSION_BUS_ADDRESS`**, our private `dbus-daemon` or a dead
    path. Without it godbus finds `/run/user/<uid>/bus` by itself and Browse opens a
    real file chooser on the desktop. `gui/fakeportal` answers `OpenFile` there.
  - The update cases stamp `update.ReleaseURL` to `Release`, a local stand-in.
  - Drive a page with `js(n, expr)` and `click()`, read it with `feed()`/`status()`.
    `go()` waits for the new run, or a check reads the last party's status.
    `confirm()` is replaced on attach; `window.asked` holds the question.
  - Cleanup kills each GUI's process group: its browser, its mpv, a restarted GUI.
- URL cases share one HTTP server (`Quiet`). To slow one client only, use `late` and
  `--user-agent=late`; `delay` holds every client, which made `[R11d2]` flaky.
- Slow networks come from `lag(port, target, delay)`, a TCP proxy. P2P can't use it,
  since peers dial the address each node advertises.

## Writing a case

- **No fixed sleeps for things that happen.** Every wait is `until(cond, ceiling)`;
  `BOOT` and `SETTLE` are ceilings. Only a check that something did *not* happen sleeps
  (`QUIET`, after its positive half holds).
- **A step every later case needs is `must()`, not `ok()`**: a failure ends the phase
  with `STOP` instead of one cause showing up as ten FAILs. Logs are still kept.
- **`osd()` returns a list**, so `.count(x)` is an exact match, never a substring. Use
  the full string (`f"paused by {A}"`) or `pauses()`.
- The suite reads nodes through their log lines (root `CLAUDE.md` has the table).
- mpv emits no `property-change` for a no-op set, so counted events must alternate.
- **Wait on the node you act on or read from**, never on another node's view of it. A
  relay announces a join before the joiner has read its handshake.
- **The `until` covers everything the next `ok` reads.** Prefer `ok(name, until(cond))`.
- **`booted` doesn't mean connected**, and neither means the node's `joined` has landed.
  Before a step that a message on its way would undo, wait for `quiet(names)`.
- **Assert what the protocol guarantees.** Before checking an order or "every node", ask
  whether another result is allowed. Nodes hear things in different orders.
- **Slow only the client under test**, never a shared server.

## Flakes

A flake is a bug until shown otherwise; several were real ones. Reproduce it with an
injected delay before fixing it. No retries in CI, and raising a ceiling is not a fix.

`--jitter SEED` holds each direction of each relay connection, and each page's `/state`
and `/log` answers, for a random time. CI runs it on every PR with a new seed. A seed fixes
the delays, not the run: threads still race, so a failure may not repeat on replay.
It can't reorder what stays in order on one connection or happens inside one process, so
those orders need a Go test (`mpv/party_test.go`). It covers `relay`, `media` and `gui`; `p2p` peers dial each other directly.
A case that can't work under it says why and is left out by name; none needs to today.

## Windows (`e2e_windows.py`)

Covers only what Windows has; the behaviour is e2e.py's. It runs in CI's `windows` job,
natively (the runner is thrown away). Facts it rests on:

- **Override an environment variable by its upper-cased name** (`environ()`): Python
  keeps `os.environ` upper-cased, and a mixed-case key sits next to the real one.
- **A 64-bit process's `ProgramFiles` is rebuilt from `ProgramW6432`**, so the GUI's
  stand-in Chrome (`gui/fakebrowser` as `chrome.exe`) needs both overridden.
- Chocolatey's `mpv.exe` on PATH is a shim that starts the real one; killing a shim
  leaves mpv running, so the job puts the real `mpv.exe`'s folder first.
- Nodes run in their own process group, so `CTRL_BREAK_EVENT` reaches them, which Go
  reads as `os.Interrupt`. GUIs have no console: they end by `taskkill /T`.

## Running it

- **`make e2e` runs it in `e2e.Dockerfile`'s container**, so no case can reach this
  machine's desktop, session bus or display. CI runs the same image. It runs as a user
  with your uid (dbus-daemon refuses one missing from `/etc/passwd`), and the last run's
  logs, only logs, are copied to `.e2e/<phase>` even when it crashes.
  - Chromium gets `--no-sandbox` there (`E2E_NO_SANDBOX`): Docker's seccomp profile
    blocks the user namespaces its sandbox needs, and the setuid `chromium-sandbox`
    fails too. Loosening the container for it would trade its isolation for the browser's.
  - `./e2e.py` on the host still works, isolated only by each GUI's environment, with
    its run folder left in `/tmp`.
- **One `upnp` container at a time on a machine:** they share docker's bridge, so a node's SSDP
  search finds another's fake router and [G34] fails. Other phases can run side by side.
- `--audio` uses a real, muted audio output. It needs a sound server, so CI doesn't
  run it.
- The script points its stdin at `/dev/null` so no mpv puts the terminal in raw mode.
  If a terminal is ever left without echo, type `stty sane` blind.
- Its `finally` kills everything it started. `stub()` exits if its stub never says
  `ready`, so a leaked stub holding the port can't hang the next run.
