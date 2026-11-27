//go:build e2e

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"image"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gioui.org/widget"
)

// The e2e control socket, built only with -tags e2e: e2e.py drives the window through it as a
// person would, by its buttons and fields, and reads back what the window shows. It listens on
// loopback and writes its address to the file WATCHPARTY_E2E names (Python has no unix sockets
// on Windows); one JSON request a line, one JSON answer a line:
//
//	{"op": "ui"}                          what the window shows (ui)
//	{"op": "log"}                         the Patient chart's text
//	{"op": "click", "name": "go"}         a button, by the names in clickable
//	{"op": "fill", "fields": {"file": …}} fields, by the names in editors; bools are "true"/"false"
//
// Each runs on the window's goroutine at the start of a frame and is answered at its end, so a
// click has been handled when its answer comes, and what it starts asynchronously has not.
func init() { e2eHook = serveControl }

type ctrlReq struct {
	Op     string            `json:"op"`
	Name   string            `json:"name"`
	Fields map[string]string `json:"fields"`
}

type ctrlAns struct {
	OK    any    `json:"ok,omitempty"`
	Error string `json:"error,omitempty"`
}

func serveControl(v *view) {
	path := os.Getenv("WATCHPARTY_E2E")
	if path == "" {
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		// renamed into place, so a reader never sees half an address
		err = os.WriteFile(path+".new", []byte(ln.Addr().String()), 0o600)
	}
	if err == nil {
		err = os.Rename(path+".new", path)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: control socket:", err)
		return
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go v.control(c)
		}
	}()
}

func (v *view) control(c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Buffer(nil, 1<<20)
	enc := json.NewEncoder(c)
	for sc.Scan() {
		var r ctrlReq
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			enc.Encode(ctrlAns{Error: err.Error()})
			continue
		}
		done := make(chan ctrlAns, 1)
		v.results <- func(gtx C) {
			ans, after := v.do(r)
			if ans.Error != "" || after == nil {
				done <- ans
				return
			}
			v.after = append(v.after, func() { done <- ctrlAns{OK: after()} })
		}
		v.wake()
		if enc.Encode(<-done) != nil {
			return
		}
	}
}

// do starts r; after, if any, answers it once the frame is drawn.
func (v *view) do(r ctrlReq) (ctrlAns, func() any) {
	switch r.Op {
	case "ui":
		return ctrlAns{}, func() any { return v.ui() }
	case "log":
		return ctrlAns{}, func() any {
			if v.now == nil {
				return ""
			}
			return strings.Join(v.now.lines, "\n")
		}
	case "click":
		c, err := v.clickable(r.Name)
		if err != nil {
			return ctrlAns{Error: err.Error()}, nil
		}
		c.Click()
		return ctrlAns{}, func() any { return true }
	case "fill":
		for k, val := range r.Fields {
			if err := v.fill(k, val); err != nil {
				return ctrlAns{Error: err.Error()}, nil
			}
		}
		return ctrlAns{}, func() any { return true }
	}
	return ctrlAns{Error: "unknown op " + strconv.Quote(r.Op)}, nil
}

