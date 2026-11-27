# gui/DESIGN.md

The window's look: the drawings in `art/` (`cat.svg`, `ground.svg`, the icons in `page.go`)
and the layout in `theme.go`, `window.go` and a file per tab. Read it before any visual change;
`gui/CLAUDE.md` has the code and the drawing traps. The visual identity is part of the product, so keep it:

- a forest-floor palette of greens and browns
- mushrooms and a family of turtles along the bottom
- a fluffy tabby mascot named **Fosdo2**
- light medical touches in the wording (Vitals, Patient chart, Diagnosis)

## Working on the look

Visual taste is the user's call, and guessing has repeatedly gone wrong.

1. Make each option a scene in `render_test.go` (a state, and a `setup` that sets it), or a
   standalone SVG for a drawing.
2. Render it (below). For a face change, render it next to the current version and any
   reference the user gave.
3. Show **two or three options** side by side, plus a GIF if it moves, and ask which.
4. Apply only the chosen one, drop the others' scenes, then check it in the real app during
   a live party.

From a reference, take *what makes it work* (proportions, eye construction, outline),
not the whole drawing.

### Rendering without a window

```sh
EGL_PLATFORM=surfaceless go test ./gui -run TestRender -render <dir>
```

- Every scene in `scenes()` becomes `<dir>/<name>.png` at 560 wide (the real window) and its
  `height`, drawn by Gio's headless GPU path (Mesa's llvmpipe in a container), so what it
  shows is what the window draws. `at` is the moment: the cat and the turtles move with it.
- A GIF is a scene drawn at several `at`s, joined by `ffmpeg`.
- **560×940** is the real window. Check every visual change there; a GNOME-on-Wayland
  window is 40dp shorter under the title bar (the `title-bar` scene).

## Palette (`art/page.go`)

| Name | Value | Use |
|---|---|---|
| `Lichen` | `#dfe6c8` | window background. Deliberately not cream |
| `Paper` | `#fbfaf1` | the card |
| `Forest` | `#23402b` | text |
| `Moss`, `MossDeep` | `#4f7a34`, `#3d6128` | primary button, selection, stethoscope tube |
| `Bark`, `Soil` | `#6e4b30`, `#4a3220` | card border and shadow, headings, ground |
| `Porcini` | `#a8642a` | heartbeat line, mushroom caps |
| `Berry` | `#9a3a35` | errors, the End button, red caps, hearts |
| `Ochre` | `#e0b452` | focus ring |

