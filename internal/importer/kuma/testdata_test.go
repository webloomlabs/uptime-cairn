package kuma

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// A synthetic kuma.db.
//
// Written rather than checked in as a fixture, because what these tests are
// about is the shape of somebody else's schema and a fixture would freeze one
// version of it. The columns below are Uptime Kuma 1.23's, minus the forty this
// importer never reads — and the tests that matter most are the ones that leave
// columns *out*, because that is what an older file looks like.
func kumaDB(t *testing.T, statements ...string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kuma.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	schema := []string{
		`CREATE TABLE monitor (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name VARCHAR(150), description TEXT, active BOOLEAN DEFAULT 1,
			type VARCHAR(20), url TEXT, hostname VARCHAR(255), port INTEGER,
			interval INTEGER DEFAULT 60, retry_interval INTEGER DEFAULT 60,
			resend_interval INTEGER DEFAULT 0, timeout DOUBLE DEFAULT 0,
			maxretries INTEGER DEFAULT 0, upside_down BOOLEAN DEFAULT 0,
			parent INTEGER, keyword VARCHAR(255), invert_keyword BOOLEAN DEFAULT 0,
			json_path TEXT, expected_value VARCHAR(255),
			accepted_statuscodes_json TEXT DEFAULT '["200-299"]',
			method TEXT DEFAULT 'GET', body TEXT, headers TEXT,
			basic_auth_user TEXT, basic_auth_pass TEXT, auth_method VARCHAR(250),
			authorization_header TEXT,
			ignore_tls BOOLEAN DEFAULT 0, max_redirects INTEGER DEFAULT 10,
			dns_resolve_type VARCHAR(5), dns_resolve_server VARCHAR(255), port_dns INTEGER,
			docker_container VARCHAR(255), docker_host INTEGER,
			push_token VARCHAR(20), packet_size INTEGER DEFAULT 56,
			grpc_url VARCHAR(255), grpc_service_name VARCHAR(255), grpc_enable_tls BOOLEAN DEFAULT 0,
			proxy_id INTEGER, expiry_notification BOOLEAN DEFAULT 1
		)`,
		`CREATE TABLE tag (id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(255), color VARCHAR(20))`,
		`CREATE TABLE monitor_tag (id INTEGER PRIMARY KEY AUTOINCREMENT, monitor_id INTEGER, tag_id INTEGER, value TEXT)`,
		`CREATE TABLE notification (id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(255), active BOOLEAN DEFAULT 1, is_default BOOLEAN DEFAULT 0, config TEXT)`,
		`CREATE TABLE monitor_notification (id INTEGER PRIMARY KEY AUTOINCREMENT, monitor_id INTEGER, notification_id INTEGER)`,
		`CREATE TABLE status_page (id INTEGER PRIMARY KEY AUTOINCREMENT, slug VARCHAR(255), title VARCHAR(255),
			description TEXT, theme VARCHAR(30), published BOOLEAN DEFAULT 1, footer_text TEXT,
			custom_css TEXT, show_powered_by BOOLEAN DEFAULT 1, google_analytics_tag_id VARCHAR(255),
			show_tags BOOLEAN DEFAULT 0, password VARCHAR(255))`,
		"CREATE TABLE `group` (id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(255), status_page_id INTEGER, weight INTEGER DEFAULT 1000, public BOOLEAN DEFAULT 1)",
		`CREATE TABLE monitor_group (id INTEGER PRIMARY KEY AUTOINCREMENT, monitor_id INTEGER, group_id INTEGER, weight INTEGER DEFAULT 1000)`,
		`CREATE TABLE heartbeat (id INTEGER PRIMARY KEY AUTOINCREMENT, important BOOLEAN DEFAULT 0,
			monitor_id INTEGER, status SMALLINT, msg TEXT, time DATETIME, ping INTEGER, duration INTEGER DEFAULT 0)`,
		`CREATE TABLE setting (id INTEGER PRIMARY KEY AUTOINCREMENT, key VARCHAR(200), value TEXT, type VARCHAR(20))`,
		`CREATE TABLE incident (id INTEGER PRIMARY KEY AUTOINCREMENT, title VARCHAR(255), content TEXT,
			style VARCHAR(30), pin BOOLEAN DEFAULT 1, active BOOLEAN DEFAULT 1, status_page_id INTEGER)`,
		`CREATE TABLE maintenance (id INTEGER PRIMARY KEY AUTOINCREMENT, title VARCHAR(150), description TEXT,
			active BOOLEAN DEFAULT 1, strategy VARCHAR(50))`,
	}
	for _, stmt := range append(schema, statements...) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %.60s...: %v", stmt, err)
		}
	}
	return path
}

// openTestSource is the reader under test, over a file this package wrote.
func openTestSource(t *testing.T, path string) *source {
	t.Helper()

	src, err := openSource(context.Background(), path, "kuma.db")
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src
}
