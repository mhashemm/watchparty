# gui/CLAUDE.md

The desktop app for people who don't want the CLI. A separate `package main` that opens a
native window (Gio) and calls `party.Run` in-process, so it needs only mpv. Its status display
is driven entirely by the log lines `party`, `mpv` and `server` already print. For any visual
change read **`DESIGN.md`** first.

Dependencies: Gio (`gioui.org`, `gioui.org/x`), `godbus` (the Linux pickers),
`golang.org/x/sys/windows`. Gio needs cgo on Linux and macOS, so the GUI builds natively
there; Windows needs none and cross-compiles. The CLI and relay stay `CGO_ENABLED=0`.

## Commands

```sh
make gui                                   # fetches gui/fonts/NotoColorEmoji.ttf (pinned, SHA-256 checked) first
go build -o watchparty-gui ./gui           # from the repo root, once the font is fetched; `go build ./gui` collides with the directory
go vet ./gui && GOOS=windows go vet ./gui  # the two build-tagged halves
EGL_PLATFORM=surfaceless go test ./gui -run TestRender -render <dir>   # every scene as <dir>/<name>.png, no window
go generate ./gui                          # icon.ico → rsrc_windows_amd64.syso (a test fails when they differ)
```

Linux builds need the headers Gio links against (Wayland, X11, xkbcommon-x11, EGL, GLES,
Vulkan); run the GUI's tests in a container that has them rather than installing them here.
At run time the window needs `libEGL`, `libGLESv2` and `libxkbcommon-x11`.

## Running it headless

Xvfb, an isolated `HOME`/`XDG_*`/`XDG_RUNTIME_DIR`, `DBUS_SESSION_BUS_ADDRESS` at a dead path,
mpv with `--vo=null --ao=null` in Settings' options. Drive it with `xdotool`: find the window
by its title (`xdotool search --name "Watch Party"`), not `--pid`, which Gio never sets.
`gui.json` holds the settings under `"settings"`. Read the party's progress from stderr (every
log line), or from `history.json` after it ends.

**`e2e.go`** (`-tags e2e`, `E2E.md`) is the control socket e2e drives the window by. Its
snapshot reads every label through the helper the layout draws it with (`goLabel`,
`versionText`, `aboutButton`, `historyButton`, `partyLine`, `since`, `shownDetail`), never
a copy of its own, or the suite checks the copy. It refuses a button the window hides or
disables: `Click` would pass a disabled one's `gtx.Disabled()`.

## The app (`main.go`)

- **`form.config()`** trims every field; an empty port is `party.DefaultPort`. It
  refuses an empty file, and an empty relay in relay mode, which `party.Run` would
  otherwise treat as direct mode and try UPNP. It does **not** check the file's type,
  so URLs work; only the picker filters.
- **The log:** `log.SetOutput(io.MultiWriter(g, os.Stderr))`, with `g` first because
  stderr can be an invalid handle under `-H=windowsgui` and MultiWriter stops at the
  first error. `Write` takes `g.mu`, so **never `log` while holding `g.mu`**.
