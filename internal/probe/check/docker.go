package check

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Docker implements the docker monitor type by asking the daemon directly over
// its REST API. No client library: the whole check is one GET against
// /containers/{name}/json, and a dependency that ships a Kubernetes-sized
// transitive tree to read three fields is a poor trade.
//
// The daemon must be reachable from wherever the probe runs, which in solo mode
// means the socket is mounted into the container. That locality is why the plan
// pairs this type with monitor-to-named-probe pinning: in a multi-probe install,
// "is this container running" is only answerable by the probe on that host, and
// nothing in this file can make an unpinned assignment land in the right place.
type Docker struct{}

// NewDocker builds the checker.
func NewDocker() *Docker { return &Docker{} }

// Type implements Checker.
func (d *Docker) Type() string { return model.TypeDocker }

// Version implements Checker.
func (d *Docker) Version() uint32 { return 1 }

// dockerConfig mirrors DockerConfig in docs/api/openapi.yaml.
type dockerConfig struct {
	Container      string `json:"container"`
	DockerHost     string `json:"docker_host"`
	RequireHealthy bool   `json:"require_healthy"`
	TLS            *struct {
		CACert     *string `json:"ca_cert"`
		ClientCert *string `json:"client_cert"`
		ClientKey  *string `json:"client_key"`
		Verify     *bool   `json:"verify"`
	} `json:"tls"`
}

const defaultDockerHost = "unix:///var/run/docker.sock"

// dockerInspect is the sliver of the inspect response this check reads. Decoding
// the whole thing would couple us to a schema that changes every release.
type dockerInspect struct {
	Name  string `json:"Name"`
	State struct {
		Status   string `json:"Status"`
		Running  bool   `json:"Running"`
		ExitCode int    `json:"ExitCode"`
		Error    string `json:"Error"`
		Health   *struct {
			Status        string `json:"Status"`
			FailingStreak int    `json:"FailingStreak"`
		} `json:"Health"`
	} `json:"State"`
}

// Validate implements Checker.
func (d *Docker) Validate(config []byte) error {
	cfg, err := decodeDockerConfig(config)
	if err != nil {
		return err
	}
	if cfg.Container == "" {
		return errors.New("container is required")
	}
	if strings.ContainsAny(cfg.Container, " /\\") {
		return fmt.Errorf("container %q: give the container name or id alone", cfg.Container)
	}
	if _, _, err := parseDockerHost(cfg.DockerHost); err != nil {
		return err
	}
	if cfg.TLS != nil {
		if _, err := dockerTLSConfig(cfg); err != nil {
			return err
		}
	}
	return nil
}

// Check implements Checker.
func (d *Docker) Check(ctx context.Context, config []byte) Observation {
	cfg, err := decodeDockerConfig(config)
	if err != nil {
		return Observation{Status: model.StatusUnknown, Class: ClassConfig, Message: err.Error()}
	}

	client, base, err := dockerClient(cfg)
	if err != nil {
		return Observation{Status: model.StatusUnknown, Class: ClassConfig, Message: err.Error()}
	}
	defer client.CloseIdleConnections()

	endpoint := base + "/containers/" + url.PathEscape(cfg.Container) + "/json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Observation{Status: model.StatusUnknown, Class: ClassConfig, Message: "build request: " + err.Error()}
	}

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		// Every failure to reach the daemon is a failure of this probe's
		// environment, not of the container. A missing socket mount must not
		// read as an application outage.
		obs := classify(err, elapsed)
		obs.Status = model.StatusUnknown
		obs.Class = ClassCapability
		obs.Message = "cannot reach the Docker daemon at " + cfg.dockerHostOrDefault() + ": " + obs.Message
		return obs
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Observation{Status: model.StatusUnknown, Class: ClassCapability, ResponseTime: &elapsed, Message: "reading daemon response: " + err.Error()}
	}

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// The daemon answered, and the answer is that the container is gone.
		// That is a fact about the target.
		return Observation{
			Status:       model.StatusDown,
			Class:        ClassAssertion,
			ResponseTime: &elapsed,
			Code:         "no_such_container",
			Message:      fmt.Sprintf("no container named %q on this daemon", cfg.Container),
		}
	case resp.StatusCode != http.StatusOK:
		return Observation{
			Status:       model.StatusUnknown,
			Class:        ClassCapability,
			ResponseTime: &elapsed,
			Code:         fmt.Sprint(resp.StatusCode),
			Message:      "Docker daemon answered " + resp.Status + ": " + strings.TrimSpace(string(body)),
		}
	}

	var inspect dockerInspect
	if err := json.Unmarshal(body, &inspect); err != nil {
		return Observation{Status: model.StatusUnknown, Class: ClassCapability, ResponseTime: &elapsed, Message: "decoding daemon response: " + err.Error()}
	}

	return dockerVerdict(cfg, inspect, elapsed)
}

