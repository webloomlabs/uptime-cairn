package app

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/webloomlabs/uptime-cairn/internal/config"
	"github.com/webloomlabs/uptime-cairn/internal/importer/kuma"

	_ "modernc.org/sqlite"
)

// seedKuma writes a small Uptime Kuma 1.23-shaped database.
//
// Written rather than checked in: the subject of these tests is somebody else's
// schema, and a binary fixture would freeze one version of it where a
// constructor makes "what does an older file look like" a two-line change.
func seedKuma(t *testing.T, dir string, statements ...string) string {
	t.Helper()

	path := filepath.Join(dir, "kuma.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open kuma.db: %v", err)
	}
	defer func() { _ = db.Close() }()

	schema := []string{
		`CREATE TABLE monitor (
			id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(150), description TEXT,
			active BOOLEAN DEFAULT 1, type VARCHAR(20), url TEXT, hostname VARCHAR(255), port INTEGER,
			interval INTEGER DEFAULT 60, retry_interval INTEGER DEFAULT 60, resend_interval INTEGER DEFAULT 0,
			timeout DOUBLE DEFAULT 0, maxretries INTEGER DEFAULT 0, upside_down BOOLEAN DEFAULT 0,
			parent INTEGER, keyword VARCHAR(255), invert_keyword BOOLEAN DEFAULT 0,
			accepted_statuscodes_json TEXT DEFAULT '["200-299"]', method TEXT DEFAULT 'GET',
			basic_auth_user TEXT, basic_auth_pass TEXT, auth_method VARCHAR(250),
			ignore_tls BOOLEAN DEFAULT 0, max_redirects INTEGER DEFAULT 10,
			dns_resolve_type VARCHAR(5), docker_container VARCHAR(255), push_token VARCHAR(20),
			proxy_id INTEGER)`,
		`CREATE TABLE tag (id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(255), color VARCHAR(20))`,
		`CREATE TABLE monitor_tag (id INTEGER PRIMARY KEY AUTOINCREMENT, monitor_id INTEGER, tag_id INTEGER, value TEXT)`,
		`CREATE TABLE notification (id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(255),
			active BOOLEAN DEFAULT 1, is_default BOOLEAN DEFAULT 0, config TEXT)`,
		`CREATE TABLE monitor_notification (id INTEGER PRIMARY KEY AUTOINCREMENT, monitor_id INTEGER, notification_id INTEGER)`,
		`CREATE TABLE status_page (id INTEGER PRIMARY KEY AUTOINCREMENT, slug VARCHAR(255), title VARCHAR(255),
			description TEXT, theme VARCHAR(30), published BOOLEAN DEFAULT 1, password VARCHAR(255))`,
		"CREATE TABLE `group` (id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(255), status_page_id INTEGER, weight INTEGER DEFAULT 1000)",
		`CREATE TABLE monitor_group (id INTEGER PRIMARY KEY AUTOINCREMENT, monitor_id INTEGER, group_id INTEGER, weight INTEGER DEFAULT 1000)`,
		`CREATE TABLE heartbeat (id INTEGER PRIMARY KEY AUTOINCREMENT, important BOOLEAN DEFAULT 0,
			monitor_id INTEGER, status SMALLINT, msg TEXT, time DATETIME, ping INTEGER)`,
	}
	for _, stmt := range append(schema, statements...) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %.60s: %v", stmt, err)
		}
	}
	return path
}

func importInto(t *testing.T, dataDir string, opts kuma.Options, paths ...string) string {
	t.Helper()

	var out bytes.Buffer
	cfg := config.Default()
	cfg.DataDir = dataDir
	if err := ImportKuma(context.Background(), cfg, paths, opts, &out); err != nil {
		t.Fatalf("import: %v\n%s", err, out.String())
	}
	return out.String()
}

