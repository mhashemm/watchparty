package main

import (
	"cmp"
	"image"
	"strconv"
	"strings"
	"time"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/x/richtext"
	"gioui.org/x/styledtext"

	"github.com/mhashemm/watchparty/gui/art"
	"github.com/mhashemm/watchparty/mpv"
)

// about is what About shows: this build, an update, and the tools it found.
type about struct {
	version string
	// latest is a newer release, if there is one; installed, one installed and waiting
	latest, installed string
	tools             []tool
	storePath         string
	defaultMpv        string
	defaultCacheDir   string
}

// settingsTab is the Settings tab's widgets.
type settingsTab struct {
	relay, mpv, mpvFlags, createKey, pport, cacheDir widget.Editor
	picker, theme                                    widget.Enum
	browseMpv, browseCache, save                     widget.Clickable
	saved                                            string
	savedAt                                          time.Time
	update, restart                                  widget.Clickable
	updating, picking                                bool
	// versionNote replaces the version line: an update's failure, a restart under way
	versionNote string
	steps       map[string]*widget.Bool
	pathText    richtext.InteractiveText
}

func newSettingsTab() *settingsTab {
	t := &settingsTab{steps: map[string]*widget.Bool{}}
	for _, e := range []*widget.Editor{&t.relay, &t.mpv, &t.mpvFlags, &t.createKey, &t.pport, &t.cacheDir} {
		e.SingleLine = true
	}
	t.createKey.Mask = '•'
	t.picker.Value = mpv.PickerMove
	t.theme.Value = "system"
	return t
}

// load fills the fields from the saved settings.
func (t *settingsTab) load(s settings) {
	t.relay.SetText(s.Relay)
	t.mpv.SetText(s.Mpv)
	t.mpvFlags.SetText(s.MpvFlags)
	t.createKey.SetText(s.CreateKey)
	t.pport.SetText(s.PPort)
	t.cacheDir.SetText(s.CacheDir)
	t.picker.Value = s.MpvPicker
	if t.picker.Value == "" {
		t.picker.Value = mpv.PickerMove
	}
	t.theme.Value = cmp.Or(s.Theme, "system")
}

// savedFor is how long "Saved." stays
const savedFor = 3 * time.Second

