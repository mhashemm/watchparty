package art

import (
	"encoding/xml"
	"image/color"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Style is how an element is painted once the rules are applied.
type Style struct {
	Fill, Stroke       color.NRGBA
	HasFill, HasStroke bool
	Width              float64
	Round              bool // round caps and joins
	FillOp, StrokeOp   float64
	Opacity            float64
	hazel              bool        // the eyes' radial gradient
	current            color.NRGBA // CSS color: what currentColor paints
}

// FillStyle paints shapes in c, as an icon's currentColor.
func FillStyle(c color.NRGBA) Style {
	return Style{Fill: c, HasFill: true, Width: 1, FillOp: 1, StrokeOp: 1, Opacity: 1, current: c}
}

// StrokeStyle draws lines in c, width wide, nothing filled.
func StrokeStyle(c color.NRGBA, width float64, round bool) Style {
	return Style{Stroke: c, HasStroke: true, Width: width, Round: round, FillOp: 1, StrokeOp: 1, Opacity: 1, current: c}
}

// Item is one shape to paint, in the coordinates the caller's matrix maps to.
type Item struct {
	Path  Path
	Fill  bool // or a stroke
	Color color.NRGBA
	Width float64
	Round bool
}

type elem struct {
	tag   string
	class []string
	attr  map[string]string
	kids  []*elem
	path  Path
	xf    Mat
	shown bool
}

func (e *elem) has(c string) bool { return slices.Contains(e.class, c) }

func (e *elem) num(k string) float64 {
	v, _ := strconv.ParseFloat(e.attr[k], 64)
	return v
}

type xnode struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Nodes   []xnode    `xml:",any"`
}

func build(n xnode) *elem {
	e := &elem{tag: n.XMLName.Local, attr: map[string]string{}, xf: Identity, shown: true}
	for _, a := range n.Attrs {
		e.attr[a.Name.Local] = a.Value
	}
	e.class = strings.Fields(e.attr["class"])
	if t, ok := e.attr["transform"]; ok {
		e.xf = parseTransform(t)
	}
	switch e.tag {
	case "path":
		e.path = parsePath(e.attr["d"])
	case "ellipse":
		e.path = ellipse(e.num("cx"), e.num("cy"), e.num("rx"), e.num("ry"))
	case "circle":
		e.path = ellipse(e.num("cx"), e.num("cy"), e.num("r"), e.num("r"))
	case "rect":
		e.path = RoundRect(e.num("x"), e.num("y"), e.num("width"), e.num("height"), e.num("rx"))
	case "defs", "title", "text":
		e.shown = false
	}
	for _, k := range n.Nodes {
		e.kids = append(e.kids, build(k))
	}
	return e
}

// parse reads an SVG; one that doesn't parse draws nothing (art_test parses every one).
func parse(src string) *elem {
	var n xnode
	if xml.Unmarshal([]byte(src), &n) != nil {
		return &elem{}
	}
	return build(n)
}

// rule is one style rule: it applies to an element with all its classes.
type rule struct {
	classes []string
	set     func(*Style)
}

func (r rule) match(e *elem) bool {
	for _, c := range r.classes {
		if !e.has(c) {
			return false
		}
	}
	return true
}

// Mood is the window's state: the cat sleeps when no party runs, and the turtles
// hurry while the party starts or reconnects. Still is the OS asking for reduced motion:
// everything at rest.
type Mood struct{ Awake, Walking, Still bool }

// animation is a CSS animation on an element at t seconds: a transform before its own,
// an opacity, and whether it shows at all.
type animation func(e *elem, t float64, m Mood) (Mat, float64, bool)

// Scene is one of the page's SVGs, with its rules and animations.
type Scene struct {
	root  *elem
	rules []rule
	anim  animation
}

// Frame is the scene t seconds in, its viewBox mapped by base.
func (s *Scene) Frame(t float64, m Mood, base Mat) []Item {
	var out []Item
	s.walk(s.root, FillStyle(color.NRGBA{A: 255}), base, 1, t, m, &out)
	return out
}

