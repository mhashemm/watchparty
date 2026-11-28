package art

import (
	"fmt"
	"math"
	"testing"

	"github.com/mhashemm/watchparty/mpv"
)

func near(a, b Pt) bool { return math.Abs(a.X-b.X) < 1e-6 && math.Abs(a.Y-b.Y) < 1e-6 }

func ends(p Path) []Pt {
	var out []Pt
	for _, op := range p {
		switch op.K {
		case Move, Line:
			out = append(out, op.P[0])
		case Quad:
			out = append(out, op.P[1])
		case Cube:
			out = append(out, op.P[2])
		}
	}
	return out
}

func TestPathData(t *testing.T) {
	t.Parallel()
	// relative moves, implicit lines after a move, H and V, and Z back to the start
	p := parsePath("m10 10 5 0h5v-5z l1 1")
	want := []Pt{{10, 10}, {15, 10}, {20, 10}, {20, 5}, {11, 11}}
	got := ends(p)
	if len(got) != len(want) {
		t.Fatalf("ends %v, want %v", got, want)
	}
	for i := range want {
		if !near(got[i], want[i]) {
			t.Fatalf("end %d: %v, want %v (%v)", i, got[i], want[i], got)
		}
	}
	// T mirrors the last Q's control point
	p = parsePath("M0 0 Q10 10 20 0 T40 0")
	if c := p[2].P[0]; p[2].K != Quad || !near(c, Pt{30, -10}) {
		t.Fatalf("T control %v", c)
	}
	// numbers that touch: "1-2" is 1 and -2, ".5.5" is .5 and .5
	if got := ends(parsePath("M1-2L.5.5")); !near(got[0], Pt{1, -2}) || !near(got[1], Pt{.5, .5}) {
		t.Fatalf("touching numbers: %v", got)
	}
}

func TestArc(t *testing.T) {
	t.Parallel()
	// half a circle of radius 10 from the top to the bottom, clockwise: through (22, 12)
	p := parsePath("M12 2A10 10 0 1 1 12 22")
	e := ends(p)
	if !near(e[len(e)-1], Pt{12, 22}) || len(p) != 3 {
		t.Fatalf("arc ends %v (%d ops)", e, len(p))
	}
	if mid := e[1]; !near(mid, Pt{22, 12}) {
		t.Fatalf("clockwise half circle passes %v, want (22,12)", mid)
	}
	// the other way round passes (2, 12)
	if mid := ends(parsePath("M12 2A10 10 0 1 0 12 22"))[1]; !near(mid, Pt{2, 12}) {
		t.Fatalf("anticlockwise half circle passes %v, want (2,12)", mid)
	}
	// flags that touch the next number
	if e := ends(parsePath("M0 0a5 5 0 0110 0")); !near(e[len(e)-1], Pt{10, 0}) {
		t.Fatalf("touching flags end at %v", e[len(e)-1])
	}
}

func TestTransform(t *testing.T) {
	t.Parallel()
	m := parseTransform("translate(100 86) scale(1.05) translate(-80 -72)")
	if got := m.Apply(Pt{80, 72}); !near(got, Pt{100, 86}) {
		t.Fatalf("%v", got)
	}
	if got := parseTransform("rotate(90 10 10)").Apply(Pt{20, 10}); !near(got, Pt{10, 20}) {
		t.Fatalf("rotate about a point: %v", got)
	}
	if s := Scale(2, 2).Scale(); s != 2 {
		t.Fatalf("scale %v", s)
	}
}

// fills is the colors of an SVG's filled items, as the scene rules paint them.
func fills(s *Scene, m Mood, tt float64) map[string]bool {
	out := map[string]bool{}
	for _, it := range s.Frame(tt, m, Identity) {
		if it.Fill {
			c := it.Color
			out[string([]byte{c.R, c.G, c.B, c.A})] = true
		}
	}
	return out
}

func has(set map[string]bool, hex string, alpha uint8) bool {
	c := Hex(hex)
	return set[string([]byte{c.R, c.G, c.B, alpha})]
}

