// Package art is the GUI's drawings, Fosdo2, the ground and the icons, kept as the SVG they
// were made in and animated as app.css animated them. It flattens them into filled and
// stroked paths for the window to draw; it knows nothing of the toolkit.
package art

import (
	"math"
	"strconv"
	"strings"
)

type Pt struct{ X, Y float64 }

type Kind int

const (
	Move Kind = iota
	Line
	Quad
	Cube
	Close
)

// Op is one path command: P holds its control points, then its end point.
type Op struct {
	K Kind
	P [3]Pt
}

type Path []Op

// Mat is an SVG matrix: x' = A*x + C*y + E, y' = B*x + D*y + F.
type Mat struct{ A, B, C, D, E, F float64 }

var Identity = Mat{1, 0, 0, 1, 0, 0}

// Mul is m·n: n applies first.
func (m Mat) Mul(n Mat) Mat {
	return Mat{
		m.A*n.A + m.C*n.B, m.B*n.A + m.D*n.B,
		m.A*n.C + m.C*n.D, m.B*n.C + m.D*n.D,
		m.A*n.E + m.C*n.F + m.E, m.B*n.E + m.D*n.F + m.F,
	}
}

func (m Mat) Apply(p Pt) Pt { return Pt{m.A*p.X + m.C*p.Y + m.E, m.B*p.X + m.D*p.Y + m.F} }

// Scale is how much m scales a length, for stroke widths.
func (m Mat) Scale() float64 { return math.Sqrt(math.Abs(m.A*m.D - m.B*m.C)) }

func Translate(x, y float64) Mat { return Mat{1, 0, 0, 1, x, y} }
func Scale(sx, sy float64) Mat   { return Mat{sx, 0, 0, sy, 0, 0} }

// Rotate turns by deg degrees, clockwise on screen as CSS does.
func Rotate(deg float64) Mat {
	s, c := math.Sincos(deg * math.Pi / 180)
	return Mat{c, s, -s, c, 0, 0}
}

// About applies m around (x, y), as a CSS transform-origin.
func About(m Mat, x, y float64) Mat { return Translate(x, y).Mul(m).Mul(Translate(-x, -y)) }

func (p Path) Transform(m Mat) Path {
	out := make(Path, len(p))
	for i, op := range p {
		out[i].K = op.K
		for j := range op.P {
			out[i].P[j] = m.Apply(op.P[j])
		}
	}
	return out
}

// parseTransform reads an SVG transform list: translate, scale and rotate.
func parseTransform(s string) Mat {
	m := Identity
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		open, end := strings.IndexByte(s, '('), strings.IndexByte(s, ')')
		if open < 0 || end < open {
			break
		}
		name, args := strings.TrimSpace(s[:open]), numbers(s[open+1:end])
		s = s[end+1:]
		t := Identity
		switch {
		case name == "translate" && len(args) == 1:
			t = Translate(args[0], 0)
		case name == "translate" && len(args) == 2:
			t = Translate(args[0], args[1])
		case name == "scale" && len(args) == 1:
			t = Scale(args[0], args[0])
		case name == "scale" && len(args) == 2:
			t = Scale(args[0], args[1])
		case name == "rotate" && len(args) == 1:
			t = Rotate(args[0])
		case name == "rotate" && len(args) == 3:
			t = About(Rotate(args[0]), args[1], args[2])
		}
		m = m.Mul(t)
	}
	return m
}

