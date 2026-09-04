package render

import (
	"fmt"
	"strings"

	"github.com/webloomlabs/uptime-cairn/internal/report"
)

// The layout pass: the seven elements of ADR-007 item 3, flowed onto pages.
//
// This is the part the ADR calls out as owned work — "we own a layout engine.
// Bounded, but ours" — and the part it warns is the time-sink: table
// page-breaking, with widow and orphan handling, header repetition, and a row
// taller than the space left. All four are handled below and each is commented
// where it happens, because the next person to touch this will be looking at a
// broken table and needs to know which rule bit.
//
// It is a single forward pass with no backtracking. Every element reports the
// height it needs before it draws, and a break happens before it rather than
// inside it — except a table, which is the one element that breaks internally
// because it is the one element that can be longer than a page.

// Page geometry, in points. A4 rather than Letter: the project is self-hosted
// and international, A4 is the majority of the world, and the difference matters
// only to somebody printing — which ADR-007 item 8 already says is not the point
// of this feature. A per-brand page size is a brand-profile field if anybody
// asks for one; it is not a decision to bury in a constant block.
const (
	pageW = 595.28
	pageH = 841.89

	marginX      = 56.0
	marginTop    = 56.0
	marginBottom = 64.0

	contentW = pageW - 2*marginX
	contentB = pageH - marginBottom
)

// Type scale. One place, so that a size changed for a heading cannot silently
// leave the table it introduces at the old one.
const (
	sizeTitle   = 24.0
	sizeClient  = 14.0
	sizeH1      = 14.5
	sizeH2      = 10.0
	sizeBody    = 9.5
	sizeSmall   = 8.0
	sizeFigure  = 15.0
	sizeKey     = 7.5
	sizeTable   = 8.5
	sizeCaption = 7.5

	lineRatio = 1.45
)

type pdfLayout struct {
	pdf    *PDF
	family Family

	// cover and figure are the two marks the brand profile colours. Resolved
	// once at the top of the flow rather than looked up per element, so the same
	// two decisions are made once and both backends make them the same way — see
	// brandcolor.go.
	coverRule   Color
	figureAccnt Color

	// y is the top of the next thing to be drawn, in top-down page coordinates.
	y float64
}

// PDFDocument renders the report as a PDF.
//
// A sibling of HTML, never a conversion of it (ADR-007 item 1): both take the
// composed element list, and neither takes the other's output. The signature
// takes the family rather than naming one, because the embedded face is a
// vendored asset and a visual identity commitment that belongs to the
// maintainer — see the note at the top of font.go.
func PDFDocument(doc report.Document, brand Brand, family Family) ([]byte, error) {
	return PDFSections(doc, brand, family, nil)
}

// PDFSections is PDFDocument with a template's chosen content blocks. A nil
// selection composes the defaults, which is what PDFDocument passes.
func PDFSections(doc report.Document, brand Brand, family Family, sections []string) ([]byte, error) {
	p, err := pdfFor(doc, brand, family, sections)
	if err != nil {
		return nil, err
	}
	return p.Bytes(doc.Meta.ReportRunID.String(), doc.Meta.GeneratedAt)
}

// pdfFor is the drawn document before it is serialised. It exists so that the
// tests can assert on pages — what broke where, what is on the cover — without
// restating the flow loop below, which is the way a test ends up passing against
// a copy of the code rather than against the code.
func pdfFor(doc report.Document, brand Brand, family Family, sections []string) (*PDF, error) {
	if family.Regular == nil {
		return nil, fmt.Errorf("render: PDF needs an embedded font family")
	}
	l := flow(ComposeSections(doc, brand, sections), family, brand)
	l.runningFooters()
	return l.pdf, nil
}

// flow places the element list onto pages. One pass, no backtracking.
func flow(elements []Element, family Family, brand Brand) *pdfLayout {
	l := &pdfLayout{
		pdf:         NewPDF(family),
		family:      family,
		coverRule:   brand.coverAccent(),
		figureAccnt: brand.figureAccent(),
	}

	// The cover is its own page. It is what an agency's client sees first and
	// the reason the white-label feature exists; a cover crammed above the first
	// figure block is a page nobody would put their own name on.
	l.newPage()

	var footer Footer
	for _, el := range elements {
		switch e := el.(type) {
		case Cover:
			l.cover(e)
			l.newPage()
		case Heading:
			l.heading(e)
		case Paragraph:
			l.paragraph(e)
		case KeyValues:
			l.keyValues(e)
		case Table:
			l.table(e)
		case Chart:
			l.chart(e)
		case Footer:
			// Held back: it closes the document, and where the document closes
			// is not known until every other element has been placed.
			footer = e
		}
	}
	l.footer(footer)
	return l
}