func TestCascade(t *testing.T) {
	t.Parallel()
	s := &Scene{root: parse(`<svg><path class="cap red" d="M0 0h1v1z"/><path class="cap" d="M0 0h1v1z"/><circle class="cheek" r="1"/></svg>`), rules: groundRules}
	f := fills(s, Mood{}, 0)
	// a later, more classes rule wins; opacity scales the alpha
	if !has(f, "#9a3a35", 255) || !has(f, "#a8642a", 255) || !has(f, "#e9a99a", uint8(math.Round(255*.7))) {
		t.Fatalf("fills %v", f)
	}
	// currentColor is the icon's color, whatever fill its parent has
	items := ParseIcon(`<svg><g fill="#000"><path fill="currentColor" d="M0 0h1v1z"/></g></svg>`).Items(FillStyle(Berry), Identity)
	if len(items) != 1 || items[0].Color != Berry {
		t.Fatalf("currentColor painted %+v", items)
	}
}

func TestEverythingDraws(t *testing.T) {
	t.Parallel()
	for name, svg := range map[string]string{"Paw": Paw, "Smiley": Smiley, "Back": Back, "Forward": Forward, "ECG": ECG, "Mushroom": Mushroom, "TrailMushroom": TrailMushroom, "Shell": Shell(5)} {
		if len(ParseIcon(svg).Items(StrokeStyle(Forest, 2, true), Identity)) == 0 && len(ParseIcon(svg).Items(FillStyle(Forest), Identity)) == 0 {
			t.Errorf("%s draws nothing", name)
		}
	}
	for _, code := range mpv.Reactions() {
		if len(ParseIcon(mpv.EmojiSVG(code)).Items(FillStyle(Forest), Identity)) == 0 {
			t.Errorf("reaction %s draws nothing", code)
		}
	}
	if n := len(Cat().Frame(0, Mood{}, Identity)); n < 40 {
		t.Errorf("the cat is %d shapes", n)
	}
	if n := len(Ground().Frame(0, Mood{}, Identity)); n < 40 {
		t.Errorf("the ground is %d shapes", n)
	}
}

