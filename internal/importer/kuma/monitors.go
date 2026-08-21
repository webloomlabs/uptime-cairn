package kuma

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// monitorColumns is every column this importer will read if the file has it.
//
// Listed exhaustively rather than SELECT *, so that a Kuma release adding a
// column changes nothing here until somebody decides what it means. The reader
// intersects this with what the file actually has, so a 1.18 database missing
// half of them imports fine.
var monitorColumns = []string{
	"id", "name", "description", "active", "type", "url", "hostname", "port",
	"interval", "retry_interval", "resend_interval", "timeout", "maxretries",
	"upside_down", "parent", "keyword", "invert_keyword", "json_path",
	"expected_value", "accepted_statuscodes_json", "method", "body", "headers",
	"basic_auth_user", "basic_auth_pass", "auth_method", "authorization_header",
	"ignore_tls", "max_redirects", "dns_resolve_type", "dns_resolve_server",
	"port_dns", "docker_container", "docker_host", "push_token", "packet_size",
	"grpc_url", "grpc_service_name", "grpc_enable_tls", "proxy_id",
	"expiry_notification",
}

// kumaMonitor is one row, already coerced.
type kumaMonitor struct {
	ID       int64
	Name     string
	Desc     string
	Type     string
	Active   bool
	Parent   *int64
	ProxyID  *int64
	Interval time.Duration
	Timeout  time.Duration
	Retries  int
	Retry    time.Duration
	Resend   int
	Upside   bool

	row map[string]any
}

func readMonitor(row map[string]any) kumaMonitor {
	m := kumaMonitor{
		ID:     number(row["id"]),
		Name:   strings.TrimSpace(text(row["name"])),
		Desc:   text(row["description"]),
		Type:   strings.ToLower(strings.TrimSpace(text(row["type"]))),
		Active: truthy(row["active"]),
		Upside: truthy(row["upside_down"]),
		row:    row,
	}
	if v, ok := row["parent"]; ok && v != nil {
		parent := number(v)
		m.Parent = &parent
	}
	if v, ok := row["proxy_id"]; ok && v != nil {
		if proxy := number(v); proxy != 0 {
			m.ProxyID = &proxy
		}
	}

	// Kuma's interval is seconds and its floor is 20, which happens to be ours
	// too. A monitor configured below the floor — possible in old versions —
	// is raised rather than refused, and the report says so.
	m.Interval = time.Duration(number(row["interval"])) * time.Second
	if m.Interval < model.MinInterval {
		m.Interval = model.MinInterval
	}

	m.Retries = int(number(row["maxretries"]))
	m.Retry = time.Duration(number(row["retry_interval"])) * time.Second
	m.Resend = int(number(row["resend_interval"]))

	// Kuma's timeout is a DOUBLE in seconds and defaults to 0, meaning "48% of
	// the interval" in its own code. Our schema requires timeout < interval, so
	// zero is replaced rather than passed through — a monitor with a zero
	// timeout would be rejected at write time for a value the user never set.
	m.Timeout = time.Duration(decimal(row["timeout"]) * float64(time.Second))
	if m.Timeout <= 0 || m.Timeout >= m.Interval {
		m.Timeout = m.Interval / 2
		if m.Timeout < time.Second {
			m.Timeout = time.Second
		}
	}
	return m
}

// mapped is what one Kuma monitor becomes.
type mapped struct {
	Type   string
	Config map[string]any

	// Note is added to the import entry's detail when the mapping was not
	// exact — a keyword mode that had to be approximated, a feature dropped.
	// Empty when the mapping was clean, and the report says "imported" rather
	// than "imported, with the following lost".
	Notes []string

	// push carries a push monitor's token out of band, so it never lands in
	// Config — where it would be a live credential sitting in plaintext in a
	// column the whole encryption layer exists to keep credentials out of.
	push *pushCarry
}

// unsupportedType is returned for a Kuma type this build has no monitor for.
type unsupportedType struct {
	kumaType string
}

func (e *unsupportedType) Error() string {
	if name, ok := knownKumaTypes[e.kumaType]; ok {
		return fmt.Sprintf("Uptime Kuma's %q monitor (%s) has no equivalent in this build", e.kumaType, name)
	}
	return fmt.Sprintf("Uptime Kuma's %q monitor has no equivalent in this build", e.kumaType)
}

