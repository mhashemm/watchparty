# gui/CLAUDE.md

This is the desktop app for people who don't want the CLI. Its visual identity is part
of the product, so keep it when you change things:

- a forest-floor palette of greens and browns
- mushrooms and a family of turtles along the bottom
- a fluffy tabby mascot named **Fosdo2**
- light medical touches in the wording (Clinic, Vitals, Patient chart, Diagnosis)

## Commands

```sh
go build -o watchparty-gui ./gui                         # from the repo root; `-o` is required, see below
go test ./gui
go vet ./gui && GOOS=windows go vet ./gui                # the two build-tagged halves
GOOS=windows go build -ldflags=-H=windowsgui -o /dev/null ./gui
./watchparty-gui                                         # needs only mpv, on PATH or next to it
```

Plain `go build ./gui` from the repo root fails: the output name `gui` collides with
the directory.

### Looking at the UI without opening a window on your desktop

`openWindow` launches Chrome/Chromium, or `xdg-open` as a fallback. To keep it
headless, run the GUI with a `PATH` that holds only an `mpv` symlink and a no-op
`xdg-open` script. Then screenshot the page:

```sh
chromium --headless --disable-gpu --hide-scrollbars --force-prefers-reduced-motion \
  --window-size=560,940 --virtual-time-budget=4000 --screenshot=out.png http://127.0.0.1:<port>/
```

- **`--force-prefers-reduced-motion`** freezes the turtles and Fosdo2 at their static
  pose. Without it, a screenshot taken at t≈0 has the turtles offscreen at
  `translateX(-90px)`.
