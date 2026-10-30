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

A separate `package main`: stdlib, `golang.org/x/sys/windows` (already in `go.mod`)
and the repo's own `party` package. No cgo, so it cross-compiles in the same
`publisher.yml` matrix as the CLI. **It calls `party.Run` in-process**, the exact
function the CLI's `main.go` calls, so it does everything on its own and needs only
mpv. The `watchparty` binary is not required. mpv stays the player. The GUI's status
display is driven entirely by the `log` lines `party`, `mpv` and `server` already
print.

| File | Role |
|---|---|
| `main.go` | HTTP server, guard, party lifecycle, log capture, `form.config()` |
| `index.html` | the entire UI: CSS, inline SVG art, vanilla JS. `//go:embed`ed |
| `os_unix.go` (`!windows`) | browser list, `openURL`, `pickFile`, no-op `hide` (macOS through `runtime.GOOS` branches) |
| `os_windows.go` | the same with Edge/Chrome paths, a PowerShell picker and `CREATE_NO_WINDOW` |
| `main_test.go` | `form.config()` per mode plus its errors, and the host/token guard table |

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
  - The capture is process-wide, so stray lines from a previous party's leftover
    goroutines can land in the next one. That's cosmetic.
- **`run`** goes up on every `/start`. A poll carrying an old `run` is answered from
  line 0, so a poll that was in flight during a restart can't splice two runs
  together. The log is uncapped in memory and reset per run.
- **`party.Run`'s return value** becomes `exitErr`. It is `nil` after `/stop` or when
  the user closes mpv (see "Return values" in `party/CLAUDE.md` for why that needs
  care), `errStopped` when something inside cancelled the party (relay
  rejection, port already in use), and a setup error otherwise (mpv not found,
  UPNP failed, no network). The page maps "mpv … not found" to its own diagnosis.

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
- **Idle exit:** the Windows build is `-H=windowsgui` and has no console window to
  close. So a ticker cancels that same context once **no party is running and no
  `/log` poll has arrived for 2 minutes**.
  - Don't shorten that: Chrome throttles timers in a minimized or hidden window to
    about one per minute, and a shorter window would kill the server under a GUI that
    is still open.
  - The first poll gets a one-minute grace period (`lastPoll` starts in the future),
    so a slow first browser launch is fine.
- The GUI never exits while a party runs, even if the window is closed. Opening the
  GUI again starts a second instance on a new port, which cannot see the first one's
  party.

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
- **Fosdo2 is a long-haired brown tabby with white paws and green eyes.** The colours
  are tokens, so recolouring never means editing the SVG:

  | Token | Value | Use |
  |---|---|---|
  | `--fur` | `#8b7763` | head and ruff |
  | `--fur-dark` | `#3d3229` | tabby stripes, ear-tip tufts, paw outlines |
  | `--fur-light` | `#d6c4a7` | muzzle, chin bib, inner-ear wisps |
  | `--paw` | `#f7f2e8` | white paws |
  | `--ear` | `#e4b4a0` | inner ears |
  | `--iris`, `--iris-rim` | `#8cb44a`, `#4d6b2b` | eyes |
  | `--blush` | `#ee9f98` | cheeks |
  | `--nose` | `#c98a79` | nose |

### Layout and voice

- One centred card, `min(520px, 100% - 32px)`.
- The **Vitals** section sits above the form and is hidden until the first `/start`.
- While a party runs, the form fieldset is `hidden` (not just disabled), so Vitals and
  the End button sit right under the title.
- **Voice:** plain, active, sentence case. The medical theme lives in the names only:
  "Fosdo2's Movie Clinic" (with a small mushroom before it), "Vitals", "Patient chart" (the raw log), and every error
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
- **Drawing order, back to front:**
  1. two **ruff** paths, scalloped quadratic bumps along the sides of an ellipse at
     `80,76`, which give the long-haired cheeks. They were generated by a small
     script: rx 50, ry 31, 6 bumps of 11 units over 135°–220° and −40°–45°.
     Pointed zigzags read as wings; keep the bumps round.
  2. the ears with tip tufts and inner wisps
  3. the head ellipse
  4. the tabby stripes: three forehead lines and the cheek bars
  5. the light muzzle and chin bib
  6. the eyes: a relaxed, half-closed "slow blink". A green iris with a small
     pupil sits under a fur-coloured upper lid (`.lid`) with an arched lid line, and
     one glint sits below the lid. This is how a content cat actually looks. Wide
     round eyes with big pupils read as startled or staring, not happy (a cat's
     pupils dilate when it's scared or excited), and the earlier wide-eyed version
     was rejected for exactly that.
  7. cheeks, nose, mouth (smile and rest variants), whiskers
  8. the `z`s
  9. the stethoscope
  10. the white paws on top
- **Moods** come from `body[data-mood]`, set in `render()`:
  - `sleep` (idle or ended): closed eyes, floating `z`s
  - `awake` (running, and on errors): the half-closed eyes with the CSS blink, an **open smile
    with a pink tongue** (`.mouth-smile`) and stronger blush. Asleep, he has the
    small closed "w" mouth (`.mouth-rest`) instead.
  - **Fosdo2 is always content: there is deliberately no worried or sad face.**
    Errors are carried by the red "Diagnosis:" status text alone. Keep anything
    above the eyes clear: an earlier pair of temple stripes sat right over the
    enlarged eyes and read as worried eyebrows.
- Clicking the cat spawns a `.heart` in `main` that removes itself on `animationend`.

### Ground, mushrooms and turtles (`svg.ground`)

- **The ground has its own space and never overlaps content.** `body` is a
  full-height flex column (`height: 100vh; overflow: hidden`) with two children:
  - `.scroll` (`flex: 1; overflow-y: auto`) holds `main`, so only the area above the
    ground scrolls. A `mask-image` fades its last 28px, so the card dissolves into
    the forest floor instead of ending on a hard line. `main`'s 28px bottom margin
    keeps the end of the content clear of that fade.
  - `.ground-strip` (`flex: none`, 130px) holds the 560×130 SVG, centred. Its
    background puts a full-width soil band under the SVG's own soil path, so wide
    browser tabs have ground past the SVG's edges.
  - An earlier version pinned the ground with `position: fixed` over the page, so the
    mushrooms covered whatever text had scrolled beneath them. Don't go back to an
    overlay.
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
- No console window flashes for the picker.
- mpv's window shows up when it is launched from a `-H=windowsgui` parent through
  `mpv.Init`'s `HideWindow | CREATE_NEW_CONSOLE`.
- Edge opens in `--app` mode from the listed paths.

Verified on Linux, in-process, against real mpv: relay and direct parties driven
through two GUIs, pause and resume propagating, the party feed, Stop sending "has
left" and leaving no orphaned mpv, the wrong-secret diagnosis, the missing-mpv
diagnosis, and the guard (403 without a token or with a foreign `Host`).
