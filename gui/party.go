package main

import (
	"image"
	"image/color"
	"strconv"
	"strings"
	"time"

	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/x/richtext"

	"github.com/mhashemm/watchparty/gui/art"
	"github.com/mhashemm/watchparty/mpv"
	"github.com/mhashemm/watchparty/party"
)

// partyNow is what the Party tab shows of this window's latest party, read each frame.
type partyNow struct {
	// ran: a party started in this window, so vitals show
	ran     bool
	running bool
	scr     screen
	w       *watch
	said    []said
	lines   []string
}

// partyActions is what the Party tab asks of the app.
type partyActions interface {
	start(f form) error
	stop()
	chat(text string) error
	react(code string) error
	pick(field string) (string, error)
	saveChart() (string, error)
}

// partyTab is the Party tab's widgets and what they remember between frames.
type partyTab struct {
	invite, hostname, file, relay, room, secret, addrs, psecret, port widget.Editor
	playlist, local                                                   widget.Bool
	mode                                                              widget.Enum
	browse, goButton                                                  widget.Clickable
	inviteFilled                                                      bool
	err                                                               string

	status, inviteText, chartText widget.Selectable
	detail                        richtext.InteractiveText
	members, chart, saidMore      widget.Bool
	copy, save                    widget.Clickable
	copyLabel                     string
	copiedAt                      time.Time
	saved                         string
	chartList                     widget.List

	chat            widget.Editor
	send, reactOpen widget.Clickable
	stripOpen       bool
	strip           widget.List
	back, more      widget.Clickable
	emoji           []emojiButton
	hint            string
	reactedAt       time.Time
	feed            map[*feedGroup]*widget.Clickable
}

type emojiButton struct {
	code string
	svg  string
	c    widget.Clickable
}

func newPartyTab() *partyTab {
	t := &partyTab{feed: map[*feedGroup]*widget.Clickable{}, copyLabel: "Copy invite"}
	for _, e := range []*widget.Editor{&t.hostname, &t.file, &t.relay, &t.room, &t.secret, &t.addrs, &t.psecret, &t.port} {
		e.SingleLine = true
	}
	t.secret.Mask, t.psecret.Mask = '•', '•'
	t.chat.SingleLine, t.chat.Submit, t.chat.MaxLen = true, true, mpv.ChatLimit
	t.mode.Value = "relay"
	t.local.Value = true
	t.strip.Axis = layout.Horizontal
	t.chartList.Axis = layout.Vertical
	for _, code := range mpv.Reactions() {
		t.emoji = append(t.emoji, emojiButton{code: code, svg: mpv.EmojiSVG(code)})
	}
	return t
}

// form is the Party form as the app starts it.
func (t *partyTab) form(s settings) form {
	f := form{Mode: t.mode.Value, File: t.file.Text(), Hostname: t.hostname.Text(), Relay: t.relay.Text(), Room: t.room.Text(),
		Secret: t.secret.Text(), Local: t.local.Value, Playlist: t.playlist.Value, Port: t.port.Text(), Addrs: t.addrs.Text(),
		CreateKey: s.CreateKey, CacheDir: s.CacheDir, PPort: s.PPort, Mpv: s.Mpv, MpvFlags: s.MpvFlags}
	if f.Mode == "direct" {
		f.Secret = t.psecret.Text()
	}
	return f
}

// fillInvite reads a pasted invite into the fields it names, as the page's input handler did.
func (t *partyTab) fillInvite() {
	mode, fields, playlist, filled := inviteFields(t.invite.Text())
	if mode == "" {
		t.inviteFilled = false
		return
	}
	eds := map[string]*widget.Editor{"relay": &t.relay, "room": &t.room, "secret": &t.secret, "addrs": &t.addrs, "psecret": &t.psecret}
	for k, v := range fields {
		eds[k].SetText(v)
	}
	t.mode.Value = mode
	if filled {
		t.playlist.Value = playlist
	}
	t.inviteFilled = filled
}

// partyTab lays out the Party tab: vitals once a party has started, then the form.
func (v *view) partyTab(gtx C, p *partyNow) D {
	t := v.party
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			if !p.ran {
				return D{}
			}
			return v.vitals(gtx, p)
		}),
		layout.Rigid(func(gtx C) D { return v.partyForm(gtx, t, p.running) }),
	)
}

// field is a .field: its bold label, the control, and an optional hint, 18dp under it all.
func (v *view) field(gtx C, label string, control layout.Widget, hint string) D {
	return layout.Inset{Bottom: 18}.Layout(gtx, func(gtx C) D {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				if label == "" {
					return D{}
				}
				return layout.Inset{Bottom: 6}.Layout(gtx, func(gtx C) D { return v.th.text(gtx, tBold, label) })
			}),
			layout.Rigid(control),
			layout.Rigid(func(gtx C) D {
				if hint == "" {
					return D{}
				}
				return layout.Inset{Top: 6}.Layout(gtx, func(gtx C) D { return v.th.text(gtx, tHint, hint) })
			}),
		)
	})
}

// pair lays out two widgets side by side as .grid does, 12dp apart.
func pair(gtx C, a, b layout.Widget) D {
	return layout.Flex{}.Layout(gtx,
		layout.Flexed(1, a),
		layout.Rigid(layout.Spacer{Width: 12}.Layout),
		layout.Flexed(1, b),
	)
}

