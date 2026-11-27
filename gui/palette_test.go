package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/mhashemm/watchparty/gui/art"
)

// Every colour the window's code names turns into a dark one, or is listed here as the same in
// both: one added without a dark counterpart would stay light on the dark page. Drawings'
// colours (cat.svg, ground.svg, art/scene.go, the SVG icons) aren't named this way.
func TestEveryColourHasADarkOne(t *testing.T) {
	t.Parallel()
	sameInDark := map[[3]uint8]string{
		rgb(chartBg):  "the patient chart is a dark screen in both",
		rgb(chartInk): "the patient chart's trace",
	}
	hex := regexp.MustCompile(`Hex\("(#[0-9a-fA-F]{6})"\)`)
	nrgba := regexp.MustCompile(`NRGBA\{(0x[0-9a-fA-F]+|\d+), (0x[0-9a-fA-F]+|\d+), (0x[0-9a-fA-F]+|\d+), (0x[0-9a-fA-F]+|\d+)\}`)
	files, _ := filepath.Glob("*.go")
	files = append(files, filepath.Join("art", "page.go"))
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "palette.go" { // palette.go: the dark side
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		check := func(where string, c [3]uint8) {
			found++
			if _, ok := darkPalette[c]; !ok && sameInDark[c] == "" {
				t.Errorf("%s: %s has no dark colour in darkPalette, nor a reason to stay the same", f, where)
			}
		}
		for _, m := range hex.FindAllStringSubmatch(string(src), -1) {
			check(m[1], rgb(art.Hex(m[1])))
		}
		for _, m := range nrgba.FindAllStringSubmatch(string(src), -1) {
			var c [3]uint8
			for i := range c {
				v, _ := strconv.ParseUint(m[i+1], 0, 8)
				c[i] = uint8(v)
			}
			check(m[0], c)
		}
	}
	if found < 20 {
		t.Fatalf("found only %d colours: the patterns no longer match how colours are written", found)
	}
}