- **`--virtual-time-budget` does not advance CSS animation time**, so waiting longer
  never shows a later frame. To capture a specific moment, inject `* {
  animation-play-state: paused !important; animation-delay: -<seconds>s !important; }`
  into a copy of the page (strip the `<script>` and set `<body data-mood="awake">` so
  polling doesn't reset the mood). For example, -13.6s is mid-lick. A GIF of one cycle
  is just those frames joined by `ffmpeg`.
- **`--force-device-scale-factor=2`** or 3 with a short window gives a close-up of
  Fosdo2.
- **560×940** is the size of the real app window. Check every visual change at that
  size, and at a wide browser tab (1280×800) for the fallback path.

To drive a party without clicking, read the token from the page and POST to `/start`:

```sh
T=$(curl -s $U/ | grep -o 'const TOKEN = "[^"]*"' | cut -d'"' -f2)
curl -X POST -H "X-Token: $T" $U/start -d '{"mode":"relay","file":"/abs/movie.mkv","relay":"ws://127.0.0.1:18080","mpvFlags":"--vo=null --ao=null","cooldown":"2"}'
```

More recipes that have been used and work:

- **Keep tests off the real saved file:** start each GUI with `XDG_CONFIG_HOME=<tmp>/cN`
  (one folder per GUI). `GET /state` and the file itself then show exactly what was
  remembered. Restarting the GUI on the same folder proves persistence, because the
  port changes on every launch.
- **Host and guest:** start the host with an empty room and secret, read the generated
  ones out of `/log` (`grep -o 'room [A-Z0-9]* -secret [A-Z0-9]*'`), and start the
  guest with them. The guest's `/log` then shows the host's "has joined", "paused by"
  and "has left" notices.
- **Drive mpv directly:** each party's IPC socket is the `--input-ipc-server=` argument
  of its mpv (`pgrep -a mpv`).
  Writing `{"command":["set_property","pause",false]}` or `{"command":["quit"]}` to
  it simulates the user, and `get_property` checks the other side followed.
- **The window lifecycle:** put a stand-in `chromium` script first on `PATH` that
  writes `"$@"` to a file and runs `/bin/sleep N` (absolute, since `PATH` is
  minimal). Its exit is "closing the window". An instant `exit 0` checks the hand-off
  guard.
- **The self-update:** build with `-ldflags "-X main.version=v0.0.1"`, start it in a
  temp folder, and `POST /update`. Then check the file's SHA-256 against the release's
  `digest`, and that the old process exited while a new one runs.
- **A released binary:** `gh release download <tag> -p watchparty-gui_amd64`, run it
  the same headless way, and `GET /update` must report that tag as `current`.

## Architecture

A separate `package main`: stdlib, `golang.org/x/sys/windows` (already in `go.mod`),
`github.com/godbus/dbus/v5` (pure Go, for the Linux file picker) and the repo's own
`party` package. No cgo, so it cross-compiles in the same
`publisher.yml` matrix as the CLI. **It calls `party.Run` in-process**, the exact
function the CLI's `main.go` calls, so it does everything on its own and needs only
mpv. The `watchparty` binary is not required. mpv stays the player. The GUI's status
display is driven entirely by the `log` lines `party`, `mpv` and `server` already
print.

| File | Role |
|---|---|
| `main.go` | HTTP server, guard, party lifecycle, log capture, `form.config()`, the `/state`, `/settings` and `/history/clear` handlers |
| `store.go` | the saved settings and party history (`gui.json`) |
| `update.go` | the self-update: version, GitHub release check, verified download and swap |
| `index.html` | the entire UI: CSS, inline SVG art, vanilla JS. `//go:embed`ed |
| `os_unix.go` (`!windows`) | browser list, `openURL`, `pickFile` (the XDG portal on Linux, osascript on macOS), no-op `hide` |
| `os_windows.go` | the same with Edge/Chrome paths, a PowerShell picker and `CREATE_NO_WINDOW` |
| `main_test.go` | `form.config()` per mode plus its errors, and the host/token guard table |
| `store_test.go` | a party recorded and reloaded (invite → room/secret, people from notices, dedupe, "has left" ignored), the history cap, `notice()` |
| `os_unix_test.go` | `fileURIPath`: plain, percent-encoded and non-ASCII portal URIs, plus rejecting a non-`file` URI |

### Server

- Listens on `127.0.0.1:0`. `g.host` is the exact `ip:port`, and the guard compares
  against it.
- **Guard (`ServeHTTP`), load-bearing because `/start` launches mpv with
  user-supplied flags:**
  - `r.Host` must equal `g.host`, which blocks DNS rebinding.
  - Every path except `/` needs `X-Token` equal to the `rand.Text()` token, which
    blocks CSRF. The token is templated into the page by replacing `{{TOKEN}}`, and
    the comparison is constant-time.
  - `GET /` has no token, because it is how the page gets the token. The `Host` check
    is what stops another origin from reading it.
- Routes use Go 1.22 method patterns:

  | Route | Does |
  |---|---|
  | `GET /{$}` | serves the page |
  | `POST /pick` | runs the OS file dialog and returns the path as plain text. Empty means cancelled |
  | `POST /start` | JSON `form` → `party.Config`, then `party.Run` in a goroutine. 400 on a bad form, 409 if a party is running |
  | `POST /stop` | cancels the party's context |
  | `GET /log?from=N&run=R` | `{run, from, lines, running, exitErr}` |
  | `GET /state` | `{settings, history, path, version}` from the store |
  | `POST /settings` | saves the Settings page's fields (relay, mpv, mpvFlags, cooldown, createKey, pport) |
  | `POST /history/clear` | empties the history |
  | `GET /update` | `{current, latest, available}`: asks GitHub for the latest release |
  | `POST /update` | installs the latest release, without restarting (see "Self-update") |
  | `POST /restart` | starts the installed version, closes this window and quits |

- **`form.config()`** trims every field.
  - Empty numbers fall back to `party.DefaultPort` / `party.DefaultCooldown`, the
    same constants the CLI's flag defaults use. A non-number is a 400.
  - It refuses an empty movie, and an empty relay address in relay mode. Without
    that check, `party.Run` would treat an empty `Relay` as direct mode and try UPNP.
  - Relay mode ignores the direct fields and vice versa.
- **The log:** `main` calls `log.SetOutput(io.MultiWriter(g, os.Stderr))`, and
  `gui.Write` appends each line to `g.lines`.
  - `g` comes **first** in the MultiWriter: in the `-H=windowsgui` build, stderr can
    be an invalid handle, and MultiWriter stops at the first writer that errors.
  - `Write` takes `g.mu`, so **nothing may call `log` while holding `g.mu`**, or it
    deadlocks.
- **`run`** goes up on every `/start`. A poll carrying an old `run` is answered from
  line 0, so a poll that was in flight during a restart can't splice two runs
  together. The log is uncapped in memory and reset per run.
- **`party.Run`'s return value** becomes `exitErr`. It is `nil` after `/stop` or when
  the user closes mpv (see "Return values" in `party/CLAUDE.md` for why that needs
  care), `errStopped` when something inside cancelled the party (relay
  rejection, port already in use), and a setup error otherwise (mpv not found,
  UPNP failed, no network). The page maps "mpv … not found" to its own diagnosis.

### Saved settings and history (`store.go`)

- **One JSON file, `os.UserConfigDir()/watchparty/gui.json`**: `~/.config/...` on
  Linux, `%AppData%\...` on Windows, `~/Library/Application Support/...` on macOS. The
  Settings page shows the exact path.
  - It is written atomically (temp file + rename) on every change, with mode 0600 in
    a 0700 directory. It holds room secrets and the create key.
  - `save()` must be called with `store.mu` held. Lock order is always `g.mu` →
    `store.mu`, never the reverse: `gui.Write` and `/start` hold `g.mu` while they
    record into the store.
- **Why not `localStorage`:** the server listens on a random port each launch, which
  makes the page a different origin every time. Browser storage was silently empty on
  every start, so "remember my name" never worked until it moved server-side.
- **Settings** (`settings`): the name, `local` and `port` are remembered from the last
  party, the latter two only from a Wi-Fi party. Everything on the Settings page
  (default relay, mpv, mpvFlags, cooldown, createKey, pport) is saved by its Save
  button. The first relay you use becomes the default relay if none is set.
- **History** (`session`), up to `maxHistory` (100) parties, oldest dropped:
  - `begin` records the movie, mode, relay/room/secret or friend's address, and the
    start time when `/start` succeeds. `end` stamps the end time.
  - `observe` runs on **every log line during a party** (from `gui.Write`):
    - `your address to share is …` fills in `invite` and the *real* room and secret.
      The relay generates them when the fields were empty, so the form values alone
      would be blank.
    - on-screen notices (parsed from `do: sent request:` by `notice()`) add people:
      "X has joined" and "connected to X". "has left", "paused by" and the rest are
      ignored, and each name is listed once.
  - "Watch again" refills the Party form from an entry: the movie, plus the relay,
    room and secret or the friend's address.

### Self-update (`update.go`)

- **`version`** is `"dev"` unless the release workflow stamps it with `-ldflags "-X
  main.version=<tag>"`. A `dev` build never offers or installs updates, so a local
  build can't overwrite itself with a release. Releases up to v0.0.11 carry no
  version and report `dev`, so they have to be replaced by hand once.
- **Check:** `GET` `releases/latest` from the GitHub API (15s timeout). `newer`
  compares dotted numbers after an optional `v`.
- **Install** (`POST /update`, refused while a party runs):
  1. Pick this platform's asset by `assetName(GOOS, GOARCH)`, matching the release
     matrix's names.
  2. Download it next to the running binary (same filesystem, so the swap is a
     rename).
  3. **Refuse it unless its SHA-256 matches the `digest` GitHub publishes for the
     asset.** No digest means no install.
  4. Swap it in with `replaceExe`:
     - Linux and macOS: an atomic rename over the running file. The running process
       keeps its old inode.
     - Windows: a running `.exe` can't be overwritten, so it is renamed to `.old` and
       the new one put in its place (with a rollback if that fails). Every start
       deletes a leftover `.old`.
  5. **Don't restart.** Record the tag in `g.installed` and answer. The page then
     opens a modal `<dialog>`: "Update installed. A restart is required" with
     **Restart later** (focused first, and Esc does the same) and **Restart now**.
     - Later does nothing. About shows "vX is installed" plus a Restart now button,
       and the next normal start runs the new file anyway.
     - **Restart now** (`POST /restart`, refused during a party) starts the new
       binary, kills this instance's browser process so its window closes
       (`g.closeWindow`), and quits.
     - The restart uses `g.exe`, resolved once at startup by `executable()`. After
       the rename, the running process's own path lookup can point at the replaced
       file.
     - Restarting without asking left two windows open, which is why it asks.

### Finding mpv

An empty mpv field becomes the absolute path of a sibling `mpv(.exe)`, and falls back
to plain `mpv` on `PATH`. The path must be absolute: since Go 1.19, `exec.LookPath`
refuses to resolve a bare `mpv` from the current directory, so on Windows "drop
mpv.exe in the same folder" would not work otherwise. mpv is never bundled. Users
install it themselves.

### Lifetime and stopping

- **Stop** cancels the party's context, exactly like Ctrl-C in the CLI. mpv is killed
  (`exec.CommandContext`), and `Run`'s defers send `/bye`, shut the listener and
  delete the UPNP mapping. The other side gets "X has left". This is the same on
  every OS: no signals, no taskkill.
- **The GUI's own context** is a `signal.NotifyContext`. Each party's context derives
  from it. On SIGINT/SIGTERM, `main` cancels the running party and `wg.Wait()`s for
  `Run` to return before exiting, so Ctrl-C on the GUI never leaks a UPNP mapping.
- **Closing the window shuts everything down.** `openWindow` launches the browser as
  **its own instance**, with a fresh temporary profile (`--user-data-dir` under
  `os.TempDir()`), so the browser process the GUI started lives exactly as long as
  the window. When it exits, `main` logs `gui: window closed` and cancels the GUI's
  context. That ends any running party gracefully (`/bye`, mpv closed, UPNP mapping
  deleted), and the temporary profile is deleted.
  - **A fresh profile per launch, not a shared one.** With a shared profile, a second
    GUI's browser would hand its window to the first instance and exit at once.
  - **The hand-off guard:** a browser that exits within `handoffWindow` (5s) is
    assumed to have passed the page to another, already-running instance, not to have
    been closed. Then the window can't be watched, and the idle exit below applies.
    Edge's background "startup boost" on Windows is the expected case. Without the
    guard, the app would quit the instant it opened.
- **Idle exit**, the fallback when the window can't be watched (the default browser
  opened a tab, or a hand-off): a ticker cancels the context once **no party is
  running and no `/log` poll has arrived for 2 minutes**. The Windows build is
  `-H=windowsgui` with no console to close, so this matters there.
  - Don't shorten that: Chrome throttles timers in a minimized or hidden window to
    about one per minute, and a shorter window would kill the server under a GUI that
    is still open.
  - The first poll gets a one-minute grace period (`lastPoll` starts in the future),
    so a slow first browser launch is fine.