func (v *view) partyForm(gtx C, t *partyTab, running bool) D {
	for {
		ev, ok := t.invite.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.ChangeEvent); ok {
			t.fillInvite()
		}
	}
	t.mode.Update(gtx)
	if t.goButton.Clicked(gtx) {
		if running {
			v.ask(confirm{title: "End the party?", text: "mpv closes for you, and everyone else keeps watching.", ok: "End the party", do: func() {
				if v.act != nil {
					v.act.stop()
				}
			}})
		} else if v.act != nil {
			t.err = ""
			if strings.TrimSpace(t.file.Text()) == "" {
				t.err = "Pick a video first."
				gtx.Execute(focusCmd(&t.file))
			} else if err := v.act.start(t.form(v.settings)); err != nil {
				t.err = "Diagnosis: " + err.Error() + "."
			}
		}
	}
	if t.browse.Clicked(gtx) && v.act != nil {
		if path, err := v.act.pick("file"); err != nil {
			t.err = "No file picker found on this computer. Type or paste the full path instead."
		} else if path != "" {
			t.file.SetText(path)
		}
		gtx.Execute(focusCmd(&t.file))
	}
	var kids []layout.FlexChild
	if !running {
		kids = append(kids,
			layout.Rigid(func(gtx C) D {
				hint := ""
				if t.inviteFilled {
					hint = "Filled in from the invite. Now pick your video."
				}
				return v.field(gtx, "Got an invite? Paste it here", func(gtx C) D {
					// a textarea is inline: its line keeps the text's descent under it
					return layout.Inset{Bottom: unit.Dp(float32(v.th.lineBox(gtx, tBody).below) / gtx.Metric.PxPerDp)}.Layout(gtx, func(gtx C) D {
						return v.th.input(gtx, &t.invite, "-relay ws://… -room … -secret …   or   -addrs 192.168.1.5:6969 -secret …", 12, 2, false)
					})
				}, hint)
			}),
			layout.Rigid(func(gtx C) D {
				return v.field(gtx, "Your name", func(gtx C) D { return v.th.input(gtx, &t.hostname, "Shown to everyone when you pause", 12, 1, false) }, "")
			}),
			layout.Rigid(func(gtx C) D {
				return v.field(gtx, "File", func(gtx C) D {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx C) D {
							return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
								layout.Flexed(1, func(gtx C) D {
									return v.th.input(gtx, &t.file, "Path to a video, .torrent or magnet link", 12, 1, false)
								}),
								layout.Rigid(layout.Spacer{Width: 8}.Layout),
								layout.Rigid(func(gtx C) D {
									// as tall as the field beside it, as a flex row stretches it
									gtx.Constraints.Min.Y = v.th.lineBox(gtx, tBody).height + gtx.Dp(24)
									return v.th.button(gtx, &t.browse, bPlain, false, "Browse")
								}),
							)
						}),
						layout.Rigid(func(gtx C) D {
							return layout.Inset{Top: 10}.Layout(gtx, func(gtx C) D {
								return v.th.check(gtx, &t.playlist, "Also load the other videos in this folder")
							})
						}),
					)
				}, "")
			}),
			layout.Rigid(func(gtx C) D {
				return v.field(gtx, "How are you connecting?", func(gtx C) D {
					return v.th.options(gtx, &t.mode,
						[3]string{"relay", "Over the internet", "Through a relay, works from anywhere"},
						[3]string{"direct", "Peer-to-Peer", "Straight to your friends, no relay"})
				}, "")
			}),
			layout.Rigid(func(gtx C) D {
				if t.mode.Value == "direct" {
					return v.directFields(gtx, t)
				}
				return v.relayFields(gtx, t)
			}),
		)
	}
	kids = append(kids,
		layout.Rigid(func(gtx C) D {
			if t.err == "" {
				return D{}
			}
			return layout.Inset{Bottom: 14}.Layout(gtx, func(gtx C) D {
				return v.th.box(gtx, boxStyle{bg: badBg, radius: 12, inset: layout.Inset{Top: 10, Bottom: 10, Left: 12, Right: 12}, wide: true}, func(gtx C) D {
					return v.th.text(gtx, tBody.with(art.Berry), t.err)
				})
			})
		}),
		// its 6dp margin above shows under the fields; with them hidden it folds into vitals' 24
		layout.Rigid(func(gtx C) D {
			if running {
				return v.th.button(gtx, &t.goButton, bStop, false, "End the party")
			}
			return layout.Inset{Top: 6}.Layout(gtx, func(gtx C) D { return v.th.button(gtx, &t.goButton, bGo, false, "Start the party") })
		}),
	)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}

func (v *view) relayFields(gtx C, t *partyTab) D {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			return v.field(gtx, "Relay address", func(gtx C) D { return v.th.input(gtx, &t.relay, "ws://example.com:8080", 12, 1, false) }, "")
		}),
		layout.Rigid(func(gtx C) D {
			return pair(gtx,
				func(gtx C) D {
					return v.field(gtx, "Room", func(gtx C) D { return v.th.input(gtx, &t.room, "New room if empty", 12, 1, false) }, "")
				},
				func(gtx C) D {
					return v.field(gtx, "Secret", func(gtx C) D { return v.th.input(gtx, &t.secret, "New secret if empty", 12, 1, false) }, "")
				})
		}),
		layout.Rigid(func(gtx C) D {
			// margin: -10px 0 18px, under the fields' 18: grid items' margins add up
			return layout.Inset{Top: -10, Bottom: 18}.Layout(gtx, func(gtx C) D {
				return v.th.text(gtx, tHint, "Leave room and secret empty to open a new party, then share the invite.")
			})
		}),
	)
}