// knownKumaTypes exists so the report can say what a type *was* rather than
// only that it was unsupported.
//
// Data model §10 gap 1: Kuma has roughly forty types against this build's nine,
// and the decision recorded there — import as a disabled monitor of the nearest
// type, or skip entirely — is resolved in favour of *neither*, because both
// answers are wrong for a reason.
//
// Skipping loses the name, the interval, the tags and the notification
// attachments a user spent an afternoon setting up, for a monitor they will
// have to rebuild by hand anyway. Importing as "the nearest type" invents a
// check: a MQTT monitor imported as a TCP check on port 1883 is green while the
// broker rejects every publish, which is a monitoring tool lying, and the whole
// project exists on the other side of that line.
//
// So: the monitor is recorded in the report as `unsupported`, with its name and
// its type, and nothing is written. What that buys is a list the user can work
// from, in the order their own install had them, which is the artefact that
// makes a migration finishable. See the doc comment on Run for what the report
// promises.
var knownKumaTypes = map[string]string{
	"real-browser":       "browser check",
	"mqtt":               "MQTT broker",
	"kafka-producer":     "Kafka producer",
	"radius":             "RADIUS",
	"snmp":               "SNMP",
	"steam":              "Steam game server",
	"gamedig":            "game server",
	"mysql":              "MySQL",
	"postgres":           "PostgreSQL",
	"sqlserver":          "SQL Server",
	"mongodb":            "MongoDB",
	"redis":              "Redis",
	"rabbitmq":           "RabbitMQ",
	"tailscale-ping":     "Tailscale ping",
	"manual":             "manual status",
	"json-query":         "JSON query",
	"smtp":               "SMTP",
	"dns-over-https":     "DNS over HTTPS",
	"push-passive":       "passive push",
	"group":              "group",
	"http-keyword":       "HTTP keyword",
	"http-json-query":    "HTTP JSON query",
	"keyword":            "HTTP keyword",
	"grpc-keyword":       "gRPC keyword",
	"ping":               "ping",
	"port":               "TCP port",
	"dns":                "DNS",
	"docker":             "Docker container",
	"push":               "push",
	"http":               "HTTP",
	"certificate-expiry": "certificate expiry",
}

// mapMonitor turns a Kuma monitor into a Cairn one.
//
// Every branch here is a claim that the imported monitor checks the same thing
// the original did. Where it cannot make that claim it returns an error rather
// than an approximation — see knownKumaTypes for why "the nearest type" is not
// on offer.
func mapMonitor(m kumaMonitor) (mapped, error) {
	switch m.Type {
	case "http", "keyword", "http-keyword", "json-query", "http-json-query":
		return mapHTTP(m)
	case "port":
		return mapPort(m)
	case "ping":
		return mapPing(m)
	case "dns":
		return mapDNS(m)
	case "docker":
		return mapDocker(m)
	case "push":
		return mapPush(m)
	case "grpc-keyword":
		return mapGRPC(m)
	case "certificate-expiry":
		return mapTLSExpiry(m)
	default:
		return mapped{}, &unsupportedType{kumaType: m.Type}
	}
}