// clickable is a button by name, refused when the window doesn't show it or shows it disabled,
// as a person couldn't press it: a click queued on a hidden button would land whenever it next
// shows, and Click passes a disabled one's gtx.Disabled().
func (v *view) clickable(name string) (*widget.Clickable, error) {
	t, p := v.party, v.prefs
	running := v.now != nil && v.now.running
	scr := screen{}
	if v.now != nil {
		scr = v.now.scr
	}
	shown := func(tab string) error {
		if v.dialog != nil {
			return fmt.Errorf("%s is under the open dialog %q", name, v.dialog.title)
		}
		if v.tab != tab && !(tab == "party" && v.tab == "") {
			return fmt.Errorf("%s is on the %s tab, not %s", name, tab, v.tabShown())
		}
		return nil
	}
	resting := time.Since(t.reactedAt) < reactEvery
	off := func(c *widget.Clickable, disabled bool) (*widget.Clickable, error) {
		if disabled {
			return nil, fmt.Errorf("%s is disabled", name)
		}
		return c, nil
	}
	on := func(tab string, c *widget.Clickable, ok bool) (*widget.Clickable, error) {
		if err := shown(tab); err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%s isn't shown", name)
		}
		return c, nil
	}
	for i, tn := range tabNames {
		if name == "tab:"+tn {
			if v.dialog != nil {
				return nil, fmt.Errorf("the tabs are under the open dialog %q", v.dialog.title)
			}
			return &v.tabs[i], nil
		}
	}
	if kind, i, ok := strings.Cut(name, ":"); ok && (kind == "history" || kind == "react") {
		n, err := strconv.Atoi(i)
		if err != nil {
			return nil, err
		}
		if kind == "react" {
			if n < 0 || n >= len(t.emoji) {
				return nil, fmt.Errorf("no reaction %d", n)
			}
			return on("party", &t.emoji[n].c, scr.chat && t.stripOpen)
		}
		// history:0 is the newest party's button, as the list shows them
		if n < 0 || n >= len(v.parties) {
			return nil, fmt.Errorf("no party %d in the history", n)
		}
		p := v.parties[len(v.parties)-1-n]
		c, err := on("history", v.history.button(p.Started), historyButton(p, time.Now()) != "")
		if err != nil {
			return nil, err
		}
		return off(c, running)
	}
	switch name {
	case "dialog:cancel", "dialog:ok":
		if v.dialog == nil {
			return nil, fmt.Errorf("no dialog is open")
		}
		if name == "dialog:ok" {
			return off(&v.dialog.ok, v.dialog.nowOff)
		}
		return &v.dialog.cancel, nil
	case "tip":
		if v.dialog != nil {
			return nil, fmt.Errorf("the tips button is under the open dialog %q", v.dialog.title)
		}
		return &v.tip, nil
	case "cat":
		if v.dialog != nil {
			return nil, fmt.Errorf("the cat is under the open dialog %q", v.dialog.title)
		}
		return &v.catTap, nil
	case "go":
		return on("party", &t.goButton, true)
	case "browse":
		return on("party", &t.browse, !running)
	case "copy":
		return on("party", &t.copy, running && scr.invite != "")
	case "send":
		c, err := on("party", &t.send, scr.chat)
		if err != nil {
			return nil, err
		}
		return off(c, scr.away)
	case "react-open":
		c, err := on("party", &t.reactOpen, scr.chat)
		if err != nil {
			return nil, err
		}
		return off(c, scr.away || resting)
	case "save-chart":
		return on("party", &t.save, v.now != nil && v.now.ran && t.chart.Value)
	case "clear-history":
		return on("history", &v.history.clear, len(v.parties) > 0)
	case "settings-save":
		return on("settings", &p.save, true)
	case "browse-mpv":
		return on("settings", &p.browseMpv, true)
	case "browse-cacheDir":
		return on("settings", &p.browseCache, true)
	case "update":
		c, err := on("settings", &p.update, v.aboutButton() != "" && v.about.installed == "")
		if err != nil {
			return nil, err
		}
		return off(c, running || p.updating)
	case "restart":
		c, err := on("settings", &p.restart, v.aboutButton() == "Restart now")
		if err != nil {
			return nil, err
		}
		return off(c, running)
	}
	return nil, fmt.Errorf("no button called %q", name)
}

// fill sets a field as typing it would.
func (v *view) fill(name, val string) error {
	t, p := v.party, v.prefs
	eds := map[string]*widget.Editor{
		"invite": &t.invite, "hostname": &t.hostname, "file": &t.file, "relay": &t.relay, "room": &t.room,
		"secret": &t.secret, "addrs": &t.addrs, "psecret": &t.psecret, "port": &t.port, "chat": &t.chat,
		"set-relay": &p.relay, "mpv": &p.mpv, "mpvFlags": &p.mpvFlags, "createKey": &p.createKey,
		"pport": &p.pport, "cacheDir": &p.cacheDir,
	}
	bools := map[string]*widget.Bool{"playlist": &t.playlist, "local": &t.local, "members": &t.members, "chart": &t.chart, "said-more": &t.saidMore}
	switch {
	case eds[name] != nil:
		eds[name].SetText(val)
	case bools[name] != nil:
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		bools[name].Value = b
	case name == "mode":
		if val != "relay" && val != "direct" {
			return fmt.Errorf("mode is relay or direct, not %q", val)
		}
		t.mode.Value = val
	case name == "picker":
		p.picker.Value = val
	default:
		return fmt.Errorf("no field called %q", name)
	}
	return nil
}

func (v *view) tabShown() string {
	if v.tab == "" {
		return "party"
	}
	return v.tab
}

type uiFeed struct {
	Name  string   `json:"name"`
	Open  bool     `json:"open"`
	Items []string `json:"items"`
}

type uiParty struct {
	Title  string   `json:"title"`
	Line   string   `json:"line"`
	Button string   `json:"button"`
	Videos []string `json:"videos"`
}