func (v *view) directFields(gtx C, t *partyTab) D {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx C) D {
			// a label's 6dp under it: in a grid item, it adds to the field's 18
			return v.field(gtx, "", func(gtx C) D {
				return layout.Inset{Bottom: 6}.Layout(gtx, func(gtx C) D { return v.th.check(gtx, &t.local, "We're on the same Wi-Fi") })
			}, "")
		}),
		layout.Rigid(func(gtx C) D {
			return v.field(gtx, "Friend's address", func(gtx C) D { return v.th.input(gtx, &t.addrs, "Leave empty if you're first", 12, 1, false) }, "")
		}),
		layout.Rigid(func(gtx C) D {
			return pair(gtx,
				func(gtx C) D {
					return v.field(gtx, "Secret", func(gtx C) D { return v.th.input(gtx, &t.psecret, "New secret if empty", 12, 1, false) }, "")
				},
				func(gtx C) D {
					return v.field(gtx, "Your port", func(gtx C) D { return v.th.input(gtx, &t.port, "6969", 12, 1, false) }, "")
				})
		}),
	)
}

// heading is an h2: Gluten 20, bark, 10dp above what follows.
func (v *view) heading(gtx C, s string) D {
	return layout.Inset{Bottom: 10}.Layout(gtx, func(gtx C) D { return v.th.text(gtx, tHeading, s) })
}

var (
	paleBg    = art.Hex("#f3eedf") // .rows, .compare, .share
	stripBg   = art.Hex("#fffdf6")
	barBg     = art.Hex("#e8e6d2")
	trailGrey = art.Hex("#d9d5bd")
	chartBg   = art.Hex("#22301f")
	chartInk  = art.Hex("#d5e2b8")
	monoFont  = font.Font{Typeface: "monospace"}
)

// vitals is the party's own section: how it is going, who's in, the invite, chat, feed and log.
func (v *view) vitals(gtx C, p *partyNow) D {
	t, s := v.party, p.scr
	return layout.Inset{Bottom: 24}.Layout(gtx, func(gtx C) D {
		m := op.Record(gtx.Ops)
		d := layout.Inset{Bottom: 6}.Layout(gtx, func(gtx C) D {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx C) D {
					return layout.Inset{Bottom: 10}.Layout(gtx, func(gtx C) D {
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(func(gtx C) D { return v.th.text(gtx, tHeading, "Vitals") }),
							layout.Rigid(layout.Spacer{Width: 10}.Layout),
							layout.Rigid(func(gtx C) D { return v.ecg(gtx, s.alive) }),
						)
					})
				}),
				layout.Rigid(func(gtx C) D { return v.trail(gtx, s.trail) }),
				layout.Rigid(func(gtx C) D { return layout.Inset{Bottom: 18}.Layout(gtx, func(gtx C) D { return v.nowLine(gtx, s) }) }),
				layout.Rigid(func(gtx C) D {
					if len(s.members) == 0 {
						return D{}
					}
					return v.memberList(gtx, s.members)
				}),
				layout.Rigid(func(gtx C) D {
					if !s.running || s.invite == "" {
						return D{}
					}
					return v.share(gtx, s.invite)
				}),
				layout.Rigid(func(gtx C) D {
					if !s.chat {
						return D{}
					}
					return v.chatBox(gtx, s.away, p.said)
				}),
				layout.Rigid(func(gtx C) D { return v.heading(gtx, "Party feed") }),
				layout.Rigid(func(gtx C) D { return v.feed(gtx, p.w) }),
				layout.Rigid(func(gtx C) D { return v.patientChart(gtx, t, s.chartOpen, p.lines) }),
			)
		})
		call := m.Stop()
		call.Add(gtx.Ops)
		o := op.Offset(image.Pt(0, d.Size.Y)).Push(gtx.Ops)
		dashed(gtx, gtx.Constraints.Max.X)
		o.Pop()
		d.Size.Y += gtx.Dp(2)
		return d
	})
}

// ecg is the heartbeat beside Vitals: a line drawn over and over while the party is alive.
func (v *view) ecg(gtx C, alive bool) D {
	px := float64(gtx.Dp(1))
	st := art.StrokeStyle(art.Porcini, 2, true)
	items := art.Icon(art.ECG, st, art.Scale(px, px))
	if alive {
		// stroke-dasharray 90, its offset from 180 to 0 every 1.6s
		off := 180 * (1 - float32(mod(v.t(gtx), 1.6)/1.6))
		paintArt(gtx.Ops, items, float32(90*px), float32(90*px), off*float32(px))
	} else {
		paintArt(gtx.Ops, items)
	}
	return D{Size: image.Pt(gtx.Dp(64), gtx.Dp(20))}
}

func mod(a, b float64) float64 { return a - b*float64(int(a/b)) }

// trail is the steps to get in, a mushroom each, joined by dashes, solid once done.
func (v *view) trail(gtx C, stops []trailStop) D {
	if len(stops) == 0 {
		return D{}
	}
	// its 4dp margin above folds into the heading's 10
	return layout.Inset{Bottom: 12}.Layout(gtx, func(gtx C) D {
		W := gtx.Constraints.Max.X
		w := W / len(stops)
		h := 0
		for i, st := range stops {
			cx := w*i + w/2
			if i < len(stops)-1 {
				y, from, to := gtx.Dp(15), cx+gtx.Dp(18), cx+w-gtx.Dp(18)
				o := op.Offset(image.Pt(from, y)).Push(gtx.Ops)
				if st.state == "done" {
					paint.FillShape(gtx.Ops, art.Moss, clip.Rect{Max: image.Pt(to-from, gtx.Dp(2))}.Op())
				} else {
					dashed(gtx, to-from)
				}
				o.Pop()
			}
			c, size, top, style := trailGrey, unit.Dp(30), 0, tSmall.with(art.InkSoft)
			style.size, style.align = 12.8, text.Middle
			switch st.state {
			case "done":
				c = art.Porcini
			case "now":
				c, size, top = art.Berry, 36, -3
				style.color, style.font = art.Forest, boldFont
			}
			o := op.Offset(image.Pt(cx-gtx.Dp(size)/2, gtx.Dp(unit.Dp(top)))).Push(gtx.Ops)
			icon(gtx, art.TrailMushroom, art.FillStyle(c), size)
			o.Pop()
			lg := gtx
			lg.Constraints = layout.Constraints{Min: image.Pt(w, 0), Max: image.Pt(w, 1<<20)}
			o = op.Offset(image.Pt(w*i, gtx.Dp(34))).Push(gtx.Ops)
			d := v.th.text(lg, style, st.label)
			o.Pop()
			h = max(h, gtx.Dp(34)+d.Size.Y)
		}
		return D{Size: image.Pt(W, h)}
	})
}

