package render

import (
	_ "embed"
	"fmt"
	"sync"
)

// The embedded report face.
//
// Roboto, at regular and bold, from the Google Fonts static release. Chosen by
// the maintainer on 2026-09-01 and vendored under the SIL Open Font License 1.1;
// the licence travels beside the files in fonts/OFL.txt and its text is repeated
// in the repository's NOTICE, because the OFL requires the licence to accompany
// the font wherever it goes — and here it goes inside a binary.
//
// # Why a file rather than a base-14 name
//
// ADR-007 item 4 rejects the fourteen standard PDF fonts. They cost no bytes but
// lock encoding to WinAnsi and produce a document that looks generic, which
// defeats a white-label feature whose entire purpose is that the client believes
// their agency made it.
//
// # Why this face
//
// Two properties were checked against the files rather than assumed, and both
// are asserted by the tests beside this file:
//
//   - **Tabular figures by default.** All ten digits share one advance width, so
//     a column of percentages lines up on the decimal point. This matters more
//     here than in most typography: the PDF backend applies no OpenType
//     features, so `tnum` is unreachable and a face with proportional figures
//     would produce a drifting decimal point in every table. The HTML report
//     asks for tabular numerals in CSS; this is what makes the PDF agree with it.
//   - **Coverage of what the report actually draws**, including the `×` of the
//     burn rate, the en dash in a period range and the ellipsis of a truncated
//     table cell.
//
// # The commitment
//
// ADR-007 calls the embedded font a binary-size commitment and a visual identity
// commitment in one: changing it reflows every future report, though not the
// artifacts already rendered and stored — those are files, and files do not
// re-render. About 310 KB of the binary, against the ADR's stated budget of a
// few hundred kilobytes to a megabyte.

//go:embed fonts/Roboto-Regular.ttf
var robotoRegular []byte

//go:embed fonts/Roboto-Bold.ttf
var robotoBold []byte

var (
	embeddedOnce sync.Once
	embedded     Family
	embeddedErr  error
)

// Embedded is the family the PDF backend draws with.
//
// Parsed once and cached: the face is immutable and the parse walks a cmap, so
// doing it per run would be work with no answer that changes. A parse failure is
// returned rather than panicked — the bytes are compiled in and so a failure is
// a build-time mistake, but a report run is not the place to discover it with a
// stack trace.
func Embedded() (Family, error) {
	embeddedOnce.Do(func() {
		regular, err := ParseTrueType(robotoRegular)
		if err != nil {
			embeddedErr = fmt.Errorf("parse embedded regular face: %w", err)
			return
		}
		bold, err := ParseTrueType(robotoBold)
		if err != nil {
			embeddedErr = fmt.Errorf("parse embedded bold face: %w", err)
			return
		}
		embedded = Family{Regular: regular, Bold: bold}
	})
	return embedded, embeddedErr
}
