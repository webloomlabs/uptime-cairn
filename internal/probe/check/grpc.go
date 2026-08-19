package check

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// GRPC implements the grpc monitor type against the standard
// grpc.health.v1.Health/Check protocol.
//
// A fresh connection per check, for the same reason HTTP takes one: a cached
// connection reports on a channel established minutes ago and measures none of
// DNS, TCP, TLS, or HTTP/2 setup — which is most of what goes wrong.
type GRPC struct{}

// NewGRPC builds the checker.
func NewGRPC() *GRPC { return &GRPC{} }

// Type implements Checker.
func (g *GRPC) Type() string { return model.TypeGRPC }

// Version implements Checker.
func (g *GRPC) Version() uint32 { return 1 }

// grpcConfig mirrors GrpcConfig in docs/api/openapi.yaml.
type grpcConfig struct {
	Address          string            `json:"address"`
	ServiceName      *string           `json:"service_name"`
	UseTLS           *bool             `json:"use_tls"`
	VerifyTLS        *bool             `json:"verify_tls"`
	AcceptedStatuses []string          `json:"accepted_statuses"`
	Metadata         map[string]string `json:"metadata"`
}

var healthStatuses = map[string]healthpb.HealthCheckResponse_ServingStatus{
	"UNKNOWN":         healthpb.HealthCheckResponse_UNKNOWN,
	"SERVING":         healthpb.HealthCheckResponse_SERVING,
	"NOT_SERVING":     healthpb.HealthCheckResponse_NOT_SERVING,
	"SERVICE_UNKNOWN": healthpb.HealthCheckResponse_SERVICE_UNKNOWN,
}

// Validate implements Checker.
func (g *GRPC) Validate(config []byte) error {
	cfg, err := decodeGRPCConfig(config)
	if err != nil {
		return err
	}
	if cfg.Address == "" {
		return errors.New("address is required")
	}
	if strings.Contains(cfg.Address, "://") {
		return fmt.Errorf("address %q looks like a URL; give host:port", cfg.Address)
	}
	if _, _, err := splitHostPort(cfg.Address); err != nil {
		return err
	}
	for _, s := range cfg.AcceptedStatuses {
		if _, ok := healthStatuses[s]; !ok {
			return fmt.Errorf("accepted_statuses %q: want SERVING, NOT_SERVING, UNKNOWN, or SERVICE_UNKNOWN", s)
		}
	}
	for key := range cfg.Metadata {
		// grpc-go rejects these at call time with an error that names neither
		// the monitor nor the offending key.
		if key != strings.ToLower(key) {
			return fmt.Errorf("metadata key %q must be lower-case", key)
		}
		if strings.HasPrefix(key, "grpc-") {
			return fmt.Errorf("metadata key %q is reserved by gRPC", key)
		}
	}
	return nil
}

// Check implements Checker.
func (g *GRPC) Check(ctx context.Context, config []byte) Observation {
	cfg, err := decodeGRPCConfig(config)
	if err != nil {
		return Observation{Status: model.StatusUnknown, Class: ClassConfig, Message: err.Error()}
	}

	accepted := cfg.AcceptedStatuses
	if len(accepted) == 0 {
		accepted = []string{"SERVING"}
	}

	transport := insecure.NewCredentials()
	if cfg.UseTLS == nil || *cfg.UseTLS {
		host, _, _ := splitHostPort(cfg.Address)
		transport = credentials.NewTLS(&tls.Config{
			ServerName:         host,
			InsecureSkipVerify: cfg.VerifyTLS != nil && !*cfg.VerifyTLS, //nolint:gosec // verify_tls=false is an explicit, UI-warned per-monitor choice
			MinVersion:         tls.VersionTLS12,
		})
	}

	conn, err := grpc.NewClient(cfg.Address, grpc.WithTransportCredentials(transport))
	if err != nil {
		// A construction failure is about the address we were given, not about
		// the server: nothing has been dialled yet.
		return Observation{Status: model.StatusUnknown, Class: ClassConfig, Message: "grpc client: " + err.Error()}
	}
	defer func() { _ = conn.Close() }()

	callCtx := ctx
	if len(cfg.Metadata) > 0 {
		callCtx = metadata.NewOutgoingContext(ctx, metadata.New(cfg.Metadata))
	}

	start := time.Now()
	// WaitForReady is deliberately off. The default fails fast on a connection
	// error, which is the answer this monitor wants; waiting would convert every
	// refused connection into a timeout and lose the reason.
	resp, err := healthpb.NewHealthClient(conn).Check(callCtx, &healthpb.HealthCheckRequest{
		Service: serviceName(cfg),
	})
	elapsed := time.Since(start)
	if err != nil {
		return classifyGRPC(err, elapsed, cfg)
	}

	obs := Observation{
		Status:       model.StatusUp,
		ResponseTime: &elapsed,
		Code:         resp.GetStatus().String(),
	}
	for _, want := range accepted {
		if healthStatuses[want] == resp.GetStatus() {
			return obs
		}
	}
	obs.Status = model.StatusDown
	obs.Class = ClassAssertion
	obs.Message = fmt.Sprintf("health status %s is not in %s", resp.GetStatus(), strings.Join(accepted, ", "))
	return obs
}

// classifyGRPC maps a gRPC status onto the outcome taxonomy. The distinction
// that matters is the same one everywhere else: a server that answered "no" is
// down, a call this probe could not complete is unknown.
func classifyGRPC(err error, elapsed time.Duration, cfg grpcConfig) Observation {
	obs := Observation{Status: model.StatusDown, ResponseTime: &elapsed, Class: ClassNetwork, Message: err.Error()}

	st, ok := status.FromError(err)
	if !ok {
		return obs
	}
	obs.Code = st.Code().String()

	switch st.Code() {
	case codes.DeadlineExceeded:
		obs.Class = ClassTimeout
		obs.Message = "timed out after " + elapsed.Round(time.Millisecond).String()
	case codes.Canceled:
		// Shutdown, not a verdict.
		obs.Status = model.StatusUnknown
		obs.Message = "check cancelled"
	case codes.NotFound:
		// The health service is there and says it has never heard of this
		// service name. That is an answer about the target.
		obs.Class = ClassAssertion
		obs.Message = fmt.Sprintf("server does not know service %q", serviceName(cfg))
	case codes.Unimplemented:
		obs.Class = ClassAssertion
		obs.Message = "server does not implement grpc.health.v1.Health"
	case codes.Unavailable:
		obs.Class = ClassNetwork
		obs.Message = "unavailable: " + st.Message()
		if strings.Contains(st.Message(), "tls:") || strings.Contains(st.Message(), "certificate") {
			obs.Class = ClassTLS
		}
	default:
		obs.Message = st.Code().String() + ": " + st.Message()
	}
	return obs
}

func serviceName(cfg grpcConfig) string {
	if cfg.ServiceName == nil {
		// The empty string is the protocol's own spelling of "overall server
		// health", not a missing value.
		return ""
	}
	return *cfg.ServiceName
}

func decodeGRPCConfig(config []byte) (grpcConfig, error) {
	var cfg grpcConfig
	dec := json.NewDecoder(strings.NewReader(string(config)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}