// nowLine is the status line, its detail, the progress bar and a refusal's comparison.
func (v *view) nowLine(gtx C, s screen) D {
	t := v.party
	st := textStyle{font: boldFont, size: 17.6, color: art.Forest, lineHeight: 1.5}
	if s.bad {
		st.color = art.Berry
	}
	detail := s.detail
	if s.refusal != nil && len(s.refusal.Detail) > 0 {
		detail = nil
		for _, b := range s.refusal.Detail {
			detail = append(detail, bold{b[0], b[1]})
		}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx C) D { return v.th.selectable(gtx, &t.status, st, s.text) }),
		layout.Rigid(func(gtx C) D {
			if len(detail) == 0 {
				return D{}
			}
			return layout.Inset{Top: 4}.Layout(gtx, func(gtx C) D { return v.boldText(gtx, &t.detail, detail, 14.4) })
		}),
		layout.Rigid(func(gtx C) D {
			if s.pct < 0 {
				return D{}
			}
			return layout.Inset{Top: 8}.Layout(gtx, func(gtx C) D {
				return bar(gtx, gtx.Constraints.Max.X, 10, s.pct/100, art.Moss, art.Hex("#5d8a3f"))
			})
		}),
		layout.Rigid(func(gtx C) D {
			if s.refusal == nil {
				return D{}
			}
			return layout.Inset{Top: 10}.Layout(gtx, func(gtx C) D { return v.compare(gtx, s.refusal) })
		}),
	)
}

// boldText is a line of ink-soft text whose bold parts are forest.
func (v *view) boldText(gtx C, state *richtext.InteractiveText, parts []bold, size unit.Sp) D {
	var spans []richtext.SpanStyle
	for _, p := range parts {
		if p.b != "" {
			spans = append(spans, richtext.SpanStyle{Font: boldFont, Size: size, Color: art.Forest, Content: p.b})
		}
		if p.plain != "" {
			spans = append(spans, richtext.SpanStyle{Font: bodyFont, Size: size, Color: art.InkSoft, Content: p.plain})
		}
	}
	gtx.Constraints.Min.X = 0
	lb := v.th.lineBox(gtx, textStyle{font: bodyFont, size: size, lineHeight: 1.5})
	d := lb.place(gtx, func(gtx C, size, lineHeight unit.Sp) D {
		// at the size exact shapes them
		for i := range spans {
			spans[i].Size = size
		}
		rt := richtext.Text(state, v.th.shaper, spans...)
		rt.LineHeight, rt.LineHeightScale = lineHeight, 1
		// a second coat, as ink gives the other text: richtext takes colours, not a material
		m := op.Record(gtx.Ops)
		d := rt.Layout(gtx)
		call := m.Stop()
		call.Add(gtx.Ops)
		o := paint.PushOpacity(gtx.Ops, inkAgain)
		call.Add(gtx.Ops)
		o.Pop()
		return d
	}, size)
	// richtext's height is its lines' already, where a Label's ends at the last glyphs
	d.Size.Y -= lb.height - lb.glyphs
	return d
}

// bar is a striped progress bar h dp tall, frac full.
func bar(gtx C, w int, h unit.Dp, frac float64, a, b color.NRGBA) D {
	H := gtx.Dp(h)
	r := gtx.Dp(6)
	frac = max(0, min(1, frac))
	rrect(gtx, image.Rect(0, 0, w, H), r, art.Rule)
	bw := borderPx(gtx, 1.5)
	inner := image.Rect(bw, bw, w-bw, H-bw)
	rrect(gtx, inner, r-bw, barBg)
	fill := inner
	fill.Max.X = inner.Min.X + int(float64(inner.Dx())*frac)
	if fill.Dx() <= 0 {
		return D{Size: image.Pt(w, H)}
	}
	defer clip.UniformRRect(fill, min(r, fill.Dx()/2)).Push(gtx.Ops).Pop()
	paint.ColorOp{Color: a}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	// repeating-linear-gradient(-45deg, a 0 8px, b 8px 16px): bands 8px across, at 45°
	step := float32(gtx.Dp(8)) * 1.41421356
	for x := float32(fill.Min.X) - float32(H) + step; x < float32(fill.Max.X)+float32(H); x += 2 * step {
		var p clip.Path
		p.Begin(gtx.Ops)
		p.MoveTo(f32.Pt(x, float32(H)))
		p.LineTo(f32.Pt(x+step, float32(H)))
		p.LineTo(f32.Pt(x+step+float32(H), 0))
		p.LineTo(f32.Pt(x+float32(H), 0))
		p.Close()
		paint.FillShape(gtx.Ops, b, clip.Outline{Path: p.End()}.Op())
	}
	return D{Size: image.Pt(w, H)}
}