- In that fallback, the GUI never exits while a party runs, even if the tab is
  closed. Opening the GUI again starts a second instance on a new port, which cannot
  see the first one's party.

### OS helpers

- **Windows picker:** PowerShell `-STA` with a WinForms `OpenFileDialog`.
  - `[Console]::OutputEncoding = UTF8` is needed or non-ASCII paths come back mangled.
  - A `TopMost` owner form keeps the dialog from opening behind the app window.
- **`hide()` uses only `CREATE_NO_WINDOW`.** Don't add `HideWindow`: it sets SW_HIDE
  for the child's first `ShowWindow`, which can hide the PowerShell file dialog
  itself.
- **Linux picker: only the XDG desktop portal** (`portalPick`). There is no fallback to
  a specific picker program, and that is deliberate.
  - The portal is the standard, desktop-independent way to get the desktop's own file
    dialog. GNOME, KDE, COSMIC and most others run it.
  - An earlier version hard-coded `zenity` and `kdialog`. That broke on desktops that
    ship neither, Pop!_OS 24.04's COSMIC among them: Browse there only ever said "No
    file picker found". Don't add specific programs back.
  - The call sequence:
    1. Subscribe to `org.freedesktop.portal.Request.Response` on the predicted request
       path `/org/freedesktop/portal/desktop/request/<sender>/<token>`. The sender is
       the bus name without the `:`, with `.` replaced by `_`.
    2. Call `FileChooser.OpenFile` with that `handle_token`. Subscribing first means
       the reply can't be missed.
    3. Wait for the signal. Response code 0 carries `uris`, and anything else means
       cancelled.
    4. `fileURIPath` turns the `file://` URI into a path, percent-decoding spaces and
       non-ASCII.
  - Any portal error (no session bus, no portal) is returned, and the page asks the
    user to type or paste the path.
  - Checking a desktop without opening a dialog: `busctl --user introspect
    org.freedesktop.portal.Desktop /org/freedesktop/portal/desktop
    org.freedesktop.portal.FileChooser` must list `.OpenFile ssa{sv}`.