func (l *pdfLayout) newPage() {
	l.pdf.NewPage(pageW, pageH)
	l.y = marginTop
}

// space reserves vertical room, breaking the page if it will not fit.
//
// Returns true if a break happened, which the callers that must not be orphaned
// from what follows them — a heading, a chart title — use to redraw at the top.
func (l *pdfLayout) space(h float64) bool {
	if l.y+h <= contentB {
		return false
	}
	l.newPage()
	return true
}

func (l *pdfLayout) face(w Weight) Font { return l.family.Face(w) }

// text draws one line and advances no cursor. Positioning is the caller's job,
// because every element has a different idea of leading.
func (l *pdfLayout) text(x, baseline float64, s string, size float64, weight Weight, c Color, a Anchor) {
	if s == "" {
		return
	}
	l.pdf.Text(x, baseline, Run{Text: s}, TextStyle{SizePt: size, Weight: weight, Fill: c, Anchor: a})
}

// --- elements --------------------------------------------------------------

func (l *pdfLayout) cover(c Cover) {
	l.y = marginTop + 90

	if len(c.Logo) > 0 {
		// Bounded to the same box the HTML gives it, so a brand that looks right
		// on the page looks right in the file.
		l.pdf.Image(Rect{X: marginX, Y: l.y, W: 240, H: 56}, c.LogoMIME, c.Logo)
		l.y += 56 + 28
	}

	l.y += sizeTitle
	l.text(marginX, l.y, c.Title, sizeTitle, Bold, inkColor, Start)
	l.y += 18

	if c.ClientName != "" {
		l.y += sizeClient
		l.text(marginX, l.y, "Prepared for "+c.ClientName, sizeClient, Bold, inkColor, Start)
		l.y += 8
	}

	l.y += sizeBody
	l.text(marginX, l.y, c.Period, sizeBody+1, Regular, mutedColor, Start)
	l.y += 14
	l.text(marginX, l.y, "Generated "+c.Generated, sizeSmall, Regular, mutedColor, Start)
	l.y += 20

	l.pdf.Rect(Rect{X: marginX, Y: l.y, W: contentW, H: 2}, Fill(l.coverRule))
}

func (l *pdfLayout) heading(h Heading) {
	size, weight, colour := sizeH1, Bold, inkColor
	gapAbove, gapBelow := 26.0, 10.0
	if h.Level > 1 {
		size, colour = sizeH2, mutedColor
		gapAbove, gapBelow = 18.0, 7.0
	}

	// Keep-with-next, the cheap version: a heading needs room for itself and for
	// something under it. Sixty points is about three body lines or the top of a
	// figure block — enough that a heading is never the last thing on a page,
	// which is the failure this rule exists to prevent.
	needed := gapAbove + size + gapBelow + 60
	if l.space(needed) {
		gapAbove = 0
	}

	l.y += gapAbove + size
	l.text(marginX, l.y, h.Text, size, weight, colour, Start)
	l.y += 4

	if h.Level == 1 {
		l.pdf.Rect(Rect{X: marginX, Y: l.y, W: contentW, H: 0.6}, Fill(gridColor))
	}
	l.y += gapBelow
}

func (l *pdfLayout) paragraph(p Paragraph) {
	size, colour := sizeBody, inkColor
	if p.Muted {
		size, colour = sizeSmall, mutedColor
	}
	leading := size * lineRatio

	for _, line := range wrap(l.face(Regular), p.Text, size, contentW) {
		l.space(leading)
		l.y += leading
		l.text(marginX, l.y, line, size, Regular, colour, Start)
	}
	l.y += 8
}

