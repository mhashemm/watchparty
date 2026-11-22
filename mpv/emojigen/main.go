// emojigen writes reactions' emoji (mpv/emoji.json) from Twemoji's SVGs, as ASS drawings mpv
// draws itself: libass can't draw a colour emoji font, and a drawing scales, turns and fades
// on any mpv and OS. The GUI draws the same shapes as SVG (mpv.EmojiSVG). Run from the repo root:
//
//	go run ./mpv/emojigen > mpv/emoji.json
//
// It reads the SVGs of the pinned Twemoji release from GitHub, so only a run needs the network.
package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// twemoji is the release the art comes from; its graphics are CC-BY 4.0, credited in the help.
const twemoji = "v17.0.3"

// Reactions are the emoji in the picker, in its order, as their code points.
var reactions = []string{
	"1f602", "1f62d", "2639", "1f621", "1f97a", "1f60b", "1f92e", "1f919", "1f631", "1f62c", "1f610", "1fae4", "1fae8",
	"1f62e", "1f49e", "1f493", "1f495", "1f494", "1f970", "1f92a", "1f92d", "1f975", "1f634", "1f922", "1f622", "1faea",
}

// grid is drawing units per SVG unit: ASS drawings take whole numbers best, and \p4 divides
// them by 8 again, so Twemoji's 36 units are 36 pixels at \fscx100.
const grid = 8

func main() {
	out := struct {
		Source string `json:"source"`
		// Order is the picker's; Art each emoji's shapes, back to front, as [colour, drawing]
		Order []string               `json:"order"`
		Art   map[string][][2]string `json:"art"`
	}{"Twemoji " + twemoji + " (CC-BY 4.0, https://github.com/jdecked/twemoji), by mpv/emojigen", reactions, map[string][][2]string{}}
	for _, c := range reactions {
		svg, err := fetch(c)
		if err != nil {
			fmt.Fprintln(os.Stderr, c, err)
			os.Exit(1)
		}
		shapes, err := convert(svg)
		if err != nil {
			fmt.Fprintln(os.Stderr, c, err)
			os.Exit(1)
		}
		for _, s := range shapes {
			out.Art[c] = append(out.Art[c], [2]string{s.colour, s.drawing})
		}
	}
	b, _ := json.MarshalIndent(out, "", " ")
	os.Stdout.Write(append(b, '\n'))
}

func fetch(code string) ([]byte, error) {
	res, err := http.Get("https://raw.githubusercontent.com/jdecked/twemoji/" + twemoji + "/assets/svg/" + code + ".svg")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", res.Status)
	}
	return io.ReadAll(res.Body)
}

type shape struct{ colour, drawing string }

// node is an SVG element, with what the art uses.
type node struct {
	XMLName   xml.Name
	Fill      string `xml:"fill,attr"`
	D         string `xml:"d,attr"`
	Transform string `xml:"transform,attr"`
	Cx        string `xml:"cx,attr"`
	Cy        string `xml:"cy,attr"`
	R         string `xml:"r,attr"`
	Rx        string `xml:"rx,attr"`
	Ry        string `xml:"ry,attr"`
	Nodes     []node `xml:",any"`
}

func convert(svg []byte) ([]shape, error) {
	root := node{}
	if err := xml.Unmarshal(svg, &root); err != nil {
		return nil, err
	}
	var shapes []shape
	var walk func(n node, fill string, m matrix) error
	walk = func(n node, fill string, m matrix) error {
		if n.Fill != "" {
			fill = n.Fill
		}
		if n.Transform != "" {
			t, err := parseTransform(n.Transform)
			if err != nil {
				return err
			}
			m = m.times(t)
		}
		var p path
		switch n.XMLName.Local {
		case "svg", "g":
			for _, c := range n.Nodes {
				if err := walk(c, fill, m); err != nil {
					return err
				}
			}
			return nil
		case "defs", "clipPath":
			// the one clip (🫨's) only trims its shake lines to a diamond: drawn whole
			return nil
		case "path":
			var err error
			if p, err = parsePath(n.D); err != nil {
				return err
			}
		case "circle":
			p = ellipse(num(n.Cx), num(n.Cy), num(n.R), num(n.R))
		case "ellipse":
			p = ellipse(num(n.Cx), num(n.Cy), num(n.Rx), num(n.Ry))
		default:
			return fmt.Errorf("an SVG <%s>, which emojigen doesn't draw", n.XMLName.Local)
		}
		if fill == "none" {
			return nil
		}
		colour, err := assColour(fill)
		if err != nil {
			return err
		}
		shapes = append(shapes, shape{colour, p.transformed(m).ass()})
		return nil
	}
	return shapes, walk(root, "#000", identity)
}

