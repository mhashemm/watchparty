# gui/DESIGN.md

The look of `index.html`. Read it before any visual change; `gui/CLAUDE.md` has the
code. The visual identity is part of the product, so keep it:

- a forest-floor palette of greens and browns
- mushrooms and a family of turtles along the bottom
- a fluffy tabby mascot named **Fosdo2**
- light medical touches in the wording (Vitals, Patient chart, Diagnosis)

## Working on the look

Visual taste is the user's call, and guessing has repeatedly gone wrong.

1. Make the change in a **sandbox copy** of `index.html` (or a standalone SVG) outside
   the repo.
2. Render it (below). For a face change, render it next to the current version and any
   reference the user gave.
3. Show **two or three options** side by side, plus a GIF if it moves, and ask which.
4. Apply only the chosen one, then check it in the real app during a live party.

From a reference, take *what makes it work* (proportions, eye construction, outline),
not the whole drawing.

### Rendering without a window

Run the GUI headless (`gui/CLAUDE.md`), then:

```sh
chromium --headless --disable-gpu --hide-scrollbars --force-prefers-reduced-motion \
  --window-size=560,940 --virtual-time-budget=4000 --screenshot=out.png http://127.0.0.1:<port>/
```

- **`--force-prefers-reduced-motion`** freezes the turtles and Fosdo2 at their static
  pose; without it the turtles are offscreen at t≈0.
- **`--virtual-time-budget` does not advance CSS animation time.** To capture a moment,
  inject `* { animation-play-state: paused !important; animation-delay: -<s>s
  !important; }` into a copy of the page (strip the `<script>`, set `<body
  data-mood="awake">`). -13.6s is mid-lick. A GIF is those frames joined by `ffmpeg`.
- **`--force-device-scale-factor=2`** or 3 with a short window gives a close-up of
  Fosdo2.
- **560×940** is the real app window. Check every visual change there, and at
  1280×800 for the browser-tab fallback.

## Theme tokens (`:root`)

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

Fosdo2's colours are tokens too, so recolouring never means editing the SVG:

| Token | Value | Use |
|---|---|---|
| `--fur` | `#8b7763` | head, body, tail |
| `--fur-dark` | `#3d3229` | tabby stripes, the faint body and ruff outline |
| `--fur-light` | `#d6c4a7` | muzzle, chin, chest bib, inner-ear wisps |
| `--paw` | `#f7f2e8` | white paws |
| `--ear` | `#e4b4a0` | inner ears |
| `--blush` | `#ee9f98` | cheeks |
| `--nose` | `#c98a79` | nose |
| `--eye-in`, `--eye-mid`, `--eye-gold`, `--eye-out` | `#4f8f34`, `#86b04a`, `#c8a245`, `#946330` | the `#hazel` radial gradient, green at the pupil to brown at the rim |

**Fonts:** Gluten for display, Atkinson Hyperlegible for body, from Google Fonts.
Offline they fall back to Comic Sans / system-ui; embedding the woff2 files would fix
that.

## Layout and voice

- One centred card, `min(520px, 100% - 32px)`, with three views switched by pill tabs
  and mirrored in the URL hash (`#history`, `#settings`):
  - **Party**: Vitals and the form
  - **History**: recent parties, newest first, each with **Watch again**
  - **Settings**: the defaults, then **About** (version, Update button, saved-file
    path). A red dot on the tab means an update is available.
- **Vitals** is hidden until the first `/start`. While a party runs the form fieldset
  is `hidden`, so Vitals and End sit under the title.
- **Voice:** plain, active, sentence case. The title is "Hahas and Hehes Watch Party"
  with a small mushroom. The medical theme lives in names only: "Vitals", "Patient
  chart" (the raw log), and every error starts with "Diagnosis:" then what happened
  and what to do.
- No all-caps labels, no eyebrow labels above headings, no `→` on buttons, no
  middle-dot meta strings.

## Fosdo2 (`svg.cat`)

**Chosen by the user after many rejected attempts. Don't redesign him; change only
what is asked, and show options before changing his face.**

- **Placement:** `viewBox 0 0 200 220` at `width:180px`, `top:-181px`, so his paws
  (y≈201) sit on the card's top border; `main` has a 190px top margin. Resizing means
  recomputing `top` as `201/220 × renderedHeight`.
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
- **Motion**, CSS only:
  - Tail swishes −5° → 7° around `136px 186px` (`transform-box: view-box`) every
    3.2s, 7s asleep.
  - `blink` on `.eyes-open` every 6s.
  - **Lick:** one 16s cycle shared by five elements; from 79% to 99% `.head-move`
    tilts 5°, the eyes swap to closed, `.paw-rest` hides while `.paw-lick` and the
    tongue show, and the paw bobs three times. They stay in sync only through the same
    duration and no delay: change one, change all.
  - CSS `transform` replaces an SVG `transform` attribute, so anything animated must
    be a separate group from anything positioned by the attribute (hence `.head-move`).
  - The lick runs only awake. Reduced motion stops tail, blink, lick and `z`s, leaving
    paw down and eyes open.
- **Moods** from `body[data-mood]`, set in `render()`: `sleep` (idle or ended: closed
  eyes, `z`s, slow tail) and `awake` (running and on errors). **There is deliberately
  no worried or sad face;** errors are carried by the red "Diagnosis:" text.
- Clicking him spawns a `.heart` in `main` that removes itself on `animationend`.

## Ground, mushrooms and turtles (`svg.ground`)

- `position: fixed` at the bottom, 560×130, centred. Content scrolls *under* it and
  **covering it is intended**; a separate non-overlapping strip was tried and
  reverted. `main` has a 150px bottom margin.
- `body::before` is a fixed 90px lichen fade; `body::after` a fixed 22px soil strip
  for wide tabs, matching the soil path's height at the SVG edges. z-order: card (1) <
  fade and soil (2) < ground (3) < Fosdo2 and hearts.
- **Turtles:** a mother and a baby (same markup at `scale(.55)`, 40px behind), on one
  60s `stroll` loop. `body.walking` (connecting) only speeds the legs and adds a
  `bob`; changing the stroll duration mid-run makes them jump.
- **Mushrooms:** `<g transform>` groups with `.cap`, `.cap.red`, `.cap.cream`, `.spot`,
  `.gill`. The same mushroom is a data-URI SVG for the favicon and feed bullets: change
  both.
- Reduced motion parks the turtles mid-ground. Hearts still play, since they answer a
  click.