// keyValues draws the figure block as a three-column grid, mirroring the HTML
// `repeat(auto-fit, minmax(180px, 1fr))` at the width a page actually has.
//
// A cell never splits across a page: a value without its label, or a label
// without the note that qualifies it, is the failure mode §4.3 spends its whole
// length preventing.
func (l *pdfLayout) keyValues(kv KeyValues) {
	if len(kv.Items) == 0 {
		return
	}
	const columns = 3
	const gutter = 14.0
	cellW := (contentW - gutter*(columns-1)) / columns

	for row := 0; row < len(kv.Items); row += columns {
		end := min(row+columns, len(kv.Items))
		items := kv.Items[row:end]

		height := 0.0
		notes := make([][]string, len(items))
		for i, item := range items {
			notes[i] = wrap(l.face(Regular), item.Note, sizeCaption, cellW-12)
			h := sizeKey + 4 + sizeFigure + float64(len(notes[i]))*(sizeCaption*1.3) + 10
			height = max(height, h)
		}

		l.space(height)
		top := l.y
		for i, item := range items {
			x := marginX + float64(i)*(cellW+gutter)

			// The left rule the HTML draws with a border, for the same reason:
			// three figures side by side with no separation read as one
			// sentence.
			l.pdf.Rect(Rect{X: x, Y: top, W: 2, H: height - 8}, Fill(l.figureAccnt))

			y := top + sizeKey
			l.text(x+10, y, item.Key, sizeKey, Regular, mutedColor, Start)
			y += 4 + sizeFigure
			l.text(x+10, y, item.Value, sizeFigure, Bold, inkColor, Start)
			for _, line := range notes[i] {
				y += sizeCaption * 1.3
				l.text(x+10, y, line, sizeCaption, Regular, mutedColor, Start)
			}
		}
		l.y = top + height
	}
	l.y += 6
}

func (l *pdfLayout) chart(c Chart) {
	// The chart is drawn at the page's content width rather than the HTML's
	// fixed 660 points. That is not a second chart: the HTML SVG scales to its
	// container, so both are the same drawing at a different scale, and the
	// labels inside are the one thing that does not scale with it.
	titleH := 0.0
	if c.Title != "" {
		titleH = sizeCaption + 6
	}
	body := stripHeight
	if c.Kind == ChartLatencyLine {
		body = latencyHeight
	}
	captionLines := wrap(l.face(Regular), c.Caption, sizeCaption, contentW)
	captionH := float64(len(captionLines)) * (sizeCaption * 1.35)

	l.space(titleH + body + captionH + 12)

	if c.Title != "" {
		l.y += sizeCaption
		l.text(marginX, l.y, c.Title, sizeCaption, Regular, mutedColor, Start)
		l.y += 6
	}

	area := Rect{X: marginX, Y: l.y, W: contentW, H: body}
	switch c.Kind {
	case ChartUptimeStrip:
		UptimeStrip(l.pdf, area, c.Days)
	case ChartLatencyLine:
		plot := Rect{X: area.X + latencyAxisRoom, Y: area.Y + 6, W: area.W - latencyAxisRoom - 4, H: area.H - 28}
		low, high, ok := LatencyLine(l.pdf, plot, c.Latency)
		if ok {
			l.text(area.X+latencyAxisRoom-6, plot.Y+8, millisLabel(high), sizeCaption-0.5, Regular, mutedColor, End)
			l.text(area.X+latencyAxisRoom-6, plot.Y+plot.H, millisLabel(low), sizeCaption-0.5, Regular, mutedColor, End)
			if len(c.Latency) > 0 {
				l.text(area.X+latencyAxisRoom, area.Y+area.H-6,
					c.Latency[0].Date.Format("2 Jan"), sizeCaption-0.5, Regular, mutedColor, Start)
				l.text(area.X+area.W-4, area.Y+area.H-6,
					c.Latency[len(c.Latency)-1].Date.Format("2 Jan"), sizeCaption-0.5, Regular, mutedColor, End)
			}
		} else {
			l.text(area.X+area.W/2, area.Y+area.H/2, "No measurements in this period",
				sizeBody, Regular, mutedColor, Middle)
		}
	}
	l.y += body + 6

	for _, line := range captionLines {
		l.y += sizeCaption * 1.35
		l.text(marginX, l.y, line, sizeCaption, Regular, mutedColor, Start)
	}
	l.y += 12
}

