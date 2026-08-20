package check

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// fakeRegistry serves the IANA bootstrap file and one registry's RDAP endpoint
// from the same server, which is enough to exercise the whole RDAP path without
// asking a real registry anything.
func fakeRegistry(t *testing.T, expiry string, requests *int) *DomainExpiry {
	t.Helper()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/bootstrap":
			_, _ = w.Write([]byte(`{"services":[[["test","example"],["` + server.URL + `/rdap/"]]]}`))
		case strings.HasPrefix(r.URL.Path, "/rdap/domain/"):
			if requests != nil {
				*requests++
			}
			if strings.Contains(r.URL.Path, "gone.test") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"events":[
				{"eventAction":"registration","eventDate":"2010-01-01T00:00:00Z"},
				{"eventAction":"expiration","eventDate":"` + expiry + `"}
			],"entities":[
				{"roles":["registrar"],"vcardArray":["vcard",[["version",{},"text","4.0"],["fn",{},"text","Example Registrar, Inc."]]]}
			]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	checker := NewDomainExpiry()
	checker.bootstrapURL = server.URL + "/bootstrap"
	return checker
}

func TestDomainExpiryRDAP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		expiry time.Time
		config string
		want   model.Status
		class  ErrorClass
	}{
		{
			name:   "well clear of the threshold",
			expiry: time.Now().Add(400 * 24 * time.Hour),
			config: `{"domain":"good.test"}`,
			want:   model.StatusUp,
		},
		{
			name:   "inside the default 30-day threshold",
			expiry: time.Now().Add(10 * 24 * time.Hour),
			config: `{"domain":"soon.test"}`,
			want:   model.StatusDown,
			class:  ClassAssertion,
		},
		{
			name:   "threshold can be lowered to zero",
			expiry: time.Now().Add(10 * 24 * time.Hour),
			config: `{"domain":"soon.test","days_remaining_threshold":0}`,
			want:   model.StatusUp,
		},
		{
			name:   "already lapsed",
			expiry: time.Now().Add(-24 * time.Hour),
			config: `{"domain":"lapsed.test"}`,
			want:   model.StatusDown,
			class:  ClassAssertion,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			checker := fakeRegistry(t, tc.expiry.UTC().Format(time.RFC3339), nil)
			if err := checker.Validate([]byte(tc.config)); err != nil {
				t.Fatalf("validate: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			obs := checker.Check(ctx, []byte(tc.config))
			if obs.Status != tc.want {
				t.Errorf("status = %s, want %s (%s)", obs.Status, tc.want, obs.Message)
			}
			if tc.class != "" && obs.Class != tc.class {
				t.Errorf("class = %q, want %q", obs.Class, tc.class)
			}
		})
	}
}

