package check

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

func TestTCPCheck(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	open := `{"hostname":"` + host + `","port":` + portText + `}`

	checker := NewTCP()
	if err := checker.Validate([]byte(open)); err != nil {
		t.Fatalf("validate: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	obs := checker.Check(ctx, []byte(open))
	if obs.Status != model.StatusUp {
		t.Errorf("open port reported %s: %s", obs.Status, obs.Message)
	}
	if obs.ResponseTime == nil {
		t.Error("a completed handshake recorded no response time")
	}

	// A port nothing listens on. Refused is a fact about the target, so it is
	// down — not unknown, which is reserved for failures of the probe.
	closedPort := freePort(t)
	closed := `{"hostname":"127.0.0.1","port":` + strconv.Itoa(closedPort) + `}`
	obs = checker.Check(ctx, []byte(closed))
	if obs.Status != model.StatusDown {
		t.Errorf("refused connection reported %s, want down", obs.Status)
	}
	if obs.Class != ClassNetwork {
		t.Errorf("refused connection classed %q, want %q", obs.Class, ClassNetwork)
	}
}

func TestTCPValidate(t *testing.T) {
	t.Parallel()

	checker := NewTCP()
	rejected := map[string]string{
		"no hostname":       `{"port":443}`,
		"no port":           `{"hostname":"example.com"}`,
		"port out of range": `{"hostname":"example.com","port":70000}`,
		"url in hostname":   `{"hostname":"https://example.com","port":443}`,
		"port in hostname":  `{"hostname":"example.com:443","port":443}`,
		"bad ip family":     `{"hostname":"example.com","port":443,"ip_family":"ipv7"}`,
		"unknown field":     `{"hostname":"example.com","port":443,"nope":1}`,
	}
	for name, config := range rejected {
		if err := checker.Validate([]byte(config)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	// An IPv6 literal is full of colons and must not be mistaken for host:port.
	if err := checker.Validate([]byte(`{"hostname":"::1","port":443}`)); err != nil {
		t.Errorf("IPv6 literal rejected: %v", err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}
