package sqlite

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

func testBrand(name string, isDefault bool) model.BrandProfile {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	return model.BrandProfile{
		ID:            model.NewID(),
		OrgID:         model.SentinelOrgID,
		Name:          name,
		CompanyName:   "Smith & Co",
		PrimaryColor:  "#1a8f5a",
		AccentColor:   "#3b5bdb",
		FooterText:    "Confidential.",
		CoverText:     "Monthly availability summary.",
		HidePoweredBy: false,
		IsDefault:     isDefault,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

func testTemplate(name string) model.ReportTemplate {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	target := 99.9
	rt := 500
	return model.ReportTemplate{
		ID:                   model.NewID(),
		OrgID:                model.SentinelOrgID,
		Name:                 name,
		Description:          "Monthly SLA for the retainer.",
		Type:                 model.ReportTypeSLA,
		Period:               model.ReportPeriodMonth,
		PeriodStyle:          model.ReportStyleCalendar,
		SLATarget:            &target,
		ResponseTimeTargetMS: &rt,
		MaintenanceHandling:  "exclude",
		Formats:              []string{model.FormatPDF, model.FormatJSON},
		CreatedAt:            now,
		UpdatedAt:            now,
	}
}

// A colour round-trips exactly. It is stored as written rather than normalised,
// because a client's brand colour is a string somebody pasted from a brand guide
// and handing it back in a different case is the kind of small wrongness that
// makes a white-label feature feel like somebody else's.
func TestBrandProfileRoundTrips(t *testing.T) {
	t.Parallel()

	s := open(t)
	p := testBrand("Acme", true)
	p.PrimaryColor = "#1A8F5A"

	if err := s.CreateBrandProfile(t.Context(), p); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetBrandProfile(t.Context(), p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.PrimaryColor != "#1A8F5A" {
		t.Errorf("primary colour = %q, want it unchanged", got.PrimaryColor)
	}
	if got.Name != p.Name || got.CompanyName != p.CompanyName || got.CoverText != p.CoverText {
		t.Errorf("profile did not round-trip: %+v", got)
	}
	if !got.IsDefault {
		t.Error("the default flag was lost")
	}
}

// Making a profile the default demotes whichever one held it. The unique partial
// index would refuse the second insert instead, and "there is already a default"
// is not something an operator can act on when making this one the default is
// exactly what they asked for.
func TestSettingADefaultDemotesThePreviousOne(t *testing.T) {
	t.Parallel()

	s := open(t)
	first, second := testBrand("First", true), testBrand("Second", true)

	if err := s.CreateBrandProfile(t.Context(), first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if err := s.CreateBrandProfile(t.Context(), second); err != nil {
		t.Fatalf("create second: %v", err)
	}

	got, err := s.DefaultBrandProfile(t.Context())
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if got.ID != second.ID {
		t.Errorf("default is %q, want the one just marked default", got.Name)
	}

	demoted, err := s.GetBrandProfile(t.Context(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if demoted.IsDefault {
		t.Error("the previous default is still marked default; the unique index will refuse the next write")
	}
}

// Re-saving the default profile must not demote itself, which is the obvious way
// to write the demotion query wrong and the one a test catches immediately.
func TestSavingTheDefaultKeepsItDefault(t *testing.T) {
	t.Parallel()

	s := open(t)
	p := testBrand("Only", true)
	if err := s.CreateBrandProfile(t.Context(), p); err != nil {
		t.Fatal(err)
	}

	p.Name = "Only, renamed"
	p.UpdatedAt = p.UpdatedAt.Add(time.Minute)
	if err := s.UpdateBrandProfile(t.Context(), p); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := s.GetBrandProfile(t.Context(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsDefault {
		t.Error("saving the default profile demoted it")
	}
}

// An install with no default gets ErrNotFound rather than a zero profile. The
// two are different states with different answers: fall back to
// settings.appearance, or render as saved.
func TestNoDefaultProfileIsNotFoundRatherThanEmpty(t *testing.T) {
	t.Parallel()

	s := open(t)
	if _, err := s.DefaultBrandProfile(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// The logo is read by name rather than carried on every profile read. A list of
// twelve clients would otherwise move twelve megabytes to render a screen that
// shows twelve names.
func TestTheLogoIsNotCarriedOnTheProfileRead(t *testing.T) {
	t.Parallel()

	s := open(t)
	p := testBrand("Acme", false)
	if err := s.CreateBrandProfile(t.Context(), p); err != nil {
		t.Fatal(err)
	}

	png := []byte("\x89PNG\r\n\x1a\n pretend this is a logo")
	p.LogoUpdatedAt = &p.UpdatedAt
	if err := s.SetBrandLogo(t.Context(), p.ID, png, model.LogoPNG, p); err != nil {
		t.Fatalf("set logo: %v", err)
	}

	got, err := s.GetBrandProfile(t.Context(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Logo != nil {
		t.Error("the profile read carried the logo bytes")
	}
	if got.LogoContentType != model.LogoPNG {
		t.Errorf("content type = %q, want %q", got.LogoContentType, model.LogoPNG)
	}
	if got.LogoBytes != int64(len(png)) {
		t.Errorf("logo bytes = %d, want %d", got.LogoBytes, len(png))
	}

	bytes, contentType, err := s.BrandLogo(t.Context(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(bytes) != string(png) || contentType != model.LogoPNG {
		t.Error("the logo did not round-trip through BrandLogo")
	}
}

// Deleting a profile does not block on the templates using it, and does not take
// them with it: they fall back to the default, which is the behaviour a template
// with no profile already has.
func TestDeletingAProfileLeavesItsTemplatesAlone(t *testing.T) {
	t.Parallel()

	s := open(t)
	p := testBrand("Acme", false)
	if err := s.CreateBrandProfile(t.Context(), p); err != nil {
		t.Fatal(err)
	}

	tpl := testTemplate("Monthly")
	tpl.BrandProfileID = &p.ID
	if err := s.CreateReportTemplate(t.Context(), tpl); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteBrandProfile(t.Context(), p.ID); err != nil {
		t.Fatalf("delete profile: %v", err)
	}

	got, err := s.GetReportTemplate(t.Context(), tpl.ID)
	if err != nil {
		t.Fatalf("the template went with the profile: %v", err)
	}
	if got.BrandProfileID != nil {
		t.Error("the template still references a profile that no longer exists")
	}
}

// The scope is stored as a rule and comes back as one. This is the property the
// whole table is shaped around: an agency that adds a monitor to a client's tag
// expects it in that client's next report without editing the report.
func TestTemplateScopeRoundTripsAsARule(t *testing.T) {
	t.Parallel()

	s := open(t)
	tpl := testTemplate("Retainer")
	incident := model.NewID()
	tpl.Scope = model.ReportScope{
		MonitorIDs: []model.ID{model.NewID(), model.NewID()},
		GroupIDs:   []model.ID{model.NewID()},
		TagIDs:     []model.ID{model.NewID()},
		IncidentID: &incident,
	}

	if err := s.CreateReportTemplate(t.Context(), tpl); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetReportTemplate(t.Context(), tpl.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if len(got.Scope.MonitorIDs) != 2 || len(got.Scope.GroupIDs) != 1 || len(got.Scope.TagIDs) != 1 {
		t.Fatalf("scope = %+v", got.Scope)
	}
	if got.Scope.MonitorIDs[0] != tpl.Scope.MonitorIDs[0] {
		t.Error("monitor ids came back in a different order or a different value")
	}
	if got.Scope.IncidentID == nil || *got.Scope.IncidentID != incident {
		t.Error("the incident id did not round-trip")
	}
}

// The scope column is readable by a human with a SQLite shell, because that is
// who reads it: somebody in a support conversation about why a monitor is or is
// not in a client's report. Base64 of sixteen bytes would not answer that.
func TestScopeIsStoredAsReadableUUIDs(t *testing.T) {
	t.Parallel()

	s := open(t)
	tpl := testTemplate("Retainer")
	monitor := model.NewID()
	tpl.Scope.MonitorIDs = []model.ID{monitor}
	if err := s.CreateReportTemplate(t.Context(), tpl); err != nil {
		t.Fatal(err)
	}

	var scope string
	if err := s.ro.QueryRowContext(t.Context(),
		`SELECT scope FROM report_templates WHERE id = ?`, tpl.ID[:]).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	if want := monitor.String(); !strings.Contains(scope, want) {
		t.Errorf("stored scope %s does not contain the readable id %s", scope, want)
	}
}

// A template with nothing optional set still round-trips. Nulls are the common
// case for a solo user who accepted the defaults, and a read path that assumes
// its own writes is a read path that fails on the first row somebody else made.
func TestATemplateWithEverythingOptionalUnsetRoundTrips(t *testing.T) {
	t.Parallel()

	s := open(t)
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	tpl := model.ReportTemplate{
		ID:                  model.NewID(),
		OrgID:               model.SentinelOrgID,
		Name:                "Bare",
		Type:                model.ReportTypeUptime,
		Period:              model.ReportPeriodMonth,
		PeriodStyle:         model.ReportStyleCalendar,
		MaintenanceHandling: "exclude",
		CreatedAt:           now,
		UpdatedAt:           now,
	}

	if err := s.CreateReportTemplate(t.Context(), tpl); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetReportTemplate(t.Context(), tpl.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.SLATarget != nil || got.ResponseTimeTargetMS != nil || got.BrandProfileID != nil {
		t.Errorf("an unset optional came back set: %+v", got)
	}
	if got.Comparison != nil {
		t.Error("comparison came back non-nil for a template that has none")
	}
	if !got.Scope.IsEmpty() {
		t.Errorf("scope = %+v, want empty", got.Scope)
	}
	if len(got.Formats) != 0 {
		t.Errorf("formats = %v, want none", got.Formats)
	}
}

// An update rewrites the whole definition, including clearing what was set. A
// path that can set a target and not unset it leaves an operator unable to undo
// their own change.
func TestUpdateClearsWhatWasSet(t *testing.T) {
	t.Parallel()

	s := open(t)
	tpl := testTemplate("Monthly")
	if err := s.CreateReportTemplate(t.Context(), tpl); err != nil {
		t.Fatal(err)
	}

	tpl.SLATarget = nil
	tpl.ResponseTimeTargetMS = nil
	tpl.Formats = []string{model.FormatHTML}
	tpl.UpdatedAt = tpl.UpdatedAt.Add(time.Minute)
	if err := s.UpdateReportTemplate(t.Context(), tpl); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := s.GetReportTemplate(t.Context(), tpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SLATarget != nil || got.ResponseTimeTargetMS != nil {
		t.Errorf("a cleared field survived the update: %+v", got)
	}
	if len(got.Formats) != 1 || got.Formats[0] != model.FormatHTML {
		t.Errorf("formats = %v, want [html]", got.Formats)
	}
}

// Listing is keyset-paginated on the same (updated_at, id) key every other
// collection uses, so the cursor semantics do not vary by resource.
func TestListingTemplatesPagesOnTheCursorKey(t *testing.T) {
	t.Parallel()

	s := open(t)
	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	for i := range 5 {
		tpl := testTemplate("Template")
		tpl.UpdatedAt = base.Add(time.Duration(i) * time.Minute)
		if err := s.CreateReportTemplate(t.Context(), tpl); err != nil {
			t.Fatal(err)
		}
	}

	first, more, err := s.ListReportTemplates(t.Context(), nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || !more {
		t.Fatalf("first page = %d rows, more = %v", len(first), more)
	}
	if !first[0].UpdatedAt.After(first[1].UpdatedAt) {
		t.Error("the page is not newest-first")
	}

	cursor := &Cursor{UpdatedAt: first[1].UpdatedAt, ID: first[1].ID}
	second, more, err := s.ListReportTemplates(t.Context(), cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || !more {
		t.Fatalf("second page = %d rows, more = %v", len(second), more)
	}
	for _, a := range first {
		for _, b := range second {
			if a.ID == b.ID {
				t.Errorf("template %s appears on both pages", a.ID)
			}
		}
	}
}

// A missing row is ErrNotFound rather than a zero value, on every path.
func TestMissingRowsAreNotFound(t *testing.T) {
	t.Parallel()

	s := open(t)
	missing := model.NewID()

	if _, err := s.GetReportTemplate(t.Context(), missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("get template: %v, want ErrNotFound", err)
	}
	if err := s.DeleteReportTemplate(t.Context(), missing, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete template: %v, want ErrNotFound", err)
	}
	if _, err := s.GetBrandProfile(t.Context(), missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("get profile: %v, want ErrNotFound", err)
	}
	if err := s.DeleteBrandProfile(t.Context(), missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete profile: %v, want ErrNotFound", err)
	}
	if _, _, err := s.BrandLogo(t.Context(), missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("brand logo: %v, want ErrNotFound", err)
	}
}

// **Deleting a template keeps the runs it produced.** That is the maintainer's
// ruling of 2026-08-31 and the reason the delete is soft: a run is the record of
// what a client was sent, the frozen spec promises it is retained, and the
// earlier cascade deleted it along with every artifact row.
func TestDeletingATemplateKeepsItsRuns(t *testing.T) {
	t.Parallel()

	s := open(t)
	tpl := testTemplate("Monthly")
	if err := s.CreateReportTemplate(t.Context(), tpl); err != nil {
		t.Fatal(err)
	}
	run := seedRun(t, s, tpl.ID, nil)

	if err := s.DeleteReportTemplate(t.Context(), tpl.ID, deletedAt); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var (
		count      int
		templateID []byte
	)
	if err := s.ro.QueryRowContext(t.Context(),
		`SELECT count(*) FROM report_runs WHERE id = ?`, run[:]).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("the run went with its template")
	}

	// And the link still resolves, which is the half a nullable foreign key
	// would have lost: a run that survives but cannot say what it was a report
	// of is a record that cannot answer the question it exists to answer.
	if err := s.ro.QueryRowContext(t.Context(),
		`SELECT report_template_id FROM report_runs WHERE id = ?`, run[:]).Scan(&templateID); err != nil {
		t.Fatal(err)
	}
	var got model.ID
	copy(got[:], templateID)
	if got != tpl.ID {
		t.Error("the run no longer names its template")
	}

	definition, err := s.ReportTemplateForRun(t.Context(), tpl.ID)
	if err != nil {
		t.Fatalf("the deleted definition cannot be read back for the run: %v", err)
	}
	if definition.Name != tpl.Name {
		t.Errorf("definition name = %q, want %q", definition.Name, tpl.Name)
	}
}

// A deleted template is gone from every path a request travels: not gettable,
// not listable, not updatable. Forgetting one of those filters is how a deleted
// template reappears in a list, which is the standing cost of soft delete and
// the reason it is tested as a set rather than one call at a time.
func TestADeletedTemplateIsGoneFromEveryLiveReadPath(t *testing.T) {
	t.Parallel()

	s := open(t)
	tpl := testTemplate("Monthly")
	if err := s.CreateReportTemplate(t.Context(), tpl); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteReportTemplate(t.Context(), tpl.ID, deletedAt); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetReportTemplate(t.Context(), tpl.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get: %v, want ErrNotFound", err)
	}
	if err := s.UpdateReportTemplate(t.Context(), tpl); !errors.Is(err, ErrNotFound) {
		t.Errorf("update: %v, want ErrNotFound", err)
	}
	if err := s.DeleteReportTemplate(t.Context(), tpl.ID, deletedAt); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v, want ErrNotFound rather than a silent success", err)
	}

	list, _, err := s.ListReportTemplates(t.Context(), nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range list {
		if got.ID == tpl.ID {
			t.Error("a deleted template is still listed")
		}
	}
}

// A schedule whose template was deleted must stop firing. A deleted report that
// keeps arriving in a client's inbox is worse than either deleting it or not,
// and the scheduler's due query is the only thing standing between the two.
func TestDeletingATemplateStopsItsSchedules(t *testing.T) {
	t.Parallel()

	s := open(t)
	tpl := testTemplate("Monthly")
	if err := s.CreateReportTemplate(t.Context(), tpl); err != nil {
		t.Fatal(err)
	}
	schedule := seedSchedule(t, s, tpl.ID)

	if err := s.DeleteReportTemplate(t.Context(), tpl.ID, deletedAt); err != nil {
		t.Fatal(err)
	}

	var due int
	if err := s.ro.QueryRowContext(t.Context(), `
		SELECT count(*) FROM report_schedules
		WHERE enabled = 1 AND next_run_at IS NOT NULL AND deleted_at IS NULL`).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if due != 0 {
		t.Errorf("%d schedule(s) are still due for a deleted template", due)
	}

	var deleted sql.NullInt64
	if err := s.ro.QueryRowContext(t.Context(),
		`SELECT deleted_at FROM report_schedules WHERE id = ?`, schedule[:]).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if !deleted.Valid {
		t.Error("the schedule was left live under a deleted template")
	}
}

// The database enforces the rule rather than the handlers. A hard DELETE of a
// template with runs — from a SQL shell, or from a purge path somebody writes
// later — is refused, so "runs outlive their definition" cannot be undone by
// code that did not know about it.
func TestAHardDeleteOfATemplateWithRunsIsRefused(t *testing.T) {
	t.Parallel()

	s := open(t)
	tpl := testTemplate("Monthly")
	if err := s.CreateReportTemplate(t.Context(), tpl); err != nil {
		t.Fatal(err)
	}
	seedRun(t, s, tpl.ID, nil)

	_, err := s.db.ExecContext(t.Context(), `DELETE FROM report_templates WHERE id = ?`, tpl.ID[:])
	if err == nil {
		t.Fatal("a hard delete of a template with runs succeeded; RESTRICT is not in force")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Errorf("refused with %v, want a foreign key constraint failure", err)
	}
}

var deletedAt = time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)

// seedRun writes a run directly. These tests are about what happens to a run
// when its definition is deleted, not about how a run is produced — which does
// not exist yet and would make a failure here ambiguous between the two.
func seedRun(t *testing.T, s *Store, templateID model.ID, scheduleID *model.ID) model.ID {
	t.Helper()

	id := model.NewID()
	now := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	if _, err := s.db.ExecContext(t.Context(), `
		INSERT INTO report_runs (id, org_id, report_template_id, report_schedule_id, state,
		    period_start, period_end, timezone, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		id[:], model.SentinelOrgID[:], templateID[:], nullID(scheduleID), model.RunSucceeded,
		millis(now.AddDate(0, -1, 0)), millis(now), "UTC", millis(now)); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return id
}

func seedSchedule(t *testing.T, s *Store, templateID model.ID) model.ID {
	t.Helper()

	id := model.NewID()
	now := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	if _, err := s.db.ExecContext(t.Context(), `
		INSERT INTO report_schedules (id, org_id, report_template_id, enabled, frequency,
		    timezone, send_at, next_run_at, created_at, updated_at)
		VALUES (?,?,?,1,?,?,?,?,?,?)`,
		id[:], model.SentinelOrgID[:], templateID[:], model.ReportFrequencyMonthly,
		"UTC", "09:00", millis(now.AddDate(0, 1, 0)), millis(now), millis(now)); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	return id
}