func (v *view) settingsTab(gtx C) D {
	t := v.prefs
	running := v.now != nil && v.now.running
	if t.save.Clicked(gtx) {
		v.saveSettings(gtx)
	}
	for _, b := range []struct {
		c     *widget.Clickable
		e     *widget.Editor
		field string
	}{{&t.browseMpv, &t.mpv, "mpv"}, {&t.browseCache, &t.cacheDir, "cacheDir"}} {
		if b.c.Clicked(gtx) && v.app != nil && !t.picking {
			t.picking = true
			v.async(func() func(gtx C) {
				path, err := v.app.pick(b.field)
				return func(gtx C) {
					t.picking = false
					if err == nil && path != "" {
						b.e.SetText(path)
					}
					gtx.Execute(focusCmd(b.e))
				}
			})
		}
	}
	if t.saved != "" && gtx.Now.Sub(t.savedAt) >= savedFor {
		t.saved = ""
	}
	if t.saved != "" {
		gtx.Execute(op.InvalidateCmd{At: t.savedAt.Add(savedFor)})
	}
	if t.update.Clicked(gtx) && !running && v.app != nil && !t.updating {
		t.updating = true
		v.async(func() func(gtx C) {
			tag, err := v.app.install()
			return func(gtx C) {
				t.updating = false
				if err != nil {
					t.versionNote = "Update failed: " + strings.TrimSpace(err.Error()) + "."
				} else {
					v.installed(tag)
				}
			}
		})
	}
	if t.restart.Clicked(gtx) && !running {
		v.restartNow()
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx C) D { return v.heading(gtx, "Defaults") }),
		layout.Rigid(func(gtx C) D {
			return v.field(gtx, "Default relay address", func(gtx C) D { return v.th.input(gtx, &t.relay, "ws://example.com:8080", 12, 1, false) }, "Filled in for you on the Party page.")
		}),
		layout.Rigid(func(gtx C) D {
			return v.field(gtx, "mpv path", func(gtx C) D { return v.browseRow(gtx, &t.mpv, v.about.defaultMpv, &t.browseMpv) }, "")
		}),
		layout.Rigid(func(gtx C) D {
			return v.field(gtx, "Extra mpv options", func(gtx C) D { return v.th.input(gtx, &t.mpvFlags, "--fullscreen --volume=70", 12, 1, false) }, "")
		}),
		layout.Rigid(func(gtx C) D {
			return v.field(gtx, "Theme", func(gtx C) D {
				d := v.th.optionGrid(gtx, &t.theme, 3,
					[3]string{"system", "System", "As your computer is"},
					[3]string{"light", "Light", "Forest by day"},
					[3]string{"dark", "Dark", "Soil at night"})
				if t.theme.Value != cmp.Or(v.settings.Theme, "system") {
					v.pickTheme(gtx, t.theme.Value)
				}
				return d
			}, "")
		}),
		layout.Rigid(func(gtx C) D {
			return v.field(gtx, "Reaction picker on mpv", func(gtx C) D {
				return v.th.optionGrid(gtx, &t.picker, 2,
					[3]string{mpv.PickerMove, "When the mouse moves", "Anywhere on mpv"},
					[3]string{mpv.PickerCorner, "Near its corner", "When the mouse moves to the bottom right"},
					[3]string{mpv.PickerOff, "Never", "The one beside Send always shows"})
			}, "The smiley in mpv's bottom-right corner, which opens the reactions.")
		}),
		layout.Rigid(func(gtx C) D {
			return pair(gtx,
				func(gtx C) D {
					return v.field(gtx, "Room create key", func(gtx C) D { return v.th.input(gtx, &t.createKey, "Only to start a party on a relay", 12, 1, false) }, "")
				},
				func(gtx C) D {
					return v.field(gtx, "Public port", func(gtx C) D { return v.th.input(gtx, &t.pport, "6969", 12, 1, false) }, "")
				})
		}),
		layout.Rigid(func(gtx C) D {
			return v.field(gtx, "Cache folder", func(gtx C) D { return v.browseRow(gtx, &t.cacheDir, v.about.defaultCacheDir, &t.browseCache) }, "Where torrents download.")
		}),
		layout.Rigid(func(gtx C) D {
			return baselineRow(gtx,
				func(gtx C) D { return v.th.button(gtx, &t.save, bPlain, false, "Save settings") },
				func(gtx C) D {
					if t.saved == "" {
						return D{}
					}
					// the space between them in the page's markup, then the span's 10dp
					return layout.Inset{Left: 10}.Layout(gtx, func(gtx C) D { return v.th.text(gtx, tHint, "\u00a0"+t.saved) })
				})
		}),
		layout.Rigid(func(gtx C) D {
			return layout.Inset{Top: 26}.Layout(gtx, func(gtx C) D { return v.aboutSection(gtx, running) })
		}),
	)
}

// browseRow is a field and its Browse button, as tall as the field.
func (v *view) browseRow(gtx C, e *widget.Editor, hint string, browse *widget.Clickable) D {
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, func(gtx C) D { return v.th.input(gtx, e, hint, 12, 1, false) }),
		layout.Rigid(layout.Spacer{Width: 8}.Layout),
		layout.Rigid(func(gtx C) D {
			gtx.Constraints.Min.Y = v.th.lineBox(gtx, tBody).height + gtx.Dp(24)
			return v.th.button(gtx, browse, bPlain, false, "Browse")
		}),
	)
}

// pickTheme applies a theme as it is picked, without Save, and remembers it. The next frame
// paints it: this one is half drawn in the palette it started with.
func (v *view) pickTheme(gtx C, theme string) {
	v.settings.Theme = theme
	v.themed()
	gtx.Execute(op.InvalidateCmd{})
	if v.app != nil {
		if err := v.app.saveTheme(theme); err != nil {
			v.prefs.saved, v.prefs.savedAt = "Couldn't save: "+strings.TrimSpace(err.Error()), gtx.Now
		}
	}
}

// themed sets dark from the theme picked in Settings, or the OS's appearance under System.
func (v *view) themed() {
	switch v.settings.Theme {
	case "light":
		v.dark = false
	case "dark":
		v.dark = true
	default:
		v.dark = v.osDark
	}
}

func (v *view) saveSettings(gtx C) {
	t := v.prefs
	s := v.settings
	s.Relay, s.Mpv, s.MpvFlags = strings.TrimSpace(t.relay.Text()), strings.TrimSpace(t.mpv.Text()), strings.TrimSpace(t.mpvFlags.Text())
	s.CreateKey, s.PPort, s.CacheDir = strings.TrimSpace(t.createKey.Text()), strings.TrimSpace(t.pport.Text()), strings.TrimSpace(t.cacheDir.Text())
	s.MpvPicker = t.picker.Value
	t.savedAt = gtx.Now
	if v.app != nil {
		if err := v.app.saveSettings(s); err != nil {
			t.saved = "Couldn't save: " + strings.TrimSpace(err.Error())
			return
		}
	}
	t.saved = "Saved."
	mpvChanged := s.Mpv != v.settings.Mpv
	v.settings = s
	// the Party page's relay, if it has none yet
	if v.party.relay.Text() == "" {
		v.party.relay.SetText(s.Relay)
	}
	if mpvChanged {
		v.checkTools()
	}
}

