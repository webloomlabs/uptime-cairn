package report

import (
	"context"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

var expiryNow = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

func calendarFixture(t *testing.T) (*fakeStore, model.Monitor) {
	t.Helper()

	m := monitorNamed("api")
	return &fakeStore{
		monitors: []model.Monitor{m},
		totals:   map[model.ID]store.HistoryBucket{m.ID: {Up: 999, Down: 1}},
		expiries: []model.UpcomingExpiry{{
			Kind:          model.ExpiryCertificate,
			MonitorID:     m.ID,
			MonitorName:   "api",
			Subject:       "api.example.com",
			Issuer:        "Let's Encrypt",
			ExpiresAt:     expiryNow.AddDate(0, 0, 12),
			DaysRemaining: 12,
			ObservedAt:    expiryNow,
		}},
	}, m
}

func buildCalendar(t *testing.T, f *fakeStore, reportType string) Document {
	t.Helper()

	doc, err := Build(context.Background(), f, Spec{
		Type: reportType, Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "UTC",
	}, defaultRetention(), model.NewID(), expiryNow)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return doc
}

// A custom report carries the calendar, because `certificate_expiry` is a
// section only a custom template can select.
func TestCustomReportCarriesTheExpiryCalendar(t *testing.T) {
	t.Parallel()

	f, _ := calendarFixture(t)
	doc := buildCalendar(t, f, model.ReportTypeCustom)

	if len(doc.Expiries) != 1 || doc.Expiries[0].Subject != "api.example.com" {
		t.Fatalf("expiries = %+v, want the one certificate", doc.Expiries)
	}
}

// **Every other type pays nothing for it.**
//
// This is the same gate the incident log runs under and it is a cost decision
// rather than a stylistic one: the four-reads-whatever-the-scope property is
// what the extended load gate measures, and no type but `custom` can name the
// section that would draw the block. A read for a block that cannot be rendered
// would be fifty concurrent runs each paying for nothing on the first of the
// month.
func TestOnlyACustomReportReadsTheCalendar(t *testing.T) {
	t.Parallel()

	for _, reportType := range []string{
		model.ReportTypeUptime,
		model.ReportTypeSLA,
		model.ReportTypePostMortem,
		model.ReportTypeComparative,
	} {
		t.Run(reportType, func(t *testing.T) {
			t.Parallel()

			f, _ := calendarFixture(t)
			doc := buildCalendar(t, f, reportType)

			if got := f.calls["ListUpcomingExpiries"]; got != 0 {
				t.Errorf("calendar read %d times for a %s report, want 0", got, reportType)
			}
			if doc.Expiries != nil {
				t.Errorf("expiries = %+v on a %s report, want none", doc.Expiries, reportType)
			}
		})
	}
}

// The calendar is asked only about the monitors in scope.
//
// The failure this guards is a disclosure rather than a gap: an unnarrowed query
// would put every certificate on the install into one client's document, and an
// agency running reports for two clients off one install is the case the whole
// scope mechanism exists for.
func TestTheCalendarIsNarrowedToTheReportScope(t *testing.T) {
	t.Parallel()

	f, m := calendarFixture(t)
	other := monitorNamed("also-mine")
	f.monitors = append(f.monitors, other)
	f.totals[other.ID] = store.HistoryBucket{Up: 500}

	buildCalendar(t, f, model.ReportTypeCustom)

	got := f.expiryFilter.MonitorIDs
	if len(got) != 2 || got[0] != m.ID || got[1] != other.ID {
		t.Errorf("filter monitors = %v, want exactly the two in scope", got)
	}
	if f.expiryFilter.WithinDays == nil || *f.expiryFilter.WithinDays != ExpiryHorizonDays {
		t.Errorf("horizon = %v, want %d days — an unbounded calendar heads the "+
			"table with a registration expiring in the next decade",
			f.expiryFilter.WithinDays, ExpiryHorizonDays)
	}
}

// `days_remaining` counts from the run's own instant, not the wall clock.
//
// ADR-007 requires the same model rendered twice to be byte-identical. A figure
// counted from time.Now would break that inside a single run: the PDF and the
// JSON of one report, rendered either side of midnight, would disagree by a day
// about how long a certificate has left.
func TestTheCalendarCountsFromTheRunsOwnInstant(t *testing.T) {
	t.Parallel()

	f, _ := calendarFixture(t)
	buildCalendar(t, f, model.ReportTypeCustom)

	if !f.expiryNow.Equal(expiryNow) {
		t.Errorf("counted from %s, want the run's generated_at %s", f.expiryNow, expiryNow)
	}
}

// A scope that resolved to nothing does not ask.
//
// Build returns early on an empty scope, and it matters here beyond the saved
// read: ExpiryFilter treats an empty MonitorIDs as "every monitor", so a query
// issued with the empty set would answer with the whole install's calendar on
// exactly the document that should be emptiest.
func TestAnEmptyScopeDoesNotReadTheCalendar(t *testing.T) {
	t.Parallel()

	f := &fakeStore{}
	doc := buildCalendar(t, f, model.ReportTypeCustom)

	if got := f.calls["ListUpcomingExpiries"]; got != 0 {
		t.Errorf("calendar read %d times for an empty scope, want 0", got)
	}
	if len(doc.Expiries) != 0 {
		t.Errorf("expiries = %+v on an empty scope, want none", doc.Expiries)
	}
}
