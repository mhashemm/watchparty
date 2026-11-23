-- watchparty's reactions: a burst of an emoji across mpv's screen, drawn here frame by frame,
-- so the node sends one message for a whole burst; and the picker people choose one from,
-- a button that shows while the mouse moves and opens a row of them in the middle.

-- art is each emoji's shapes, by code point: emoji.json's, which mpv/react.go puts at the end
-- of this file
local art = {}
-- art_order is the picker's order
local art_order = {}

-- a burst: copies of the emoji fly from the centre to the edges, growing, and fade out
local copies, lasts, stagger = 16, 1.8, 0.25
local from_size, to_size = 40, 120
-- the most copies on screen at once, from all bursts
local most = 64

-- the picker's, below: rested_until is when this node's person may react again; resting starts
-- a rest; render draws the picker
local rested_until = 0
local resting = function(_) end
local render

-- unseeded, every mpv's first burst would fly the same way
math.randomseed(os.time())

local ov = mp.create_osd_overlay("ass-events")
local flying = {}
local timer

local function screen()
  local d = mp.get_property_native("osd-dimensions")
  if d and d.w and d.w > 0 and d.h > 0 then
    return d.w, d.h
  end
  return 1280, 720
end

local function draw(code, x, y, size, turn, alpha)
  local s = size / 36 * 100
  local out = {}
  for _, shape in ipairs(art[code]) do
    out[#out + 1] = string.format("{\\an7\\pos(%.1f,%.1f)\\org(%.1f,%.1f)\\frz%.1f\\fscx%.1f\\fscy%.1f\\bord0\\shad0\\alpha&H%02X&\\1c%s\\p4}%s",
      x - size / 2, y - size / 2, x, y, turn, s, s, alpha, shape[1], shape[2])
  end
  return table.concat(out, "\n")
end

