package main

import (
	"context"
	"image"
	"log"
	"runtime"
	"strings"
	"time"

	"gioui.org/app"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"

	"github.com/mhashemm/watchparty/gui/art"
)

// window runs the window until it closes: it draws the view, and closes when the app's
// context ends (a signal, a restart).
func (g *gui) window(w *app.Window) error {
	sh, err := newShaper()
	if err != nil {
		return err
	}
	v := newView(&theme{shaper: sh, clock: systemClock()}, time.Now())
	v.app, v.wake = g, w.Invalidate
	g.mu.Lock()
	g.changed = w.Invalidate
	g.mu.Unlock()
	last := g.lastWindow()
	opts := []app.Option{app.Title(windowTitle), app.Size(560, 940)}
	if last.W > 0 && last.H > 0 {
		opts[1] = app.Size(unit.Dp(max(last.W, minW)), unit.Dp(max(last.H, minH)))
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		// the desktop draws the title bar when it will (KDE, COSMIC, any X11 window manager),
		// and ConfigEvent says when it won't (GNOME on Wayland): then the window draws its own
		opts = append(opts, app.Decorated(false))
	}
	w.Option(opts...)
	if last.Maximized {
		w.Option(app.Maximized.Option())
	}
	v.load()
	if e2eHook != nil {
		e2eHook(v)
	}
	// the OS's reduced motion and dark appearance, followed while the window is open
	ctx, cancel := context.WithCancel(g.ctx)
	defer cancel()
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		watchLook(ctx, func(l look) {
			select {
			case v.results <- func(gtx C) { v.still, v.dark = l.still, l.dark }:
				v.wake()
			case <-ctx.Done():
			}
		})
	}()
	go func() {
		<-g.ctx.Done()
		w.Perform(system.ActionClose)
	}()
	var (
		ops op.Ops
		bar titleBar
		own bool
		// left is the window as it is now, saved when it closes; pxPerDp converts its size
		left    windowSize
		pxPerDp float32
		// hwnd is the Windows window's handle; framed, whether its frame was told frameDark
		hwnd      uintptr
		framed    bool
		frameDark bool
	)
	for {
		switch e := w.Event().(type) {
		case app.DestroyEvent:
			if left.W > 0 {
				if err := g.saveWindow(left); err != nil {
					log.Printf("gui: saving the window's size: %v\n", err)
				}
			}
			return e.Err
		case app.ConfigEvent:
			own = !e.Config.Decorated
			bar.deco.Maximized = e.Config.Mode == app.Maximized
			switch e.Config.Mode {
			case app.Windowed:
				// a maximized window's size is the screen's: keep the one it goes back to
				if pxPerDp > 0 {
					left.W, left.H = float32(e.Config.Size.X)/pxPerDp, float32(e.Config.Size.Y)/pxPerDp
				}
				left.Maximized = false
			case app.Maximized:
				left.Maximized = true
			}
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			if pxPerDp == 0 && e.Metric.PxPerDp > 0 {
				pxPerDp = e.Metric.PxPerDp
				if left.W == 0 && !left.Maximized {
					left.W, left.H = float32(e.Size.X)/pxPerDp, float32(e.Size.Y)/pxPerDp
				}
			}
			if own {
				if a := bar.deco.Update(gtx); a != 0 {
					w.Perform(a)
				}
				d := v.titleBar(gtx, &bar, windowTitle)
				op.Offset(image.Pt(0, d.Size.Y)).Add(gtx.Ops)
				gtx.Constraints = layout.Exact(gtx.Constraints.Max.Sub(image.Pt(0, d.Size.Y)))
			}
			v.frame(gtx)
			if hwnd != 0 && (!framed || frameDark != v.dark) {
				darkFrame(hwnd, v.dark)
				framed, frameDark = true, v.dark
			}
			e.Frame(gtx.Ops)
		case app.ViewEvent:
			hwnd, framed = viewHandle(e), false
			viewEvent(e, g.exe)
		}
	}
}

// minW and minH are the smallest a remembered size reopens at, in dp
const minW, minH = 360, 480

// e2eHook starts e2e.go's control socket, in a build with -tags e2e
var e2eHook func(v *view)

// async runs work off the window's goroutine (a dialog, the network), and its result on it,
// in the next frame.
func (v *view) async(work func() func(gtx C)) {
	go func() {
		v.results <- work()
		v.wake()
	}()
}

// sync applies what async work left, then reads what the app has new: the lines into the
// party's watch, its chat, and how it stands.
func (v *view) sync(gtx C) {
	for {
		select {
		case f := <-v.results:
			f(gtx)
			continue
		default:
		}
		break
	}
	if v.app == nil {
		return
	}
	n := v.app.news(v.run, v.from, v.saidFrom)
	if n.Run != v.run {
		v.run, v.now = n.Run, &partyNow{ran: true, w: newWatch(v.started)}
	}
	if v.run == 0 {
		return
	}
	p := v.now
	for _, l := range n.Lines {
		p.w.line(l)
	}
	p.lines = append(p.lines, n.Lines...)
	v.from, v.saidFrom = n.From+len(n.Lines), n.SaidFrom+len(n.Said)
	// chat is only while the party runs: gone from the window at its end
	if n.Running {
		p.said = append(p.said, n.Said...)
		if over := len(p.said) - saidKept; over > 0 {
			p.said = p.said[over:]
		}
	} else {
		p.said = nil
	}
	if p.running && !n.Running {
		v.reload()
	}
	p.running = n.Running
	p.scr = p.w.render(n.Running, n.ExitErr, n.Refusal, v.started.Playlist, gtx.Now)
	v.mood = art.Mood{Awake: !p.scr.sleep, Walking: p.scr.walking}
	// the trail shows once a step has waited: a frame then, as nothing else may come
	if p.w.trailStep != "" && !p.w.trailShown {
		gtx.Execute(op.InvalidateCmd{At: p.w.trailAt.Add(trailAfter)})
	}
}

// startParty starts a party from f: Start the party, or Continue.
func (v *view) startParty(f form) {
	v.started = f
	if err := v.app.start(f); err != nil {
		v.party.err = "Diagnosis: " + strings.TrimSpace(err.Error()) + "."
		return
	}
	v.reload()
}

// load fills the window from the store once it opens, and asks about updates and the tools.
func (v *view) load() {
	st := v.app.state()
	v.settings, v.parties = st.settings, st.history
	t := v.party
	t.hostname.SetText(st.settings.Hostname)
	t.relay.SetText(st.settings.Relay)
	t.port.SetText(st.settings.Port)
	t.local.Value = st.settings.Local
	v.prefs.load(st.settings)
	v.about = about{version: version, storePath: st.path, defaultMpv: st.defaultMpv, defaultCacheDir: st.defaultCacheDir}
	v.checkTools()
	v.async(func() func(gtx C) {
		tag, _ := v.app.latest(context.Background())
		return func(gtx C) {
			if tag != "" && v.about.installed == "" {
				v.about.latest, v.updateDot = tag, true
			}
		}
	})
}

// reload reads the store again: a party began or ended, the history was cleared or shown.
func (v *view) reload() {
	if v.app == nil {
		return
	}
	st := v.app.state()
	v.settings, v.parties = st.settings, st.history
}

// checkTools asks for About's tools; only the latest ask's answer is shown.
func (v *view) checkTools() {
	if v.app == nil {
		return
	}
	v.toolsAsked++
	asked := v.toolsAsked
	v.async(func() func(gtx C) {
		list := v.app.tools(context.Background())
		return func(gtx C) {
			if asked == v.toolsAsked {
				v.about.tools = list
			}
		}
	})
}