func TestCatMoods(t *testing.T) {
	t.Parallel()
	cat := Cat()
	pupil := "#1f1a14"
	// asleep: eyes closed, no lick, whatever the time
	for _, tt := range []float64{0, 14} {
		if has(fills(cat, Mood{}, tt), pupil, 255) {
			t.Fatalf("a sleeping cat's pupils show at %vs", tt)
		}
	}
	awake := Mood{Awake: true}
	if !has(fills(cat, awake, 0), pupil, 255) {
		t.Fatal("an awake cat has no pupils")
	}
	// the lick, 81% to 97% of every 16s: the tongue shows and the eyes close
	tongue := "#ef8f8c"
	for _, c := range []struct {
		t    float64
		lick bool
	}{{1, false}, {12.4, false}, {13.2, true}, {14, true}, {15.4, true}, {15.9, false}} {
		if has(fills(cat, awake, c.t), tongue, 255) != c.lick || has(fills(cat, awake, c.t), pupil, 255) == c.lick {
			t.Fatalf("at %vs the lick shows: %v, want %v", c.t, !c.lick, c.lick)
		}
	}
	// the closed eyes are lines, like the mouth: asleep, or awake mid-lick
	lines := func(m Mood, tt float64) int {
		n := 0
		for _, it := range cat.Frame(tt, m, Identity) {
			if !it.Fill && it.Color == Forest {
				n++
			}
		}
		return n
	}
	if a, b, c := lines(Mood{}, 0), lines(awake, 0), lines(awake, 14); a != 2 || b != 1 || c != 2 {
		t.Fatalf("closed eyes and mouth: asleep %d, awake %d, licking %d; want 2, 1, 2", a, b, c)
	}
	if !has(fills(cat, awake, 16+14), tongue, 255) {
		t.Fatal("the lick doesn't repeat")
	}
	// the eyes: a gradient's rings, the innermost the gradient's centre colour, in both eyes
	n := 0
	for _, it := range cat.Frame(0, awake, Identity) {
		if it.Fill && it.Color == hazelAt(1/1.2/hazelBands) {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d eye centres", n)
	}
}

func TestKeyframes(t *testing.T) {
	t.Parallel()
	swish := func(tt float64) float64 { return key(tt, 3.2, 0, true, easeInOut, stop{0, -5}, stop{1, 7}) }
	// alternate: there at 3.2s, back at 6.4s, halfway at the middle of either
	for _, c := range []struct{ t, want float64 }{{0, -5}, {1.6, 1}, {3.2, 7}, {4.8, 1}, {6.4, -5}} {
		if got := swish(c.t); math.Abs(got-c.want) > 1e-6 {
			t.Errorf("swish(%v) = %v, want %v", c.t, got, c.want)
		}
	}
	// a delay holds the first stop
	if got := key(.2, .7, .35, true, easeInOut, stop{0, 0}, stop{1, -2}); got != 0 {
		t.Errorf("delayed leg moved: %v", got)
	}
	// ease is CSS's: slow start, fast middle
	if e := ease(.5); math.Abs(e-.8024) > 1e-3 {
		t.Errorf("ease(.5) = %v, want ≈ .8024", e)
	}
	// the turtles stroll from -90 to 620 over a minute
	turtle := &elem{class: []string{"turtle"}}
	if m, _, _ := groundAnim(turtle, 30, Mood{}); math.Abs(m.E-265) > 1e-9 {
		t.Errorf("turtle at 30s: %v", m.E)
	}
	// walking: the legs step faster
	leg := &elem{class: []string{"leg", "a"}}
	slow, _, _ := groundAnim(leg, .3, Mood{})
	fast, _, _ := groundAnim(leg, .3, Mood{Walking: true})
	if fast.F != -2 || slow.F == -2 {
		t.Errorf("leg at .3s: walking %v, strolling %v", fast.F, slow.F)
	}
	z := Zzz(1.08) // the first z at 30% of its 3.6s: most opaque
	if math.Abs(z[0].O-.9) > 1e-9 || z[2].O != 0 {
		t.Errorf("zzz %+v", z)
	}
}

// With reduced motion nothing moves: the cat and the turtles draw the same at any moment, the
// cat in its mood, the turtles parked mid-ground.
func TestStillScenesDoNotMove(t *testing.T) {
	t.Parallel()
	for _, m := range []Mood{{Still: true}, {Awake: true, Still: true}, {Awake: true, Walking: true, Still: true}} {
		for name, s := range map[string]*Scene{"cat": Cat(), "ground": Ground()} {
			a, b := s.Frame(1, m, Identity), s.Frame(13.6, m, Identity)
			if fmt.Sprint(a) != fmt.Sprint(b) {
				t.Errorf("%s in %+v moved between 1s and 13.6s", name, m)
			}
		}
	}
	moving := Ground()
	if fmt.Sprint(moving.Frame(1, Mood{}, Identity)) == fmt.Sprint(moving.Frame(13.6, Mood{}, Identity)) {
		t.Error("without Still the turtles don't move: the test proves nothing")
	}
}

// In a wide window the turtles stroll from beyond its left edge to beyond its right one, at
// the pace they keep in a 560 wide one; parked, they wait in its middle.
func TestTurtlesStrollTheWholeWidth(t *testing.T) {
	t.Parallel()
	turtle := &elem{class: []string{"turtle"}}
	x := func(at float64, m Mood) float64 {
		mat, _, shown := groundAnim(turtle, at, m)
		if !shown {
			t.Fatalf("hidden at %v in %+v", at, m)
		}
		return mat.E
	}
	for _, w := range []float64{0, 560, 1280, 2560} {
		m := Mood{Width: w}
		span := max(w, 560) + 150
		if got := x(0, m); got != -90 {
			t.Errorf("width %v: starts at %v", w, got)
		}
		period := 60 * span / 710
		if got := x(period*.999, m); got < max(w, 560) {
			t.Errorf("width %v: at the end of its walk it is at %v, still inside", w, got)
		}
		if pace := (x(10, m) - x(0, m)) / 10; math.Abs(pace-710.0/60) > 1e-9 {
			t.Errorf("width %v: %v units a second, not %v", w, pace, 710.0/60)
		}
		m.Still = true
		if got := x(5, m); got != 230+(max(w, 560)-560)/2 {
			t.Errorf("width %v: parked at %v", w, got)
		}
	}
	if _, _, shown := groundAnim(turtle, 0, Mood{Layer: PlantLayer}); shown {
		t.Error("a turtle in the plants' layer")
	}
}

// A scene drawn frame after frame keeps its placed paths while they stay put: it must draw
// exactly what a fresh one draws, at every moment, however it moves.
func TestAKeptSceneDrawsAsAFreshOne(t *testing.T) {
	t.Parallel()
	for name, scene := range map[string]func() *Scene{"cat": Cat, "ground": Ground} {
		kept := scene()
		for i := range 90 {
			at, mood := float64(i)*.37, Mood{Awake: i%3 > 0, Walking: i%2 == 0, Width: 560 + float64(i%4)*200}
			base := Scale(1+float64(i%2), 1)
			got, want := kept.Frame(at, mood, base), scene().Frame(at, mood, base)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("%s at %.2fs (%+v): a kept scene drew differently from a fresh one", name, at, mood)
			}
		}
	}
}