func (l *pdfLayout) footer(f Footer) {
	if f.Text == "" && f.HidePoweredBy {
		return
	}
	l.space(40)
	l.y += 16
	l.pdf.Rect(Rect{X: marginX, Y: l.y, W: contentW, H: 0.6}, Fill(gridColor))
	l.y += 6

	for _, line := range wrap(l.face(Regular), f.Text, sizeSmall, contentW) {
		l.y += sizeSmall * lineRatio
		l.text(marginX, l.y, line, sizeSmall, Regular, mutedColor, Start)
	}
	if !f.HidePoweredBy {
		l.y += sizeSmall * lineRatio
		l.text(marginX, l.y, "Generated by Uptime Cairn", sizeSmall, Regular, mutedColor, Start)
	}
}

// runningFooters writes "page n of m" into the bottom margin of every page.
//
// A second pass, because the total is not known until the first one finishes. A
// report is a document somebody prints, staples and disputes a figure in; a page
// with no number in it is a page that can be removed from the middle of one
// without anybody noticing.
func (l *pdfLayout) runningFooters() {
	total := l.pdf.PageCount()
	for i := range total {
		l.pdf.selectPage(i)
		l.pdf.Text(pageW-marginX, pageH-marginBottom+30,
			Run{Text: fmt.Sprintf("Page %d of %d", i+1, total)},
			TextStyle{SizePt: sizeCaption, Fill: mutedColor, Anchor: End})
	}
}

// --- tables ----------------------------------------------------------------

// maxCellLines bounds a wrapped cell.
//
// It is what guarantees that no row is taller than a page, which is the third of
// ADR-007's named table hazards and the only one with no good answer: a row that
// cannot fit anywhere has to either overflow the margin or lose text. Three
// lines of a table cell is roughly 150 characters — long past any monitor name
// somebody meant — and the truncation is marked with an ellipsis so that a
// reader can see something was cut rather than reading a shortened name as the
// whole of it.
const maxCellLines = 3

func (l *pdfLayout) table(t Table) {
	if len(t.Columns) == 0 {
		return
	}

	widths := l.columnWidths(t)
	const padX, padY = 8.0, 5.0
	headerH := sizeTable + 2*padY

	// Every row is measured before anything is drawn, because a break decision
	// needs to know what is coming, not only what has been.
	rows := make([][][]string, len(t.Rows))
	heights := make([]float64, len(t.Rows))
	for i, row := range t.Rows {
		lines := make([][]string, len(t.Columns))
		tallest := 1
		for c := range t.Columns {
			cell := ""
			if c < len(row) {
				cell = row[c]
			}
			lines[c] = clampLines(wrap(l.face(Regular), cell, sizeTable, widths[c]-2*padX), maxCellLines)
			tallest = max(tallest, len(lines[c]))
		}
		rows[i] = lines
		heights[i] = float64(tallest)*(sizeTable*1.35) + 2*padY
	}

	i := 0
	for i < len(rows) {
		// A header alone at the foot of a page is an orphan. It needs itself and
		// at least one row, or the whole thing starts on the next page.
		if l.y+headerH+heights[i] > contentB {
			l.newPage()
		}
		l.tableHeader(t, widths, headerH, padX, padY)

		// How many rows fit from here.
		fit := 0
		used := 0.0
		for j := i; j < len(rows); j++ {
			if l.y+used+heights[j] > contentB {
				break
			}
			used += heights[j]
			fit++
		}

		// Widow control: never carry a single row over on its own. Pulling one
		// row back costs a line of whitespace here and buys a continuation page
		// that does not consist of a repeated header and one line — which reads
		// as a printing fault rather than as a table.
		if remaining := len(rows) - i - fit; remaining == 1 && fit > 1 {
			fit--
		}
		if fit == 0 {
			// Only reachable if a single row is taller than a whole page, which
			// clampLines makes impossible. Drawing one anyway beats looping.
			fit = 1
		}

		for j := i; j < i+fit; j++ {
			l.tableRow(t, widths, rows[j], heights[j], padX, padY)
		}
		i += fit
	}
	l.y += 12
}

func (l *pdfLayout) tableHeader(t Table, widths []float64, h, padX, padY float64) {
	x := marginX
	for c, col := range t.Columns {
		anchor, tx := Start, x+padX
		if col.Numeric {
			anchor, tx = End, x+widths[c]-padX
		}
		l.text(tx, l.y+padY+sizeTable, strings.ToUpper(col.Title), sizeTable-1.5, Bold, mutedColor, anchor)
		x += widths[c]
	}
	l.y += h
	l.pdf.Rect(Rect{X: marginX, Y: l.y - 1, W: contentW, H: 0.8}, Fill(gridColor))
}

