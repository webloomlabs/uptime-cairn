package render

import (
	"encoding/base64"
	"fmt"
	"html"
	"strings"

	"github.com/webloomlabs/uptime-cairn/internal/report"
)

// chart geometry, in points. Fixed rather than responsive: the same numbers are
// what the PDF backend will lay out against, and a chart that reflows in HTML
// and not in PDF is two charts again.
const (
	chartWidth      = 660.0
	stripHeight     = 22.0
	latencyHeight   = 120.0
	latencyAxisRoom = 34.0
)

// HTML renders the canonical report.
//
// Canonical in the sense ADR-007 means: it is the format that ships regardless,
// and the one the PDF is measured against — **not** the one the PDF is made
// from. Both consume the element list; neither consumes the other.
//
// Self-contained by construction: styles are inline in a single <style>, charts
// are inline SVG, and a logo is a data URI. An artifact is a record, and a
// record that needs a CDN to render is not one. It is also what makes the file
// safe to email, which is how most of these actually travel.
func HTML(doc report.Document, brand Brand) ([]byte, error) {
	return HTMLSections(doc, brand, nil)
}

// HTMLSections is HTML with a template's chosen content blocks. A nil selection
// composes the defaults, which is what HTML passes.
func HTMLSections(doc report.Document, brand Brand, sections []string) ([]byte, error) {
	elements := ComposeSections(doc, brand, sections)

	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"en\">\n<head>\n")
	b.WriteString(`<meta charset="utf-8">` + "\n")
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">` + "\n")
	// A report is a client's operational data. Even where it is only ever
	// emailed, a share link puts it on a URL, and a stray indexer finding one is
	// a disclosure with no code defect behind it.
	b.WriteString(`<meta name="robots" content="noindex, nofollow">` + "\n")
	fmt.Fprintf(&b, "<title>%s</title>\n", html.EscapeString(documentTitle(elements)))
	b.WriteString("<style>\n" + reportCSS + brandCSS(brand) + "</style>\n</head>\n<body>\n<main class=\"report\">\n")

	for _, el := range elements {
		writeElement(&b, el)
	}

	b.WriteString("</main>\n</body>\n</html>\n")
	return []byte(b.String()), nil
}

// brandCSS recolours the two marks brandcolor.go names, and nothing else.
//
// Appended rather than interpolated into reportCSS, so an unbranded report is
// byte-identical to what it was before brand colours existed — which keeps the
// golden file a record of the default rather than of the last profile somebody
// tested with. The value is re-rendered from a parsed Color rather than
// interpolated from the stored string: a profile row written by hand cannot then
// close the declaration and inject a rule of its own.
func brandCSS(brand Brand) string {
	if _, ok := ParseHexColor(brand.PrimaryColor); !ok {
		return ""
	}
	return ".cover { border-bottom-color: " + brand.coverAccent().Hex() + "; }\n" +
		".figures > div { border-left-color: " + brand.figureAccent().Hex() + "; }\n"
}

func documentTitle(elements []Element) string {
	for _, el := range elements {
		if cover, ok := el.(Cover); ok {
			if cover.ClientName != "" {
				return cover.Title + " — " + cover.ClientName
			}
			return cover.Title
		}
	}
	return "Uptime report"
}