// installed is an update installed: About says so, and the dialog offers the restart.
func (v *view) installed(tag string) {
	v.about.installed = tag
	v.updateDot = true
	v.prefs.versionNote = ""
	v.dialog = &modal{title: "Update installed", text: "Version " + tag + " is installed. A restart is required to start using it.", later: "Restart later", now: "Restart now", do: v.restartNow}
}

func (v *view) restartNow() {
	if v.app == nil {
		return
	}
	if err := v.app.restart(); err != nil {
		v.prefs.versionNote = "Restart failed: " + strings.TrimSpace(err.Error()) + "."
		return
	}
	v.prefs.versionNote = "Restarting into " + v.about.installed + "."
}

// versionText is About's version line, or what replaced it.
func (v *view) versionText() string {
	t, a := v.prefs, v.about
	switch {
	case t.versionNote != "":
		return t.versionNote
	case a.installed != "":
		return a.installed + " is installed. Restart the app to start using it."
	case a.version == "dev":
		return "Development build"
	}
	return "Version " + a.version
}

// aboutButton is About's button beside the version: Restart now, Update to …, or none.
func (v *view) aboutButton() string {
	t, a := v.prefs, v.about
	switch {
	case a.installed != "" && t.versionNote == "":
		return "Restart now"
	case a.latest != "" && a.installed == "":
		if t.updating {
			return "Updating…"
		}
		return "Update to " + a.latest
	}
	return ""
}

var tCode = textStyle{font: monoFont, size: 13.6, color: art.Soil, lineHeight: 1.5, anywhere: true}

// aboutSection is About: the version and its update, the tools, and where things are kept.
func (v *view) aboutSection(gtx C, running bool) D {
	t, a := v.prefs, v.about
	version, button := v.versionText(), v.aboutButton()
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx C) D { return dashed(gtx, gtx.Constraints.Max.X) }),
		layout.Rigid(func(gtx C) D {
			return layout.Inset{Top: 18}.Layout(gtx, func(gtx C) D { return v.heading(gtx, "About") })
		}),
		layout.Rigid(func(gtx C) D {
			return layout.Inset{Bottom: 10}.Layout(gtx, func(gtx C) D {
				return baselineRow(gtx,
					func(gtx C) D { return v.th.text(gtx, tBody, version+" ") },
					func(gtx C) D {
						switch button {
						case "":
							return D{}
						case "Restart now":
							return layout.Inset{Left: 6}.Layout(gtx, func(gtx C) D { return v.th.button(gtx, &t.restart, bSmall, running, button) })
						}
						return layout.Inset{Left: 6}.Layout(gtx, func(gtx C) D { return v.th.button(gtx, &t.update, bSmall, running || t.updating, button) })
					})
			})
		}),
		layout.Rigid(func(gtx C) D {
			if len(a.tools) == 0 {
				return D{}
			}
			// ul margin 14 0, the paragraph's 10 above folding into it
			return layout.Inset{Top: 4, Bottom: 14}.Layout(gtx, func(gtx C) D { return v.toolList(gtx, a.tools) })
		}),
		layout.Rigid(func(gtx C) D {
			// .about p: the paragraph's margins, not a hint's
			return layout.Inset{Bottom: 10}.Layout(gtx, func(gtx C) D {
				gtx.Constraints.Min.X = 0
				return v.hintWithCode(gtx, &t.pathText, "Your name, settings and party history are saved on this computer in ", a.storePath)
			})
		}),
	)
}

