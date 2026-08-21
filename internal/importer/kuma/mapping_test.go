package kuma

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

func firstMonitor(t *testing.T, src *source) kumaMonitor {
	t.Helper()

	rows, err := src.query(context.Background(), "monitor", monitorColumns, "ORDER BY id")
	if err != nil {
		t.Fatalf("read monitors: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the fixture has no monitors")
	}
	return readMonitor(rows[0])
}

// The defect this whole reader exists for: Uptime Kuma's monitor table has
// grown a column almost every release, and a fixed SELECT naming one an older
// file does not have fails the entire import with a SQL error. The person on
// the other end of that error is migrating away from a tool they have used for
// two years.
func TestAnOlderSchemaMissingColumnsStillImports(t *testing.T) {
	t.Parallel()

	// A 1.18-shaped monitor table: no timeout, no parent, no packet_size, no
	// description, none of the gRPC columns.
	path := kumaDB(t)
	src := openTestSource(t, path)
	if _, err := src.db.Exec(`DROP TABLE monitor`); err == nil {
		t.Skip("the read-only handle accepted a write, so this test cannot set up")
	}
	_ = src.Close()

	old := kumaDB(t,
		`DROP TABLE monitor`,
		`CREATE TABLE monitor (
			id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(150), active BOOLEAN DEFAULT 1,
			type VARCHAR(20), url TEXT, hostname VARCHAR(255), port INTEGER,
			interval INTEGER DEFAULT 60, retry_interval INTEGER DEFAULT 60,
			maxretries INTEGER DEFAULT 0, upside_down BOOLEAN DEFAULT 0,
			keyword VARCHAR(255), accepted_statuscodes_json TEXT DEFAULT '["200-299"]')`,
		`INSERT INTO monitor (name, type, url, interval) VALUES ('Checkout', 'http', 'https://example.com/health', 60)`,
	)

	src = openTestSource(t, old)
	m := firstMonitor(t, src)
	if m.Name != "Checkout" {
		t.Fatalf("name = %q", m.Name)
	}
	// timeout is absent, so it comes from the interval rather than being zero —
	// which the schema would reject, for a value the user never set.
	if m.Timeout <= 0 || m.Timeout >= m.Interval {
		t.Errorf("timeout = %s against an interval of %s", m.Timeout, m.Interval)
	}

	converted, err := mapMonitor(m)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if converted.Type != model.TypeHTTP || converted.Config["url"] != "https://example.com/health" {
		t.Errorf("mapped to %+v", converted)
	}
}

// Data model §10 gap 1, resolved: a type this build cannot represent is
// recorded rather than approximated. Importing a MQTT monitor as a TCP check on
// port 1883 would be green while the broker rejects every publish, which is a
// monitoring tool lying.
func TestAnUnsupportedTypeIsNamedRatherThanApproximated(t *testing.T) {
	t.Parallel()

	src := openTestSource(t, kumaDB(t,
		`INSERT INTO monitor (name, type, hostname, port, interval) VALUES ('Broker', 'mqtt', 'mqtt.example.com', 1883, 60)`))

	_, err := mapMonitor(firstMonitor(t, src))
	if err == nil {
		t.Fatal("an mqtt monitor was mapped to something; nothing here checks an MQTT broker")
	}
	var unsupported *unsupportedType
	if !asUnsupported(err, &unsupported) {
		t.Fatalf("error %v is not an unsupported type", err)
	}
	if got := err.Error(); got == "" || !contains(got, "mqtt") || !contains(got, "MQTT broker") {
		t.Errorf("error %q does not name the type in terms the user would recognise", got)
	}
}

