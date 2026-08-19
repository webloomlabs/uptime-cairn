package check

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// serveHealth starts a real gRPC server with the standard health service, so the
// checker is exercised against the protocol rather than against a stub of it.
func serveHealth(t *testing.T, statuses map[string]healthpb.HealthCheckResponse_ServingStatus) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server := grpc.NewServer()
	healthServer := health.NewServer()
	for service, status := range statuses {
		healthServer.SetServingStatus(service, status)
	}
	healthpb.RegisterHealthServer(server, healthServer)

	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	return listener.Addr().String()
}

func TestGRPCHealth(t *testing.T) {
	t.Parallel()

	address := serveHealth(t, map[string]healthpb.HealthCheckResponse_ServingStatus{
		"":             healthpb.HealthCheckResponse_SERVING,
		"cairn.Broken": healthpb.HealthCheckResponse_NOT_SERVING,
	})

	tests := []struct {
		name   string
		config string
		want   model.Status
		class  ErrorClass
		code   string
	}{
		{
			name:   "overall health is serving",
			config: `{"address":"` + address + `","use_tls":false}`,
			want:   model.StatusUp,
			code:   "SERVING",
		},
		{
			name:   "a not-serving service fails the assertion",
			config: `{"address":"` + address + `","use_tls":false,"service_name":"cairn.Broken"}`,
			want:   model.StatusDown,
			class:  ClassAssertion,
			code:   "NOT_SERVING",
		},
		{
			// An operator watching a draining service can accept both.
			name:   "not_serving can be accepted explicitly",
			config: `{"address":"` + address + `","use_tls":false,"service_name":"cairn.Broken","accepted_statuses":["SERVING","NOT_SERVING"]}`,
			want:   model.StatusUp,
			code:   "NOT_SERVING",
		},
		{
			// The server answered and said it has never heard of the service.
			// That is a fact about the target, so it is down.
			name:   "an unknown service is down",
			config: `{"address":"` + address + `","use_tls":false,"service_name":"cairn.Missing"}`,
			want:   model.StatusDown,
			class:  ClassAssertion,
		},
	}

	checker := NewGRPC()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

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
			if tc.code != "" && obs.Code != tc.code {
				t.Errorf("code = %q, want %q", obs.Code, tc.code)
			}
			if obs.Status == model.StatusUp && obs.ResponseTime == nil {
				t.Error("a successful check recorded no response time")
			}
		})
	}
}

// A refused connection is down, and it must fail fast rather than waiting out
// the monitor's whole timeout — which is what WaitForReady would have done, and
// it would have lost the reason on the way.
func TestGRPCRefusedFailsFast(t *testing.T) {
	t.Parallel()

	config := `{"address":"127.0.0.1:` + strconv.Itoa(freePort(t)) + `","use_tls":false}`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	obs := NewGRPC().Check(ctx, []byte(config))
	elapsed := time.Since(start)

	if obs.Status != model.StatusDown {
		t.Errorf("status = %s, want down", obs.Status)
	}
	if obs.Class != ClassNetwork {
		t.Errorf("class = %q, want %q", obs.Class, ClassNetwork)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %s to notice a refused connection; the call is waiting for readiness", elapsed)
	}
}

func TestGRPCValidate(t *testing.T) {
	t.Parallel()

	checker := NewGRPC()
	rejected := map[string]string{
		"no address":              `{}`,
		"no port":                 `{"address":"example.com"}`,
		"url":                     `{"address":"https://example.com:443"}`,
		"bad status":              `{"address":"example.com:443","accepted_statuses":["HEALTHY"]}`,
		"upper-case metadata key": `{"address":"example.com:443","metadata":{"X-Trace":"1"}}`,
		"reserved metadata key":   `{"address":"example.com:443","metadata":{"grpc-timeout":"1"}}`,
		"unknown field":           `{"address":"example.com:443","nope":1}`,
	}
	for name, config := range rejected {
		if err := checker.Validate([]byte(config)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := checker.Validate([]byte(`{"address":"example.com:443"}`)); err != nil {
		t.Errorf("minimal config rejected: %v", err)
	}
}
