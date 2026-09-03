package render

import "strings"

// Brand colour, applied to both backends at the same two places.
//
// The restraint is the design decision. A brand profile could recolour every
// rule, heading and axis on the page, and the result would be a document that is
// harder to read than the default in exactly the cases where the operator picked
// a colour without thinking about contrast against white. So the colour is used
// **twice**: the rule under the cover title, and the accent bar beside each
// figure. Both are decorative, neither carries meaning, and a hard-to-read
// choice therefore costs nothing but taste.
//
// What is deliberately *not* recoloured: the chart series. Green, red and grey
// on the uptime strip are the legend, not the palette — a brand that turned the
// "no downtime observed" bar orange would be a report whose caption disagreed
// with its own figure.
//
// Both backends read this file, which is what stops the two from drifting into
// different ideas of where a brand colour goes.

// coverAccent is the rule under the cover title.
//
// Falls back to the document ink, which is what the report looked like before
// brand profiles existed and is a deliberate look rather than a missing one.
func (b Brand) coverAccent() Color { return b.accent(inkColor) }

// accent resolves the profile's primary colour, or the supplied default.
func (b Brand) accent(fallback Color) Color {
	if c, ok := ParseHexColor(b.PrimaryColor); ok {
		return c
	}
	return fallback
}

// figureAccent is the thin bar beside a key-value figure. It falls back to the
// grid colour rather than to the ink, because at that weight full-strength ink
// reads as a border rather than as a decoration.
func (b Brand) figureAccent() Color { return b.accent(gridColor) }

// ParseHexColor reads `#rrggbb`, the one form a brand profile stores.
//
// Three-digit shorthand and named colours are refused rather than guessed at.
// The API validates the field on the way in with the same rule, so anything
// reaching here in another shape is a row written by hand — and falling back to
// the default is a better answer than rendering a client's report in whatever a
// lenient parser made of it.
func ParseHexColor(s string) (Color, bool) {
	s = strings.TrimSpace(s)
	if len(s) != 7 || s[0] != '#' {
		return Color{}, false
	}
	var out [3]uint8
	for i := 0; i < 3; i++ {
		hi, ok := hexDigit(s[1+2*i])
		if !ok {
			return Color{}, false
		}
		lo, ok := hexDigit(s[2+2*i])
		if !ok {
			return Color{}, false
		}
		out[i] = hi<<4 | lo
	}
	return Color{R: out[0], G: out[1], B: out[2]}, true
}

func hexDigit(b byte) (uint8, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	}
	return 0, false
}
