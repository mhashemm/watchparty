package main

import (
	"image"
	"strings"
	"time"

	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
	"gioui.org/widget"

	"github.com/mhashemm/watchparty/gui/art"
)

// historyActions is what the History tab asks of the app.
type historyActions interface {
	clearHistory() error
}

// historyTab is the History tab's widgets: per party, by when it started, its buttons.
type historyTab struct {
	again map[time.Time]*widget.Clickable
	clear widget.Clickable
	// confirm opens the dialog that asks before clearing
	confirm bool
}

func (t *historyTab) button(started time.Time) *widget.Clickable {
	if t.again == nil {
		t.again = map[time.Time]*widget.Clickable{}
	}
	c := t.again[started]
	if c == nil {
		c = new(widget.Clickable)
		t.again[started] = c
	}
	return c
}

var (
	bContinue = buttonStyle{box: boxStyle{bg: art.Moss, border: art.MossDeep, borderW: 2, radius: 10, inset: layout.Inset{Top: 4, Bottom: 4, Left: 10, Right: 10}}, text: textStyle{font: boldFont, size: 14, color: art.White}, hover: art.MossDeep}
	tVideos   = textStyle{font: bodyFont, size: 14.4, color: art.Forest, lineHeight: 1.5}
)

// historyTab lays out "Recent parties", newest first, each with its videos and a way back in.
func (v *view) historyTab(gtx C) D {
	t := &v.history
	running := v.now != nil && v.now.running
	parties := v.parties
	if t.clear.Clicked(gtx) {
		v.ask(confirm{title: "Clear the whole party history?", ok: "Clear history", do: func() {
			if v.hist != nil {
				v.hist.clearHistory()
			}
		}})
	}
	var kids []layout.FlexChild
	kids = append(kids, layout.Rigid(func(gtx C) D { return v.heading(gtx, "Recent parties") }))
	if len(parties) == 0 {
		// its 6dp above folds into the heading's 10
		kids = append(kids, layout.Rigid(func(gtx C) D {
			return v.th.text(gtx, tHint, "No parties yet. Each one shows up here after you start it, with the video and everyone who joined.")
		}))
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
	}
	for i := len(parties) - 1; i >= 0; i-- {
		p := parties[i]
		c := t.button(p.Started)
		if c.Clicked(gtx) && !running {
			v.resume(p)
		}
		kids = append(kids, layout.Rigid(func(gtx C) D { return v.partyRow(gtx, p, c, running) }))
	}
	kids = append(kids, layout.Rigid(func(gtx C) D {
		return layout.Inset{Top: 18}.Layout(gtx, func(gtx C) D { return v.th.button(gtx, &t.clear, bPlain, false, "Clear history") })
	}))
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}

// partyRow is one party: its file (or folder) in bold, when and with whom under it, Continue or
// Watch again beside both, and the videos it played below, 12dp above and under, a rule last.
func (v *view) partyRow(gtx C, p session, c *widget.Clickable, running bool) D {
	when := p.Started.Local().Format("Jan 2, 2006") + ", " + v.th.clock.format(p.Started.Local())
	who := "just you"
	if len(p.People) > 0 {
		who = "with " + names(p.People)
	}
	how := "over the internet"
	if p.Mode == "direct" {
		how = "peer-to-peer"
	}
	label, style := "", bSmall
	switch {
	case continuable(p, gtx.Now):
		label, style = "Continue", bContinue
	case p.Mode == "direct":
		label = "Watch again"
	}
	hide := ""
	if !p.Playlist {
		hide = fileName(p.File)
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			return layout.Inset{Top: 12, Bottom: 12}.Layout(gtx, func(gtx C) D {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						// grid: 1fr auto, 12dp apart; the button spans both lines, in their middle
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							layout.Flexed(1, func(gtx C) D {
								return fill(gtx, layout.Flex{Axis: layout.Vertical}.Layout(gtx,
									layout.Rigid(func(gtx C) D {
										return v.th.text(gtx, tBold, partyTitle(p, partyFiles(p)))
									}),
									layout.Rigid(layout.Spacer{Height: 2}.Layout),
									layout.Rigid(func(gtx C) D { return v.th.text(gtx, tHint, when+", "+who+", "+how) }),
								))
							}),
							layout.Rigid(func(gtx C) D {
								if label == "" {
									return D{}
								}
								return layout.Inset{Left: 12 + 6}.Layout(gtx, func(gtx C) D { return v.th.button(gtx, c, style, running, label) })
							}),
						)
					}),
					layout.Rigid(func(gtx C) D {
						if len(p.Videos) == 0 {
							return D{}
						}
						return layout.Inset{Top: 2 + 8, Left: 5}.Layout(gtx, func(gtx C) D { return v.videoList(gtx, p.Videos, hide) })
					}),
				)
			})
		}),
		layout.Rigid(rule),
	)
}

