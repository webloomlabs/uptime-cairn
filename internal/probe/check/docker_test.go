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

// fakeDaemon serves the sliver of the Docker API this checker reads.
func fakeDaemon(t *testing.T, containers map[string]string) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/json")
		body, ok := containers[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such container"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	return strings.TrimPrefix(server.URL, "http://")
}

func TestDockerCheck(t *testing.T) {
	t.Parallel()

	host := fakeDaemon(t, map[string]string{
		"running":     `{"Name":"/running","State":{"Status":"running","Running":true}}`,
		"exited":      `{"Name":"/exited","State":{"Status":"exited","Running":false,"ExitCode":137}}`,
		"healthy":     `{"Name":"/healthy","State":{"Status":"running","Running":true,"Health":{"Status":"healthy"}}}`,
		"unhealthy":   `{"Name":"/unhealthy","State":{"Status":"running","Running":true,"Health":{"Status":"unhealthy","FailingStreak":3}}}`,
		"starting":    `{"Name":"/starting","State":{"Status":"running","Running":true,"Health":{"Status":"starting"}}}`,
		"nohealthchk": `{"Name":"/nohealthchk","State":{"Status":"running","Running":true}}`,
	})

	tests := []struct {
		name      string
		container string
		require   bool
		want      model.Status
		class     ErrorClass
		code      string
	}{
		{name: "running", container: "running", want: model.StatusUp, code: "running"},
		{name: "exited", container: "exited", want: model.StatusDown, class: ClassAssertion, code: "exited"},
		{name: "missing container", container: "nope", want: model.StatusDown, class: ClassAssertion, code: "no_such_container"},
		{name: "running is enough by default", container: "unhealthy", want: model.StatusUp, code: "running"},
		{name: "healthy with require_healthy", container: "healthy", require: true, want: model.StatusUp, code: "healthy"},
		{name: "unhealthy with require_healthy", container: "unhealthy", require: true, want: model.StatusDown, class: ClassAssertion, code: "unhealthy"},
		{
			// Inside the start period the container has not been given a chance
			// to answer. Not down, and not a lie either way.
			name: "starting with require_healthy", container: "starting", require: true,
			want: model.StatusUnknown, code: "starting",
		},
		{
			// require_healthy on an image with no HEALTHCHECK cannot be
			// satisfied. Reporting up would report on an assertion that never
			// ran, which is the failure mode this codebase refuses everywhere.
			name: "no healthcheck with require_healthy", container: "nohealthchk", require: true,
			want: model.StatusDown, class: ClassAssertion,
		},
	}

	checker := NewDocker()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			config := `{"container":"` + tc.container + `","docker_host":"tcp://` + host + `"`
			if tc.require {
				config += `,"require_healthy":true`
			}
			config += "}"

			if err := checker.Validate([]byte(config)); err != nil {
				t.Fatalf("validate: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			obs := checker.Check(ctx, []byte(config))
			if obs.Status != tc.want {
				t.Errorf("status = %s, want %s (%s)", obs.Status, tc.want, obs.Message)
			}
			if tc.class != "" && obs.Class != tc.class {
				t.Errorf("class = %q, want %q", obs.Class, tc.class)
			}
			if tc.code != "" && obs.Code != tc.code {
				t.Errorf("code = %q, want %q", obs.Code, tc.code)
			}
		})
	}
}

// A daemon this probe cannot reach is a fact about the probe's environment — a
// socket that was not mounted — and must never read as an application outage.
func TestDockerUnreachableDaemonIsUnknown(t *testing.T) {
	t.Parallel()

	config := `{"container":"anything","docker_host":"unix:///nonexistent/docker.sock"}`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	obs := NewDocker().Check(ctx, []byte(config))
	if obs.Status != model.StatusUnknown {
		t.Errorf("status = %s, want unknown (%s)", obs.Status, obs.Message)
	}
	if obs.Class != ClassCapability {
		t.Errorf("class = %q, want %q", obs.Class, ClassCapability)
	}
}

func TestDockerValidate(t *testing.T) {
	t.Parallel()

	checker := NewDocker()
	rejected := map[string]string{
		"no container":        `{}`,
		"path in name":        `{"container":"some/name"}`,
		"bad host scheme":     `{"container":"x","docker_host":"ftp://host:21"}`,
		"host with no scheme": `{"container":"x","docker_host":"/var/run/docker.sock"}`,
		"npipe":               `{"container":"x","docker_host":"npipe:////./pipe/docker_engine"}`,
		"tcp without port":    `{"container":"x","docker_host":"tcp://host"}`,
		"cert without key":    `{"container":"x","tls":{"client_cert":"-----BEGIN CERTIFICATE-----"}}`,
		"bad ca pem":          `{"container":"x","tls":{"ca_cert":"not a certificate"}}`,
		"unknown field":       `{"container":"x","nope":1}`,
	}
	for name, config := range rejected {
		if err := checker.Validate([]byte(config)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	// The default socket path needs no configuration at all.
	if err := checker.Validate([]byte(`{"container":"web"}`)); err != nil {
		t.Errorf("minimal config rejected: %v", err)
	}
}
