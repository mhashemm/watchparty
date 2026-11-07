# gui/CLAUDE.md

The desktop app for people who don't want the CLI. A separate `package main` that
calls `party.Run` in-process, so it needs only mpv. Its status display is driven
entirely by the log lines `party`, `mpv` and `server` already print. For any visual
change read **`DESIGN.md`** first.

Security: the `Host` + `X-Token` guard on every route but `GET /` is the whole defense
against a web page or another local app driving the GUI (which launches mpv). Cross-cutting
mindset, trust boundary and method: `SECURITY.md` (repo root).

Dependencies: stdlib, `golang.org/x/sys/windows`, and `godbus` (pure Go, for the Linux
file picker). No cgo, so it cross-compiles in the release matrix.

## Commands

```sh
go build -o watchparty-gui ./gui                         # from the repo root; plain `go build ./gui` collides with the directory
go test ./gui                                            # TestPageScriptParses needs node, or it skips
go vet ./gui && GOOS=windows go vet ./gui                # the two build-tagged halves
GOOS=windows go build -ldflags=-H=windowsgui -o /dev/null ./gui
```

## Running it headless

`openWindow` launches a Chromium-family browser from its desktop entry, or `xdg-open`.
To keep it off your screen, point `XDG_DATA_HOME` and `XDG_DATA_DIRS` at an empty
folder and use a `PATH` with only an `mpv` symlink and a no-op `xdg-open`. `PATH`
alone isn't enough, since entries name binaries absolutely. The Flatpak and snap
folders under `/var/lib` are scanned regardless, so a system-wide one still opens.

Drive a party with the token from the page:

```sh
T=$(curl -s $U/ | grep -o 'const TOKEN = "[^"]*"' | cut -d'"' -f2)
curl -X POST -H "X-Token: $T" $U/start -d '{"mode":"relay","file":"/abs/video.mkv","relay":"ws://127.0.0.1:18080","mpvFlags":"--vo=null --ao=null"}'
```

- **Isolate the store:** `XDG_CONFIG_HOME=<tmp>/cN`, one folder per GUI. Restarting on
  the same folder proves persistence, since the port changes every launch.
- **Host and guest:** start the host with an empty room and secret, read them from
  `/log` (`grep -o 'room [A-Z0-9]* -secret [A-Z0-9]*'`), start the guest with them.
- **Drive mpv directly:** each party's socket is its mpv's `--input-ipc-server=`
  argument (`pgrep -a mpv`). Write `set_property`/`quit` commands to it.
- **`make e2e E2E="--mode gui"`** automates the window lifecycle (a real window on Xvfb
  too), a browser handing the page off, and self-update (`E2E.md`). The recipe left
  here is for what it doesn't run.
- **A released binary:** `gh release download <tag> -p watchparty-gui_amd64`, run it
  headless, and `GET /update` must report that tag as `current`.

## Server (`main.go`)

- Listens on `127.0.0.1:0`. **The guard is load-bearing, because `/start` launches mpv
  with user-supplied flags:** `r.Host` must equal the exact `ip:port` (DNS rebinding),
  and every path except `GET /` needs `X-Token`, the `rand.Text()` token templated into
  the page as `{{TOKEN}}` and compared in constant time (CSRF).
- **`form.config()`** trims every field; an empty port is `party.DefaultPort`. It
  refuses an empty file, and an empty relay in relay mode, which `party.Run` would
  otherwise treat as direct mode and try UPNP. It does **not** check the file's type,
  so URLs work; only the picker filters.
- **The log:** `log.SetOutput(io.MultiWriter(g, os.Stderr))`, with `g` first because
  stderr can be an invalid handle under `-H=windowsgui` and MultiWriter stops at the
  first error. `Write` takes `g.mu`, so **never `log` while holding `g.mu`**.
- **`run`** goes up on every `/start`; a poll with an old `run` is answered from line 0,
  so two runs never splice.
- An empty mpv field becomes the absolute path of a sibling `mpv(.exe)`, else `mpv` on
  `PATH`. Absolute, because `exec.LookPath` won't resolve a bare name from the current
  directory. mpv is never bundled.