func asUnsupported(err error, target **unsupportedType) bool {
	u, ok := err.(*unsupportedType)
	if ok {
		*target = u
	}
	return ok
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// Kuma's keyword monitor inverted is our not_contains, and its match is always
// case-insensitive. Reproducing its behaviour matters more than looking
// stricter: a monitor that starts failing after a migration because the match
// got case-sensitive is a migration the user blames the new tool for.
func TestKeywordSemanticsAreReproducedRatherThanTightened(t *testing.T) {
	t.Parallel()

	src := openTestSource(t, kumaDB(t,
		`INSERT INTO monitor (name, type, url, keyword, invert_keyword, interval)
		 VALUES ('Checkout', 'keyword', 'https://example.com/', 'Service Unavailable', 1, 60)`))

	converted, err := mapMonitor(firstMonitor(t, src))
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	keyword, ok := converted.Config["keyword"].(map[string]any)
	if !ok {
		t.Fatalf("config = %+v, want a keyword assertion", converted.Config)
	}
	if keyword["mode"] != "not_contains" {
		t.Errorf("mode = %v, want not_contains for an inverted Kuma keyword", keyword["mode"])
	}
	if keyword["case_sensitive"] != false {
		t.Errorf("case_sensitive = %v; Kuma matches case-insensitively and offers no switch", keyword["case_sensitive"])
	}
}

// The JSON query subset. A jsonata expression this build cannot evaluate is not
// half-translated into something that quietly checks a different thing.
func TestJSONQueriesOutsideTheSubsetAreReportedRatherThanReinterpreted(t *testing.T) {
	t.Parallel()

	for expr, want := range map[string]string{
		"$.status":       "$.status",
		"status":         "$.status",
		"data.items[0]":  "$.data.items[0]",
		"$":              "$",
		"$count(items)":  "",
		"items[price>5]": "",
		"a & b":          "",
	} {
		got, ok := convertJSONPath(expr)
		if want == "" {
			if ok {
				t.Errorf("convertJSONPath(%q) = %q, want a refusal", expr, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("convertJSONPath(%q) = (%q, %v), want %q", expr, got, ok, want)
		}
	}

	src := openTestSource(t, kumaDB(t,
		`INSERT INTO monitor (name, type, url, json_path, expected_value, interval)
		 VALUES ('API', 'json-query', 'https://example.com/', '$count(items)', '3', 60)`))
	converted, err := mapMonitor(firstMonitor(t, src))
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if _, present := converted.Config["json_path"]; present {
		t.Error("an untranslatable jsonata expression was imported as a json_path assertion")
	}
	if len(converted.Notes) == 0 {
		t.Error("the dropped assertion was not recorded, so the report would not mention it")
	}
}

// The subset that does translate has to produce an assertion the checker
// accepts, or the monitor is written and then refused at assignment time with
// only the log to explain it.
func TestATranslatedJSONQueryMatchesTheCheckersSchema(t *testing.T) {
	t.Parallel()

	src := openTestSource(t, kumaDB(t,
		`INSERT INTO monitor (name, type, url, json_path, expected_value, interval)
		 VALUES ('API', 'json-query', 'https://example.com/', '$.status', 'ok', 60)`))
	converted, err := mapMonitor(firstMonitor(t, src))
	if err != nil {
		t.Fatalf("map: %v", err)
	}

	raw, ok := converted.Config["json_path"].(json.RawMessage)
	if !ok {
		t.Fatalf("json_path = %v", converted.Config["json_path"])
	}
	var assertion map[string]any
	if err := json.Unmarshal(raw, &assertion); err != nil {
		t.Fatalf("decode assertion: %v", err)
	}
	if assertion["path"] != "$.status" || assertion["operator"] != "eq" || assertion["expected"] != "ok" {
		t.Errorf("assertion = %v; the checker's schema is {path, operator, expected} with DisallowUnknownFields", assertion)
	}
}

// Data model §10 gap 3, resolved by saying so. A proxied monitor imports as the
// check it is, without the proxy, and the report says the check will now be made
// from this host — which is materially different from the monitor quietly going
// red.
func TestAProxiedMonitorSaysWhatItLost(t *testing.T) {
	t.Parallel()

	src := openTestSource(t, kumaDB(t,
		`INSERT INTO monitor (name, type, url, proxy_id, interval)
		 VALUES ('Behind a proxy', 'http', 'https://internal.example.com/', 4, 60)`))

	converted, err := mapMonitor(firstMonitor(t, src))
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	found := false
	for _, note := range converted.Notes {
		if contains(note, "proxy") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want the proxy named", converted.Notes)
	}
}

// A push monitor's token is deliberately not carried over: two installs
// answering to one dead-man's-switch token is a switch that reports up when the
// job has stopped running.
func TestAPushMonitorGetsANewTokenAndSaysSo(t *testing.T) {
	t.Parallel()

	src := openTestSource(t, kumaDB(t,
		`INSERT INTO monitor (name, type, push_token, interval) VALUES ('Nightly backup', 'push', 'abc123', 60)`))

	converted, err := mapMonitor(firstMonitor(t, src))
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if converted.Type != model.TypePush {
		t.Fatalf("type = %q", converted.Type)
	}
	if _, carried := converted.Config["push_token"]; carried {
		t.Error("the Uptime Kuma push token was carried over; two systems could then satisfy one dead-man's switch")
	}
	if len(converted.Notes) == 0 {
		t.Error("the new push URL was not mentioned, so nobody would repoint the job")
	}
}

// An interval below this build's floor is raised rather than refused, because a
// monitor the schema would reject is a monitor that does not migrate.
func TestAnIntervalBelowTheFloorIsRaised(t *testing.T) {
	t.Parallel()

	src := openTestSource(t, kumaDB(t,
		`INSERT INTO monitor (name, type, url, interval) VALUES ('Fast', 'http', 'https://example.com/', 5)`))

	m := firstMonitor(t, src)
	if m.Interval != model.MinInterval {
		t.Errorf("interval = %s, want it raised to the %s floor", m.Interval, model.MinInterval)
	}
	if m.Timeout >= m.Interval {
		t.Errorf("timeout %s is not below the interval %s, which the schema requires", m.Timeout, m.Interval)
	}
}