- **`changed`** (the window's `Invalidate`) is called under `g.mu` on every line, message and
  end: it must never block.
- **`run`** goes up on every start; `news` asked with an old `run` answers from line 0, so two
  runs never splice.
- An empty mpv field becomes `mpv` on `PATH`, else the absolute path of a sibling `mpv(.exe)`
  or the installer's `mpv\mpv.exe` (`ownCopy`), so the installed mpv works even if its Path
  entry is missing. Absolute, because `exec.LookPath` won't resolve a bare name from the
  current directory.

## The window (`run.go`, `window.go` and a file per tab)

- **The view belongs to the window's goroutine.** It reads the app each frame through
  `news` (lines and chat since it last looked, and how the party stands) and feeds the lines
  to `status.go`'s `watch`. Anything that waits on the OS or the network (pickers, Save
  chart, the update check and install, About's tools) goes through `async`, whose result is
  applied in the next frame; a blocking call in a frame freezes the window.
- **It draws at 30 fps while something moves** (`moving`), and only on changes when the OS
  asks for reduced motion: on software rendering a frame costs ~75 ms of CPU. So anything that
  changes with time (a label going back, the trail's half second) must ask for its own frame
  with `op.InvalidateCmd{At: …}`; nothing else will come.
- **Closing the window ends everything:** `DestroyEvent` → the party's context cancelled,
  `Run` waited for, exit. A signal or a restart cancels the app's context, which closes the
  window the same way. No goroutine outlives `wg.Wait()`.
- **macOS:** `app.Main` must own the main thread, and the bundle must not set `LSUIElement`
  (it hides the window's Dock icon).
- **Title bar:** Linux asks for `Decorated(false)`, so the desktop draws the bar when it will
  (KDE, COSMIC, any X11 window manager) and `ConfigEvent`'s `Decorated` is false only when
  it won't (GNOME on Wayland); then `titlebar.go` draws the themed one. Never on Windows or
  macOS, where `Decorated(false)` removes the native frame.
- **`app.ID` is `watchparty`**, the desktop entry `install.sh` writes: Wayland's app id and
  X11's class, which the desktop matches to the entry's name and icon.
- **Windows:** `viewEvent` gives the window's handle the AppUserModelID `setup.nsi` puts on
  the shortcuts, plus relaunch properties naming `watchparty-gui.exe`; without it a pinned
  shortcut gets a second taskbar button. The icon is resource 1, which Gio loads. Windows
  keeps an app's own title bar light in dark mode unless asked: `darkFrame` sets
  `DWMWA_USE_IMMERSIVE_DARK_MODE` (20, or 19 before 20H1) whenever `dark` changes.
- **In-window dialogs** (`modal`): the first button has the focus when one opens, Escape
  cancels, and the backdrop takes every click under it.

## Drawing like the page (`theme.go`)

The look was matched to the old page row by row; these traps look like simplifications:

- **Gio blends in linear light:** text half covering a pixel comes out paler than a browser's.
  `ink` paints the glyphs, then a second coat at `inkAgain` (.6); a full coat overshoots.
- **Gio rounds font sizes to whole pixels:** `exact` lays text out 8× larger and scales it back.
- **Line boxes are CSS's:** `lineBox` puts the baseline where Chromium would (normal line
  height is the font's ascent plus descent, each rounded; buttons use normal), and keeps a
  fractional line height between lines. Long text stacks at fractional heights (`tipText`).
- **`Clickable` and `Enum` clip what they draw:** shadows and focus rings go outside them
  (`around`). Flex's Baseline alignment undercounts heights: `baselineRow`.
- **Every painted colour goes through `shade`** (`palette.go`), which swaps it for the
  dark palette's by RGB; `rrect`, `ink`, `colorOp` and `paintArt` already do. Paint a
  shadow with `shadow`, and a drawing with `asDrawn(gtx)`, or the palette repaints it. One
  RGB can't take two dark colours, so a new role needs its own colour.
- **Bundled fonts are renamed** ("wp Atkinson", "wp Gluten", "wp Emoji"), or a system Noto
  Color Emoji shadows the bundled one; system fonts stay on so any script renders.

## Saved settings and history (`store.go`)

- **Two files in `os.UserConfigDir()/watchparty/`,** `gui.json` and `history.json`, written
  atomically, 0600 in a 0700 directory: they hold the create key and room secrets. Saving needs
  `store.mu`; lock order is always `g.mu` → `store.mu`.
- **Settings:** name, and `local`/`port` from the last peer-to-peer party, are remembered automatically,
  the rest by the Settings tab's button. The first relay used becomes the default if none is set.
  The window's size is `gui.json`'s `window`, saved as it closes: beside the settings, not in
  them, so the Settings tab's copy can't save a stale one over it.
- **History**, up to 100 parties, filled in from log lines (`observe`): the *real* room and
  secret come from the invite line, not the form, which can leave them empty.
- **Continue** starts the form plus `continue`, the party's `started`. Same room and secret, so
  the same party: `begin` moves its entry to the end, open again, and `Run` starts at `stopped`,
  which the node chose (with keep-open=always, the file after one it finished). So the slice
  order is the truth: never sort it. Hidden (`continuable`) for
  peer-to-peer (a guest's saved address can change; Watch again only fills the form), a
  `finished` party, or 30 days after it ended, when the relay has forgotten the room.

## Self-update (`update.go`)

The download, SHA-256 check and binary swap live in the `update` package
(`update/CLAUDE.md`); this file only wires them to the window. What's GUI-specific:

- `version` is `"dev"` unless the release stamps `-X main.version=<tag>`. A `dev` build
  offers no update, so a local build can't overwrite itself.
- Update and Restart are refused during a party. `update.Install` swaps next to `g.exe`;
  `main` clears a leftover `g.exe + ".old"` (Windows) at startup.
- **Don't restart on install.** The window asks, Restart later first. **Restart now**
  starts `g.exe` (resolved at startup, since the path can point at the replaced file
  after the rename) and closes this one.

## OS helpers (`os_unix.go`, `os_windows.go`)

- **The video picker shows only videos** and `.torrent`, by extension from `pickExts`, built on
  `party.VideoExts` (Go's sniffer misses `.mov` and `.ts`), which also decides what a
  playlist holds. Add an extension when a real video doesn't show up. mpv's picker has no
  filter: mpv has no extension on Linux, and a video filter hides it.
- **Linux: only the XDG desktop portal** (`portalAsk`: `OpenFile`, and `SaveFile` for the
  chart), never a specific program; hard-coded zenity/kdialog broke on desktops without them.
  Subscribe to `Response` on the predicted request path *before* calling the method. The
  filter type is `a(sa(us))`, and a wrong type refuses the whole call
  (`TestVideoFilterIsWhatThePortalExpects`). A folder is `OpenFile` with `directory`
  (portal version 3 and up) and no filter. Any portal error asks the user to type the
  path. Check a desktop with `busctl --user introspect org.freedesktop.portal.Desktop
  /org/freedesktop/portal/desktop org.freedesktop.portal.FileChooser` (`.OpenFile
  ssa{sv}`).
- **Windows:** PowerShell `-STA` with a WinForms `OpenFileDialog` (`SaveFileDialog` for the
  chart), UTF-8 output, owned by an invisible form that is shown, activated, then made
  non-`TopMost`, which is what brings the dialog to the front without pinning it there.
  `hide()` uses only `CREATE_NO_WINDOW`; `HideWindow` can hide the dialog itself.
- **macOS:** `osascript choose file of type {"public.movie", "mkv", …}`, `choose file name`
  for the chart. A non-zero exit means cancelled on macOS and Windows.

## Log lines

`status.go`'s `watch` and `observe` (`store.go`) read only log lines (root `CLAUDE.md`), **right
after the 20-byte timestamp, never as a substring:** `refused <header>` and `upnp:` lines put a
stranger's text mid-line, and it can spell any of them. Chat and reactions are never a line, nor
on disk: `Heard` keeps them in memory (`news`' `Said`) until the party ends.
