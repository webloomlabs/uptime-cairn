package render

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/report"
)

// withExpiries is the sample document plus a calendar, soonest first — the order
// the store returns and the order the section prints.
func withExpiries() report.Document {
	doc := sample()
	generated := doc.Meta.GeneratedAt
	doc.Expiries = []model.UpcomingExpiry{
		{
			Kind:          model.ExpiryDomain,
			MonitorID:     fixedMonitorID,
			MonitorName:   "checkout",
			Subject:       "acme.example",
			Issuer:        "Example Registrar",
			ExpiresAt:     generated.AddDate(0, 0, -11),
			DaysRemaining: -11,
			ObservedAt:    generated,
		},
		{
			Kind:          model.ExpiryCertificate,
			MonitorID:     fixedMonitorID,
			MonitorName:   "checkout",
			Subject:       "checkout.acme.example",
			Issuer:        "",
			ExpiresAt:     generated.AddDate(0, 0, 12),
			DaysRemaining: 12,
			ObservedAt:    generated,
		},
	}
	return doc
}

func expiryTable(t *testing.T, elements []Element) Table {
	t.Helper()

	var seen bool
	for _, e := range elements {
		if h, ok := e.(Heading); ok && h.Text == "Certificates and domains" {
			seen = true
			continue
		}
		if table, ok := e.(Table); ok && seen {
			return table
		}
	}
	t.Fatalf("no expiry table in %v", kinds(elements))
	return Table{}
}

// Selecting the section draws the calendar.
func TestSelectingTheExpirySectionDrawsTheCalendar(t *testing.T) {
	t.Parallel()

	elements := ComposeSections(withExpiries(), brandFixture(),
		[]string{model.SectionCertificateExpiry})

	if !has(elements, "heading:Certificates and domains") {
		t.Fatalf("no calendar block: %v", kinds(elements))
	}
	table := expiryTable(t, elements)
	if len(table.Rows) != 2 {
		t.Fatalf("%d rows, want 2", len(table.Rows))
	}
	// Only the calendar. A section selection is a narrowing, and drawing the
	// monitor blocks beside it would make the choice meaningless.
	if has(elements, "heading:checkout") {
		t.Errorf("the per-monitor block was drawn for a calendar-only selection: %v", kinds(elements))
	}
}

// Not selecting it draws nothing, even with a calendar on the document.
//
// The document carries expiries whenever the report type is `custom`, which is
// independent of whether the template named the section. Composition is where
// that choice is honoured, and a block that appeared because the data was there
// would make the selection advisory.
func TestAnUnselectedCalendarIsNotDrawn(t *testing.T) {
	t.Parallel()

	elements := ComposeSections(withExpiries(), brandFixture(),
		[]string{model.SectionSummary})

	if has(elements, "heading:Certificates and domains") {
		t.Errorf("the calendar was drawn without being selected: %v", kinds(elements))
	}
}

// **The order is the store's, and the expired entry leads.**
//
// Soonest first across both kinds, which puts what has already lapsed at the top
// — the row somebody opened the section to find. A table sorted by anything else
// would bury it.
func TestTheCalendarLeadsWithWhatHasAlreadyExpired(t *testing.T) {
	t.Parallel()

	table := expiryTable(t, ComposeSections(withExpiries(), brandFixture(),
		[]string{model.SectionCertificateExpiry}))

	if got := table.Rows[0][0]; got != "acme.example" {
		t.Errorf("first row = %q, want the lapsed domain", got)
	}
	if got := table.Rows[0][5]; got != "expired 11 days ago" {
		t.Errorf("days cell = %q — a bare \"-11\" in a numeric column reads as "+
			"eleven before it reads as minus eleven", got)
	}
	if got := table.Rows[1][5]; got != "12" {
		t.Errorf("second row days = %q, want a bare count", got)
	}
}

// A missing issuer is a dash rather than a blank cell, because an empty cell in
// a table reads as a rendering fault rather than as an absent fact. The
// certificate in the fixture has none; the registration does.
func TestAnAbsentIssuerIsMarkedRatherThanBlank(t *testing.T) {
	t.Parallel()

	table := expiryTable(t, ComposeSections(withExpiries(), brandFixture(),
		[]string{model.SectionCertificateExpiry}))

	if got := table.Rows[1][3]; got != "—" {
		t.Errorf("issuer cell = %q, want a dash", got)
	}
	if got := table.Rows[0][3]; got != "Example Registrar" {
		t.Errorf("registrar cell = %q, want the registrar in the issuer column", got)
	}
}

