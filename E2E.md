# E2E.md

Notes for working on `e2e.py`. What each phase covers is in its docstring; this file
has the rules for writing and running cases. Needs `mpv` and `ffmpeg` on PATH and a
`-local`-reachable interface.

## Phases

The three phases run at once as three copies of the script, on `--port-base` + 0, 20
and 40, which works because relay-mode nodes open no ports. The slowest phase sets the
total, so a new slow case goes in whichever phase is shortest. `caps` is the longest
today: `[R20]` waits out the real 30s ping and 10s pong timeout.

- **`p2p`** nodes run with `-evictAfter 1ms` so strike cases don't wait; `[13b]` runs
  one on the 5-minute default.
- **`relay`**: every case ends nodes with a signal except `[R15b]`, which quits mpv.
  Keep it: the mpv-quit bug passed every other case.
- URL cases share one HTTP server (`Quiet`). To slow one client only, use `late` and
  `--user-agent=late`; `delay` holds every client, which made `[R11d2]` flaky.
- Slow networks come from `lag(port, target, delay)`, a TCP proxy. P2P can't use it,
  since peers dial the address each node advertises.

## Writing a case

- **No fixed sleeps for things that happen.** Every wait is `until(cond, ceiling)`;
  `BOOT` and `SETTLE` are ceilings. Only a check that something did *not* happen sleeps
  (`QUIET`, after its positive half holds).
- **`osd()` returns a list**, so `.count(x)` is an exact match, never a substring. Use
  the full string (`f"paused by {A}"`) or `pauses()`.
- The suite reads nodes through their log lines (root `CLAUDE.md` has the table).
- mpv emits no `property-change` for a no-op set, so counted events must alternate.

## Running it

- `--audio` uses a real, muted audio output. It needs a sound server, so CI doesn't
  run it.
- The script points its stdin at `/dev/null` so no mpv puts the terminal in raw mode.
  If a terminal is ever left without echo, type `stty sane` blind.
- Its `finally` kills everything it started. `stub()` exits if its stub never says
  `ready`, so a leaked stub holding the port can't hang the next run.