func (s *Scene) walk(e *elem, parent Style, m Mat, op float64, t float64, mood Mood, out *[]Item) {
	if !e.shown {
		return
	}
	st := parent
	st.Opacity, st.hazel = 1, false
	paint := func(v string) (color.NRGBA, bool) {
		if v == "none" {
			return color.NRGBA{}, false
		}
		if v == "currentColor" {
			return parent.current, true
		}
		return Hex(v), true
	}
	if v, ok := e.attr["fill"]; ok {
		st.Fill, st.HasFill = paint(v)
	}
	if v, ok := e.attr["stroke"]; ok {
		st.Stroke, st.HasStroke = paint(v)
	}
	if v, ok := e.attr["stroke-width"]; ok {
		st.Width, _ = strconv.ParseFloat(v, 64)
	}
	if e.attr["stroke-linecap"] == "round" || e.attr["stroke-linejoin"] == "round" {
		st.Round = true
	}
	for _, r := range s.rules {
		if r.match(e) {
			r.set(&st)
		}
	}
	// inline style, of which the page uses only a stroke width
	if v, ok := strings.CutPrefix(e.attr["style"], "stroke-width:"); ok {
		st.Width, _ = strconv.ParseFloat(v, 64)
	}
	xf := e.xf
	if s.anim != nil {
		am, ao, show := s.anim(e, t, mood)
		if !show {
			return
		}
		xf = am.Mul(xf)
		op *= ao
	}
	m = m.Mul(xf)
	op *= st.Opacity
	if op < 0.004 {
		return
	}
	if e.path != nil {
		emit(e, st, m, op, out)
	}
	for _, k := range e.kids {
		s.walk(k, st, m, op, t, mood, out)
	}
}

func fade(c color.NRGBA, op float64) color.NRGBA {
	c.A = uint8(math.Round(float64(c.A) * op))
	return c
}

// hazel is the eyes' gradient (cat.svg's #hazel), its stops by offset.
var hazel = []struct {
	at float64
	c  color.NRGBA
}{{0, Hex("#4f8f34")}, {.45, Hex("#86b04a")}, {.78, Hex("#c8a245")}, {1, Hex("#946330")}}

func hazelAt(o float64) color.NRGBA {
	for i := 1; i < len(hazel); i++ {
		if o <= hazel[i].at {
			a, b := hazel[i-1], hazel[i]
			k := (o - a.at) / (b.at - a.at)
			mix := func(x, y uint8) uint8 { return uint8(math.Round(float64(x) + (float64(y)-float64(x))*k)) }
			return color.NRGBA{mix(a.c.R, b.c.R), mix(a.c.G, b.c.G), mix(a.c.B, b.c.B), 255}
		}
	}
	return hazel[len(hazel)-1].c
}

// hazelBands is how many rings draw the gradient: at the eye's size they read as smooth
const hazelBands = 24

func emit(e *elem, st Style, m Mat, op float64, out *[]Item) {
	p := e.path.Transform(m)
	switch {
	case st.hazel:
		// a radial gradient (cx .5, cy .55, r .6 of the eye's box) as rings, outermost first;
		// the ring at the eye's edge is offset 1/1.2 of the way out
		cx, cy, rx, ry := e.num("cx"), e.num("cy"), e.num("rx"), e.num("ry")
		edge := 1 / 1.2
		*out = append(*out, Item{Path: p, Fill: true, Color: fade(hazelAt(edge), op*st.FillOp)})
		for i := hazelBands; i >= 1; i-- {
			o := edge * float64(i) / hazelBands
			// the gradient's centre sits a tenth of the eye's height low
			band := ellipse(cx, cy+.1*ry, 1.2*rx*o, 1.2*ry*o)
			if o > .7 { // keep the outer rings inside the eye
				band = ellipse(cx, cy+.1*ry*(1-(o-.7)/(edge-.7)), 1.2*rx*o, 1.2*ry*o)
			}
			*out = append(*out, Item{Path: band.Transform(m), Fill: true, Color: fade(hazelAt(o), op*st.FillOp)})
		}
	case st.HasFill:
		*out = append(*out, Item{Path: p, Fill: true, Color: fade(st.Fill, op*st.FillOp)})
	}
	if st.HasStroke && st.Width > 0 {
		*out = append(*out, Item{Path: p, Color: fade(st.Stroke, op*st.StrokeOp), Width: st.Width * m.Scale(), Round: st.Round})
	}
}

// Hex is a CSS #rgb or #rrggbb color.
func Hex(s string) color.NRGBA {
	s = strings.TrimPrefix(s, "#")
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	v, _ := strconv.ParseUint(s, 16, 32)
	return color.NRGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255}
}

// Icon draws a small SVG with no classes, the root painted as st (an icon's currentColor
// is st's fill), its viewBox mapped by base.
func Icon(svg string, st Style, base Mat) []Item {
	s := &Scene{root: parse(svg)}
	var out []Item
	s.walk(s.root, st, base, 1, 0, Mood{}, &out)
	return out
}
