//go:generate go run ./emojifont fonts/Twemoji-reactions.ttf

package main

import (
	_ "embed"

	"gioui.org/font"
	"gioui.org/font/opentype"
	"gioui.org/text"
)

// The window's fonts, shipped in the binary, licenses beside them in fonts/. The emoji are the
// reactions only, drawn by go generate from mpv's art (emojifont), so text shows them as the
// reactions look; other emoji come from the system's fonts.
var (
	//go:embed fonts/AtkinsonHyperlegible-Regular.ttf
	atkinsonRegular []byte
	//go:embed fonts/AtkinsonHyperlegible-Bold.ttf
	atkinsonBold []byte
	//go:embed fonts/Gluten-Bold.ttf
	glutenBold []byte
	//go:embed fonts/Twemoji-reactions.ttf
	emojiFont []byte
)

// The two typefaces. Each falls back to the bundled emoji before any system font, or a
// system font's black-and-white smileys would draw a reaction; other scripts and emoji still
// come from the system's fonts. The bundled faces go by names of their own (bundled): a
// system font of the same family would otherwise be picked instead.
var (
	bodyFont    = font.Font{Typeface: "wp Atkinson, wp Emoji"}
	boldFont    = font.Font{Typeface: "wp Atkinson, wp Emoji", Weight: font.Bold}
	displayFont = font.Font{Typeface: "wp Gluten, wp Emoji", Weight: font.Bold}
)

// bundled is each embedded font and the family it goes by here.
var bundled = []struct {
	ttf    *[]byte
	family font.Typeface
}{{&atkinsonRegular, "wp Atkinson"}, {&atkinsonBold, "wp Atkinson"}, {&glutenBold, "wp Gluten"}, {&emojiFont, "wp Emoji"}}

// newShaper is the window's text shaper: the bundled fonts, then the system's.
func newShaper() (*text.Shaper, error) {
	var faces []font.FontFace
	for _, b := range bundled {
		f, err := opentype.ParseCollection(*b.ttf)
		if err != nil {
			return nil, err
		}
		for i := range f {
			f[i].Font.Typeface = b.family
		}
		faces = append(faces, f...)
	}
	return text.NewShaper(text.WithCollection(faces)), nil
}