// rows is a rounded pale box of rows with 1dp rules between them (.rows, .compare).
func (v *view) rows(gtx C, n int, row func(gtx C, i int) D) D {
	return v.th.box(gtx, boxStyle{bg: paleBg, border: art.Rule, borderW: 2, radius: 14, wide: true}, func(gtx C) D {
		var kids []layout.FlexChild
		for i := range n {
			if i > 0 {
				kids = append(kids, layout.Rigid(rule))
			}
			kids = append(kids, layout.Rigid(func(gtx C) D { return row(gtx, i) }))
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
	})
}

// compare is a refusal's table: what the party plays beside what this node picked.
func (v *view) compare(gtx C, r *party.NotTheSameFile) D {
	small := textStyle{font: bodyFont, size: 14.4, color: art.Forest, lineHeight: 1.5}
	cell := func(text string, mono bool, st textStyle) layout.Widget {
		return func(gtx C) D {
			if mono && text != "" {
				st.font, st.size = monoFont, 13.6
			}
			return fill(gtx, v.th.text(gtx, st, text))
		}
	}
	line := func(a, b, c layout.Widget) D {
		return layout.Inset{Top: 7, Bottom: 7, Left: 12, Right: 12}.Layout(gtx, func(gtx C) D {
			return layout.Flex{}.Layout(gtx,
				layout.Rigid(func(gtx C) D {
					gtx.Constraints.Min.X, gtx.Constraints.Max.X = gtx.Dp(89), gtx.Dp(89)
					return a(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: 8}.Layout),
				layout.Flexed(1, b),
				layout.Rigid(layout.Spacer{Width: 8}.Layout),
				layout.Flexed(1, c),
			)
		})
	}
	head := small
	head.font, head.color = boldFont, art.Bark
	return v.rows(gtx, 1+len(r.Rows), func(gtx C, i int) D {
		if i == 0 {
			return line(cell("", false, small), cell("The party's", false, head), cell("Yours", false, head))
		}
		x := r.Rows[i-1]
		yours := small
		if x.Differs {
			yours.font, yours.color = boldFont, art.Berry
		}
		return line(cell(x.Label, false, small.with(art.InkSoft)), cell(x.Party, x.Mono, small), cell(x.Yours, x.Mono, yours))
	})
}

// memberList is "In the party, N": a row per member, a slow one's seconds ready under it.
func (v *view) memberList(gtx C, members []memberRow) D {
	t := v.party
	summary := tHeading
	return layout.Inset{Bottom: 18}.Layout(gtx, func(gtx C) D {
		return v.th.disclosure(gtx, &t.members, summary, "In the party, "+strconv.Itoa(len(members)), func(gtx C) D {
			return layout.Inset{Top: 10}.Layout(gtx, func(gtx C) D {
				return v.rows(gtx, len(members), func(gtx C, i int) D {
					m := members[i]
					since := "already here"
					if !m.since.IsZero() {
						since = "since " + v.th.clock.format(m.since)
					}
					return layout.Inset{Top: 9, Bottom: 9, Left: 12, Right: 12}.Layout(gtx, func(gtx C) D {
						return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
							layout.Rigid(func(gtx C) D {
								return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
									layout.Rigid(func(gtx C) D { return iconWith(gtx, art.Shell(i), art.FillStyle(art.Forest), 22, 10) }),
									layout.Flexed(1, func(gtx C) D { return fill(gtx, v.th.text(gtx, tBold, m.name)) }),
									layout.Rigid(layout.Spacer{Width: 10}.Layout),
									layout.Rigid(func(gtx C) D { return v.th.text(gtx, tHint, since) }),
								)
							}),
							layout.Rigid(func(gtx C) D {
								if m.ahead < 0 {
									return D{}
								}
								st := textStyle{font: boldFont, size: 12.8, color: art.Porcini, lineHeight: 1.5}
								// a wrapped line of the row, the row's 10dp gap above it
								return layout.Inset{Top: 10, Left: 32}.Layout(gtx, func(gtx C) D {
									return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
										layout.Rigid(func(gtx C) D { return v.th.text(gtx, st, strconv.Itoa(m.ahead)+"s ready") }),
										layout.Rigid(layout.Spacer{Width: 8}.Layout),
										layout.Flexed(1, func(gtx C) D {
											// full at a minute ready, the longest a member waits between saying so
											return bar(gtx, gtx.Constraints.Max.X, 8, float64(m.ahead)/60, art.Porcini, art.Hex("#b97638"))
										}),
									)
								})
							}),
						)
					})
				})
			})
		})
	})
}

// share is the invite box: the invite to select or copy.
func (v *view) share(gtx C, invite string) D {
	t := v.party
	if t.copy.Clicked(gtx) {
		gtx.Execute(clipboardWrite(invite))
		t.copyLabel, t.copiedAt = "Copied", gtx.Now
	}
	if t.copyLabel != "Copy invite" && gtx.Now.Sub(t.copiedAt) > 2*time.Second {
		t.copyLabel = "Copy invite"
	}
	return layout.Inset{Bottom: 18}.Layout(gtx, func(gtx C) D {
		return v.th.box(gtx, boxStyle{bg: paleBg, border: art.Rule, borderW: 2, radius: 14, inset: layout.UniformInset(12), wide: true}, func(gtx C) D {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx C) D {
					return layout.Inset{Bottom: 6}.Layout(gtx, func(gtx C) D { return v.th.text(gtx, tBold, "Invite to share") })
				}),
				layout.Rigid(func(gtx C) D {
					// its 6dp above folds into the label's 6
					return layout.Inset{Bottom: 10}.Layout(gtx, func(gtx C) D {
						return v.th.selectable(gtx, &t.inviteText, textStyle{font: monoFont, size: 14.4, color: art.Soil, lineHeight: 1.4, anywhere: true}, invite)
					})
				}),
				layout.Rigid(func(gtx C) D { return v.th.button(gtx, &t.copy, bPlain, false, t.copyLabel) }),
			)
		})
	})
}