// A registry answering 404 means the name is not registered. That is a verdict
// about the target and the exact outage this monitor exists to catch, so it must
// not be folded into "cannot read the registration".
func TestDomainExpiryUnregisteredIsDown(t *testing.T) {
	t.Parallel()

	checker := fakeRegistry(t, time.Now().Format(time.RFC3339), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	obs := checker.Check(ctx, []byte(`{"domain":"gone.test","source":"rdap"}`))
	if obs.Status != model.StatusDown {
		t.Errorf("status = %s, want down (%s)", obs.Status, obs.Message)
	}
	if obs.Code != "not_registered" {
		t.Errorf("code = %q, want not_registered", obs.Code)
	}
}

// A registry we cannot reach is unknown, never down: a rate-limited endpoint
// must not open an incident against a domain with three years left on it.
func TestDomainExpiryUnreachableRegistryIsUnknown(t *testing.T) {
	t.Parallel()

	checker := NewDomainExpiry()
	checker.bootstrapURL = "http://127.0.0.1:1/bootstrap"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	obs := checker.Check(ctx, []byte(`{"domain":"unreachable.test","source":"rdap"}`))
	if obs.Status != model.StatusUnknown {
		t.Errorf("status = %s, want unknown (%s)", obs.Status, obs.Message)
	}
}

// The spec's once-a-day floor. Registries rate-limit and mean it, so a monitor
// on a 60-second interval must still produce exactly one lookup a day — while
// its heartbeats keep counting down, which is why the cache holds the date and
// not the verdict.
func TestDomainExpiryHonoursTheDailyFloor(t *testing.T) {
	t.Parallel()

	requests := 0
	checker := fakeRegistry(t, time.Now().Add(400*24*time.Hour).UTC().Format(time.RFC3339), &requests)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for range 5 {
		if obs := checker.Check(ctx, []byte(`{"domain":"good.test"}`)); obs.Status != model.StatusUp {
			t.Fatalf("status = %s (%s)", obs.Status, obs.Message)
		}
	}
	if requests != 1 {
		t.Errorf("made %d registry lookups for 5 checks, want 1", requests)
	}

	// A different domain is a different entry, not a cache hit.
	_ = checker.Check(ctx, []byte(`{"domain":"other.test"}`))
	if requests != 2 {
		t.Errorf("made %d lookups after a second domain, want 2", requests)
	}
}

func TestDomainExpiryValidate(t *testing.T) {
	t.Parallel()

	checker := NewDomainExpiry()
	rejected := map[string]string{
		"no domain":     `{}`,
		"no tld":        `{"domain":"localhost"}`,
		"an ip address": `{"domain":"192.0.2.1"}`,
		"a url":         `{"domain":"https://example.com"}`,
		"bad source":    `{"domain":"example.com","source":"registry"}`,
		"bad threshold": `{"domain":"example.com","days_remaining_threshold":9999}`,
		"unknown field": `{"domain":"example.com","nope":1}`,
	}
	for name, config := range rejected {
		if err := checker.Validate([]byte(config)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := checker.Validate([]byte(`{"domain":"example.co.uk"}`)); err != nil {
		t.Errorf("multi-label suffix rejected: %v", err)
	}
}

func TestTLDOf(t *testing.T) {
	t.Parallel()

	// The registry for a name under a multi-label suffix is still the last
	// label, which is what both the RDAP bootstrap file and IANA are keyed on.
	for domain, want := range map[string]string{
		"example.com":   "com",
		"example.co.uk": "uk",
		"Example.COM.":  "com",
		"a.b.c.example": "example",
	} {
		got, err := tldOf(domain)
		if err != nil {
			t.Errorf("tldOf(%q): %v", domain, err)
			continue
		}
		if got != want {
			t.Errorf("tldOf(%q) = %q, want %q", domain, got, want)
		}
	}
}

// WHOIS has no schema: every registry spells the expiry field differently and
// dates it differently. A missing entry here is a monitor that reports unknown
// forever, so the table is worth asserting against directly.
func TestWHOISExpiryParsing(t *testing.T) {
	t.Parallel()

	bodies := map[string]string{
		"verisign":  "Domain Name: EXAMPLE.COM\r\nRegistry Expiry Date: 2027-08-13T04:00:00Z\r\n",
		"generic":   "Expiration Date: 2027-08-13T04:00:00Z\n",
		"dotted":    "expires:          13.08.2027\n",
		"day-month": "Expiry date:  13-Aug-2027\n",
		"ru":        "paid-till:     2027-08-13T04:00:00Z\n",
		"plain":     "Expires On: 2027-08-13\n",
	}
	for name, body := range bodies {
		when, err := whoisExpiry(body)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if when.Year() != 2027 || when.Month() != time.August || when.Day() != 13 {
			t.Errorf("%s parsed to %s, want 2027-08-13", name, when.Format(time.RFC3339))
		}
	}

	// A response with no expiry field and a no-match marker means the name is
	// unregistered, which is a verdict rather than a parse failure.
	if _, err := whoisExpiry("No match for \"NOPE.COM\".\n"); err == nil {
		t.Error("a no-match response parsed as an expiry date")
	} else if err != errNotRegistered {
		t.Errorf("no-match response gave %v, want errNotRegistered", err)
	}

	// A response we simply cannot read is not evidence of anything.
	if _, err := whoisExpiry("Domain Name: EXAMPLE.COM\nRegistrar: Someone\n"); err == nil {
		t.Error("an unreadable response parsed as an expiry date")
	}
}

func TestWHOISField(t *testing.T) {
	t.Parallel()

	body := "% comment\nwhois:        whois.nic.example\nRegistrar WHOIS Server: whois.registrar.test\n"
	if got := whoisField(body, "whois"); got != "whois.nic.example" {
		t.Errorf("whois field = %q", got)
	}
	// Case-insensitive, because registries do not agree on capitalisation.
	if got := whoisField(body, "registrar whois server"); got != "whois.registrar.test" {
		t.Errorf("referral field = %q", got)
	}
	// An empty value is absent, not present-and-blank: IANA really does emit
	// "whois:" with nothing after it for the TLDs that publish no server.
	if got := whoisField("whois:\n", "whois"); got != "" {
		t.Errorf("empty field = %q, want an empty string", got)
	}
}

// The registrar arrives in the same response as the date, so reading it costs
// nothing — but it arrives in jCard, which is an array of heterogeneous arrays
// and the one part of RFC 9083 that is easy to get quietly wrong.
func TestRDAPRegistrar(t *testing.T) {
	t.Parallel()

	var payload struct {
		Entities []rdapEntity `json:"entities"`
	}
	body := `{"entities":[
		{"roles":["technical"],"vcardArray":["vcard",[["version",{},"text","4.0"],["fn",{},"text","Someone Else"]]]},
		{"roles":["registrar","abuse"],"vcardArray":["vcard",[["version",{},"text","4.0"],["fn",{},"text","Example Registrar, Inc."]]]}
	]}`
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := rdapRegistrar(payload.Entities); got != "Example Registrar, Inc." {
		t.Errorf("registrar = %q, want the entity holding the registrar role", got)
	}

	// A response that names no registrar leaves the field empty rather than
	// picking whichever entity came first.
	var none struct {
		Entities []rdapEntity `json:"entities"`
	}
	if err := json.Unmarshal([]byte(`{"entities":[{"roles":["technical"],"vcardArray":["vcard",[["fn",{},"text","Nobody"]]]}]}`), &none); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := rdapRegistrar(none.Entities); got != "" {
		t.Errorf("registrar = %q, want empty when no entity holds the role", got)
	}

	// jCard entries are arrays of mixed types. A malformed one is a field left
	// blank, never a panic in the middle of a check.
	var malformed struct {
		Entities []rdapEntity `json:"entities"`
	}
	if err := json.Unmarshal([]byte(`{"entities":[{"roles":["registrar"],"vcardArray":["vcard",[["fn",{}]]]}]}`), &malformed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := rdapRegistrar(malformed.Entities); got != "" {
		t.Errorf("registrar = %q, want empty from a malformed vcard", got)
	}
}

func TestWHOISRegistrar(t *testing.T) {
	t.Parallel()

	// "Registrar:" and "Registrar WHOIS Server:" both start with the same word,
	// and matching on a prefix would file the referral server as the registrar's
	// name on every thick registry there is.
	body := "Domain Name: EXAMPLE.COM\r\nRegistrar WHOIS Server: whois.registrar.test\r\nRegistrar: Example Registrar, Inc.\r\n"
	if got := whoisRegistrar(body); got != "Example Registrar, Inc." {
		t.Errorf("registrar = %q, want the registrar name", got)
	}
	if got := whoisRegistrar("Sponsoring Registrar: Old Spelling Ltd\n"); got != "Old Spelling Ltd" {
		t.Errorf("registrar = %q, want the sponsoring-registrar fallback", got)
	}
	if got := whoisRegistrar("Domain Name: EXAMPLE.COM\n"); got != "" {
		t.Errorf("registrar = %q, want empty when the record names none", got)
	}
}

// The registration is reported on every check, not only on the one that hit the
// registry: the date has not changed in between, and the control plane decides
// what is worth writing from the fact rather than from its absence.
func TestDomainExpiryObservesTheRegistration(t *testing.T) {
	t.Parallel()

	requests := 0
	expiry := time.Now().Add(400 * 24 * time.Hour).UTC().Truncate(time.Second)
	checker := fakeRegistry(t, expiry.Format(time.RFC3339), &requests)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for attempt := 1; attempt <= 2; attempt++ {
		obs := checker.Check(ctx, []byte(`{"domain":"good.test"}`))
		if obs.Status != model.StatusUp {
			t.Fatalf("attempt %d: status = %s (%s)", attempt, obs.Status, obs.Message)
		}
		if obs.Domain == nil {
			t.Fatalf("attempt %d: no registration observed", attempt)
		}
		if !obs.Domain.ExpiresAt.Equal(expiry) {
			t.Errorf("attempt %d: expires_at = %s, want %s", attempt, obs.Domain.ExpiresAt, expiry)
		}
		if obs.Domain.Registrar != "Example Registrar, Inc." {
			t.Errorf("attempt %d: registrar = %q", attempt, obs.Domain.Registrar)
		}
		// Lowercase, because that is the only spelling the schema's CHECK
		// constraint accepts — and the message next to it says "RDAP".
		if obs.Domain.Source != "rdap" {
			t.Errorf("attempt %d: source = %q, want rdap", attempt, obs.Domain.Source)
		}
		if obs.Domain.DaysRemainingThreshold == nil || *obs.Domain.DaysRemainingThreshold != defaultDomainThreshold {
			t.Errorf("attempt %d: threshold = %v, want the default", attempt, obs.Domain.DaysRemainingThreshold)
		}
	}

	// One registry lookup for both checks: the cache is the whole reason this
	// type can report on every check without being rate-limited off the
	// registry.
	if requests != 1 {
		t.Errorf("registry lookups = %d, want 1", requests)
	}
}

// A registry that could not be read tells us nothing about the registration, so
// there is nothing to record — and recording the previous date again would let
// the expiry page claim a lapsed domain is fine.
func TestDomainExpiryUnreadableObservesNothing(t *testing.T) {
	t.Parallel()

	checker := NewDomainExpiry()
	checker.bootstrapURL = "http://127.0.0.1:1/bootstrap"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	obs := checker.Check(ctx, []byte(`{"domain":"unreachable.test","source":"rdap"}`))
	if obs.Status != model.StatusUnknown {
		t.Fatalf("status = %s, want unknown", obs.Status)
	}
	if obs.Domain != nil {
		t.Error("an unreadable registry produced a registration observation")
	}
}