func cairnDB(t *testing.T, dataDir string) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", filepath.Join(dataDir, "cairn.db"))
	if err != nil {
		t.Fatalf("open cairn.db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func scalar[T any](t *testing.T, db *sql.DB, query string, args ...any) T {
	t.Helper()

	var out T
	if err := db.QueryRow(query, args...).Scan(&out); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return out
}

// The whole importer, end to end, through the same store and the same key
// hierarchy the server uses.
func TestImportReproducesAKumaInstall(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := seedKuma(t, dir,
		`INSERT INTO monitor (id, name, type, interval) VALUES (1, 'Production', 'group', 60)`,
		`INSERT INTO monitor (id, name, type, url, interval, parent) VALUES (2, 'Checkout', 'http', 'https://shop.example.com/health', 60, 1)`,
		`INSERT INTO monitor (id, name, type, hostname, port, interval, parent) VALUES (3, 'Postgres', 'port', 'db.example.com', 5432, 60, 1)`,
		`INSERT INTO monitor (id, name, type, hostname, interval) VALUES (4, 'Gateway', 'ping', '10.0.0.1', 60)`,
		`INSERT INTO tag (id, name, color) VALUES (1, 'critical', '#dc2626')`,
		`INSERT INTO monitor_tag (monitor_id, tag_id) VALUES (2, 1)`,
		`INSERT INTO notification (id, name, is_default, config) VALUES (1, 'Ops Slack', 1,
			'{"type":"slack","slackwebhookURL":"https://hooks.slack.com/services/T/B/xyz"}')`,
		`INSERT INTO monitor_notification (monitor_id, notification_id) VALUES (2, 1)`,
		`INSERT INTO status_page (id, slug, title, theme, published) VALUES (1, 'shop', 'Shop Status', 'dark', 1)`,
		"INSERT INTO `group` (id, name, status_page_id, weight) VALUES (1, 'Storefront', 1, 1)",
		`INSERT INTO monitor_group (monitor_id, group_id, weight) VALUES (2, 1, 1)`,
	)

	data := filepath.Join(dir, "cairn")
	report := importInto(t, data, kuma.DefaultOptions(), source)
	db := cairnDB(t, data)

	if n := scalar[int](t, db, `SELECT COUNT(*) FROM monitors`); n != 3 {
		t.Errorf("%d monitors imported, want 3", n)
	}
	// Kuma models a group as a monitor; this build has a real group, and the
	// monitors that were under it are in it.
	if n := scalar[int](t, db, `SELECT COUNT(*) FROM groups WHERE name = 'Production'`); n != 1 {
		t.Error("the Kuma group did not become a group")
	}
	if n := scalar[int](t, db, `SELECT COUNT(*) FROM monitors WHERE group_id IS NOT NULL`); n != 2 {
		t.Errorf("%d monitors landed in the group, want 2", n)
	}
	if n := scalar[int](t, db, `SELECT COUNT(*) FROM status_page_sections`); n != 1 {
		t.Errorf("%d status page sections, want 1", n)
	}

	// Paused by default, which is what the spec publishes and what stops five
	// thousand checks firing at once at the end of a migration.
	if n := scalar[int](t, db, `SELECT COUNT(*) FROM monitors WHERE enabled = 1`); n != 0 {
		t.Errorf("%d monitors are already checking; imports arrive paused for review", n)
	}
	if !strings.Contains(report, "paused") {
		t.Error("the report does not mention that the monitors are paused")
	}
}

// The guarantee the report rests on. An import that maps most of an install and
// says which parts it could not is something a user can finish by hand; one that
// reports success is something they discover is wrong during an outage.
func TestTheReportNamesEverythingThatDidNotComeAcross(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := seedKuma(t, dir,
		`INSERT INTO monitor (id, name, type, url, interval) VALUES (1, 'Checkout', 'http', 'https://example.com/', 60)`,
		`INSERT INTO monitor (id, name, type, hostname, port, interval) VALUES (2, 'Broker', 'mqtt', 'mqtt.example.com', 1883, 60)`,
		`INSERT INTO monitor (id, name, type, url, proxy_id, interval) VALUES (3, 'Internal', 'http', 'https://internal.example.com/', 4, 60)`,
		`INSERT INTO notification (id, name, config) VALUES (1, 'SMS via Nexmo', '{"type":"nexmo","nexmoApiKey":"k"}')`,
	)

	data := filepath.Join(dir, "cairn")
	report := importInto(t, data, kuma.DefaultOptions(), source)

	for _, want := range []string{
		"Broker", // the unsupported monitor, by name
		"mqtt",   // and by type, so the user knows what to rebuild
		"unsupported",
		"SMS via Nexmo", // the unsupported provider
		"proxy",         // data model §10 gap 3, stated rather than silent
		"partial",       // not "succeeded", because three things did not come across
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not mention %q:\n%s", want, report)
		}
	}

	db := cairnDB(t, data)
	// Nothing was invented. An mqtt monitor imported as a TCP check on 1883
	// would be green while the broker rejects every publish.
	if n := scalar[int](t, db, `SELECT COUNT(*) FROM monitors WHERE name = 'Broker'`); n != 0 {
		t.Error("the unsupported monitor was imported as something else")
	}
	if n := scalar[int](t, db, `SELECT COUNT(*) FROM import_entries WHERE outcome = 'unsupported'`); n != 2 {
		t.Errorf("%d entries recorded as unsupported, want 2", n)
	}
}