func writeElement(b *strings.Builder, el Element) {
	switch e := el.(type) {
	case Cover:
		b.WriteString(`<header class="cover">` + "\n")
		if len(e.Logo) > 0 {
			fmt.Fprintf(b, `<img class="logo" alt="" src="data:%s;base64,%s">`+"\n",
				e.LogoMIME, base64.StdEncoding.EncodeToString(e.Logo))
		}
		fmt.Fprintf(b, `<h1>%s</h1>`+"\n", esc(e.Title))
		if e.ClientName != "" {
			fmt.Fprintf(b, `<p class="client">Prepared for %s</p>`+"\n", esc(e.ClientName))
		}
		fmt.Fprintf(b, `<p class="period">%s</p>`+"\n", esc(e.Period))
		fmt.Fprintf(b, `<p class="generated">Generated %s</p>`+"\n", esc(e.Generated))
		b.WriteString("</header>\n")

	case Heading:
		tag := "h2"
		if e.Level > 1 {
			tag = "h3"
		}
		fmt.Fprintf(b, "<%s>%s</%s>\n", tag, esc(e.Text), tag)

	case Paragraph:
		class := ""
		if e.Muted {
			class = ` class="muted"`
		}
		fmt.Fprintf(b, "<p%s>%s</p>\n", class, esc(e.Text))

	case KeyValues:
		b.WriteString(`<dl class="figures">` + "\n")
		for _, item := range e.Items {
			b.WriteString("<div>\n")
			fmt.Fprintf(b, "<dt>%s</dt>\n", esc(item.Key))
			fmt.Fprintf(b, "<dd>%s", esc(item.Value))
			if item.Note != "" {
				fmt.Fprintf(b, `<span class="note">%s</span>`, esc(item.Note))
			}
			b.WriteString("</dd>\n</div>\n")
		}
		b.WriteString("</dl>\n")

	case Table:
		b.WriteString(`<table>` + "\n<thead>\n<tr>")
		for _, c := range e.Columns {
			class := ""
			if c.Numeric {
				class = ` class="num"`
			}
			fmt.Fprintf(b, "<th%s>%s</th>", class, esc(c.Title))
		}
		b.WriteString("</tr>\n</thead>\n<tbody>\n")
		for _, row := range e.Rows {
			b.WriteString("<tr>")
			for i, cell := range row {
				class := ""
				if i < len(e.Columns) && e.Columns[i].Numeric {
					class = ` class="num"`
				}
				fmt.Fprintf(b, "<td%s>%s</td>", class, esc(cell))
			}
			b.WriteString("</tr>\n")
		}
		b.WriteString("</tbody>\n</table>\n")

	case Chart:
		b.WriteString(`<figure class="chart">` + "\n")
		if e.Title != "" {
			fmt.Fprintf(b, `<figcaption class="chart-title">%s</figcaption>`+"\n", esc(e.Title))
		}
		b.WriteString(drawChart(e) + "\n")
		if e.Caption != "" {
			fmt.Fprintf(b, `<figcaption class="note">%s</figcaption>`+"\n", esc(e.Caption))
		}
		b.WriteString("</figure>\n")

	case Footer:
		b.WriteString(`<footer>` + "\n")
		if e.Text != "" {
			fmt.Fprintf(b, "<p>%s</p>\n", esc(e.Text))
		}
		if !e.HidePoweredBy {
			b.WriteString("<p class=\"note\">Generated by Uptime Cairn</p>\n")
		}
		b.WriteString("</footer>\n")
	}
}

// drawChart renders one chart through the SVG backend — the same calls the PDF
// backend will receive, which is the property that keeps the two from drifting.
func drawChart(c Chart) string {
	switch c.Kind {
	case ChartUptimeStrip:
		svg := NewSVG(chartWidth, stripHeight)
		UptimeStrip(svg, Rect{W: chartWidth, H: stripHeight}, c.Points)
		return svg.Document()

	case ChartLatencyLine:
		svg := NewSVG(chartWidth, latencyHeight)
		plot := Rect{X: latencyAxisRoom, Y: 6, W: chartWidth - latencyAxisRoom - 4, H: latencyHeight - 28}
		low, high, ok := LatencyLine(svg, plot, c.Points)
		if !ok {
			// Nothing measured. An empty frame with a word in it beats an empty
			// box the reader has to interpret.
			svg.Text(chartWidth/2, latencyHeight/2, Run{Text: "No measurements in this period"},
				TextStyle{SizePt: 10, Fill: mutedColor, Anchor: Middle})
			return svg.Document()
		}
		// The axis, because an unlabelled y-axis makes the chart decoration.
		svg.Text(latencyAxisRoom-6, plot.Y+8, Run{Text: millisLabel(high)},
			TextStyle{SizePt: 8, Fill: mutedColor, Anchor: End})
		svg.Text(latencyAxisRoom-6, plot.Y+plot.H, Run{Text: millisLabel(low)},
			TextStyle{SizePt: 8, Fill: mutedColor, Anchor: End})
		if len(c.Points) > 0 {
			svg.Text(latencyAxisRoom, latencyHeight-6, Run{Text: c.Points[0].At.Format(c.axisFormat())},
				TextStyle{SizePt: 8, Fill: mutedColor, Anchor: Start})
			svg.Text(chartWidth-4, latencyHeight-6, Run{Text: c.Points[len(c.Points)-1].At.Format(c.axisFormat())},
				TextStyle{SizePt: 8, Fill: mutedColor, Anchor: End})
		}
		return svg.Document()
	}
	return ""
}