- **macOS picker:** `osascript`.
- On macOS and Windows, a non-zero exit from the picker command (`*exec.ExitError`)
  means cancelled and returns an empty path.
- **Window:** each browser in `browsers` is tried with `--app=URL
  --window-size=560,940 --user-data-dir=<temp> --no-first-run
  --no-default-browser-check`, then `openURL` (`rundll32` / `xdg-open` / `open`)
  opens the default browser as a plain tab.
  - **Only Chromium-family browsers are in the list**, because only they have an
    app-window mode. Linux tries Chrome, Chromium, Edge and Brave by name.
  - Windows lists absolute paths, **Chrome first, then Edge**. Edge ships with every
    Windows 10/11, so the app window always works there. Chrome goes first because
    Edge's startup boost can hand windows off. `msedge` is not on `PATH`, and `cmd /c
    start msedge` shows an error dialog when it is missing.
  - **Firefox has no app mode**, so it's never used for the window. It only appears as
    the default-browser tab when no Chromium-family browser is installed. Closing that
    tab can't be detected, so the idle exit applies.
  - macOS has no list (the browsers aren't on `PATH`), so it always opens a tab.

## Log-line contract

`handle()` in `index.html` matches the binary's existing log output. Change any of
these strings in `party/`, `mpv/` or `server/` and the GUI silently stops reacting:

| Log substring | UI effect |
|---|---|
| `your address to share is <invite>` | invite box + Copy button (text taken verbatim) |
| `relay: connected` | clears reconnecting/waiting |
| `relay: dial:` / `relay: disconnected` | "Lost the relay. Reconnecting…", turtles hurry |
| `is not open yet` (inside the dial line) | "Waiting for the host to open this room…" |
| `rejected by relay` | "Diagnosis: the relay rejected this room or secret…" |
| `init done` | "You're in…", heartbeat line starts pulsing |
| `do: sent request: {"command":["show-text",…` | party feed entry (`command[1]`) |

The invite paste box parses the same `-relay … -room … -secret …` or `ip:port` text,
and also accepts a whole pasted log line, because it strips everything up to `your
address to share is`. `-createKey` is never part of an invite: guests don't need it.

## UI (`index.html`)

### Theme tokens (`:root`)

| Token | Value | Use |
|---|---|---|
| `--lichen` | `#dfe6c8` | page background. Deliberately not cream |
| `--paper` | `#fbfaf1` | the card |
| `--forest` | `#23402b` | text |
| `--moss`, `--moss-deep` | `#4f7a34`, `#3d6128` | primary button, selection, stethoscope tube |
| `--bark`, `--soil` | `#6e4b30`, `#4a3220` | card border and shadow, headings, ground |
| `--porcini` | `#a8642a` | heartbeat line, mushroom caps |
| `--berry` | `#9a3a35` | errors, the End button, red caps, hearts |
| `--ochre` | `#e0b452` | focus ring |

- **Fonts:** **Gluten** for display (titles, the big button) and **Atkinson
  Hyperlegible** for body text. Both load from Google Fonts. Offline, the fallbacks are
  Comic Sans / system-ui. That is a known trade, and the next step would be embedding
  the woff2 files.
- **Fosdo2 is a fluffy long-haired brown tabby with white paws and hazel eyes,**
  drawn as a full chibi cat sitting on the card. The colours are tokens, so
  recolouring never means editing the SVG:

  | Token | Value | Use |
  |---|---|---|
  | `--fur` | `#8b7763` | head, body, tail |
  | `--fur-dark` | `#3d3229` | tabby stripes, the faint body and ruff outline |
  | `--fur-light` | `#d6c4a7` | muzzle, chin, chest bib, inner-ear wisps |
  | `--paw` | `#f7f2e8` | white paws |
  | `--ear` | `#e4b4a0` | inner ears |
  | `--blush` | `#ee9f98` | cheeks |
  | `--nose` | `#c98a79` | nose |
  | `--eye-in`, `--eye-mid`, `--eye-gold`, `--eye-out` | `#4f8f34`, `#86b04a`, `#c8a245`, `#946330` | the hazel `#hazel` radial gradient: green at the pupil, through yellow-green and gold, to brown at the rim |

### Layout and voice

- One centred card, `min(520px, 100% - 32px)`, with three views switched by the pill
  tabs under the title:
  - **Party**: Vitals and the form
  - **History**: recent parties, newest first, each with **Watch again**
  - **Settings**: the defaults, then **About** with the version, the Update button and
    the saved-file path. A red dot on the Settings tab means an update is available.

  The view is mirrored in the URL hash (`#history`, `#settings`), so a reload keeps it
  and a screenshot can open a view directly.
- The **Vitals** section sits above the form and is hidden until the first `/start`.
- While a party runs, the form fieldset is `hidden` (not just disabled), so Vitals and
  the End button sit right under the title.
- **Voice:** plain, active, sentence case. The medical theme lives in the names only:
  "Fosdo2's Movie Clinic" (with a small mushroom before it), "Vitals", "Patient chart" (the raw log), and every error
  starts with "Diagnosis:" followed by what happened and what to do.
- Avoid the generic AI-page tells: no all-caps labels, no small "eyebrow" labels above
  headings, no `→` on buttons, no middle-dot meta strings.

### Fosdo2 (`svg.cat`)

**This design was chosen by the user after many rejected attempts. Don't redesign
him; change only what is asked, and show options before changing his face.**

- **Placement.** `viewBox 0 0 200 220` at `width:180px` (198px tall), positioned
  `top:-181px`, so his paws (viewBox y≈201) sit on the card's top border. `main` has
  a 190px top margin to make room. If you change the size, recompute `top` as
  `201/220 × renderedHeight`.
- **Structure, back to front:**
  1. `defs`: the hazel gradient
  2. `.tail`: a bushy striped tail
  3. the round body, its stripes and the cream chest bib
  4. the left paw, then `.paw-rest` (the right paw and its toes)
  5. the stethoscope
  6. `.head-move` > `<g transform="translate(100 86) scale(1.05) translate(-80 -72)">`,
     the head
  7. `.paw-lick` (the raised arm and paw) and the lick tongue
  8. the `z`s
- **The head is the original fluffy face** the user liked, originally drawn in a
  160×142 space, hence the transform:
  - scalloped `.ruff` cheeks, ears with tip tufts and inner wisps
  - three forehead stripes plus temple curves, a light muzzle and chin
  - cheeks, a "w" mouth, whiskers
  - `.head` is only the ears and upper skull. Its bottom edge (y 80) is hidden inside
    the head ellipse. When the head peeked over the card the old flat bottom was
    covered by paws, but on a full body it showed as a hard edge under the chin.
- **Eyes:** a hazel iris (9×9.5, the `#hazel` radial gradient), a vertical oval pupil
  (3.4×6.2) and two glints. Earlier eyes were rejected:
  - a pale ring around a huge pupil read as startled
  - half-closed lids read as high
  - small ring-and-dot eyes read as a dummy
  - a fully kawaii redraw of the whole face read as not him
- **Motion** (CSS only, each part on its own animation):
  - **The tail** swishes −5° → 7° around its base (`136px 186px`, `transform-box:
    view-box`), every 3.2s alternating. It slows to 7s while asleep.
  - **Blink:** `blink` on `.eyes-open`, every 6s.
  - **Lick:** a 16s cycle shared by five elements. From 79% to 99%:
    - `.head-move` tilts 5° (`lick-tilt`)
    - `.eyes-open` hides and `.eyes-closed` shows (`lick-hide` / `lick-show`)
    - `.paw-rest` hides while `.paw-lick` and the tongue show
    - the paw bobs three times (`lick-bob`)

    They stay in sync only because they all use the same 16s duration with no delay.
    Change one duration and change them all.
  - **SVG transform rule:** CSS `transform` replaces an SVG `transform` attribute, so
    anything animated with CSS must be a *separate* group from anything positioned
    with the attribute. That's why `.head-move` wraps the transformed head group.
  - **The lick only runs awake** (`body:not([data-mood="sleep"])`). Asleep, he shows
    closed eyes and the `z`s.
  - **Reduced motion** stops the tail, blink, lick and `z`s. The lick parts then stay
    at their static state: paw down, eyes open.
    - **Previewing a frame:** see "Looking at the UI" above.
- **Moods** come from `body[data-mood]`, set in `render()`:
  - `sleep` (idle or ended): closed eyes, floating `z`s, a slow tail
  - `awake` (running, and on errors): hazel eyes, blinking, tail swishing, licking
  - **He is always content: there is deliberately no worried or sad face.** Errors are
    carried by the red "Diagnosis:" status text alone.
- Clicking the cat spawns a `.heart` in `main` (at `top: -120px`, by his face) that
  removes itself on `animationend`.

### Ground, mushrooms and turtles (`svg.ground`)

- `position: fixed` at the bottom, 560×130 and centred, so it is always visible.
  The content scrolls *under* it, and **covering whatever has scrolled beneath is
  intended**. The user prefers scrolling to read over a ground that takes its own space.
  A layout with a separate, non-overlapping ground strip was tried and reverted, so
  don't reintroduce it.
- `main` has a 150px bottom margin so the card can scroll clear of the ground.
- Layering:
  - `body::before`: a fixed 90px lichen fade, so content dissolves into the ground
    instead of being cut off.
  - `body::after`: a fixed 22px full-width soil strip, so wide browser tabs have
    ground past the SVG's edges. Its height matches the soil path's height at the
    SVG edges.
  - z-order: card (1) < fade and soil (2) < ground SVG (3) < Fosdo2 (2, inside `main`)
    and hearts.
- **Turtles:** a mother and a baby. The baby uses the same markup at `scale(.55)`,
  trailing 40px behind. Both `.turtle` groups run one 60s `stroll` loop all the time.
  - `body.walking` (connecting / reconnecting) only speeds up the legs and adds a
    `bob`. It must **not** change the stroll duration: changing `animation-duration`
    mid-run makes the turtles jump.
- **Mushrooms:** plain `<g transform>` groups with `.cap` (porcini), `.cap.red`,
  `.cap.cream`, `.spot` and `.gill`. The same mushroom is also a data-URI SVG used for
  the favicon and the party-feed bullets. Change one, change both.

### Behaviour notes

- **`[hidden] { display: none !important; }`** is required. `.grid` sets
  `display:grid`, which otherwise beats the `hidden` attribute, and the direct-mode
  fields showed up in relay mode.
- **`render()`** writes the button text to `#go-label`, not `#go`. Writing
  `#go.textContent` would delete the paw icon.
- **Nothing is kept in browser storage.** See "Saved settings and history" for why.
  The page loads everything from `GET /state`.
- **Reduced motion** stops every ambient animation and leaves the turtles parked
  mid-ground. Hearts still play, because they answer a click.
- **Copy invite** uses `navigator.clipboard`. That works because `127.0.0.1` is a
  secure context. If it fails, the invite text is selected and the button says to
  press Ctrl+C.

## Working on the look

Visual taste here is the user's call, and guessing has repeatedly gone wrong. What
works:

1. Make the change in a **sandbox copy** of `index.html` (or a standalone SVG) outside
   the repo.
2. Render it with the commands in "Looking at the UI". For a face change, render it
   next to the current version and any reference the user gave.
3. Show **two or three options** side by side, plus a GIF if it moves, and ask which
   one.
4. Apply only the chosen one, then check it in the real app during a live party.

When a reference is given, work out *what makes it work* (proportions, eye
construction, outline, simplicity) and apply that essence. Don't copy it wholesale:
a full kawaii redraw of Fosdo2's face lost what made him him.

## Not yet verified on Windows

Everything Windows-specific compiles, but has not been run on Windows. Check these
first:

- The PowerShell picker appears in front of the window.
- No console window flashes for the picker.
- mpv's window shows up when it is launched from a `-H=windowsgui` parent through
  `mpv.Init`'s `HideWindow | CREATE_NEW_CONSOLE`.
- The mpv named pipe (`\\.\pipe\mpv<random>`) connects, and a party syncs with a
  Linux peer.
- Chrome and Edge open in `--app` mode from the listed paths, and with a fresh
  `--user-data-dir` the launched process stays alive until the window is closed. If
  either one hands off and exits at once, the hand-off guard keeps the app running,
  but closing the window then won't stop it.
- The Windows self-update swap (`.exe` → `.old`, new file in place, `.old` deleted on
  the next start).