// reactEvery is how often a person can react (mpv/react.go): the button rests that long
const reactEvery = 2 * time.Second

// saidShown is how many chat messages show before "All messages"
const saidShown = 3

var (
	bPill  = buttonStyle{box: boxStyle{bg: art.Moss, border: art.MossDeep, borderW: 2, radius: 999, inset: layout.Inset{Top: 10, Bottom: 10, Left: 14, Right: 14}}, text: tButton.with(art.White), hover: art.MossDeep, icon: art.Paw, iconSize: 18, iconGap: 6}
	bRound = buttonStyle{box: boxStyle{bg: art.White, border: art.Rule, borderW: 2, radius: 999, inset: layout.UniformInset(9)}, hover: paleBg}
)

// chatBox is "Say something": the box, the reactions and what was said, newest first.
func (v *view) chatBox(gtx C, away bool, said []said) D {
	t := v.party
	for {
		ev, ok := t.chat.Update(gtx)
		if !ok {
			break
		}
		switch ev.(type) {
		case widget.SubmitEvent:
			v.sendChat(gtx)
		case widget.ChangeEvent:
			t.hint = ""
		}
	}
	if t.send.Clicked(gtx) {
		v.sendChat(gtx)
	}
	resting := gtx.Now.Sub(t.reactedAt) < reactEvery
	if t.reactOpen.Clicked(gtx) {
		t.stripOpen = !t.stripOpen
	}
	if away || resting {
		t.stripOpen = false
	}
	if resting {
		gtx.Execute(op.InvalidateCmd{At: t.reactedAt.Add(reactEvery)})
	}
	placeholder := "Shows on everyone's mpv for a few seconds"
	if away {
		placeholder = "Reconnecting to the relay…"
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx C) D { return v.heading(gtx, "Say something") }),
		layout.Rigid(func(gtx C) D {
			return layout.Inset{Bottom: 18}.Layout(gtx, func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx C) D { return v.th.input(gtx, &t.chat, placeholder, 999, 1, away) }),
					layout.Rigid(layout.Spacer{Width: 8}.Layout),
					layout.Rigid(func(gtx C) D {
						s := bRound
						if t.stripOpen {
							s.box.bg = paleBg
						}
						return v.th.iconButton(gtx, &t.reactOpen, s, away || resting, art.Smiley, art.FillStyle(art.Bark), 26)
					}),
					layout.Rigid(layout.Spacer{Width: 8}.Layout),
					layout.Rigid(func(gtx C) D { return v.th.button(gtx, &t.send, bPill, away, "Send") }),
				)
			})
		}),
		layout.Rigid(func(gtx C) D {
			if !t.stripOpen {
				return D{}
			}
			return layout.Inset{Top: -10, Bottom: 14}.Layout(gtx, func(gtx C) D { return v.reactRow(gtx) })
		}),
		layout.Rigid(func(gtx C) D {
			if t.hint == "" {
				return D{}
			}
			return layout.Inset{Top: -12, Bottom: 14, Left: 16}.Layout(gtx, func(gtx C) D { return v.th.text(gtx, tHint, t.hint) })
		}),
		layout.Rigid(func(gtx C) D {
			n := min(len(said), saidShown)
			if n == 0 {
				return D{}
			}
			return layout.Inset{Top: -6, Bottom: 12}.Layout(gtx, func(gtx C) D { return v.saidList(gtx, said, 0, n) })
		}),
		layout.Rigid(func(gtx C) D {
			if len(said) <= saidShown {
				return D{}
			}
			return layout.Inset{Top: -6, Bottom: 18}.Layout(gtx, func(gtx C) D {
				// the summary's 6 and the list's -6 cancel; the list's 12 below folds into the 18
				return v.details(gtx, &t.saidMore, tBold.with(art.Bark), "All messages, "+strconv.Itoa(len(said)), 0, func(gtx C) D {
					return v.saidList(gtx, said, saidShown, len(said))
				})
			})
		}),
	)
}

func (v *view) sendChat(gtx C) {
	t := v.party
	text := t.chat.Text()
	if strings.TrimSpace(text) == "" || v.act == nil {
		return
	}
	if err := v.act.chat(text); err != nil {
		t.hint = "Not sent: " + err.Error()
		return
	}
	t.chat.SetText("")
	t.hint = ""
}

