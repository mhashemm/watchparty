# gui/CLAUDE.md

This is the desktop app for people who don't want the CLI. Its visual identity is part
of the product, so keep it when you change things:

- a forest-floor palette of greens and browns
- mushrooms and a family of turtles along the bottom
- a fluffy tabby mascot named **Fosdo2**
- light medical touches in the wording

## Commands

```sh
go build -o watchparty-gui ./gui                         # from the repo root; `-o` is required, see below
go test ./gui
go vet ./gui && GOOS=windows go vet ./gui                # the two build-tagged halves
GOOS=windows go build -ldflags=-H=windowsgui -o /dev/null ./gui
./watchparty-gui -watchparty ./watchparty                # override the sibling lookup
```

Plain `go build ./gui` from the repo root fails: the output name `gui` collides with
the directory.

### Looking at the UI without opening a window on your desktop

`openWindow` launches Chrome/Chromium, or `xdg-open` as a fallback. To keep it
headless, run the GUI with a `PATH` that holds only the built binaries, an `mpv`
symlink and a no-op `xdg-open` script. Then screenshot the page:

```sh
chromium --headless --disable-gpu --hide-scrollbars --force-prefers-reduced-motion \
  --window-size=560,940 --virtual-time-budget=4000 --screenshot=out.png http://127.0.0.1:<port>/
```

- **`--force-prefers-reduced-motion`** freezes the turtles at a fixed position. Without
  it, a screenshot taken at t≈0 has them offscreen at `translateX(-90px)`.
- **`--force-device-scale-factor=2`** with a short window gives a close-up of Fosdo2.
- **560×940** is the size of the real app window. Check every visual change at that
  size, and at a wide browser tab (1280×800) for the fallback path.

To drive a party without clicking, read the token from the page and POST to `/start`:

```sh
T=$(curl -s $U/ | grep -o 'const TOKEN = "[^"]*"' | cut -d'"' -f2)
curl -X POST -H "X-Token: $T" $U/start -d '{"mode":"relay","file":"/abs/movie.mkv","relay":"ws://127.0.0.1:18080","mpvFlags":"--vo=null --ao=null","cooldown":"2"}'
```

## Architecture

A separate `package main`: stdlib plus `golang.org/x/sys/windows` (already in
`go.mod`), no cgo, so it cross-compiles in the same `publisher.yml` matrix as the
main binary. **It runs the `watchparty` binary as a subprocess and imports nothing
from the repo.** mpv stays the player. The GUI only replaces typing flags, and its
status display is driven entirely by the child's log output.

| File | Role |
|---|---|
| `main.go` | HTTP server, guard, child process, log buffer, `form.args()` |
| `index.html` | the entire UI: CSS, inline SVG art, vanilla JS. `//go:embed`ed |
| `os_unix.go` (`!windows`) | browser list, `openURL`, `pickFile`, no-op `hide`, `interrupt` via signals (macOS through `runtime.GOOS` branches) |
| `os_windows.go` | the same with Edge/Chrome paths, a PowerShell picker, `CREATE_NO_WINDOW`, `taskkill` |
| `main_test.go` | `form.args()` per mode, and the host/token guard table |

### Server

- Listens on `127.0.0.1:0`. `g.host` is the exact `ip:port`, and the guard compares
  against it.
- **Guard (`ServeHTTP`), load-bearing because `/start` spawns a process with
  user-supplied mpv flags:**
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
  | `POST /start` | JSON `form` → argv. 400 without a file, 409 if a party is running |
  | `POST /stop` | stops the child |
  | `GET /log?from=N&run=R` | `{run, from, lines, running, exitErr}` |

- **`form.args()`** emits only a fixed set of flags, each as one `-name=value` argv
  element with no shell, and skips empty values so the binary's own defaults apply.
  The `=` form keeps a value starting with `-` from being read as a flag. Relay mode
  ignores the direct fields and vice versa.
- **The log** is the child's stdout and stderr (`log` writes to stderr) joined
  through one `io.Pipe`. It is read with `bufio.Reader.ReadString`, not a `Scanner`,
  because a Scanner stops on a too-long line, the pipe fills, and the child blocks
  on its next write.
- **`run`** goes up on every `/start`. A poll carrying an old `run` is answered from
  line 0, so a poll that was in flight during a restart can't splice two runs
  together. The log is uncapped in memory and reset per run.
- **`cmd.WaitDelay = 5s`** stops `Wait` hanging if a grandchild ever holds the pipe.

### Finding the binaries

- **watchparty:** if the GUI's own file name contains `-gui`, it first looks for the
  same name with `-gui` removed, then for `watchparty(.exe)` next to itself, then
  `PATH`. This pairs the release names: `watchparty-gui_amd64` → `watchparty_amd64`,
  `watchparty-gui.exe` → `watchparty.exe`. The `-gui` check matters: without it, a
  renamed GUI binary would find *itself* and spawn copies of itself.
- **mpv:** an empty mpv field becomes the absolute path of a sibling `mpv(.exe)`. It
  must be absolute: since Go 1.19, `exec.LookPath` refuses to resolve a bare `mpv`
  from the current directory, so on Windows "drop mpv.exe in the same folder" would
  not work otherwise.

### Lifetime

The Windows build is `-H=windowsgui` and has no console window to close. So a ticker
exits the process once **no child is running and no `/log` poll has arrived for 2
minutes**. Don't shorten that: Chrome throttles timers in a minimized or hidden window
to about one per minute, and a shorter window would kill the server under a GUI that
is still open. The first poll gets a one-minute grace period (`lastPoll` starts in
the future) so a slow first browser launch is fine.