func (l *pdfLayout) tableRow(t Table, widths []float64, lines [][]string, h, padX, padY float64) {
	top := l.y
	x := marginX
	for c, col := range t.Columns {
		anchor, tx := Start, x+padX
		if col.Numeric {
			anchor, tx = End, x+widths[c]-padX
		}
		y := top + padY + sizeTable
		for _, line := range lines[c] {
			l.text(tx, y, line, sizeTable, Regular, inkColor, anchor)
			y += sizeTable * 1.35
		}
		x += widths[c]
	}
	l.y = top + h
	l.pdf.Rect(Rect{X: marginX, Y: l.y - 0.5, W: contentW, H: 0.5}, Fill(gridColor))
}

// columnWidths gives each column what its widest cell needs, then takes the
// overflow back from the widest text column rather than from everything.
//
// Shrinking proportionally would narrow a date column that is already exactly as
// wide as a date, and the wrapping that follows would put "2026" on a second
// line. The column that can afford to wrap is the one with prose in it.
func (l *pdfLayout) columnWidths(t Table) []float64 {
	const padX = 8.0
	natural := make([]float64, len(t.Columns))
	total := 0.0

	for c, col := range t.Columns {
		w := Measure(l.face(Bold), strings.ToUpper(col.Title), sizeTable-1.5)
		for _, row := range t.Rows {
			if c < len(row) {
				w = max(w, Measure(l.face(Regular), row[c], sizeTable))
			}
		}
		natural[c] = w + 2*padX
		total += natural[c]
	}

	if total <= contentW {
		// Spare width goes to the last non-numeric column, so the table fills the
		// measure instead of ending in a ragged edge two thirds across the page.
		spare := contentW - total
		target := len(t.Columns) - 1
		for c := range t.Columns {
			if !t.Columns[c].Numeric {
				target = c
			}
		}
		natural[target] += spare
		return natural
	}

	over := total - contentW
	for over > 0 {
		widest, widestW := -1, 0.0
		for c := range t.Columns {
			if !t.Columns[c].Numeric && natural[c] > widestW {
				widest, widestW = c, natural[c]
			}
		}
		if widest < 0 {
			// Every column is numeric and the table is still too wide. Nothing
			// left to do but scale them all and let the figures sit closer.
			scale := contentW / total
			for c := range natural {
				natural[c] *= scale
			}
			return natural
		}
		take := min(over, widestW*0.4)
		natural[widest] -= take
		over -= take
	}
	return natural
}

// --- text measurement ------------------------------------------------------

// wrap breaks text to a width, in points.
//
// Greedy and space-separated, which is what a report needs and the whole of what
// Phase 2 promises: ADR-007 item 5 puts non-Latin scripts out of scope, and a
// line-breaking algorithm that respects them belongs with the shaping layer that
// arrives at the same time.
//
// A single word wider than the line is broken by character rather than left to
// run off the page. That word is a URL or a monitor named without spaces, and
// both are ordinary.
func wrap(f Font, s string, size, width float64) []string {
	if s == "" {
		return nil
	}
	if f == nil || width <= 0 {
		return []string{s}
	}

	var out []string
	for _, word := range strings.Fields(s) {
		if len(out) == 0 {
			out = append(out, word)
			continue
		}
		candidate := out[len(out)-1] + " " + word
		if Measure(f, candidate, size) <= width {
			out[len(out)-1] = candidate
			continue
		}
		out = append(out, word)
	}

	// Second pass for the over-long word, kept separate so the common path stays
	// one comparison per word.
	var final []string
	for _, line := range out {
		for Measure(f, line, size) > width && len([]rune(line)) > 1 {
			runes := []rune(line)
			cut := len(runes)
			for cut > 1 && Measure(f, string(runes[:cut]), size) > width {
				cut--
			}
			final = append(final, string(runes[:cut]))
			line = string(runes[cut:])
		}
		final = append(final, line)
	}
	return final
}

func clampLines(lines []string, max int) []string {
	if len(lines) <= max {
		return lines
	}
	out := append([]string(nil), lines[:max]...)
	out[max-1] = strings.TrimRight(out[max-1], " ") + "…"
	return out
}