// saidList is messages [from, to) counted from the newest: time, who, and the text or reaction.
func (v *view) saidList(gtx C, said []said, from, to int) D {
	var kids []layout.FlexChild
	for i := from; i < to; i++ {
		m := said[len(said)-1-i]
		if i > from {
			kids = append(kids, layout.Rigid(rule))
		}
		kids = append(kids, layout.Rigid(func(gtx C) D {
			ink, who := art.Forest, art.Bark
			if m.Ours {
				ink, who = art.MossDeep, art.MossDeep
			}
			return layout.Inset{Top: 6, Bottom: 6}.Layout(gtx, func(gtx C) D {
				return layout.Flex{}.Layout(gtx,
					layout.Rigid(func(gtx C) D { return v.timeCell(gtx, time.UnixMilli(m.At)) }),
					layout.Rigid(layout.Spacer{Width: 10}.Layout),
					layout.Rigid(func(gtx C) D { return v.th.text(gtx, tBold.with(who), m.Who) }),
					layout.Rigid(layout.Spacer{Width: 10}.Layout),
					layout.Flexed(1, func(gtx C) D {
						if m.React != "" {
							return layout.Inset{Top: 1}.Layout(gtx, func(gtx C) D { return icon(gtx, mpv.EmojiSVG(m.React), art.FillStyle(art.Forest), 22) })
						}
						return v.th.text(gtx, tBody.with(ink), m.Text)
					}),
				)
			})
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}

// timeCell is a <time>: 14sp ink-soft, at least 3.2em wide, a pixel lower than the text.
func (v *view) timeCell(gtx C, at time.Time) D {
	return layout.Inset{Top: 1}.Layout(gtx, func(gtx C) D {
		gtx.Constraints.Min.X = gtx.Sp(16 * 3.2)
		d := v.th.text(gtx, tHint, v.th.clock.format(at))
		d.Size.X = max(d.Size.X, gtx.Constraints.Min.X)
		return d
	})
}

// reactRow is the reactions in one row that scrolls sideways, an arrow at each end with more.
func (v *view) reactRow(gtx C) D {
	t := v.party
	for i := range t.emoji {
		if t.emoji[i].c.Clicked(gtx) && v.act != nil {
			t.reactedAt = gtx.Now
			t.stripOpen = false
			if err := v.act.react(t.emoji[i].code); err != nil {
				t.hint = "Your reaction wasn't sent: " + err.Error()
			}
		}
	}
	if t.back.Clicked(gtx) {
		t.strip.ScrollBy(-float32(gtx.Constraints.Max.X-gtx.Dp(72)) / float32(gtx.Dp(44)))
	}
	if t.more.Clicked(gtx) {
		t.strip.ScrollBy(float32(gtx.Constraints.Max.X-gtx.Dp(72)) / float32(gtx.Dp(44)))
	}
	return v.th.box(gtx, boxStyle{bg: stripBg, border: art.Rule, borderW: 2, radius: 14, wide: true}, func(gtx C) D {
		d := layout.Inset{Top: 6, Bottom: 6, Left: 8, Right: 8}.Layout(gtx, func(gtx C) D {
			return t.strip.Layout(gtx, len(t.emoji), func(gtx C, i int) D {
				e := &t.emoji[i]
				return layout.Inset{Right: 2}.Layout(gtx, func(gtx C) D {
					return e.c.Layout(gtx, func(gtx C) D {
						pointer.CursorPointer.Add(gtx.Ops)
						// 34 and 4 of padding round it: all: unset made it content-box
						sz := gtx.Dp(42)
						if e.c.Hovered() {
							rrect(gtx, image.Rect(0, 0, sz, sz), gtx.Dp(8), art.Rule)
						}
						o := op.Offset(image.Pt(gtx.Dp(4), gtx.Dp(4))).Push(gtx.Ops)
						icon(gtx, e.svg, art.FillStyle(art.Forest), 34)
						o.Pop()
						return D{Size: image.Pt(sz, sz)}
					})
				})
			})
		})
		pos := t.strip.Position
		v.stripEnds(gtx, d.Size, pos.First > 0 || pos.Offset > 0, pos.First+pos.Count < len(t.emoji) || pos.OffsetLast < 0)
		return d
	})
}

// stripEnds fades each end of the strip that has more and puts its arrow over it.
func (v *view) stripEnds(gtx C, size image.Point, back, more bool) {
	t := v.party
	fade := gtx.Dp(56)
	clear := stripBg
	clear.A = 0
	end := func(left bool, c *widget.Clickable, svg string) {
		x0, x1 := 0, fade
		c0, c1 := stripBg, clear
		if !left {
			x0, x1, c0, c1 = size.X-fade, size.X, clear, stripBg
		}
		r := clip.Rect{Min: image.Pt(x0, gtx.Dp(2)), Max: image.Pt(x1, size.Y-gtx.Dp(2))}.Push(gtx.Ops)
		paint.LinearGradientOp{Stop1: f32.Pt(float32(x0), 0), Color1: c0, Stop2: f32.Pt(float32(x1), 0), Color2: c1}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
		r.Pop()
		sz := gtx.Dp(26)
		x := -gtx.Dp(8) - gtx.Dp(2)
		if !left {
			x = size.X - sz + gtx.Dp(8) - gtx.Dp(2)
		}
		o := op.Offset(image.Pt(x, (size.Y-sz)/2)).Push(gtx.Ops)
		cg := gtx
		cg.Constraints = layout.Exact(image.Pt(sz, sz))
		c.Layout(cg, func(gtx C) D {
			pointer.CursorPointer.Add(gtx.Ops)
			bg := stripBg
			if c.Hovered() {
				bg = paleBg
			}
			rrect(gtx, image.Rect(0, gtx.Dp(1), sz, sz+gtx.Dp(1)), sz/2, color.NRGBA{0x4a, 0x32, 0x20, 0x22})
			rrect(gtx, image.Rect(0, 0, sz, sz), sz/2, art.Rule)
			rrect(gtx, image.Rect(0, 0, sz, sz).Inset(gtx.Dp(2)), sz/2, bg)
			px := float64(gtx.Dp(16)) / 24
			io := op.Offset(image.Pt(gtx.Dp(5), gtx.Dp(5))).Push(gtx.Ops)
			paintArt(gtx.Ops, art.Icon(svg, art.StrokeStyle(art.Bark, 2.6, true), art.Scale(px, px)))
			io.Pop()
			return D{Size: image.Pt(sz, sz)}
		})
		o.Pop()
	}
	if back {
		end(true, &t.back, art.Back)
	}
	if more {
		end(false, &t.more, art.Forward)
	}
}

// details is a <details> with its dashed top rule: margin 6 0 16, 12dp under the rule.
func (v *view) details(gtx C, open *widget.Bool, summary textStyle, title string, gap unit.Dp, body layout.Widget) D {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx C) D { return dashed(gtx, gtx.Constraints.Max.X) }),
		layout.Rigid(func(gtx C) D {
			return layout.Inset{Top: 12}.Layout(gtx, func(gtx C) D {
				return v.th.disclosure(gtx, open, summary, title, func(gtx C) D {
					return layout.Inset{Top: gap}.Layout(gtx, body)
				})
			})
		}),
	)
}

// feed is the party feed: a group per video, the latest open, each item's time and text.
func (v *view) feed(gtx C, w *watch) D {
	t := v.party
	return layout.Inset{Bottom: 18}.Layout(gtx, func(gtx C) D {
		if w == nil || len(w.feed) == 0 {
			return v.th.text(gtx, tBody.with(art.InkSoft), "Nothing yet. Joins, pauses and seeks show up here.")
		}
		var kids []layout.FlexChild
		for _, g := range w.feed {
			c := t.feed[g]
			if c == nil {
				c = new(widget.Clickable)
				t.feed[g] = c
			}
			if c.Clicked(gtx) {
				g.open = !g.open
			}
			kids = append(kids, layout.Rigid(func(gtx C) D {
				bg, border, ink := color.NRGBA{}, art.Rule, art.Bark
				if g.open {
					bg, border, ink = segOn, art.Moss, art.Forest
				}
				// the last one's margin folds into the feed's, as CSS collapses them
				gap := unit.Dp(8)
				if g == w.feed[len(w.feed)-1] {
					gap = 0
				}
				return layout.Inset{Bottom: gap}.Layout(gtx, func(gtx C) D {
					return v.th.box(gtx, boxStyle{bg: bg, border: border, borderW: 1.5, radius: 12, inset: layout.Inset{Top: 10, Bottom: 10, Left: 14, Right: 14}, wide: true}, func(gtx C) D {
						return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
							layout.Rigid(func(gtx C) D {
								return c.Layout(gtx, func(gtx C) D {
									pointer.CursorPointer.Add(gtx.Ops)
									st := tBold.with(ink)
									return baselineRow(gtx,
										func(gtx C) D { return v.th.marker(gtx, g.open, st) },
										func(gtx C) D { return v.th.text(gtx, st, g.name) },
									)
								})
							}),
							layout.Rigid(func(gtx C) D {
								if !g.open {
									return D{}
								}
								return layout.Inset{Top: 6}.Layout(gtx, func(gtx C) D { return v.feedItems(gtx, g.items) })
							}),
						)
					})
				})
			}))
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
	})
}

func (v *view) feedItems(gtx C, items []*feedItem) D {
	if len(items) == 0 {
		return v.th.text(gtx, tBody.with(art.InkSoft), "Nothing yet.")
	}
	var kids []layout.FlexChild
	for i, it := range items {
		if i > 0 {
			kids = append(kids, layout.Rigid(rule))
		}
		kids = append(kids, layout.Rigid(func(gtx C) D {
			return layout.Inset{Top: 6, Bottom: 6}.Layout(gtx, func(gtx C) D {
				return layout.Flex{}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						return layout.Inset{Top: 3}.Layout(gtx, func(gtx C) D { return icon(gtx, art.Mushroom, art.FillStyle(art.Porcini), 16) })
					}),
					layout.Rigid(layout.Spacer{Width: 10}.Layout),
					layout.Rigid(func(gtx C) D { return v.timeCell(gtx, it.at) }),
					layout.Rigid(layout.Spacer{Width: 10}.Layout),
					layout.Flexed(1, func(gtx C) D { return v.th.text(gtx, tBody, it.text) }),
				)
			})
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}

// chartKept is how much of the log the chart shows; Save chart writes all of it
const chartKept = 150000

// patientChart is the log, dark and scrolling, 260dp at most, and Save chart.
func (v *view) patientChart(gtx C, t *partyTab, open bool, lines []string) D {
	if open && !t.chart.Value {
		t.chart.Value = true
	}
	if t.save.Clicked(gtx) && v.act != nil {
		if where, err := v.act.saveChart(); err != nil {
			t.saved = "Couldn't save: " + err.Error()
		} else if where != "" {
			t.saved = "Saved as " + where + "."
		}
	}
	// its 6dp above folds into the feed's 18
	return layout.Inset{Bottom: 16}.Layout(gtx, func(gtx C) D {
		return v.details(gtx, &t.chart, tBold.with(art.Bark), "Patient chart", 14, func(gtx C) D {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx C) D {
					log := strings.Join(lines, "\n")
					if len(log) > chartKept {
						log = log[len(log)-chartKept:]
					}
					return v.th.box(gtx, boxStyle{bg: chartBg, radius: 12, inset: layout.UniformInset(12), wide: true}, func(gtx C) D {
						gtx.Constraints.Max.Y = gtx.Dp(260 - 24)
						return t.chartList.Layout(gtx, 1, func(gtx C, _ int) D {
							return v.th.selectable(gtx, &t.chartText, textStyle{font: monoFont, size: 12.48, color: chartInk, lineHeight: 1.45, anywhere: true}, log)
						})
					})
				}),
				layout.Rigid(func(gtx C) D {
					return layout.Inset{Top: 16}.Layout(gtx, func(gtx C) D {
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(layout.Spacer{Width: 6}.Layout),
							layout.Rigid(func(gtx C) D { return v.th.button(gtx, &t.save, bSmall, false, "Save chart") }),
							layout.Rigid(layout.Spacer{Width: 6}.Layout),
							layout.Flexed(1, func(gtx C) D {
								if t.saved == "" {
									return D{}
								}
								return v.th.text(gtx, tHint, t.saved)
							}),
						)
					})
				}),
			)
		})
	})
}