func millisLabel(v float64) string { return trimZeros(fmt.Sprintf("%.0f", v)) + "ms" }

func esc(s string) string { return html.EscapeString(s) }

// reportCSS is the whole of the styling, inline.
//
// The print rules are a complement rather than a substitute (ADR-007 item 8): a
// PDF saved by hand fifty times is not the exit condition, and this does not
// pretend otherwise. What they buy is that the operator who prints one before
// the PDF backend lands gets something that does not break mid-figure.
const reportCSS = `
:root { color-scheme: light; }
* { box-sizing: border-box; }
body {
  margin: 0; padding: 32px 24px;
  font: 14px/1.55 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
  color: #1a1f2b; background: #f6f7f9;
}
.report { max-width: 760px; margin: 0 auto; background: #fff; padding: 40px; border-radius: 6px; }
.cover { border-bottom: 2px solid #1a1f2b; padding-bottom: 20px; margin-bottom: 24px; }
.cover .logo { max-height: 56px; max-width: 240px; margin-bottom: 16px; display: block; }
h1 { font-size: 26px; margin: 0 0 6px; letter-spacing: -0.01em; }
h2 { font-size: 17px; margin: 32px 0 10px; padding-bottom: 6px; border-bottom: 1px solid #e2e5ea; }
h3 { font-size: 14px; margin: 22px 0 8px; color: #6b7284; text-transform: uppercase; letter-spacing: 0.06em; }
p { margin: 0 0 10px; }
.client { font-size: 15px; font-weight: 600; margin: 0 0 2px; }
.period { color: #6b7284; margin: 0; }
.generated { color: #6b7284; font-size: 12px; margin: 2px 0 0; }
.muted, .note { color: #6b7284; font-size: 12px; }
.figures { display: grid; grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); gap: 14px 20px; margin: 0 0 18px; }
.figures > div { border-left: 3px solid #e2e5ea; padding-left: 10px; }
dt { font-size: 12px; color: #6b7284; margin-bottom: 2px; }
dd { margin: 0; font-size: 19px; font-weight: 600; font-variant-numeric: tabular-nums; }
dd .note { display: block; font-weight: 400; font-size: 11px; margin-top: 2px; }
table { border-collapse: collapse; width: 100%; margin: 0 0 18px; font-variant-numeric: tabular-nums; }
th, td { text-align: left; padding: 7px 10px; border-bottom: 1px solid #e2e5ea; }
th { font-size: 11px; text-transform: uppercase; letter-spacing: 0.06em; color: #6b7284; }
.num { text-align: right; }
.chart { margin: 0 0 18px; }
.chart svg { width: 100%; height: auto; display: block; }
.chart-title { font-size: 12px; color: #6b7284; margin-bottom: 6px; }
footer { margin-top: 36px; padding-top: 14px; border-top: 1px solid #e2e5ea; color: #6b7284; font-size: 12px; }
@media print {
  body { background: #fff; padding: 0; }
  .report { max-width: none; padding: 0; border-radius: 0; }
  h2, h3 { break-after: avoid-page; }
  .figures, .chart, tr { break-inside: avoid; }
  thead { display: table-header-group; }
}
`
