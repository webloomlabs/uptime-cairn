package check

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The correspondence this whole feature rests on: every property the frozen
// OpenAPI spec marks writeOnly on a monitor's config is a property this package
// takes out of the stored config and encrypts.
//
// Asserted rather than reviewed, because the failure mode is silent. A monitor
// type gains a credential in the spec, nobody adds it to SecretFields, and the
// result is not an error — it is a password sitting in the clear, indefinitely,
// in a column somebody will one day paste into a bug report.

// specSchemas maps a monitor type to the schema in docs/api/openapi.yaml that
// defines its config.
var specSchemas = map[string]string{
	"http":          "HttpConfig",
	"tcp":           "TcpConfig",
	"icmp":          "IcmpConfig",
	"dns":           "DnsConfig",
	"tls_expiry":    "TlsExpiryConfig",
	"domain_expiry": "DomainExpiryConfig",
	"docker":        "DockerConfig",
	"grpc":          "GrpcConfig",
}

func TestSecretFieldsMatchTheSpec(t *testing.T) {
	t.Parallel()

	spec := readSpec(t)

	registry := NewRegistry()
	registry.Register(NewHTTP())
	registry.Register(NewTCP())
	registry.Register(NewICMP())
	registry.Register(NewDNS())
	registry.Register(NewTLSExpiry())
	registry.Register(NewDomainExpiry())
	registry.Register(NewDocker())
	registry.Register(NewGRPC())

	for _, monitorType := range registry.Types() {
		schema, ok := specSchemas[monitorType]
		if !ok {
			t.Errorf("%s has no entry in specSchemas, so nothing checks its credentials", monitorType)
			continue
		}

		want := writeOnlyPaths(t, spec, schema)
		got := append([]string(nil), registry.SecretFields(monitorType)...)
		sort.Strings(got)
		sort.Strings(want)

		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: SecretFields = %v, spec marks %v writeOnly", monitorType, got, want)
		}
	}
}

// gRPC metadata is the one field the spec describes as encrypted without marking
// it writeOnly — it is an open map, and the keys are configuration worth reading
// back even though the values are not. Named here so the test above's exact
// match still holds and the exception stays deliberate.
var describedAsEncrypted = map[string][]string{
	"GrpcConfig": {"metadata"},
}

// writeOnlyPaths scans one schema block and returns the dotted paths of its
// writeOnly properties.
//
// A hand-rolled indentation walk rather than a YAML parser, because pulling in a
// YAML dependency to read one file in one test is exactly the trade AGENTS.md §5
// says not to make. The spec is machine-generated-tidy and two spaces per level
// throughout, which is the only assumption here.
func writeOnlyPaths(t *testing.T, spec []string, schema string) []string {
	t.Helper()

	start, indent := -1, 0
	for i, line := range spec {
		trimmed := strings.TrimSpace(line)
		if trimmed == schema+":" {
			start, indent = i, leadingSpaces(line)
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s is not in the spec", schema)
	}

	var (
		paths []string
		stack []string // one entry per indentation level below the schema root
	)
	for _, line := range spec[start+1:] {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		depth := leadingSpaces(line)
		if depth <= indent {
			break // the next schema
		}

		key, hasKey := yamlKey(line)
		if !hasKey {
			continue
		}

		// One stack slot per two-space level below the schema's own indent.
		level := (depth - indent) / 2
		for len(stack) < level {
			stack = append(stack, "")
		}
		stack = stack[:level]
		stack = append(stack, key)

		if strings.Contains(line, "writeOnly: true") {
			paths = append(paths, dottedPath(stack))
		}
	}

	paths = append(paths, describedAsEncrypted[schema]...)
	return paths
}

// dottedPath drops the YAML scaffolding — properties, items, the schema's own
// composition keywords — leaving the path a config document actually has.
func dottedPath(stack []string) string {
	var out []string
	for _, key := range stack {
		switch key {
		case "properties", "items", "allOf", "oneOf", "additionalProperties", "":
			continue
		}
		out = append(out, key)
	}
	return strings.Join(out, ".")
}

func leadingSpaces(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

func yamlKey(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "- ") {
		return "", false
	}
	key, _, found := strings.Cut(trimmed, ":")
	if !found || key == "" || strings.ContainsAny(key, " \"{[") {
		return "", false
	}
	return key, true
}

func readSpec(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read the frozen spec: %v", err)
	}
	return strings.Split(string(raw), "\n")
}

// The scanner is doing real work, so it gets its own assertion: if it silently
// found nothing, the test above would pass for every checker by agreeing that
// there is nothing to encrypt.
func TestSpecScannerFindsTheKnownFields(t *testing.T) {
	t.Parallel()

	spec := readSpec(t)
	cases := map[string][]string{
		"HttpConfig":   {"auth.password", "auth.token"},
		"DockerConfig": {"tls.ca_cert", "tls.client_cert", "tls.client_key"},
		"TcpConfig":    nil,
	}

	for schema, want := range cases {
		got := writeOnlyPaths(t, spec, schema)
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: scanner found %v, want %v", schema, got, want)
		}
	}
}

// Nothing else may quietly become confidential: a checker that starts returning
// paths without the spec marking them would pass the comparison above only
// because both sides moved.
func TestCheckersWithoutCredentialsHaveNone(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	registry.Register(NewTCP())
	registry.Register(NewICMP())
	registry.Register(NewDNS())
	registry.Register(NewTLSExpiry())
	registry.Register(NewDomainExpiry())

	for _, monitorType := range registry.Types() {
		if fields := registry.SecretFields(monitorType); len(fields) != 0 {
			t.Errorf("%s reports secret fields %v; it checks anonymously", monitorType, fields)
		}
	}
}
