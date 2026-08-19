package check

import (
	"context"
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