// ui is what the window shows, by what a person reads in it.
func (v *view) ui() map[string]any {
	t, p, a := v.party, v.prefs, v.about
	n := v.now
	if n == nil {
		n = &partyNow{}
	}
	s := n.scr
	var detail strings.Builder
	for _, b := range s.shownDetail() {
		detail.WriteString(b.b + b.plain)
	}
	trail := [][2]string{}
	for _, st := range s.trail {
		trail = append(trail, [2]string{st.label, st.state})
	}
	members := [][2]any{}
	for _, m := range s.members {
		members = append(members, [2]any{m.name, v.since(m)})
	}
	ahead := map[string]int{}
	for _, m := range s.members {
		if m.ahead >= 0 {
			ahead[m.name] = m.ahead
		}
	}
	feed := []uiFeed{}
	if n.w != nil {
		for _, g := range n.w.feed {
			f := uiFeed{Name: g.name, Open: g.open, Items: []string{}}
			for _, it := range g.items {
				f.Items = append(f.Items, it.text)
			}
			feed = append(feed, f)
		}
	}
	said := []map[string]any{}
	for i := len(n.said) - 1; i >= 0; i-- {
		m := n.said[i]
		said = append(said, map[string]any{"who": m.Who, "text": m.Text, "react": m.React, "ours": m.Ours})
	}
	history := []uiParty{}
	for i := len(v.parties) - 1; i >= 0; i-- {
		x := v.parties[i]
		r := uiParty{Title: partyTitle(x, partyFiles(x)), Line: v.partyLine(x), Button: historyButton(x, time.Now()), Videos: []string{}}
		for _, vid := range x.Videos {
			r.Videos = append(r.Videos, vid.Name+", "+v.videoWhen(vid))
		}
		history = append(history, r)
	}
	running := n.running
	var dialog map[string]any
	if d := v.dialog; d != nil {
		dialog = map[string]any{"title": d.title, "text": d.text, "cancel": d.later, "ok": d.now, "okOff": d.nowOff}
	}
	return map[string]any{
		"tab":     v.tabShown(),
		"run":     v.run,
		"ran":     n.ran,
		"running": running,
		"status":  s.text,
		"bad":     s.bad,
		"detail":  detail.String(),
		"pct":     s.pct,
		"trail":   trail,
		"refusal": s.refusal,
		"alive":   s.alive,
		"sleep":   s.sleep,
		"walking": s.walking,
		"members": members,
		"ahead":   ahead,
		"invite":  map[bool]string{true: s.invite}[running],
		"copy":    t.copyLabel,
		"chat": map[string]any{"shown": s.chat, "away": s.away, "text": t.chat.Text(), "hint": t.hint, "strip": t.stripOpen,
			"resting": time.Since(t.reactedAt) < reactEvery, "more": t.saidMore.Value, "reactions": v.reactionCodes()},
		"said":  said,
		"feed":  feed,
		"chart": map[string]any{"open": t.chart.Value, "saved": t.saved, "saving": t.saving},
		"error": t.err,
		"form": map[string]any{"shown": !running, "mode": t.mode.Value, "invite": t.invite.Text(), "inviteFilled": t.inviteFilled,
			"hostname": t.hostname.Text(), "file": t.file.Text(), "playlist": t.playlist.Value, "relay": t.relay.Text(),
			"room": t.room.Text(), "secret": t.secret.Text(), "addrs": t.addrs.Text(), "psecret": t.psecret.Text(),
			"port": t.port.Text(), "local": t.local.Value, "picking": t.picking},
		"go":      goLabel(running),
		"history": history,
		"settings": map[string]any{"relay": p.relay.Text(), "mpv": p.mpv.Text(), "mpvFlags": p.mpvFlags.Text(),
			"createKey": p.createKey.Text(), "pport": p.pport.Text(), "cacheDir": p.cacheDir.Text(), "picker": p.picker.Value,
			"saved": p.saved, "mpvDefault": a.defaultMpv, "cacheDirDefault": a.defaultCacheDir, "picking": p.picking},
		"about": map[string]any{"version": v.versionText(), "button": v.aboutButton(), "buttonOff": running || p.updating,
			"dot": v.updateDot, "storePath": a.storePath, "tools": a.tools},
		"dialog": dialog,
		"tips":   v.tipsUI(),
		"hearts": len(v.hearts),
		"still":  v.still,
		"moving": v.moving(),
		"paceMs": v.pace.next().Milliseconds(),
		"dark":   v.dark,
	}
}

// tipsUI is the tips as they read, and where they are in the window, for a real click.
func (v *view) tipsUI() map[string]any {
	var titles []string
	var text strings.Builder
	for _, t := range tips {
		titles = append(titles, t.title)
		text.WriteString(t.title + "\n" + t.line + "\n")
		for _, p := range t.points {
			text.WriteString(strings.NewReplacer("<b>", "", "</b>", "").Replace(p) + "\n")
		}
	}
	a := v.tips.area.Sub(image.Pt(0, v.list.Position.Offset))
	return map[string]any{"open": v.tipOpen, "titles": titles, "text": text.String(), "area": [4]int{a.Min.X, a.Min.Y, a.Max.X, a.Max.Y},
		"scrolled": v.tips.list.Position.First > 0 || v.tips.list.Position.Offset > 0, "pageScrolled": v.list.Position.Offset > 0}
}

func (v *view) reactionCodes() []string {
	var codes []string
	for _, e := range v.party.emoji {
		codes = append(codes, e.code)
	}
	return codes
}