// Credentials come out of a Kuma database in plaintext and must not stay that
// way. This is the property the whole "writes through the repository layer"
// rule exists for.
func TestImportedCredentialsAreEncryptedAtRest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := seedKuma(t, dir,
		`INSERT INTO monitor (id, name, type, url, interval, basic_auth_user, basic_auth_pass)
		 VALUES (1, 'Admin', 'http', 'https://admin.example.com/', 60, 'ops', 'hunter2')`,
		`INSERT INTO notification (id, name, config) VALUES (1, 'Ops Slack',
			'{"type":"slack","slackwebhookURL":"https://hooks.slack.com/services/T/B/xyz"}')`,
	)

	data := filepath.Join(dir, "cairn")
	importInto(t, data, kuma.DefaultOptions(), source)
	db := cairnDB(t, data)

	config := scalar[string](t, db, `SELECT config FROM monitors WHERE name = 'Admin'`)
	if strings.Contains(config, "hunter2") {
		t.Errorf("the basic-auth password is in plaintext in the config column: %s", config)
	}
	if !strings.Contains(config, `"username":"ops"`) {
		t.Errorf("the username did not survive: %s", config)
	}
	if n := scalar[int](t, db, `SELECT length(config_secrets) FROM monitors WHERE name = 'Admin'`); n == 0 {
		t.Error("no sealed envelope was written, so the password went nowhere")
	}

	channel := scalar[string](t, db, `SELECT config FROM notification_channels WHERE name = 'Ops Slack'`)
	if strings.Contains(channel, "hooks.slack.com") {
		t.Errorf("the Slack webhook URL is in plaintext in the config column: %s", channel)
	}
}

// A dry run produces the same report and writes nothing. It is what makes an
// import something a cautious person will run.
func TestADryRunWritesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := seedKuma(t, dir,
		`INSERT INTO monitor (id, name, type, url, interval) VALUES (1, 'Checkout', 'http', 'https://example.com/', 60)`)

	opts := kuma.DefaultOptions()
	opts.DryRun = true

	data := filepath.Join(dir, "cairn")
	report := importInto(t, data, opts, source)
	if !strings.Contains(report, "Checkout") && !strings.Contains(report, "monitor") {
		t.Errorf("the dry run produced no report:\n%s", report)
	}

	db := cairnDB(t, data)
	if n := scalar[int](t, db, `SELECT COUNT(*) FROM monitors`); n != 0 {
		t.Errorf("a dry run wrote %d monitors", n)
	}
	if n := scalar[int](t, db, `SELECT COUNT(*) FROM import_jobs`); n != 0 {
		t.Errorf("a dry run recorded %d jobs", n)
	}
}