The GUI never exits while a party runs, even if the window is closed. Opening the GUI
again starts a second instance on a new port, which cannot see the first one's party.

### Stopping

- **Unix:** `SIGINT`. The binary's `signal.NotifyContext` cancels, mpv is killed, and
  the deferred `/bye` and UPNP cleanup run. The other side gets "X has left". Verified.
- **Windows:** `taskkill /T /PID` **without `/F`**. That sends WM_CLOSE to the tree's
  windows, mpv quits normally, `cmd.Wait` returns in watchparty, and its defers run.
  watchparty itself has no window, so taskkill complains about it, and that is fine.
- **Both:** 5s later, if the same child is still running, it is forced (`Kill` /
  `/F`).

### OS helpers

- **Windows picker:** PowerShell `-STA` with a WinForms `OpenFileDialog`.
  - `[Console]::OutputEncoding = UTF8` is needed or non-ASCII paths come back mangled.
  - A `TopMost` owner form keeps the dialog from opening behind the app window.
- **`hide()` uses only `CREATE_NO_WINDOW`.** Don't add `HideWindow`: it sets SW_HIDE
  for the child's first `ShowWindow`, which can hide the PowerShell file dialog
  itself.
- **Linux picker:** `zenity`, then `kdialog`. **macOS picker:** `osascript`. A
  non-zero exit (`*exec.ExitError`) means cancelled and returns an empty path. If no
  picker exists the call returns an error, and the page asks the user to type the path.
- **Window:** each browser in `browsers` is tried with `--app=URL
  --window-size=560,940`, then `openURL` (`rundll32` / `xdg-open` / `open`).
  - Windows lists absolute Edge/Chrome paths, because `msedge` is not on `PATH` and
    `cmd /c start msedge` shows an error dialog when it is missing.
  - The browser is never killed, and the GUI does not learn when the window closes.
    That is what the poll heartbeat is for.

## Log-line contract

`handle()` in `index.html` matches the binary's existing log output. Change any of
these strings in `main.go`, `ws.go` or `mpv/` and the GUI silently stops reacting:

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
| `--porcini` | `#a8642a` | ℞, heartbeat line, mushroom caps |
| `--berry` | `#9a3a35` | errors, the End button, red caps, hearts |
| `--ochre` | `#e0b452` | focus ring |

- **Fonts:** **Gluten** for display (titles, the big button) and **Atkinson
  Hyperlegible** for body text. Both load from Google Fonts. Offline, the fallbacks are
  Comic Sans / system-ui. That is a known trade, and the next step would be embedding
  the woff2 files.
- **Fosdo2's colours** are `--fur`, `--fur-dark` (stripes, paw outlines) and `--ear`
  (inner ears, nose, cheeks). It is drawn as a
  grey tabby. Recolour through these tokens, and
  redraw markings in the cat SVG.

### Layout and voice

- One centred card, `min(520px, 100% - 32px)`.
- The **Vitals** section sits above the form and is hidden until the first `/start`.
- While a party runs, the form fieldset is `hidden` (not just disabled), so Vitals and
  the End button sit right under the title.
- **Voice:** plain, active, sentence case. The medical theme lives in the names only:
  "℞ Fosdo2's Movie Clinic", "Vitals", "Patient chart" (the raw log), and every error
  starts with "Diagnosis:" followed by what happened and what to do.
- Avoid the generic AI-page tells: no all-caps labels, no eyebrows, no `→` on
  buttons, no middle-dot meta strings.

### Fosdo2 (`svg.cat`)

- `viewBox 0 0 160 142` at `width:170px`, positioned `top:-101px` so that viewBox
  y≈95 lands on the card's top border. The paws (`cy=100`) straddle the border, and
  the stethoscope (Y-shaped tube plus a chestpiece at `119,129`) hangs over the card
  face. **If you change the viewBox height or width, recompute `top`**
  (`95/viewBoxHeight × renderedHeight`). Also keep the card's `padding-top` (54px) big
  enough that the chestpiece clears the title.
- **Moods** come from `body[data-mood]`, set in `render()`:
  - `sleep` (idle or ended): closed eyes, floating `z`s
  - `awake` (running): open eyes with the CSS blink
  - `worried` (error or reconnecting): open eyes plus sad brows, which slope *up*
    toward the middle. The other way round reads as angry.
- Clicking the cat spawns a `.heart` in `main` that removes itself on `animationend`.

### Ground, mushrooms and turtles (`svg.ground`)

- `position: fixed` at the bottom, 560×130 and centred, so it is always visible.
  The content scrolls *under* it.
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
- **`localStorage`** (`watchparty-gui`, every access in try/catch) remembers every
  field except the movie path, which changes each time.
- **Reduced motion** stops every ambient animation and leaves the turtles parked
  mid-ground. Hearts still play, because they answer a click.
- **Copy invite** uses `navigator.clipboard`. That works because `127.0.0.1` is a
  secure context. If it fails, the invite text is selected and the button says to
  press Ctrl+C.

## Not yet verified

Everything Windows-specific compiles, but has not been run on Windows:

- The PowerShell picker appears in front of the window.
- No console window flashes for the child, the picker or taskkill.
- `taskkill /T` without `/F` closes mpv and lets watchparty send `/bye`.
- Edge opens in `--app` mode from the listed paths.

Verified on Linux: relay and direct parties driven through two GUIs, pause and resume
propagating, the party feed, Stop sending "has left" and leaving no orphaned mpv, the
wrong-secret diagnosis, the guard (403 without a token or with a foreign `Host`), and
the release-name sibling pairing.