**Dark** (`darkPalette` in `palette.go`, "soil at night", the user's pick of three): while
the OS's appearance is dark, each colour above turns into a warm brown-black one: `Lichen`
`#1c1712`, `Paper` `#28211a`, `Forest` `#efe6d4`, `Bark` `#e0b98a`, `Moss` `#93c06c`. Moss
gets lighter and white darker, so text and ticks on moss turn dark. Shadows and the dialog
backdrop turn black. Drawings keep their own colours: Fosdo2, the ground, the mushrooms,
shells and emoji. Check a visual change in both: every scene has a `-dark` twin.

Fosdo2's colours are named in `page.go`'s rules too, so recolouring never means editing the SVG:

| Name | Value | Use |
|---|---|---|
| `fur` | `#8b7763` | head, body, tail |
| `furDark` | `#3d3229` | tabby stripes, the faint body and ruff outline |
| `furLight` | `#d6c4a7` | muzzle, chin, chest bib, inner-ear wisps |
| `.paw` | `#f7f2e8` | white paws |
| `.ear` | `#e4b4a0` | inner ears |
| `.cheek` | `#ee9f98` | cheeks |
| `.nose` | `#c98a79` | nose |
| `hazel` (`scene.go`) | `#4f8f34`, `#86b04a`, `#c8a245`, `#946330` | the `#hazel` radial gradient, green at the pupil to brown at the rim |

**Fonts,** all bundled: Gluten for display, Atkinson Hyperlegible for body, Noto Color Emoji
for emoji (fetched at build time). System fonts stay on, for scripts the bundled ones lack.

## Layout and voice

- One centred card, `min(520dp, 100% - 32dp)`, with three views switched by pill tabs:
  - **Party**: Vitals and the form
  - **History**: recent parties, newest first, each titled by its file's full path (a
    playlist by its folder's), and **Continue** (filled moss) on a party over the internet,
    **Watch again** (outlined) on a peer-to-peer one
  - **Settings**: the defaults, then **About** (version, Update button; on Windows and macOS
    one row each for mpv, ffmpeg and yt-dlp: version and path, how to update yt-dlp, and for
    a missing one an `i` that unfolds how to install it by hand; saved-files folder). A red dot on the tab means an update is available.
- **The `?`** in the card's corner opens the help over the card, on every view and during
  a party. One topic per section, named for what it is, never for one kind of video.
- **Vitals** is hidden until the first party starts. While one runs the form is hidden, so
  Vitals and End sit under the title.
  - **Starting, a mushroom trail** (chosen over a checklist and a step counter): one
    mushroom per step, labelled Torrent or File(s), Relay or Router, mpv, Video. Done ones
    are porcini on a solid moss path, the current one berry and larger, the rest pale on a
    dashed path. Under the status, a detail line (numbers bold) and a striped moss bar when
    there is a fraction. The trail shows only once a step has taken half a second (a quick
    start would flash it), and goes once the node is in; buffering reuses detail and bar.
  - **"In the party, N"**, a closed `details` (expanding the chosen row list): a row per
    member with a turtle shell in rotating colours, "since 9:08 PM", or "already here" for
    those who were in before you, whose join time the app never learns.
  - **A slow member** (option B of three): the status says whose network is slow and to stay
    paused, the detail how many seconds each has ready, and their row a porcini bar, full
    at a minute. Gone on the next resume.
  - **A refused file** (rows, chosen over two cards and detail lines): under the Diagnosis,
    a row per fact both sides have (video, size, checksum, a playlist's count and checksum),
    the party's beside yours, what differs on your side in berry. Never an empty cell (chosen
    over filler words and sending more): what only one side has, like which of your videos
    differs, is the detail line above the rows.
  - **Say something** (option B of three), between the invite and the feed once the node is in:
    a pill field and a moss Send with the paw. Under it the chat, never in the feed (option A of
    three): rows like the feed's, newest first, yours in moss; the latest three, the rest under
    "All messages, N" (the user's own addition). On mpv, chat is top-right (option C of three).
  - **Reactions:** an outlined smiley (icon 1 of three, like WhatsApp's) beside Send opens one
    row of the 26 emoji that scrolls sideways, the wheel too; a pick closes it. On mpv the same
    smiley shows bottom-right while the mouse moves and opens the row in the middle (C of
    three, one row the user's own change). The art is Twemoji's, drawn the same on both.
    Settings' "Reaction picker on mpv" (cards like the connection's, the user's ask): when the
    mouse moves (default), near its corner, or never; applied at once. After a pick both smileys
    rest 2s, mpv's faded, the GUI's disabled. Each row opens where it was left (the user's ask).
    People didn't see the row scrolls, so each end with more has a round arrow that scrolls a
    rowful less one, and fades inside its border; an emoji an end cuts scrolls as its arrow does,
    in both (mpv dims it): A and B of three together, the user's pick over a position bar.
- **Voice:** plain, active, sentence case. The title is "Hahas and Hehes Watch Party"
  with a small mushroom. The medical theme lives in names only: "Vitals", "Patient
  chart" (the raw log), and every error starts with "Diagnosis:" then what happened
  and what to do.
- No all-caps labels, no eyebrow labels above headings, no `→` on buttons, no
  middle-dot meta strings.

## Fosdo2 (`svg.cat`)

**Chosen by the user after many rejected attempts. Don't redesign him; change only
what is asked, and show options before changing his face.**

- **Placement** (`paintCat`): `viewBox 0 0 200 220` drawn 180dp wide, 9dp down, so his paws
  (y≈201) sit on the card's top border at 190dp. Resizing means recomputing the top as
  `190 - 201/220 × drawnHeight`.
- **Back to front:** `defs` (hazel gradient), `.tail`, body with stripes and bib, left
  paw then `.paw-rest`, stethoscope, `.head-move` > the head group (`translate(100 86)
  scale(1.05) translate(-80 -72)`, since it was drawn in a 160×142 space), `.paw-lick`
  and the tongue, the `z`s.
- **The head:** scalloped `.ruff` cheeks, ears with tufts and wisps, three forehead
  stripes, a light muzzle, cheeks, a "w" mouth, whiskers. `.head` is only the ears and
  upper skull; its bottom edge (y 80) hides inside the head ellipse.
- **Eyes:** a hazel iris (9×9.5), a vertical oval pupil (3.4×6.2) and two glints.
  Rejected before: a pale ring around a huge pupil (startled), half-closed lids
  (high), small ring-and-dot eyes (a dummy), a full kawaii redraw (not him).
- **Motion** (`art/scene.go`):
  - Tail swishes −5° → 7° around (136, 186) in the viewBox every
    3.2s, 7s asleep.
  - `blink` on `.eyes-open` every 6s.
  - **Lick:** one 16s cycle shared by five elements; from 79% to 99% `.head-move`
    tilts 5°, the eyes swap to closed, `.paw-rest` hides while `.paw-lick` and the
    tongue show, and the paw bobs three times. They stay in sync only through the same
    duration and no delay: change one, change all.
  - An animation replaces an element's own `transform` attribute, so anything animated must
    be a separate group from anything positioned by the attribute (hence `.head-move`).
  - The lick runs only awake. With the OS's reduced motion everything rests: eyes open
    awake, closed asleep, the paw down, the `z`s still at .8.
- **Moods** (`art.Mood`), from the watch's screen: asleep (idle or ended: closed
  eyes, `z`s, slow tail) and awake (running and on errors). **There is deliberately
  no worried or sad face;** errors are carried by the red "Diagnosis:" text.
- Clicking him sends up a heart that fades as it rises. Text can't be selected, so petting
  him fast selects nothing; only the invite, the status and the chart can, to copy them.

## Ground, mushrooms and turtles (`svg.ground`)

- Fixed at the bottom (`paintGround`), 560×130, centred, drawn after the card: content
  scrolls *under* it and **covering it is intended**; a separate non-overlapping strip was
  tried and reverted. The card ends 150dp above the window's bottom.
- **A wider window** (the user's pick, the `party-wide` scene): the soil path stretches across
  it, the mushrooms and grass repeat every 560 from the middle out, and the turtles stroll
  the whole width at the same pace, so resizing mid-stroll moves them. Layers: soil, plants,
  turtles, the SVG's own order.
- Under it a 90dp lichen fade, and a 22dp soil strip for wide windows, matching the soil
  path's height at the SVG edges. Order: card < fade and soil < ground < Fosdo2 and hearts.
- **Turtles:** a mother and a baby (same markup at `scale(.55)`, 40px behind), on one
  60s `stroll` loop. Walking (connecting) only speeds the legs and adds a `bob`; changing
  the stroll duration mid-run makes them jump. Reduced motion parks them mid-ground.
- **Mushrooms:** `<g transform>` groups with `.cap`, `.cap.red`, `.cap.cream`, `.spot`,
  `.gill`. The small mushroom (`art.Mushroom`: the title, the feed's bullets, the title bar)
  is also `install.sh`'s app icon: change both.