func dockerVerdict(cfg dockerConfig, inspect dockerInspect, elapsed time.Duration) Observation {
	obs := Observation{
		Status:       model.StatusUp,
		ResponseTime: &elapsed,
		Code:         inspect.State.Status,
	}

	if !inspect.State.Running {
		obs.Status = model.StatusDown
		obs.Class = ClassAssertion
		obs.Message = fmt.Sprintf("container is %s", inspect.State.Status)
		if inspect.State.Status == "exited" {
			obs.Message += fmt.Sprintf(" with code %d", inspect.State.ExitCode)
		}
		if inspect.State.Error != "" {
			obs.Message += ": " + inspect.State.Error
		}
		return obs
	}

	if !cfg.RequireHealthy {
		return obs
	}

	if inspect.State.Health == nil {
		// require_healthy on an image with no HEALTHCHECK cannot be satisfied,
		// and reporting up would be reporting on an assertion that never ran —
		// the same silent pass json_path is refused for.
		obs.Status = model.StatusDown
		obs.Class = ClassAssertion
		obs.Message = "require_healthy is set but the container defines no healthcheck, so its health cannot be observed"
		return obs
	}

	obs.Code = inspect.State.Health.Status
	switch inspect.State.Health.Status {
	case "healthy":
		return obs
	case "starting":
		// Not down: the container is inside its start period and has not been
		// given the chance to answer yet. Unknown rather than pending because a
		// probe may only report up, down, unknown, or skipped — pending is a
		// verdict that needs the control plane's consecutive-failure count.
		obs.Status = model.StatusUnknown
		obs.Class = ClassNone
		obs.Message = "container healthcheck is still starting"
		return obs
	default:
		obs.Status = model.StatusDown
		obs.Class = ClassAssertion
		obs.Message = fmt.Sprintf("container healthcheck is %s after %d failing %s",
			inspect.State.Health.Status, inspect.State.Health.FailingStreak,
			plural(inspect.State.Health.FailingStreak, "check"))
		return obs
	}
}

// dockerClient builds a client for the configured endpoint and returns the URL
// prefix to hang the path off. For a unix socket the host in the URL is a
// placeholder — the dialler ignores it — but net/http insists one exists.
func dockerClient(cfg dockerConfig) (*http.Client, string, error) {
	scheme, address, err := parseDockerHost(cfg.DockerHost)
	if err != nil {
		return nil, "", err
	}

	transport := &http.Transport{DisableKeepAlives: true}
	base := ""

	switch scheme {
	case "unix":
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", address)
		}
		base = "http://docker"
	case "http", "tcp":
		base = "http://" + address
	case "https":
		base = "https://" + address
	}

	if cfg.TLS != nil {
		tlsConf, err := dockerTLSConfig(cfg)
		if err != nil {
			return nil, "", err
		}
		transport.TLSClientConfig = tlsConf
		if scheme == "tcp" {
			// A daemon given client TLS material is a TLS daemon; 2376 over
			// plain HTTP would fail with a confusing protocol error.
			base = "https://" + address
		}
	}

	return &http.Client{Transport: transport}, base, nil
}

// dockerTLSConfig builds the client TLS material.
//
// The PEM arrives in the monitor config as plaintext today. Everything in data
// model §12 says it should be encrypted at rest through internal/secrets, and
// nothing in the write path does that yet — the encryption layer exists and
// carries the TOTP secret, but no monitor config passes through it.
func dockerTLSConfig(cfg dockerConfig) (*tls.Config, error) {
	conf := &tls.Config{MinVersion: tls.VersionTLS12}
	t := cfg.TLS

	if t.Verify != nil && !*t.Verify {
		conf.InsecureSkipVerify = true //nolint:gosec // verify=false is an explicit per-monitor choice for a private daemon CA
	}
	if t.CACert != nil && *t.CACert != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(*t.CACert)) {
			return nil, errors.New("tls.ca_cert is not valid PEM")
		}
		conf.RootCAs = pool
	}

	hasCert := t.ClientCert != nil && *t.ClientCert != ""
	hasKey := t.ClientKey != nil && *t.ClientKey != ""
	switch {
	case hasCert != hasKey:
		return nil, errors.New("tls.client_cert and tls.client_key must be given together")
	case hasCert:
		pair, err := tls.X509KeyPair([]byte(*t.ClientCert), []byte(*t.ClientKey))
		if err != nil {
			return nil, fmt.Errorf("tls client keypair: %w", err)
		}
		conf.Certificates = []tls.Certificate{pair}
	}
	return conf, nil
}

func parseDockerHost(host string) (scheme, address string, err error) {
	if host == "" {
		host = defaultDockerHost
	}
	scheme, address, found := strings.Cut(host, "://")
	if !found {
		return "", "", fmt.Errorf("docker_host %q: want unix:///path or tcp://host:port", host)
	}

	switch scheme {
	case "unix":
		if address == "" {
			return "", "", errors.New("docker_host has no socket path")
		}
	case "tcp", "http", "https":
		if _, _, err := splitHostPort(address); err != nil {
			return "", "", fmt.Errorf("docker_host: %w", err)
		}
	case "npipe":
		// Named pipes need a Windows-only dialler this probe does not carry, and
		// saying so beats a dial error about an unknown network.
		return "", "", errors.New("docker_host npipe:// is not supported by this probe")
	default:
		return "", "", fmt.Errorf("docker_host scheme %q: want unix, tcp, http, or https", scheme)
	}
	return scheme, address, nil
}

func (c dockerConfig) dockerHostOrDefault() string {
	if c.DockerHost == "" {
		return defaultDockerHost
	}
	return c.DockerHost
}

func decodeDockerConfig(config []byte) (dockerConfig, error) {
	var cfg dockerConfig
	dec := json.NewDecoder(strings.NewReader(string(config)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}
