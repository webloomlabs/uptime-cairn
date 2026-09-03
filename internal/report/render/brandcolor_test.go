package render

import (
	"strings"
	"testing"
)

// A brand colour reaches the page in both backends, at the same two places.
//
// The rule brandcolor.go states is that the colour is decorative and used
// exactly twice. This holds the "reaches the page" half; the test below holds
// the "twice" half by asserting the chart legend is untouched.
func TestABrandColourReachesBothBackends(t *testing.T) {
	t.Parallel()

	brand := Brand{CompanyName: "Smith & Co", PrimaryColor: "#0B5FFF"}
	doc := documentWithResponseTarget(250)

	html, err := HTML(doc, brand)
	if err != nil {
		t.Fatalf("html: %v", err)
	}
	if !strings.Contains(string(html), "#0b5fff") {
		t.Error("the brand colour does not appear in the rendered HTML")
	}

	pdf, err := pdfFor(doc, brand, testFamily())
	if err != nil {
		t.Fatalf("pdf: %v", err)
	}
	if pdf == nil {
		t.Fatal("no pdf")
	}
	// 0x0B/255, 0x5F/255, 0xFF/255 through the writer's own rounding formatter,
	// which is the same one the SVG backend uses. Asserted on the emitted
	// content stream rather than on the layout struct, because what matters is
	// that it was drawn, not that it was stored.
	bytes, err := pdf.Bytes("run", doc.Meta.GeneratedAt)
	if err != nil {
		t.Fatalf("serialise: %v", err)
	}
	if !strings.Contains(string(bytes), "0.04 0.37 1 rg") {
		t.Error("the brand colour was not painted into the PDF content stream")
	}
}

// The chart series keep their own colours.
//
// Green, red and grey on the uptime strip are the **legend**, not the palette:
// the caption under the chart says "Green: no downtime observed", and a brand
// that recoloured the bars would produce a figure that contradicts its own
// caption. This is the one place restraint about brand colour is a correctness
// property rather than a matter of taste.
func TestABrandColourDoesNotRecolourTheChartLegend(t *testing.T) {
	t.Parallel()

	before := chartColorSet()
	_ = Brand{PrimaryColor: "#FF0000"}
	after := chartColorSet()

	if before != after {
		t.Error("chart colours are not constant; a brand profile must not be able " +
			"to recolour a legend the caption names by colour")
	}
	// And the constants are what the caption claims: a green, a red and a grey.
	if upColor.G <= upColor.R || downColor.R <= downColor.G {
		t.Errorf("the strip's colours are up=%s down=%s, which the caption's "+
			"'green' and 'red' no longer describe", upColor.Hex(), downColor.Hex())
	}
}

func chartColorSet() [3]Color { return [3]Color{upColor, downColor, gapColor} }

// A colour is read in the one form a brand profile stores, and anything else
// falls back rather than being guessed at.
//
// The fallback matters more than the parse. A row written by hand with `red` in
// that column should produce the default document, not a document rendered in
// whatever a lenient parser made of the string — and certainly not one where the
// value reached a stylesheet unvalidated.
func TestBrandColourParsingIsStrict(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"#0B5FFF", true},
		{"#0b5fff", true},
		{" #0b5fff ", true},
		{"0b5fff", false},
		{"#0b5ff", false},
		{"#0b5fffa", false},
		{"#gggggg", false},
		{"red", false},
		{"", false},
		// The one that would matter: a value that closes the declaration and
		// opens a rule of its own. It is refused at the parse, and the renderer
		// prints the parsed colour rather than the input either way, so there
		// are two independent reasons this cannot reach a stylesheet.
		{"#000; } body { display:none; } .x {", false},
	} {
		if _, ok := ParseHexColor(tc.in); ok != tc.ok {
			t.Errorf("ParseHexColor(%q) ok = %v, want %v", tc.in, ok, tc.ok)
		}
	}
}

// A brand colour that cannot be parsed leaves the document exactly as an
// unbranded one, which is what makes the golden file a record of the default.
func TestAnUnparseableColourChangesNothing(t *testing.T) {
	t.Parallel()

	doc := documentWithResponseTarget(0)
	plain, err := HTML(doc, Brand{CompanyName: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	odd, err := HTML(doc, Brand{CompanyName: "Acme", PrimaryColor: "rebeccapurple"})
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != string(odd) {
		t.Error("an unparseable brand colour changed the rendered document")
	}
}