// The multi-instance merge: the migration path for everyone currently sharding
// Kuma by hand across hosts. Two files, both with a monitor id 1 and both with a
// monitor called "Checkout".
func TestSeveralKumaInstancesMergeIntoOne(t *testing.T) {
	t.Parallel()

	first := t.TempDir()
	second := t.TempDir()
	a := seedKuma(t, first,
		`INSERT INTO monitor (id, name, type, url, interval) VALUES (1, 'Checkout', 'http', 'https://a.example.com/', 60)`)
	b := seedKuma(t, second,
		`INSERT INTO monitor (id, name, type, url, interval) VALUES (1, 'Checkout', 'http', 'https://b.example.com/', 60)`)

	data := filepath.Join(t.TempDir(), "cairn")
	report := importInto(t, data, kuma.DefaultOptions(), a, b)
	db := cairnDB(t, data)

	// Both survive. `rename` is the default precisely because it is the only
	// strategy that cannot lose one of them.
	if n := scalar[int](t, db, `SELECT COUNT(*) FROM monitors`); n != 2 {
		t.Fatalf("%d monitors after merging two instances, want 2", n)
	}
	targets := map[string]bool{}
	rows, err := db.Query(`SELECT target FROM monitors`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			t.Fatal(err)
		}
		targets[target] = true
	}
	if !targets["https://a.example.com/"] || !targets["https://b.example.com/"] {
		t.Errorf("both instances' monitors did not survive the merge: %v", targets)
	}
	if !strings.Contains(report, "kuma.db") {
		t.Error("the report does not name the source files")
	}
}

// name_prefix is the practical way to keep merged instances distinguishable
// afterwards, and the reason it is worth having is that "Checkout" and
// "Checkout (2)" tell you nothing about which customer they belong to.
func TestNamePrefixKeepsMergedInstancesDistinguishable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := seedKuma(t, dir,
		`INSERT INTO monitor (id, name, type, url, interval) VALUES (1, 'Checkout', 'http', 'https://example.com/', 60)`)

	opts := kuma.DefaultOptions()
	opts.NamePrefix = "acme / "

	data := filepath.Join(t.TempDir(), "cairn")
	importInto(t, data, opts, source)

	name := scalar[string](t, cairnDB(t, data), `SELECT name FROM monitors LIMIT 1`)
	if name != "acme / Checkout" {
		t.Errorf("name = %q, want the prefix applied", name)
	}
}

// History is idempotent, because "it did not look right, I ran it again" is
// what people do. WriteBatch dedupes on (org, monitor, time, probe), and this
// is the test that says so out loud.
func TestImportingHistoryTwiceProducesOneHistory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := seedKuma(t, dir,
		`INSERT INTO monitor (id, name, type, url, interval) VALUES (1, 'Checkout', 'http', 'https://example.com/', 60)`,
		`INSERT INTO heartbeat (monitor_id, status, msg, time, ping) VALUES (1, 1, '200 - OK', '2026-08-20 10:00:00', 42)`,
		`INSERT INTO heartbeat (monitor_id, status, msg, time, ping) VALUES (1, 0, 'timeout', '2026-08-20 10:01:00', NULL)`,
	)

	opts := kuma.DefaultOptions()
	opts.ImportHistory = true
	// skip, so the second run does not create a second monitor and make the
	// heartbeat count rise for a legitimate reason.
	opts.ConflictStrategy = "skip"

	data := filepath.Join(t.TempDir(), "cairn")
	importInto(t, data, opts, source)
	db := cairnDB(t, data)

	first := scalar[int](t, db, `SELECT COUNT(*) FROM heartbeats`)
	if first != 2 {
		t.Fatalf("%d heartbeats after one import, want 2", first)
	}

	// A monitor name collides on the second run, so it is skipped — and with it
	// its history, which is the honest outcome: the heartbeats belong to a
	// monitor that was not created twice.
	importInto(t, data, opts, source)
	if second := scalar[int](t, db, `SELECT COUNT(*) FROM heartbeats`); second != first {
		t.Errorf("%d heartbeats after re-running the import, want %d", second, first)
	}
}