## Saved settings and history (`store.go`)

- **Two files in `os.UserConfigDir()/watchparty/`,** `gui.json` and `history.json`, written
  atomically, 0600 in a 0700 directory: they hold the create key and room secrets. Saving needs
  `store.mu`; lock order is always `g.mu` → `store.mu`.
- **Versions before `history.json` kept the history in `gui.json`.** `openStore` moves it, writing
  the new file first, so stopping in between loses nothing; `history.json` wins if both exist.
- **Not `localStorage`:** the random port makes the page a new origin every launch; it loads everything from `GET /state`.
- **Settings:** name, and `local`/`port` from the last peer-to-peer party, are remembered automatically,
  the rest by the Settings page's button. The first relay used becomes the default if none is set.
- **History**, up to 100 parties, filled in from log lines (`observe`): the *real* room and
  secret come from the invite line, not the form, which can leave them empty.
- **Continue** posts the form plus `continue`, the party's `started`. Same room and secret, so
  the same party: `begin` moves its entry to the end, open again, and `Run` starts at `stopped`,
  which the node chose (with keep-open=always, the file after one it finished). So the slice
  order is the truth; `mergeHistory` sorts only when it merges. Hidden (`continuable`) for
  peer-to-peer (a guest's saved address can change; Watch again only fills the form), a
  `finished` party, or 30 days after it ended, when the relay has forgotten the room.

## Self-update (`update.go`)

The download, SHA-256 check and binary swap live in the `update` package
(`update/CLAUDE.md`); this file only wires them to the page. What's GUI-specific:

- `version` is `"dev"` unless the release stamps `-X main.version=<tag>`. A `dev` build
  offers no update, so a local build can't overwrite itself.
- Update and Restart are refused during a party. `update.Install` swaps next to `g.exe`;
  `main` clears a leftover `g.exe + ".old"` (Windows) at startup.
- **Don't restart on install.** The page asks, Restart later first. **Restart now**
  starts `g.exe` (resolved at startup, since the path can point at the replaced file
  after the rename), and the page calls `window.close()`: killing a Flatpak browser's
  process alone leaves its window open.

## Lifetime and stopping

- **Stop** cancels the party's context, like Ctrl-C in the CLI, on every OS. On
  SIGINT/SIGTERM `main` cancels the party and waits for `Run`, so no UPNP mapping leaks.
- **Closing the window shuts everything down,** the fallback tab too. The page holds
  `GET /alive` open, an event stream read with `fetch` so it carries `X-Token`, with a
  heartbeat every 15s. When none is connected `closeGrace` (3s) after the last one
  dropped, `main` logs `gui: window closed` and cancels everything. The browser's process
  can't say: macOS keeps it running without windows, and Edge's startup boost hands the
  window to another process. The page reconnects the moment its stream is cut, never on a
  timer (a hidden window's run once a minute), so a browser cutting a long request costs
  nothing; it stops when the request itself fails, the GUI being gone.
- **The browser runs as its own instance** with a fresh temporary profile per launch; a
  shared profile would hand a second GUI's window to the first. On exit the GUI kills it
  and deletes the profile until it stays deleted: Chrome's network service still writes
  into it just after the main process is gone.
- **Idle exit**, for a page that never connects: no party and no `/log` poll for 2
  minutes. Don't shorten it: Chrome throttles hidden windows' timers to about one per
  minute. The first poll gets a minute's grace.

## OS helpers (`os_unix.go`, `os_windows.go`)

- **The video picker shows only videos** and `.torrent`, by extension from `pickExts`, built on
  `party.VideoExts` (Go's sniffer misses `.mov` and `.ts`), which also decides what a
  playlist holds. Add an extension when a real video doesn't show up. mpv's picker has no
  filter: mpv has no extension on Linux, and a video filter hides it.
- **Linux: only the XDG desktop portal** (`portalPick`), never a specific program;
  hard-coded zenity/kdialog broke on desktops without them. Subscribe to `Response` on
  the predicted request path *before* calling `FileChooser.OpenFile`. The filter type
  is `a(sa(us))`, and a wrong type refuses the whole call
  (`TestVideoFilterIsWhatThePortalExpects`). A folder is `OpenFile` with `directory`
  (portal version 3 and up) and no filter. Any portal error asks the user to type the
  path. Check a desktop with `busctl --user introspect org.freedesktop.portal.Desktop
  /org/freedesktop/portal/desktop org.freedesktop.portal.FileChooser` (`.OpenFile
  ssa{sv}`).
- **Windows:** PowerShell `-STA` with a WinForms `OpenFileDialog`, UTF-8 output, owned
  by an invisible form that is shown, activated, then made non-`TopMost`, which is
  what brings the dialog to the front without pinning it there. `hide()` uses only
  `CREATE_NO_WINDOW`; `HideWindow` can hide the dialog itself.
- **macOS:** `osascript choose file of type {"public.movie", "mkv", …}`. A non-zero
  exit means cancelled on macOS and Windows.
- **Window:** each browser gets `--app=URL --window-size=560,940 --user-data-dir=<temp>
  --no-first-run --no-default-browser-check`; then the default browser as a tab.
  - Its environment sets the `GOOGLE_*` keys to `no`: a build with none (Void's) otherwise
    shows a "keys are missing" bar, and this window uses no Google service. A distro's wrapper
    that exports its own keys overrides them, which is fine.
  - The page blocks DevTools' shortcuts, F5 and Ctrl/Cmd+R, and the right-click menu, except in a field, where
    right-click paste matters. That is only a deterrent: Chromium has no switch for it, and
    the policy that does it is system-wide.
  - The profile is seeded to turn off the password manager. Edge also gets
    `--disable-features=msImplicitSignin` (a name from its binary, which may change),
    or it signs every new profile into the Windows account.
  - **Only Chromium-family browsers**, since only they have `--app`. Firefox only
    appears as the fallback tab, whose closing can't be detected.
  - **Linux finds browsers through desktop entries**, as `xdg-open` does, because a
    Flatpak browser is never on `PATH`. It scans `applications/` under
    `$XDG_DATA_HOME` and `$XDG_DATA_DIRS` (spec defaults when unset, plus the Flatpak
    and snap export folders), keeps `x-scheme-handler/https` entries that aren't
    `Hidden` and match `chrom|edge|brave|vivaldi`, and puts the `mimeapps.list` default
    first. The command is the entry's `Exec`, minus field codes and Flatpak's `@@`.
  - **A sandboxed browser gets its profile where it can see it:** `X-Flatpak=<id>` →
    `~/.var/app/<id>/cache`, `X-SnapInstanceName` → `~/snap/<name>/common`, else
    `os.TempDir()`. Not run against a snap or a system-wide Flatpak.
  - Windows tries Chrome, then Edge, by absolute path; macOS Chrome, Edge, Brave, Chromium,
    Vivaldi, in `/Applications` then `~/Applications`, the binary inside the `.app`.
  - If everything fails, the log says `gui: no browser could be opened` with the URL.

## Page script (`index.html`)

- **One syntax error kills every button** while the server still answers.
  `TestPageScriptParses` parses the script with Node's `vm.Script`.
- `[hidden] { display: none !important; }` is required, or `.grid`'s `display:grid`
  shows hidden fields.
- `render()` writes the button text to `#go-label`, not `#go`, which holds the paw
  icon.
- **Save chart** re-fetches `/log?from=0`, since the `<pre>` is trimmed to 150k
  characters. **Copy invite** uses `navigator.clipboard` (127.0.0.1 is a secure
  context) and selects the text if that fails.
- `-createKey` is never in an invite, so the paste box never fills it.

## Log lines

`handle()` in `index.html` and `store.go`'s `observe` react only to log lines; the table
of which lines, and who prints them, is in the root `CLAUDE.md`.
**Both read them only right after the 20-byte timestamp, never as a substring:** `refused
<header>` and `upnp:` lines put a stranger's text mid-line, and it can spell any of them.

## On Windows

CI's `windows` job runs `e2e_windows.py`: Chrome from Program Files (headless), a party
from the page under `-H=windowsgui`, closing the window, a browser handing the page off,
and the `.old` swap. Never run: mpv's video window under that parent, a Windows–Linux party.