// The horizon and the count of lapsed entries are on the face of the section.
//
// §4.3's rule that a figure carries what produced it does not stop at the
// percentages: a calendar that looks short and a calendar that was asked a
// narrow question are indistinguishable otherwise.
func TestTheCalendarStatesItsHorizonAndItsLapsedCount(t *testing.T) {
	t.Parallel()

	var prose []string
	for _, e := range ComposeSections(withExpiries(), brandFixture(),
		[]string{model.SectionCertificateExpiry}) {
		if p, ok := e.(Paragraph); ok {
			prose = append(prose, p.Text)
		}
	}
	joined := strings.Join(prose, "\n")

	if !strings.Contains(joined, "1 of the 2 below has already expired") {
		t.Errorf("no lapsed count in the section prose:\n%s", joined)
	}
	if !strings.Contains(joined, "within 90 days") {
		t.Errorf("the horizon is not on the face of the report:\n%s", joined)
	}
}

// Dates are written in the window's own zone, the same rule the hourly charts
// follow. A certificate expiring at 09:00 UTC expires on the following day in
// Sydney, and the cover says Sydney.
func TestExpiryDatesAreWrittenInTheReportsZone(t *testing.T) {
	t.Parallel()

	doc := withExpiries()
	doc.Expiries[1].ExpiresAt = time.Date(2026, 4, 13, 20, 0, 0, 0, time.UTC)

	table := expiryTable(t, ComposeSections(doc, brandFixture(),
		[]string{model.SectionCertificateExpiry}))

	if got := table.Rows[1][4]; got != "14 Apr 2026" {
		t.Errorf("expiry date = %q, want 14 Apr 2026 — 20:00 UTC is the next day "+
			"in Australia/Sydney, which is the zone on the cover", got)
	}
}

// The JSON artifact carries the calendar, because the frozen schema types
// `ReportDocument.expiries` and a data document emits what the model holds.
func TestTheJSONArtifactCarriesTheCalendar(t *testing.T) {
	t.Parallel()

	data, err := JSON(withExpiries())
	if err != nil {
		t.Fatalf("json: %v", err)
	}

	var out struct {
		Expiries []struct {
			Kind          string  `json:"kind"`
			MonitorName   string  `json:"monitor_name"`
			Subject       *string `json:"subject"`
			Issuer        *string `json:"issuer"`
			DaysRemaining int     `json:"days_remaining"`
		} `json:"expiries"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(out.Expiries) != 2 {
		t.Fatalf("%d expiries, want 2", len(out.Expiries))
	}
	if out.Expiries[0].Kind != model.ExpiryDomain || out.Expiries[0].DaysRemaining != -11 {
		t.Errorf("first entry = %+v, want the lapsed domain with a negative count", out.Expiries[0])
	}
	// Nullable in the schema, so empty travels as null rather than as "". A
	// consumer distinguishing "not recorded" from "recorded as blank" gets the
	// answer the schema promised it.
	if out.Expiries[1].Issuer != nil {
		t.Errorf("issuer = %q, want null for a certificate with none", *out.Expiries[1].Issuer)
	}
	if out.Expiries[0].Issuer == nil || *out.Expiries[0].Issuer != "Example Registrar" {
		t.Errorf("registrar did not travel in the issuer field: %+v", out.Expiries[0])
	}
}

// An empty calendar is `[]` and never null, the rule every other collection in
// this document follows: a consumer iterating a list should not have to
// special-case the client with nothing expiring.
func TestAnEmptyCalendarIsAnArrayRatherThanNull(t *testing.T) {
	t.Parallel()

	data, err := JSON(sample())
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	if !strings.Contains(string(data), `"expiries": []`) {
		t.Errorf("expiries is not an empty array:\n%s", data)
	}
}

// Both rendered formats draw the calendar from the same composition, which is
// ADR-007's whole claim: a PDF is not a converted HTML page, it is the same
// elements drawn differently.
func TestBothRenderedFormatsCarryTheCalendar(t *testing.T) {
	t.Parallel()

	doc := withExpiries()
	sections := []string{model.SectionCertificateExpiry}

	html, err := HTMLSections(doc, brandFixture(), sections)
	if err != nil {
		t.Fatalf("html: %v", err)
	}
	if !strings.Contains(string(html), "checkout.acme.example") {
		t.Error("the HTML has no calendar row")
	}
	if !strings.Contains(string(html), "Certificates and domains") {
		t.Error("the HTML has no calendar heading")
	}

	if _, err := PDFSections(doc, brandFixture(), testFamily(), sections); err != nil {
		t.Fatalf("pdf: %v", err)
	}
}
