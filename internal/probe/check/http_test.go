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

func TestHTTPCheck(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte("Everything is fine"))
		case "/500":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("oh no"))
		case "/slow":
			time.Sleep(150 * time.Millisecond)
			_, _ = w.Write([]byte("eventually"))
		case "/auth":
			user, pass, _ := r.BasicAuth()
			if user != "alice" || pass != "hunter2" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte("welcome"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	// t.Cleanup, not defer: a defer here would close the server when this
	// function returns, which is before the parallel subtests below have run.
	t.Cleanup(server.Close)

	checker := NewHTTP()

	tests := []struct {
		name    string
		config  string
		want    model.Status
		class   ErrorClass
		message string // substring
	}{
		{
			name:   "200 is up",
			config: `{"url":"` + server.URL + `/ok"}`,
			want:   model.StatusUp,
		},
		{
			name:    "500 fails the status assertion",
			config:  `{"url":"` + server.URL + `/500"}`,
			want:    model.StatusDown,
			class:   ClassAssertion,
			message: "status 500 is not in 200-299",
		},
		{
			name:   "500 passes when explicitly accepted",
			config: `{"url":"` + server.URL + `/500","accepted_status_codes":["500"]}`,
			want:   model.StatusUp,
		},
		{
			name:   "keyword present",
			config: `{"url":"` + server.URL + `/ok","keyword":{"value":"everything","mode":"contains"}}`,
			want:   model.StatusUp,
		},
		{
			name:    "keyword absent fails",
			config:  `{"url":"` + server.URL + `/ok","keyword":{"value":"catastrophe","mode":"contains"}}`,
			want:    model.StatusDown,
			class:   ClassAssertion,
			message: "not found in response body",
		},
		{
			name:    "case sensitivity is honoured",
			config:  `{"url":"` + server.URL + `/ok","keyword":{"value":"everything","mode":"contains","case_sensitive":true}}`,
			want:    model.StatusDown,
			class:   ClassAssertion,
			message: "not found",
		},
		{
			name:    "not_contains fails when present",
			config:  `{"url":"` + server.URL + `/ok","keyword":{"value":"fine","mode":"not_contains"}}`,
			want:    model.StatusDown,
			class:   ClassAssertion,
			message: "should not be",
		},
		{
			name:   "regex matches",
			config: `{"url":"` + server.URL + `/ok","keyword":{"value":"is (fine|ok)","mode":"regex"}}`,
			want:   model.StatusUp,
		},
		{
			name:    "response time threshold fails a 200",
			config:  `{"url":"` + server.URL + `/slow","response_time_threshold_ms":10}`,
			want:    model.StatusDown,
			class:   ClassAssertion,
			message: "threshold",
		},
		{
			name:   "basic auth is sent",
			config: `{"url":"` + server.URL + `/auth","auth":{"type":"basic","username":"alice","password":"hunter2"}}`,
			want:   model.StatusUp,
		},
		{
			name:    "connection refused is down, not unknown",
			config:  `{"url":"http://127.0.0.1:1/"}`,
			want:    model.StatusDown,
			class:   ClassNetwork,
			message: "refused",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			obs := checker.Check(ctx, []byte(tc.config))
			if obs.Status != tc.want {
				t.Errorf("status = %s, want %s (message %q)", obs.Status, tc.want, obs.Message)
			}
			if tc.class != "" && obs.Class != tc.class {
				t.Errorf("class = %q, want %q", obs.Class, tc.class)
			}
			if tc.message != "" && !strings.Contains(strings.ToLower(obs.Message), strings.ToLower(tc.message)) {
				t.Errorf("message = %q, want it to contain %q", obs.Message, tc.message)
			}
		})
	}
}

// A timeout is the target failing to answer, which is a fact about the target:
// down, not unknown. Getting this backwards would silence real outages.
func TestHTTPCheckTimeoutIsDown(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	obs := NewHTTP().Check(ctx, []byte(`{"url":"`+server.URL+`/"}`))
	if obs.Status != model.StatusDown {
		t.Fatalf("status = %s, want down", obs.Status)
	}
	if obs.Class != ClassTimeout {
		t.Errorf("class = %q, want %q", obs.Class, ClassTimeout)
	}
}

// A name that does not resolve is the target's problem (down). A resolver that
// is broken is ours (unknown). The second must never be reported as the first —
// that is how one broken probe pages an entire on-call rotation.
func TestHTTPCheckUnresolvableHostIsDown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	obs := NewHTTP().Check(ctx, []byte(`{"url":"http://does-not-exist.invalid/"}`))
	if obs.Status != model.StatusDown {
		t.Fatalf("status = %s, want down (message %q)", obs.Status, obs.Message)
	}
	if obs.Class != ClassDNS {
		t.Errorf("class = %q, want %q", obs.Class, ClassDNS)
	}
}

func TestHTTPValidate(t *testing.T) {
	t.Parallel()

	checker := NewHTTP()

	valid := []string{
		`{"url":"https://example.com"}`,
		`{"url":"https://example.com","method":"POST","body":"{}","body_encoding":"json"}`,
		`{"url":"https://example.com","accepted_status_codes":["200-299","301"]}`,
		`{"url":"https://example.com","keyword":{"value":"a+","mode":"regex"}}`,
	}
	for _, config := range valid {
		if err := checker.Validate([]byte(config)); err != nil {
			t.Errorf("Validate(%s) = %v, want nil", config, err)
		}
	}

	invalid := map[string]string{
		`{}`:                          "url is required",
		`{"url":"ftp://example.com"}`: "scheme",
		`{"url":"https://example.com","method":"FETCH"}`:                       "method",
		`{"url":"https://example.com","accepted_status_codes":["999"]}`:        "100-599",
		`{"url":"https://example.com","keyword":{"value":"[","mode":"regex"}}`: "regex",
		// The one that would otherwise pass silently: an assertion this build
		// cannot evaluate must stop the monitor, not be ignored.
		`{"url":"https://example.com","json_path":{"path":"$.a","operator":"eq"}}`: "json_path",
	}
	for config, want := range invalid {
		err := checker.Validate([]byte(config))
		if err == nil {
			t.Errorf("Validate(%s) = nil, want an error mentioning %q", config, want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate(%s) = %q, want it to mention %q", config, err, want)
		}
	}
}