// toolList is mpv, ffmpeg and yt-dlp (on Windows and macOS): each one's version, where it is,
// and what to do when it is missing, its install steps behind an "i".
func (v *view) toolList(gtx C, tools []tool) D {
	var kids []layout.FlexChild
	for _, x := range tools {
		open := v.prefs.steps[x.Name]
		if open == nil {
			open = new(widget.Bool)
			v.prefs.steps[x.Name] = open
		}
		open.Update(gtx)
		kids = append(kids, layout.Rigid(rule), layout.Rigid(func(gtx C) D {
			return layout.Inset{Top: 10, Bottom: 10}.Layout(gtx, func(gtx C) D {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(func(gtx C) D { return v.th.text(gtx, tBold, x.Name) }),
							layout.Rigid(func(gtx C) D {
								if len(x.Steps) == 0 {
									return D{}
								}
								return layout.Inset{Left: 6}.Layout(gtx, func(gtx C) D { return v.infoButton(gtx, open) })
							}),
							layout.Flexed(1, func(gtx C) D {
								return layout.E.Layout(gtx, func(gtx C) D { return v.th.text(gtx, tHint, x.Version) })
							}),
						)
					}),
					layout.Rigid(func(gtx C) D {
						if x.Path == "" {
							return D{}
						}
						return layout.Inset{Top: 2}.Layout(gtx, func(gtx C) D { return v.th.text(gtx, tCode, x.Path) })
					}),
					layout.Rigid(func(gtx C) D {
						if x.Update == "" {
							return D{}
						}
						return layout.Inset{Top: 2}.Layout(gtx, func(gtx C) D { return v.th.text(gtx, tHint, x.Update) })
					}),
					layout.Rigid(func(gtx C) D {
						if !open.Value || len(x.Steps) == 0 {
							return D{}
						}
						return layout.Inset{Top: 2 + 6}.Layout(gtx, func(gtx C) D { return v.steps(gtx, x.Steps) })
					}),
				)
			})
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}

// infoButton is the round "i" that shows or hides a tool's install steps.
func (v *view) infoButton(gtx C, open *widget.Bool) D {
	return open.Layout(gtx, func(gtx C) D {
		sz := gtx.Dp(20)
		rrect(gtx, image.Rect(0, 0, sz, sz), sz/2, art.Bark)
		rrect(gtx, image.Rect(0, 0, sz, sz).Inset(gtx.Dp(2)), sz/2, art.Paper)
		g := gtx
		g.Constraints = layout.Exact(image.Pt(sz, sz))
		layout.Center.Layout(g, func(gtx C) D {
			return v.th.text(gtx, textStyle{font: displayFont, size: 12, color: art.Bark, lineHeight: 1}, "i")
		})
		return D{Size: image.Pt(sz, sz)}
	})
}

// steps is an <ol>: each step after its number, 20dp in.
func (v *view) steps(gtx C, steps []string) D {
	st := textStyle{font: bodyFont, size: 14, color: art.Forest, lineHeight: 1.5}
	var kids []layout.FlexChild
	for i, s := range steps {
		kids = append(kids, layout.Rigid(func(gtx C) D {
			return layout.Inset{Top: 2, Bottom: 2}.Layout(gtx, func(gtx C) D {
				m := op.Record(gtx.Ops)
				num := v.th.text(gtx, st, strconv.Itoa(i+1)+". ")
				call := m.Stop()
				// the number hangs in the 20dp margin, its end at the text's start
				o := op.Offset(image.Pt(gtx.Dp(20)-num.Size.X, 0)).Push(gtx.Ops)
				call.Add(gtx.Ops)
				o.Pop()
				return layout.Inset{Left: 20}.Layout(gtx, func(gtx C) D { return v.th.text(gtx, st, s) })
			})
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}

// hintWithCode is a hint whose end is code: the store's folder, in the code's font, broken
// anywhere.
func (v *view) hintWithCode(gtx C, state *richtext.InteractiveText, text, code string) D {
	spans := []richtext.SpanStyle{
		{Font: tHint.font, Size: tHint.size, Color: shade(gtx, tHint.color), Content: text},
		{Font: tCode.font, Size: tCode.size, Color: shade(gtx, tCode.color), Content: code},
	}
	lb := v.th.lineBox(gtx, tHint)
	d := lb.place(gtx, func(gtx C, size, lineHeight unit.Sp) D {
		k := float32(size) / float32(tHint.size)
		sp := append([]richtext.SpanStyle(nil), spans...)
		for i := range sp {
			sp[i].Size = unit.Sp(float32(sp[i].Size) * k)
		}
		rt := richtext.Text(state, v.th.shaper, sp...)
		rt.LineHeight, rt.LineHeightScale = lineHeight, 1
		rt.WrapPolicy = styledtext.WrapGraphemes
		m := op.Record(gtx.Ops)
		d := rt.Layout(gtx)
		call := m.Stop()
		call.Add(gtx.Ops)
		o := paint.PushOpacity(gtx.Ops, inkAgain)
		call.Add(gtx.Ops)
		o.Pop()
		return d
	}, tHint.size)
	d.Size.Y -= lb.height - lb.glyphs
	return d
}