func numbers(s string) []float64 {
	var out []float64
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if v, err := strconv.ParseFloat(f, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// pathData reads SVG path data, every command but S, absolute and relative.
type pathData struct {
	d string
	i int
}

func (r *pathData) space() {
	for r.i < len(r.d) && strings.IndexByte(" ,\t\r\n", r.d[r.i]) >= 0 {
		r.i++
	}
}

// more is whether a number comes next.
func (r *pathData) more() bool {
	r.space()
	return r.i < len(r.d) && strings.IndexByte("+-.0123456789", r.d[r.i]) >= 0
}

func (r *pathData) num() float64 {
	r.space()
	j := r.i
	if j < len(r.d) && (r.d[j] == '-' || r.d[j] == '+') {
		j++
	}
	dot := false
	for j < len(r.d) && (r.d[j] >= '0' && r.d[j] <= '9' || r.d[j] == '.' && !dot) {
		dot = dot || r.d[j] == '.'
		j++
	}
	if j < len(r.d) && (r.d[j] == 'e' || r.d[j] == 'E') {
		j++
		if j < len(r.d) && (r.d[j] == '-' || r.d[j] == '+') {
			j++
		}
		for j < len(r.d) && r.d[j] >= '0' && r.d[j] <= '9' {
			j++
		}
	}
	v, _ := strconv.ParseFloat(r.d[r.i:j], 64)
	r.i = j
	return v
}

// flag is an arc's one-digit flag, which may touch the next number ("a1 1 0 01 10 10").
func (r *pathData) flag() bool {
	r.space()
	if r.i < len(r.d) && (r.d[r.i] == '0' || r.d[r.i] == '1') {
		r.i++
		return r.d[r.i-1] == '1'
	}
	return false
}

func parsePath(d string) Path {
	r := &pathData{d: d}
	var p Path
	var cur, start, ctrl Pt
	var cmd byte
	for {
		if !r.more() {
			r.space()
			if r.i >= len(d) {
				return p
			}
			cmd = d[r.i]
			r.i++
		}
		rel := cmd >= 'a'
		pt := func() Pt {
			x, y := r.num(), r.num()
			if rel {
				return Pt{cur.X + x, cur.Y + y}
			}
			return Pt{x, y}
		}
		switch cmd | 0x20 {
		case 'm':
			cur = pt()
			start = cur
			p = append(p, Op{K: Move, P: [3]Pt{cur}})
			cmd = 'L' | cmd&0x20 // more pairs are lines
		case 'l':
			cur = pt()
			p = append(p, Op{K: Line, P: [3]Pt{cur}})
		case 'h':
			x := r.num()
			if rel {
				x += cur.X
			}
			cur = Pt{x, cur.Y}
			p = append(p, Op{K: Line, P: [3]Pt{cur}})
		case 'v':
			y := r.num()
			if rel {
				y += cur.Y
			}
			cur = Pt{cur.X, y}
			p = append(p, Op{K: Line, P: [3]Pt{cur}})
		case 'c':
			c1, c2, e := pt(), pt(), pt()
			p = append(p, Op{K: Cube, P: [3]Pt{c1, c2, e}})
			cur = e
		case 'q':
			c, e := pt(), pt()
			p = append(p, Op{K: Quad, P: [3]Pt{c, e}})
			cur, ctrl = e, c
		case 't':
			c := cur
			if len(p) > 0 && p[len(p)-1].K == Quad {
				c = Pt{2*cur.X - ctrl.X, 2*cur.Y - ctrl.Y}
			}
			e := pt()
			p = append(p, Op{K: Quad, P: [3]Pt{c, e}})
			cur, ctrl = e, c
		case 'a':
			rx, ry, rot := r.num(), r.num(), r.num()
			large, sweep := r.flag(), r.flag()
			e := pt()
			p = append(p, arc(cur, e, rx, ry, rot, large, sweep)...)
			cur = e
		case 'z':
			p = append(p, Op{K: Close})
			cur = start
		default:
			return p
		}
	}
}

// arc is an SVG elliptical arc from a to b as cubic curves of at most a quarter turn each
// (SVG 1.1 F.6.5, endpoint to centre).
func arc(a, b Pt, rx, ry, rotDeg float64, large, sweep bool) Path {
	if a == b {
		return nil
	}
	rx, ry = math.Abs(rx), math.Abs(ry)
	if rx == 0 || ry == 0 {
		return Path{{K: Line, P: [3]Pt{b}}}
	}
	sin, cos := math.Sincos(rotDeg * math.Pi / 180)
	dx, dy := (a.X-b.X)/2, (a.Y-b.Y)/2
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
	cx := cos*cx1 - sin*cy1 + (a.X+b.X)/2
	cy := sin*cx1 + cos*cy1 + (a.Y+b.Y)/2
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
	on := func(t float64) (Pt, Pt) { // the point at t and its derivative
		st, ct := math.Sincos(t)
		return Pt{cx + rx*ct*cos - ry*st*sin, cy + rx*ct*sin + ry*st*cos},
			Pt{-rx*st*cos - ry*ct*sin, -rx*st*sin + ry*ct*cos}
	}
	var p Path
	for i := range n {
		ta, tb := t1+float64(i)*step, t1+float64(i+1)*step
		pa, da := on(ta)
		pb, db := on(tb)
		if i == n-1 {
			pb = b
		}
		p = append(p, Op{K: Cube, P: [3]Pt{{pa.X + h*da.X, pa.Y + h*da.Y}, {pb.X - h*db.X, pb.Y - h*db.Y}, pb}})
	}
	return p
}

const kappa = 0.5522847498 // a quarter circle's cubic handle

func ellipse(cx, cy, rx, ry float64) Path {
	kx, ky := rx*kappa, ry*kappa
	return Path{
		{K: Move, P: [3]Pt{{cx + rx, cy}}},
		{K: Cube, P: [3]Pt{{cx + rx, cy + ky}, {cx + kx, cy + ry}, {cx, cy + ry}}},
		{K: Cube, P: [3]Pt{{cx - kx, cy + ry}, {cx - rx, cy + ky}, {cx - rx, cy}}},
		{K: Cube, P: [3]Pt{{cx - rx, cy - ky}, {cx - kx, cy - ry}, {cx, cy - ry}}},
		{K: Cube, P: [3]Pt{{cx + kx, cy - ry}, {cx + rx, cy - ky}, {cx + rx, cy}}},
		{K: Close},
	}
}

// RoundRect is a rectangle with corners of radius r.
func RoundRect(x, y, w, h, r float64) Path {
	r = math.Max(0, math.Min(r, math.Min(w, h)/2))
	k := r * (1 - kappa)
	return Path{
		{K: Move, P: [3]Pt{{x + r, y}}},
		{K: Line, P: [3]Pt{{x + w - r, y}}},
		{K: Cube, P: [3]Pt{{x + w - k, y}, {x + w, y + k}, {x + w, y + r}}},
		{K: Line, P: [3]Pt{{x + w, y + h - r}}},
		{K: Cube, P: [3]Pt{{x + w, y + h - k}, {x + w - k, y + h}, {x + w - r, y + h}}},
		{K: Line, P: [3]Pt{{x + r, y + h}}},
		{K: Cube, P: [3]Pt{{x + k, y + h}, {x, y + h - k}, {x, y + h - r}}},
		{K: Line, P: [3]Pt{{x, y + r}}},
		{K: Cube, P: [3]Pt{{x, y + k}, {x + k, y}, {x + r, y}}},
		{K: Close},
	}
}