func num(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// assColour is #RGB or #RRGGBB as ASS's &HBBGGRR&.
func assColour(fill string) (string, error) {
	h := strings.TrimPrefix(fill, "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 || h == fill {
		return "", fmt.Errorf("a fill %q, not a colour", fill)
	}
	return "&H" + strings.ToUpper(h[4:6]+h[2:4]+h[0:2]) + "&", nil
}

// matrix is an affine transform: x' = a x + c y + e, y' = b x + d y + f.
type matrix struct{ a, b, c, d, e, f float64 }

var identity = matrix{1, 0, 0, 1, 0, 0}

func (m matrix) times(n matrix) matrix {
	return matrix{
		m.a*n.a + m.c*n.b, m.b*n.a + m.d*n.b,
		m.a*n.c + m.c*n.d, m.b*n.c + m.d*n.d,
		m.a*n.e + m.c*n.f + m.e, m.b*n.e + m.d*n.f + m.f,
	}
}

func (m matrix) apply(p point) point {
	return point{m.a*p.x + m.c*p.y + m.e, m.b*p.x + m.d*p.y + m.f}
}

var transformRe = regexp.MustCompile(`(\w+)\(([^)]*)\)`)

func parseTransform(s string) (matrix, error) {
	m := identity
	for _, t := range transformRe.FindAllStringSubmatch(s, -1) {
		var v []float64
		for _, f := range numbers(t[2]) {
			v = append(v, f)
		}
		switch {
		case t[1] == "translate" && len(v) >= 1:
			ty := 0.0
			if len(v) > 1 {
				ty = v[1]
			}
			m = m.times(matrix{1, 0, 0, 1, v[0], ty})
		case t[1] == "scale" && len(v) >= 1:
			sy := v[0]
			if len(v) > 1 {
				sy = v[1]
			}
			m = m.times(matrix{v[0], 0, 0, sy, 0, 0})
		case t[1] == "rotate" && (len(v) == 1 || len(v) == 3):
			r := v[0] * math.Pi / 180
			rot := matrix{math.Cos(r), math.Sin(r), -math.Sin(r), math.Cos(r), 0, 0}
			if len(v) == 3 {
				rot = matrix{1, 0, 0, 1, v[1], v[2]}.times(rot).times(matrix{1, 0, 0, 1, -v[1], -v[2]})
			}
			m = m.times(rot)
		case t[1] == "matrix" && len(v) == 6:
			m = m.times(matrix{v[0], v[1], v[2], v[3], v[4], v[5]})
		default:
			return m, fmt.Errorf("a transform %q, which emojigen doesn't apply", t[0])
		}
	}
	return m, nil
}

var numberRe = regexp.MustCompile(`[-+]?(?:\d*\.\d+|\d+\.?)(?:[eE][-+]?\d+)?`)

func numbers(s string) []float64 {
	var out []float64
	for _, n := range numberRe.FindAllString(s, -1) {
		out = append(out, num(n))
	}
	return out
}

type point struct{ x, y float64 }

// path is subpaths of segments: a move, then lines and cubic curves, all absolute.
type path []seg

type seg struct {
	op  byte // 'm', 'l' or 'b'
	pts []point
}

func (p path) transformed(m matrix) path {
	out := make(path, len(p))
	for i, s := range p {
		pts := make([]point, len(s.pts))
		for j, q := range s.pts {
			pts[j] = m.apply(q)
		}
		out[i] = seg{s.op, pts}
	}
	return out
}

func (p path) ass() string {
	var b strings.Builder
	last := byte(0)
	for _, s := range p {
		if s.op != last || s.op == 'm' {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteByte(s.op)
			last = s.op
		}
		for _, q := range s.pts {
			fmt.Fprintf(&b, " %d %d", int(math.Round(q.x*grid)), int(math.Round(q.y*grid)))
		}
	}
	return b.String()
}

// kappa places a cubic's handles to draw a quarter circle.
const kappa = 0.5522847498

func ellipse(cx, cy, rx, ry float64) path {
	kx, ky := rx*kappa, ry*kappa
	return path{
		{'m', []point{{cx + rx, cy}}},
		{'b', []point{{cx + rx, cy + ky}, {cx + kx, cy + ry}, {cx, cy + ry}}},
		{'b', []point{{cx - kx, cy + ry}, {cx - rx, cy + ky}, {cx - rx, cy}}},
		{'b', []point{{cx - rx, cy - ky}, {cx - kx, cy - ry}, {cx, cy - ry}}},
		{'b', []point{{cx + kx, cy - ry}, {cx + rx, cy - ky}, {cx + rx, cy}}},
	}
}

// pathLexer reads an SVG path's d: commands, numbers, and an arc's flags, which minified SVGs
// write as single digits with nothing between them ("a1 1 0 011 1").
type pathLexer struct {
	s string
	i int
}

func (l *pathLexer) skip() {
	for l.i < len(l.s) && (l.s[l.i] == ' ' || l.s[l.i] == ',' || l.s[l.i] == '\n' || l.s[l.i] == '\t' || l.s[l.i] == '\r') {
		l.i++
	}
}

func (l *pathLexer) command() (byte, bool) {
	l.skip()
	if l.i < len(l.s) && strings.IndexByte("MmLlHhVvCcSsQqTtAaZz", l.s[l.i]) >= 0 {
		l.i++
		return l.s[l.i-1], true
	}
	return 0, false
}

func (l *pathLexer) more() bool {
	l.skip()
	return l.i < len(l.s) && strings.IndexByte("MmLlHhVvCcSsQqTtAaZz", l.s[l.i]) < 0
}

func (l *pathLexer) number() (float64, error) {
	l.skip()
	loc := numberRe.FindStringIndex(l.s[l.i:])
	if loc == nil || loc[0] != 0 {
		return 0, fmt.Errorf("no number at %q", l.s[l.i:min(len(l.s), l.i+10)])
	}
	f := num(l.s[l.i : l.i+loc[1]])
	l.i += loc[1]
	return f, nil
}

func (l *pathLexer) flag() (bool, error) {
	l.skip()
	if l.i < len(l.s) && (l.s[l.i] == '0' || l.s[l.i] == '1') {
		l.i++
		return l.s[l.i-1] == '1', nil
	}
	return false, fmt.Errorf("no arc flag at %q", l.s[l.i:min(len(l.s), l.i+10)])
}

func (l *pathLexer) numbers(n int) ([]float64, error) {
	v := make([]float64, n)
	for i := range v {
		var err error
		if v[i], err = l.number(); err != nil {
			return nil, err
		}
	}
	return v, nil
}

func parsePath(d string) (path, error) {
	l := &pathLexer{s: d}
	var p path
	var cur, start, lastCtrl point
	var lastOp byte
	for {
		op, ok := l.command()
		if !ok {
			if l.i >= len(l.s) {
				return p, nil
			}
			return nil, fmt.Errorf("no command at %q", l.s[l.i:min(len(l.s), l.i+10)])
		}
		rel := op >= 'a'
		up := op &^ 0x20
		at := func(x, y float64) point {
			if rel {
				return point{cur.x + x, cur.y + y}
			}
			return point{x, y}
		}
		if up == 'Z' {
			if cur != start {
				p = append(p, seg{'l', []point{start}})
			}
			cur, lastOp = start, 'Z'
			continue
		}
		for first := true; first || l.more(); first = false {
			switch up {
			case 'M':
				v, err := l.numbers(2)
				if err != nil {
					return nil, err
				}
				cur = at(v[0], v[1])
				if first {
					start = cur
					p = append(p, seg{'m', []point{cur}})
				} else {
					// more pairs after a move are lines
					p = append(p, seg{'l', []point{cur}})
				}
			case 'L':
				v, err := l.numbers(2)
				if err != nil {
					return nil, err
				}
				cur = at(v[0], v[1])
				p = append(p, seg{'l', []point{cur}})
			case 'H':
				v, err := l.number()
				if err != nil {
					return nil, err
				}
				if rel {
					v += cur.x
				}
				cur = point{v, cur.y}
				p = append(p, seg{'l', []point{cur}})
			case 'V':
				v, err := l.number()
				if err != nil {
					return nil, err
				}
				if rel {
					v += cur.y
				}
				cur = point{cur.x, v}
				p = append(p, seg{'l', []point{cur}})
			case 'C', 'S':
				var c1 point
				var v []float64
				var err error
				if up == 'C' {
					if v, err = l.numbers(6); err != nil {
						return nil, err
					}
					c1, v = at(v[0], v[1]), v[2:]
				} else {
					if v, err = l.numbers(4); err != nil {
						return nil, err
					}
					c1 = cur
					if lastOp == 'C' || lastOp == 'S' {
						c1 = point{2*cur.x - lastCtrl.x, 2*cur.y - lastCtrl.y}
					}
				}
				c2, end := at(v[0], v[1]), at(v[2], v[3])
				p = append(p, seg{'b', []point{c1, c2, end}})
				cur, lastCtrl = end, c2
			case 'Q', 'T':
				var q point
				var v []float64
				var err error
				if up == 'Q' {
					if v, err = l.numbers(4); err != nil {
						return nil, err
					}
					q, v = at(v[0], v[1]), v[2:]
				} else {
					if v, err = l.numbers(2); err != nil {
						return nil, err
					}
					q = cur
					if lastOp == 'Q' || lastOp == 'T' {
						q = point{2*cur.x - lastCtrl.x, 2*cur.y - lastCtrl.y}
					}
				}
				end := at(v[0], v[1])
				// a quadratic is a cubic with its handles two thirds of the way to q
				c1 := point{cur.x + 2.0/3*(q.x-cur.x), cur.y + 2.0/3*(q.y-cur.y)}
				c2 := point{end.x + 2.0/3*(q.x-end.x), end.y + 2.0/3*(q.y-end.y)}
				p = append(p, seg{'b', []point{c1, c2, end}})
				cur, lastCtrl = end, q
			case 'A':
				v, err := l.numbers(3)
				if err != nil {
					return nil, err
				}
				large, err := l.flag()
				if err != nil {
					return nil, err
				}
				sweep, err := l.flag()
				if err != nil {
					return nil, err
				}
				e, err := l.numbers(2)
				if err != nil {
					return nil, err
				}
				end := at(e[0], e[1])
				p = append(p, arc(cur, end, v[0], v[1], v[2], large, sweep)...)
				cur = end
			}
			lastOp = up
		}
	}
}

// arc is SVG's endpoint arc as cubic curves of at most a quarter turn each (SVG 1.1, F.6.5).
func arc(from, to point, rx, ry, rotation float64, large, sweep bool) []seg {
	if from == to {
		return nil
	}
	rx, ry = math.Abs(rx), math.Abs(ry)
	if rx == 0 || ry == 0 {
		return []seg{{'l', []point{to}}}
	}
	phi := rotation * math.Pi / 180
	cos, sin := math.Cos(phi), math.Sin(phi)
	dx, dy := (from.x-to.x)/2, (from.y-to.y)/2
	x1 := cos*dx + sin*dy
	y1 := -sin*dx + cos*dy
	if l := x1*x1/(rx*rx) + y1*y1/(ry*ry); l > 1 {
		rx, ry = rx*math.Sqrt(l), ry*math.Sqrt(l)
	}
	num := rx*rx*ry*ry - rx*rx*y1*y1 - ry*ry*x1*x1
	den := rx*rx*y1*y1 + ry*ry*x1*x1
	k := math.Sqrt(math.Max(0, num/den))
	if large == sweep {
		k = -k
	}
	cx1, cy1 := k*rx*y1/ry, -k*ry*x1/rx
	cx := cos*cx1 - sin*cy1 + (from.x+to.x)/2
	cy := sin*cx1 + cos*cy1 + (from.y+to.y)/2
	angle := func(ux, uy, vx, vy float64) float64 {
		return math.Atan2(ux*vy-uy*vx, ux*vx+uy*vy)
	}
	t1 := angle(1, 0, (x1-cx1)/rx, (y1-cy1)/ry)
	dt := angle((x1-cx1)/rx, (y1-cy1)/ry, (-x1-cx1)/rx, (-y1-cy1)/ry)
	if !sweep && dt > 0 {
		dt -= 2 * math.Pi
	} else if sweep && dt < 0 {
		dt += 2 * math.Pi
	}
	n := int(math.Ceil(math.Abs(dt) / (math.Pi / 2)))
	step := dt / float64(n)
	h := 4.0 / 3 * math.Tan(step/4)
	on := func(t float64) (point, point) {
		// a point of the ellipse at t, and its tangent
		x, y := rx*math.Cos(t), ry*math.Sin(t)
		tx, ty := -rx*math.Sin(t), ry*math.Cos(t)
		return point{cos*x - sin*y + cx, sin*x + cos*y + cy}, point{cos*tx - sin*ty, sin*tx + cos*ty}
	}
	var out []seg
	for i := range n {
		a, b := t1+float64(i)*step, t1+float64(i+1)*step
		p1, d1 := on(a)
		p2, d2 := on(b)
		if i == n-1 {
			p2 = to
		}
		out = append(out, seg{'b', []point{{p1.x + h*d1.x, p1.y + h*d1.y}, {p2.x - h*d2.x, p2.y - h*d2.y}, p2}})
	}
	return out
}
