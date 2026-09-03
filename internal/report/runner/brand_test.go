package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/report"
)

// The branding a run was produced under is copied onto the document, not
// referenced from it.
//
// This is the difference between an artifact and a view. An agency rebrands in
// June; every January report they ever sent is still on disk, and every one of
// them has to keep saying what it said when it was sent. A reference would make
// the stored JSON's answer to "who was this prepared for?" a function of the
// current state of a mutable row — so a client disputing a figure in March would
// be shown a document that had quietly changed since they received it.
//
// The rendered HTML and PDF are already standalone, because the words and the
// logo are inside the file. The JSON is the one that needed this: it is a data
// document, and without `meta.brand` it could be re-rendered into an unbranded
// page that claims to be the report a client was sent.
func TestTheDocumentCarriesTheBrandItWasRenderedUnder(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON)
	run.ReportTemplateID = s.template.ID
	s.brand = model.BrandProfile{
		ID:            model.NewID(),
		CompanyName:   "Smith & Co",
		PrimaryColor:  "#0B5FFF",
		FooterText:    "Prepared by Acme Ops",
		HidePoweredBy: true,
	}

	files := newFiles()
	if err := New(s, files, Options{Retention: retention()}).Execute(context.Background(), run, now); err != nil {
		t.Fatalf("execute: %v", err)
	}

	data, ok := files.bytes(model.FormatJSON)
	if !ok {
		t.Fatal("no JSON artifact")
	}

	var doc struct {
		Meta struct {
			Brand *struct {
				CompanyName   *string `json:"company_name"`
				PrimaryColor  *string `json:"primary_color"`
				FooterText    *string `json:"footer_text"`
				LogoURL       *string `json:"logo_url"`
				HidePoweredBy bool    `json:"hide_powered_by"`
			} `json:"brand"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	b := doc.Meta.Brand
	if b == nil {
		t.Fatal("meta.brand is null on a branded run")
	}
	if b.CompanyName == nil || *b.CompanyName != "Smith & Co" {
		t.Errorf("company_name = %v, want Smith & Co", b.CompanyName)
	}
	// **Exactly as written.** A colour is a string somebody pasted from a brand
	// guide; handing it back lowercased is what makes a white-label feature feel
	// like somebody else's product.
	if b.PrimaryColor == nil || *b.PrimaryColor != "#0B5FFF" {
		t.Errorf("primary_color = %v, want #0B5FFF unchanged", b.PrimaryColor)
	}
	if b.FooterText == nil || *b.FooterText != "Prepared by Acme Ops" {
		t.Errorf("footer_text = %v", b.FooterText)
	}
	if !b.HidePoweredBy {
		t.Error("hide_powered_by was not carried")
	}
	// The recorded spec gap, held so it does not get invented around: there is
	// no operation that serves a brand logo's bytes, so a URL here would name an
	// endpoint answering 405.
	if b.LogoURL != nil {
		t.Errorf("logo_url = %v, want null — no operation in the spec serves those bytes", *b.LogoURL)
	}
}

// An install that has never opened the brand-profile screen still produces a
// report that looks like somebody configured the product.
//
// This is the solo user's path and it is the common one: no profile exists at
// all, so the lookup fails, and the fallback is the appearance settings the
// operator already chose for their dashboard. Without it the first report a new
// install produces is anonymous — which reads as a broken feature rather than as
// an unconfigured one.
//
// Two fields and no more. There is no logo, no footer and no client name in that
// settings section, and inventing a footer here would put words on a client's
// document that nobody wrote.
func TestAnInstallWithNoBrandProfileIsBrandedFromItsAppearance(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON, model.FormatHTML)
	run.ReportTemplateID = s.template.ID
	s.brandErr = errors.New("no rows in result set")

	colour := "#7A29B8"
	s.settings = model.Settings{
		General:    model.GeneralSettings{InstanceName: "Acme Ops"},
		Appearance: model.AppearanceSettings{PrimaryColor: &colour},
	}

	files := newFiles()
	if err := New(s, files, Options{Retention: retention()}).Execute(context.Background(), run, now); err != nil {
		t.Fatalf("execute: %v", err)
	}

	data, _ := files.bytes(model.FormatJSON)
	var doc struct {
		Meta struct {
			Brand *struct {
				CompanyName  *string `json:"company_name"`
				PrimaryColor *string `json:"primary_color"`
				FooterText   *string `json:"footer_text"`
			} `json:"brand"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Meta.Brand == nil {
		t.Fatal("meta.brand is null; an instance with a name and a colour is not unbranded")
	}
	if got := doc.Meta.Brand.CompanyName; got == nil || *got != "Acme Ops" {
		t.Errorf("company_name = %v, want the instance name", got)
	}
	if got := doc.Meta.Brand.PrimaryColor; got == nil || *got != colour {
		t.Errorf("primary_color = %v, want %s", got, colour)
	}
	if got := doc.Meta.Brand.FooterText; got != nil {
		t.Errorf("footer_text = %q — nothing in the appearance settings says this, "+
			"and a report is not the place to invent words for a client to read", *got)
	}

	// And the name reaches the page a client actually opens, not only the data
	// document.
	html, ok := files.bytes(model.FormatHTML)
	if !ok {
		t.Fatal("no HTML artifact")
	}
	if !strings.Contains(string(html), "Acme Ops") {
		t.Error("the instance name is on the document but not on the rendered page")
	}
}

// An instance with nothing configured at all has no branding, and says so with a
// null rather than with an object full of empty strings.
//
// The distinction is for the consumer: "there is no branding" and "there is
// branding that says nothing" are different answers, and a BI tool that has to
// tell them apart can only do so if the field is nullable and is actually null.
func TestNoBrandingAtAllIsNullRatherThanEmpty(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON)
	run.ReportTemplateID = s.template.ID
	s.brandErr = errors.New("no rows in result set")
	s.settings = model.Settings{}

	files := newFiles()
	if err := New(s, files, Options{Retention: retention()}).Execute(context.Background(), run, now); err != nil {
		t.Fatalf("execute: %v", err)
	}

	data, _ := files.bytes(model.FormatJSON)
	var doc struct {
		Meta struct {
			Brand json.RawMessage `json:"brand"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The field is present and its value is the JSON null — not absent, which
	// would be a third answer again, and not an object.
	if got := strings.TrimSpace(string(doc.Meta.Brand)); got != "null" {
		t.Errorf("meta.brand = %s, want null on an instance with no branding at all", got)
	}
}

// Artifact retention is read from the settings row rather than from a value
// captured at start-up.
//
// The drift this closes was real: `report_artifact_days` was validated on the
// settings surface and the runner used a compiled-in constant, so an operator
// could set it to thirty days, watch the API accept it, and find a year later
// that nothing had ever expired. The rollup runner already reads its retention
// on every pass for this reason and this follows it.
func TestArtifactExpiryComesFromTheSettingsRow(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON)
	run.ReportTemplateID = s.template.ID
	days := 30
	s.settings = model.Settings{Retention: model.RetentionSettings{ReportArtifactDays: &days}}

	// A start-up default that disagrees, so a pass cannot be the default leaking
	// through.
	opts := Options{Retention: retention(), ArtifactDays: 365}
	if err := New(s, newFiles(), opts).Execute(context.Background(), run, now); err != nil {
		t.Fatalf("execute: %v", err)
	}

	_, _, artifacts := s.outcome()
	if len(artifacts) != 1 {
		t.Fatalf("%d artifacts, want 1", len(artifacts))
	}
	if artifacts[0].ExpiresAt == nil {
		t.Fatal("no expiry on an artifact under a 30-day policy")
	}
	want := now.AddDate(0, 0, 30)
	if !artifacts[0].ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %s, want %s — the settings row, not the start-up default",
			artifacts[0].ExpiresAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// Zero means indefinitely, the same convention every other field in that
// settings section uses. An artifact is expected to outlive the data it was
// computed from (ADR-008 item 6), so "keep forever" has to be expressible and
// has to be the value an operator can actually reach.
func TestZeroArtifactDaysKeepsTheBytesIndefinitely(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON)
	run.ReportTemplateID = s.template.ID
	zero := 0
	s.settings = model.Settings{Retention: model.RetentionSettings{ReportArtifactDays: &zero}}

	if err := New(s, newFiles(), Options{Retention: retention(), ArtifactDays: 365}).
		Execute(context.Background(), run, now); err != nil {
		t.Fatalf("execute: %v", err)
	}

	_, _, artifacts := s.outcome()
	if len(artifacts) != 1 || artifacts[0].ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil — zero means keep indefinitely", artifacts[0].ExpiresAt)
	}
}

// The tiers a report reads at come from the settings row too, so an operator who
// shortened retention this morning gets a document labelled with the resolution
// that policy actually permits rather than with one a restart ago allowed.
func TestResolutionFollowsStoredRetention(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON)
	run.ReportTemplateID = s.template.ID

	// A modest install keeping a week of everything below the daily tier. A
	// March window read on 1 April cannot be answered at 1m, 5m or 1h, so the
	// only tier left is the one kept indefinitely.
	short := 7
	s.settings = model.Settings{Retention: model.RetentionSettings{
		Rollup1mDays: &short, Rollup5mDays: &short, Rollup1hDays: &short,
	}}

	files := newFiles()
	if err := New(s, files, Options{Retention: retention()}).Execute(context.Background(), run, now); err != nil {
		t.Fatalf("execute: %v", err)
	}

	data, _ := files.bytes(model.FormatJSON)
	var doc struct {
		Meta struct {
			Resolution struct {
				Tier string `json:"tier"`
			} `json:"resolution"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Meta.Resolution.Tier != "1d" {
		t.Errorf("tier = %q, want 1d — stored retention no longer holds hourly data for March",
			doc.Meta.Resolution.Tier)
	}
}

// retention is the compiled-in default the tests fall back to.
func retention() report.Retention {
	return report.Retention{RawDays: 7, Rollup1mDays: 30, Rollup5mDays: 90, Rollup1hDays: 365, Rollup1dDays: 0}
}

var _ = report.Brand{}