// videoList is the videos a party played, each with when, behind a dashed line on the left.
// hide is a name not to repeat: a single file's party has it as its title.
func (v *view) videoList(gtx C, videos []played, hide string) D {
	m := op.Record(gtx.Ops)
	d := layout.Inset{Left: 2 + 12}.Layout(gtx, func(gtx C) D {
		var kids []layout.FlexChild
		for _, x := range videos {
			when := "started " + v.th.clock.format(x.Started.Local())
			if !x.Ended.IsZero() {
				when = v.th.clock.format(x.Started.Local()) + " to " + v.th.clock.format(x.Ended.Local())
			}
			kids = append(kids, layout.Rigid(func(gtx C) D {
				return layout.Inset{Top: 4, Bottom: 4}.Layout(gtx, func(gtx C) D {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx C) D {
							if x.Name == hide {
								return D{}
							}
							return v.th.text(gtx, tVideos, x.Name)
						}),
						layout.Rigid(func(gtx C) D { return v.th.text(gtx, tHint, when) }),
					)
				})
			}))
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
	})
	call := m.Stop()
	// border-left: 2px dashed
	dashedDown(gtx, d.Size.Y)
	call.Add(gtx.Ops)
	return d
}

// resume is Continue or Watch again: the form filled from the party, Continue starting it.
func (v *view) resume(p session) {
	t := v.party
	t.file.SetText(p.File)
	t.playlist.Value = p.Playlist
	v.tab = "party"
	if p.Mode == "direct" {
		t.mode.Value = "direct"
		t.addrs.SetText(p.Addrs)
		t.psecret.SetText(p.Secret)
		return
	}
	t.mode.Value = "relay"
	if p.Relay != "" {
		t.relay.SetText(p.Relay)
	}
	t.room.SetText(p.Room)
	t.secret.SetText(p.Secret)
	if v.act == nil {
		return
	}
	f := t.form(v.settings)
	f.Continue = p.Started.Format(time.RFC3339Nano)
	t.err = ""
	if err := v.act.start(f); err != nil {
		t.err = "Diagnosis: " + strings.TrimSpace(err.Error()) + "."
	}
}

// confirm is a question asked in the window before something that can't be undone.
type confirm struct {
	title, text string
	// ok is the button that does it; Cancel is the other
	ok string
	do func()
}

// ask opens c as the window's dialog.
func (v *view) ask(c confirm) {
	v.dialog = &modal{title: c.title, text: c.text, later: "Cancel", now: c.ok, do: c.do}
}

// modal is the page's <dialog>: a title, a line, and two buttons over a dimmed window.
type modal struct {
	title, text string
	later, now  string
	do          func()
	// nowOff: the button that does it is disabled (Restart now during a party)
	nowOff     bool
	cancel, ok widget.Clickable
	// opened: the first button has had the focus, as showModal gives it
	opened bool
}

var tDialogTitle = tHeading

// paintDialog draws the open dialog, if any, over everything; a click on a button closes it.
func (v *view) paintDialog(gtx C) {
	d := v.dialog
	if d == nil {
		return
	}
	if !d.opened {
		d.opened = true
		gtx.Execute(key.FocusCmd{Tag: &d.cancel})
	}
	// Escape cancels, as a <dialog>'s does
	for {
		ev, ok := gtx.Event(key.Filter{Name: key.NameEscape})
		if !ok {
			break
		}
		if e, ok := ev.(key.Event); ok && e.State == key.Press {
			v.dialog = nil
			return
		}
	}
	if d.cancel.Clicked(gtx) {
		v.dialog = nil
		return
	}
	if d.ok.Clicked(gtx) && !d.nowOff {
		v.dialog = nil
		if d.do != nil {
			d.do()
		}
		return
	}
	W, H := gtx.Constraints.Max.X, gtx.Constraints.Max.Y
	// ::backdrop, rgba(35, 64, 43, .35), and it takes every click under it
	backdrop := art.Forest
	backdrop.A = 89
	rrect(gtx, image.Rect(0, 0, W, H), 0, backdrop)
	blockInput(gtx, image.Pt(W, H))
	w := min(gtx.Dp(420), W-gtx.Dp(32))
	g := gtx
	g.Constraints = layout.Constraints{Min: image.Pt(w, 0), Max: image.Pt(w, H)}
	m := op.Record(gtx.Ops)
	box := boxStyle{bg: art.Paper, border: art.Bark, borderW: 2, radius: 18, shadow: 6, shadowColor: art.Bark, inset: layout.Inset{Top: 22, Bottom: 22, Left: 24, Right: 24}, wide: true}
	size := v.th.box(g, box, func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx C) D { return v.heading(gtx, d.title) }),
			layout.Rigid(func(gtx C) D {
				if d.text == "" {
					return D{}
				}
				return v.th.text(gtx, tBody, d.text)
			}),
			layout.Rigid(func(gtx C) D {
				// its 20dp above: the heading's 10 folds into it when there is no line
				return layout.Inset{Top: unit.Dp(20 - 10*boolDp(d.text == ""))}.Layout(gtx, func(gtx C) D {
					return layout.E.Layout(gtx, func(gtx C) D {
						return layout.Flex{}.Layout(gtx,
							layout.Rigid(func(gtx C) D { return v.th.button(gtx, &d.cancel, bPlain, false, d.later) }),
							layout.Rigid(layout.Spacer{Width: 10}.Layout),
							layout.Rigid(func(gtx C) D { return v.th.button(gtx, &d.ok, bPrimary, d.nowOff, d.now) }),
						)
					})
				})
			}),
		)
	})
	call := m.Stop()
	// in the middle of the window, as a modal dialog is
	o := op.Offset(image.Pt((W-w)/2, (H-size.Size.Y)/2)).Push(gtx.Ops)
	call.Add(gtx.Ops)
	o.Pop()
}

func boolDp(b bool) float32 {
	if b {
		return 1
	}
	return 0
}