local function frame()
  local now = mp.get_time()
  local w, h = screen()
  local reach = math.sqrt(w * w + h * h) / 2 + to_size
  local events = {}
  local left = {}
  for _, c in ipairs(flying) do
    local t = (now - c.start) / lasts
    if t < 1 then
      left[#left + 1] = c
      if t >= 0 then
        -- fast out of the centre, slowing towards the edges
        local eased = 1 - (1 - t) ^ 2
        local size = from_size + (to_size - from_size) * eased
        local x = w / 2 + math.cos(c.angle) * reach * eased * c.speed
        local y = h / 2 + math.sin(c.angle) * reach * eased * c.speed
        local alpha = t < 0.6 and 0 or math.floor((t - 0.6) / 0.4 * 255)
        events[#events + 1] = draw(c.code, x, y, size, c.spin * t, alpha)
      end
    end
  end
  flying = left
  ov.res_x, ov.res_y = w, h
  ov.data = table.concat(events, "\n")
  if #flying == 0 then
    ov:remove()
    timer:kill()
  else
    ov:update()
  end
end

-- 30 frames a second, as most video plays: while paused each frame redraws the whole picture,
-- which with software rendering took a core at 60 and half one at 30 (MPV.md)
timer = mp.add_periodic_timer(1 / 30, frame)
timer:kill()

-- burst sends code's copies flying. rest, on this node's own, is the seconds the picker rests
-- before the person can react again (the node's reactEvery).
local function burst(code, rest)
  if not art[code] then
    return
  end
  if rest then
    resting(tonumber(rest) or 0)
  end
  local now = mp.get_time()
  local turn = math.random() * 2 * math.pi
  for i = 1, copies do
    flying[#flying + 1] = {
      code = code,
      start = now + math.random() * stagger,
      angle = turn + (i + math.random() * 0.6) / copies * 2 * math.pi,
      speed = 0.75 + math.random() * 0.35,
      spin = (math.random() * 2 - 1) * 40,
    }
  end
  while #flying > most do
    table.remove(flying, 1)
  end
  if not timer:is_enabled() then
    timer:resume()
  end
end

mp.register_script_message("watchparty-react", burst)

-- The picker, as the person set it (GUI Settings): it shows when the mouse moves, when it moves
-- near its corner, or never: script-opts watchparty_reactions-picker=move|corner|off as mpv
-- starts, then watchparty-picker <mode> as they change it. Not with mpv's own controls: mpv
-- tells no script when they show (MPV.md).
local options = { picker = "move" }
require("mp.options").read_options(options, "watchparty_reactions")

-- Sizes are for a 720 pixel high screen, scaled to the real one.
local button_size, button_right, button_bottom = 60, 16, 90
local pick_size, pick_gap, pick_pad, pick_most = 52, 12, 14, 9
-- people didn't see the row scrolls: of the next emoji this much peeks in, faded, and each end
-- with more has an arrow this wide (the user's pick: arrows and the fade, A and B)
local pick_peek, pick_arrow = 0.55, 32
-- the button hides once the mouse is still this long; the row closes once the mouse left it
local hide_after, close_after = 2, 0.8
-- the button's icon: an outlined smiley, 24 pixels square at \p4
local icon = "m 96 16 b 140 16 176 52 176 96 176 140 140 176 96 176 52 176 16 140 16 96 16 52 52 16 96 16 " ..
  "m 96 32 b 61 32 32 61 32 96 32 131 61 160 96 160 131 160 160 131 160 96 160 61 131 32 96 32 " ..
  "m 80 76 b 80 83 75 88 68 88 61 88 56 83 56 76 56 69 61 64 68 64 75 64 80 69 80 76 " ..
  "m 136 76 b 136 83 131 88 124 88 117 88 112 83 112 76 112 69 117 64 124 64 131 64 136 69 136 76 " ..
  "m 56 108 b 56 130 74 148 96 148 118 148 136 130 136 108 l 120 108 b 120 121 109 132 96 132 83 132 72 121 72 108 l 56 108"

local ui = mp.create_osd_overlay("ass-events")
ui.z = 1
local mouse = { x = -1, y = -1 }
local shown, open, scrolled, hovering = false, false, 0, nil
local moved, left = 0, nil
local grabbed = false
local hider

local function panel(x, y, w, h, r, colour, alpha)
  local k = r * 0.45
  return string.format("{\\an7\\pos(0,0)\\bord0\\shad0\\1c%s\\1a&H%02X&\\p1}m %.1f %.1f l %.1f %.1f b %.1f %.1f %.1f %.1f %.1f %.1f l %.1f %.1f b %.1f %.1f %.1f %.1f %.1f %.1f l %.1f %.1f b %.1f %.1f %.1f %.1f %.1f %.1f l %.1f %.1f b %.1f %.1f %.1f %.1f %.1f %.1f",
    colour, alpha, x + r, y, x + w - r, y, x + w - k, y, x + w, y + k, x + w, y + r, x + w, y + h - r,
    x + w, y + h - k, x + w - k, y + h, x + w - r, y + h, x + r, y + h, x + k, y + h, x, y + h - k, x, y + h - r,
    x, y + r, x, y + k, x + k, y, x + r, y)
end

-- layout is where the button and the row are on a w by h screen.
local function layout()
  local w, h = screen()
  local u = h / 720
  local b = { size = button_size * u }
  b.x, b.y = w - (button_right * u) - b.size, h - (button_bottom * u) - b.size
  local size, gap, pad = pick_size * u, pick_gap * u, pick_pad * u
  local peek = size * pick_peek + gap
  local n = math.max(1, math.min(pick_most, #art_order, math.floor((w - 2 * pad - 32 * u + gap - peek) / (size + gap))))
  if n == #art_order then
    peek = 0
  end
  local r = { size = size, gap = gap, pad = pad, shown = n, arrow = pick_arrow * u }
  r.w, r.h = 2 * pad + n * size + (n - 1) * gap + peek, size + 2 * pad
  r.x, r.y = (w - r.w) / 2, (h - r.h) / 2
  -- scrolled to the end, the last emoji ends at the row's
  r.most = math.max(0, (#art_order - n) * (size + gap) - peek)
  return w, h, u, b, r
end

local function inside(x, y, w, h)
  return mouse.x >= x and mouse.x < x + w and mouse.y >= y and mouse.y < y + h
end

-- arrows is which of the row's ends have more: back, on.
local function arrows(r)
  return scrolled > 0, scrolled < r.most
end

-- under is what the mouse is over: "button", an arrow ("back" or "on"), an emoji's code, "row"
-- between them, or nil.
local function under()
  local _, _, _, b, r = layout()
  if shown and inside(b.x, b.y, b.size, b.size) then
    return "button"
  end
  if open then
    local back, on = arrows(r)
    local cy = r.y + r.h / 2
    local function near(cx)
      return (mouse.x - cx) ^ 2 + (mouse.y - cy) ^ 2 <= (r.arrow / 2) ^ 2
    end
    if back and near(r.x) then
      return "back"
    elseif on and near(r.x + r.w) then
      return "on"
    end
  end
  if open and inside(r.x, r.y, r.w, r.h) then
    local at = mouse.x - r.x - r.pad + scrolled
    local i = math.floor(at / (r.size + r.gap))
    if inside(r.x + r.pad, r.y + r.pad, r.w - 2 * r.pad, r.size) and at - i * (r.size + r.gap) < r.size and art_order[i + 1] then
      return art_order[i + 1]
    end
    return "row"
  end
  return nil
end

local function rests()
  return mp.get_time() < rested_until
end

resting = function(secs)
  rested_until = mp.get_time() + secs
  open = false
  render()
end

render = function()
  if not shown then
    ui:remove()
    return
  end
  local w, h, u, b, r = layout()
  -- resting, the button fades: a pick now would be dropped
  local alpha = rests() and 0xB0 or hovering == "button" and 0x10 or 0x40
  local out = { panel(b.x, b.y, b.size, b.size, 14 * u, "&H1E1A14&", alpha) }
  local s = b.size * 0.6 / 24 * 100
  out[#out + 1] = string.format("{\\an7\\pos(%.1f,%.1f)\\fscx%.1f\\fscy%.1f\\bord0\\shad0\\1c&HDCEFF6&\\1a&H%02X&\\p4}%s",
    b.x + b.size * 0.2, b.y + b.size * 0.2, s, s, rests() and 0x90 or 0, icon)
  if open then
    out[#out + 1] = panel(r.x, r.y, r.w, r.h, 14 * u, "&H1E1A14&", 0x40)
    local clip = string.format("\\clip(%.1f,%.1f,%.1f,%.1f)", r.x + r.pad / 2, r.y, r.x + r.w - r.pad / 2, r.y + r.h)
    for i, code in ipairs(art_order) do
      local x = r.x + r.pad + (i - 1) * (r.size + r.gap) - scrolled
      if x + r.size > r.x and x < r.x + r.w then
        if hovering == code then
          out[#out + 1] = panel(x - r.gap / 2, r.y + r.pad - r.gap / 2, r.size + r.gap, r.size + r.gap, 10 * u, "&HFFFFFF&", 0xB0)
        end
        local k = r.size / 36 * 100
        -- one cut by an end fades: there is more that way
        local cut = x < r.x + r.pad - 0.5 or x + r.size > r.x + r.w - r.pad + 0.5
        for _, shape in ipairs(art[code]) do
          out[#out + 1] = string.format("{\\an7\\pos(%.1f,%.1f)%s\\fscx%.1f\\fscy%.1f\\bord0\\shad0\\1c%s\\1a&H%02X&\\p4}%s",
            x, r.y + r.pad, clip, k, k, shape[1], cut and 0xA0 or 0, shape[2])
        end
      end
    end
    local back, on = arrows(r)
    local cy = r.y + r.h / 2
    local function arrow(cx, name, chevron)
      out[#out + 1] = panel(cx - r.arrow / 2, cy - r.arrow / 2, r.arrow, r.arrow, r.arrow / 2,
        hovering == name and "&HDFEEF3&" or "&HF6FDFF&", 0)
      out[#out + 1] = string.format("{\\an7\\pos(%.1f,%.1f)\\fscx%.1f\\fscy%.1f\\bord0\\shad0\\1c&H304B6E&\\p1}%s",
        cx, cy, 100 * u, 100 * u, chevron)
    end
    if back then
      arrow(r.x, "back", "m 3 -7 l -1 -7 l -7 0 l -1 7 l 3 7 l -3 0")
    end
    if on then
      arrow(r.x + r.w, "on", "m -3 -7 l 1 -7 l 7 0 l 1 7 l -3 7 l 3 0")
    end
  end
  ui.res_x, ui.res_y = w, h
  ui.data = table.concat(out, "\n")
  ui:update()
end

-- grab takes the mouse's buttons and wheel from mpv while it is over the picker, so a click
-- or a turn of the wheel there doesn't pause or seek, and gives them back once it leaves.
local function grab(on)
  if on == grabbed then
    return
  end
  grabbed = on
  local keys = { "MBTN_LEFT", "MBTN_LEFT_DBL", "WHEEL_UP", "WHEEL_DOWN", "WHEEL_LEFT", "WHEEL_RIGHT" }
  for _, key in ipairs(keys) do
    mp.remove_key_binding("watchparty-picker-" .. key)
  end
  if not on then
    return
  end
  -- scroll moves the row by emoji (the wheel), or by a rowful less one (an arrow: by is nil)
  local function scroll(by)
    return function(dir)
      local _, _, _, _, r = layout()
      local step = by or dir * math.max(1, r.shown - 1)
      scrolled = math.max(0, math.min(r.most, scrolled + step * (r.size + r.gap)))
      hovering = under()
      render()
    end
  end
  local acts = {
    MBTN_LEFT = function()
      local what = under()
      if what == "button" and not rests() then
        open, scrolled = not open, 0
      elseif what == "back" or what == "on" then
        scroll(nil)(what == "back" and -1 or 1)
      elseif art[what] then
        mp.commandv("script-message", "watchparty-reacted", what)
        open = false
      end
      hovering = under()
      grab(hovering ~= nil)
      render()
    end,
    WHEEL_UP = scroll(-1), WHEEL_LEFT = scroll(-1), WHEEL_DOWN = scroll(1), WHEEL_RIGHT = scroll(1),
  }
  acts.MBTN_LEFT_DBL = function() end
  for _, key in ipairs(keys) do
    mp.add_forced_key_binding(key, "watchparty-picker-" .. key, acts[key])
  end
end

local function hide()
  if hovering or mp.get_time() - moved < hide_after then
    return
  end
  shown, open = false, false
  grab(false)
  render()
end

local function closing()
  if open and not hovering and left and mp.get_time() - left >= close_after then
    open = false
    render()
  end
end

local faded = false
hider = mp.add_periodic_timer(0.25, function()
  closing()
  hide()
  if faded ~= rests() then
    faded = rests()
    render()
  end
  if not shown then
    hider:kill()
  end
end)
hider:kill()

local function away()
  mouse.x, mouse.y = -1, -1
  shown, open = false, false
  hovering = nil
  grab(false)
  render()
end

mp.register_script_message("watchparty-picker", function(mode)
  options.picker = mode
  if mode == "off" then
    away()
  end
end)

-- wakes is whether a move to the mouse's place shows the button, or keeps it: anywhere, or in
-- the screen's bottom-right quarter.
local function wakes()
  local w, h = screen()
  if options.picker == "corner" then
    return mouse.x >= w / 2 and mouse.y >= h / 2
  end
  return true
end

mp.observe_property("mouse-pos", "native", function(_, pos)
  if not pos or options.picker == "off" then
    return
  end
  if pos.hover == false then
    away()
    return
  end
  if pos.x == mouse.x and pos.y == mouse.y then
    return
  end
  mouse.x, mouse.y = pos.x, pos.y
  local was = hovering
  if wakes() then
    moved = mp.get_time()
    if not shown then
      shown = true
      hider:resume()
    end
  end
  if not shown then
    return
  end
  hovering = under()
  if was and not hovering then
    left = mp.get_time()
  end
  grab(hovering ~= nil)
  render()
end)

mp.observe_property("osd-dimensions", "native", function()
  if shown then
    hovering = under()
    render()
  end
end)