func mapHTTP(m kumaMonitor) (mapped, error) {
	raw := strings.TrimSpace(text(m.row["url"]))
	if raw == "" {
		return mapped{}, fmt.Errorf("the monitor has no url")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return mapped{}, fmt.Errorf("url %q could not be parsed", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return mapped{}, fmt.Errorf("url scheme %q is not http or https", parsed.Scheme)
	}

	out := mapped{Type: model.TypeHTTP, Config: map[string]any{"url": raw}}

	if method := strings.ToUpper(strings.TrimSpace(text(m.row["method"]))); method != "" && method != "GET" {
		out.Config["method"] = method
	}
	if body := text(m.row["body"]); body != "" {
		out.Config["body"] = body
	}
	if headers := parseHeaders(text(m.row["headers"])); len(headers) > 0 {
		out.Config["headers"] = headers
	}
	if codes := parseAcceptedCodes(text(m.row["accepted_statuscodes_json"])); len(codes) > 0 {
		out.Config["accepted_status_codes"] = codes
	}
	if truthy(m.row["ignore_tls"]) {
		out.Config["verify_tls"] = false
	}
	if v, ok := m.row["max_redirects"]; ok && present(v) {
		redirects := int(number(v))
		out.Config["max_redirects"] = redirects
		out.Config["follow_redirects"] = redirects > 0
	}

	// Keyword. Kuma's `invert_keyword` is our not_contains, and its match is
	// always a substring — it has no regex mode, so nothing is approximated
	// here in either direction.
	if keyword := text(m.row["keyword"]); keyword != "" {
		mode := "contains"
		if truthy(m.row["invert_keyword"]) {
			mode = "not_contains"
		}
		out.Config["keyword"] = map[string]any{
			"value": keyword,
			"mode":  mode,
			// Kuma matches case-insensitively and offers no switch, so this is
			// the setting that reproduces its behaviour rather than the one that
			// looks stricter.
			"case_sensitive": false,
		}
	}

	// Kuma's JSON query is a full jsonata expression. Ours is a deliberately
	// small subset — root, field names, array indices — chosen so that what a
	// monitor asserts is legible in a list. An expression outside the subset is
	// not silently reinterpreted: the monitor imports as the HTTP check it also
	// is, and the report names the assertion that did not come with it.
	if path := strings.TrimSpace(text(m.row["json_path"])); path != "" {
		expected := text(m.row["expected_value"])
		if converted, ok := convertJSONPath(path); ok {
			// eq with an expected value, or exists when Kuma had no expected
			// value to compare against — which is how it spells "the field is
			// there". Inventing an empty-string comparison instead would turn a
			// presence check into an equality check that fails on every real
			// response.
			assertion := map[string]any{"path": converted, "operator": "exists"}
			if expected != "" {
				assertion = map[string]any{"path": converted, "operator": "eq", "expected": expected}
			}
			encoded, err := json.Marshal(assertion)
			if err == nil {
				out.Config["json_path"] = json.RawMessage(encoded)
			}
		} else {
			out.Notes = append(out.Notes, fmt.Sprintf(
				"the JSON query %q is a jsonata expression outside the subset this build accepts, "+
					"so it was not imported — the monitor still checks the URL's status code", path))
		}
	}

	// Authentication. Kuma stores the password in plaintext beside the
	// username; it is written through the repository layer here, which encrypts
	// it (data model §12.6), and this importer never logs either.
	switch strings.ToLower(strings.TrimSpace(text(m.row["auth_method"]))) {
	case "", "none":
		if user := text(m.row["basic_auth_user"]); user != "" {
			out.Config["auth"] = map[string]any{
				"type": "basic", "username": user, "password": text(m.row["basic_auth_pass"]),
			}
		}
	case "basic":
		out.Config["auth"] = map[string]any{
			"type": "basic", "username": text(m.row["basic_auth_user"]),
			"password": text(m.row["basic_auth_pass"]),
		}
	case "oauth2-cc", "oauth2":
		out.Notes = append(out.Notes, "the OAuth2 client-credentials authentication was not imported: "+
			"this build offers basic and bearer authentication only, so the monitor will be refused by "+
			"the target until a credential is set on it")
	case "mtls":
		out.Notes = append(out.Notes, "the client-certificate authentication was not imported: "+
			"this build has no mTLS option for http monitors")
	case "ntlm":
		out.Notes = append(out.Notes, "the NTLM authentication was not imported: "+
			"this build offers basic and bearer authentication only")
	}
	if header := strings.TrimSpace(text(m.row["authorization_header"])); header != "" {
		if token, found := strings.CutPrefix(header, "Bearer "); found {
			out.Config["auth"] = map[string]any{"type": "bearer", "token": token}
		}
	}

	if m.ProxyID != nil {
		out.Notes = append(out.Notes, proxyNote)
	}
	return out, nil
}

// proxyNote is data model §10 gap 3, resolved by saying so plainly.
//
// Kuma supports a per-monitor HTTP proxy; nothing in the frozen OpenAPI spec
// has a proxy concept, and inventing one during an import would be adding API
// surface from inside a migration tool. So a proxied monitor imports as the
// check it is, without the proxy, and the report says the check will now be
// made from this host directly — which is a statement the user can act on,
// and is materially different from the monitor quietly going red.
const proxyNote = "the monitor used an Uptime Kuma proxy, which this build has no equivalent for: " +
	"the check will be made directly from this host instead, which may fail if the target is only " +
	"reachable through that proxy"

func mapPort(m kumaMonitor) (mapped, error) {
	host := strings.TrimSpace(text(m.row["hostname"]))
	port := int(number(m.row["port"]))
	if host == "" {
		return mapped{}, fmt.Errorf("the monitor has no hostname")
	}
	if port <= 0 || port > 65535 {
		return mapped{}, fmt.Errorf("port %d is outside 1-65535", port)
	}
	return mapped{Type: model.TypeTCP, Config: map[string]any{"hostname": host, "port": port}}, nil
}

func mapPing(m kumaMonitor) (mapped, error) {
	host := strings.TrimSpace(text(m.row["hostname"]))
	if host == "" {
		return mapped{}, fmt.Errorf("the monitor has no hostname")
	}
	config := map[string]any{"hostname": host}
	if size := int(number(m.row["packet_size"])); size > 0 {
		config["packet_size"] = size
	}
	return mapped{Type: model.TypeICMP, Config: config}, nil
}

func mapDNS(m kumaMonitor) (mapped, error) {
	host := strings.TrimSpace(text(m.row["hostname"]))
	if host == "" {
		return mapped{}, fmt.Errorf("the monitor has no hostname")
	}

	recordType := strings.ToUpper(strings.TrimSpace(text(m.row["dns_resolve_type"])))
	if recordType == "" {
		recordType = "A"
	}
	config := map[string]any{"hostname": host, "record_type": recordType}

	if resolver := strings.TrimSpace(text(m.row["dns_resolve_server"])); resolver != "" {
		config["resolver"] = resolver
	}
	if port := int(number(m.row["port_dns"])); port > 0 && port != 53 {
		config["resolver_port"] = port
	}

	out := mapped{Type: model.TypeDNS, Config: config}
	// Kuma asserts on the resolved value through the same `keyword` column it
	// uses for HTTP. Ours is a list with a match mode, which is the same
	// assertion spelled differently.
	if expected := strings.TrimSpace(text(m.row["keyword"])); expected != "" {
		config["expected_values"] = []string{expected}
		config["match_mode"] = "any"
	}
	return out, nil
}

func mapDocker(m kumaMonitor) (mapped, error) {
	container := strings.TrimSpace(text(m.row["docker_container"]))
	if container == "" {
		return mapped{}, fmt.Errorf("the monitor names no container")
	}
	return mapped{Type: model.TypeDocker, Config: map[string]any{"container": container}}, nil
}

// pushToken is carried on a mapped push monitor. It never reaches Config: the
// token is stored as a hash, and a plaintext credential sitting in a monitor's
// configuration column is the thing the encryption work exists to prevent.
type pushCarry struct{ token string }

func mapPush(m kumaMonitor) (mapped, error) {
	// The token *is* carried over, and it is worth saying why rather than
	// leaving it looking careless.
	//
	// This build stores a push token as a hash and shows the plaintext exactly
	// once, at creation. An import has no "once" — the report is a stored row,
	// and putting a live credential in it would leave it readable forever by
	// anything that can read the report. Minting a new token and *not* showing
	// it would produce a monitor whose URL nobody can ever learn, which is
	// worse still.
	//
	// So the user's own token comes across. It already exists in their kuma.db
	// and in whatever cron job sends the heartbeat, so nothing is disclosed
	// that they did not already have. The path differs between the two products
	// regardless — Kuma's /api/push/<token> against this build's
	// /api/v1/push/<token> — so the job has to be repointed either way, and
	// keeping the token makes that a host change rather than a credential
	// rotation done under time pressure.
	//
	// Both installs answering to the token during a cutover is fine and
	// arguably the point: each records its own heartbeat, and the switch on the
	// new install is satisfied by the same beat that satisfies the old one.
	token := strings.TrimSpace(text(m.row["push_token"]))
	out := mapped{Type: model.TypePush, Config: map[string]any{}}
	if token == "" {
		out.Notes = append(out.Notes, "this monitor had no push token in Uptime Kuma, so a new one was "+
			"issued and is shown on the monitor's own page")
	} else {
		out.Notes = append(out.Notes, "the push token came across unchanged, but the path did not: "+
			"point whatever sends this heartbeat at /api/v1/push/<token> on this install "+
			"instead of /api/push/<token>")
	}
	out.push = &pushCarry{token: token}
	return out, nil
}

func mapGRPC(m kumaMonitor) (mapped, error) {
	address := strings.TrimSpace(text(m.row["grpc_url"]))
	if address == "" {
		return mapped{}, fmt.Errorf("the monitor has no gRPC address")
	}
	config := map[string]any{"address": address}
	if service := strings.TrimSpace(text(m.row["grpc_service_name"])); service != "" {
		config["service_name"] = service
	}
	if truthy(m.row["grpc_enable_tls"]) {
		config["use_tls"] = true
	}

	out := mapped{Type: model.TypeGRPC, Config: config}
	// Kuma's grpc-keyword calls an arbitrary method and greps the response.
	// This build's gRPC monitor speaks the standard health protocol, which is a
	// different check against the same server — so it is imported and the
	// difference is named, rather than imported silently as if it were the same.
	out.Notes = append(out.Notes, "Uptime Kuma called a gRPC method and searched its response; "+
		"this build queries the standard gRPC health service instead, so the monitor now reports "+
		"whether the server declares itself healthy rather than what one method returned")
	return out, nil
}

func mapTLSExpiry(m kumaMonitor) (mapped, error) {
	host := strings.TrimSpace(text(m.row["hostname"]))
	if host == "" {
		if raw := text(m.row["url"]); raw != "" {
			if parsed, err := url.Parse(raw); err == nil {
				host = parsed.Hostname()
			}
		}
	}
	if host == "" {
		return mapped{}, fmt.Errorf("the monitor has no hostname")
	}
	config := map[string]any{"hostname": host}
	if port := int(number(m.row["port"])); port > 0 {
		config["port"] = port
	}
	return mapped{Type: model.TypeTLSExpiry, Config: config}, nil
}

// parseHeaders reads Kuma's headers column, which is a JSON object as text.
func parseHeaders(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil
	}
	out := make(map[string]string, len(decoded))
	for _, key := range sortedKeys(decoded) {
		if value := text(decoded[key]); value != "" {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseAcceptedCodes reads accepted_statuscodes_json, which is a JSON array of
// strings like ["200-299", "404"]. The two projects spell these identically, so
// this is a decode rather than a conversion.
func parseAcceptedCodes(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil
	}
	var decoded []string
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil
	}
	// The default is the default; carrying it explicitly would make every
	// imported monitor's config noisier than one created here.
	if len(decoded) == 1 && decoded[0] == "200-299" {
		return nil
	}
	sort.Strings(decoded)
	return decoded
}

// convertJSONPath translates the jsonata expressions that fall inside this
// build's subset, and reports failure for the rest.
//
// The subset is root, dotted field names, and array indices — deliberately
// small, so that what a monitor asserts is legible in a list rather than being
// a program. Kuma's field is full jsonata: functions, predicates, arithmetic,
// string concatenation. Anything using those is refused here rather than
// half-translated, because a JSON assertion that quietly checks something else
// is worse than one that is missing and named in the report.
func convertJSONPath(expr string) (string, bool) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "", false
	}

	// $.a.b and a.b are the same path; the leading $ is jsonata's root and ours.
	// Stripped before the character check below, or the root expression `$` and
	// every path written with one would be refused for containing a $.
	trimmed := strings.TrimPrefix(strings.TrimPrefix(expr, "$"), ".")
	if trimmed == "" {
		return "$", true
	}

	// Anything jsonata can do beyond naming a field: functions, predicates,
	// arithmetic, concatenation, quoting. Refused rather than half-translated,
	// because a JSON assertion that quietly checks something else is worse than
	// one that is missing and named in the report.
	if strings.ContainsAny(trimmed, "()$&|=<>!+*/%~^?:,'\" \t") {
		return "", false
	}

	for _, segment := range strings.Split(trimmed, ".") {
		if segment == "" {
			return "", false
		}
		name, index, bracketed := strings.Cut(segment, "[")
		if bracketed {
			if !strings.HasSuffix(index, "]") {
				return "", false
			}
			if _, err := strconv.Atoi(strings.TrimSuffix(index, "]")); err != nil {
				return "", false
			}
		}
		if name != "" && !isFieldName(name) {
			return "", false
		}
	}
	return "$." + trimmed, true
}

func isFieldName(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
