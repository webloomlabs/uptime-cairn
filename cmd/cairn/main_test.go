package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/webloomlabs/uptime-cairn/internal/model"

	_ "modernc.org/sqlite"
)

func TestVersionSubcommandMatchesFlag(t *testing.T) {
	var commandOut, flagOut bytes.Buffer
	if err := run([]string{"version"}, &commandOut, &bytes.Buffer{}); err != nil {
		t.Fatalf("version subcommand: %v", err)
	}
	if err := run([]string{"-version"}, &flagOut, &bytes.Buffer{}); err != nil {
		t.Fatalf("-version flag: %v", err)
	}
	if commandOut.String() != flagOut.String() {
		t.Fatalf("version output differs:\nsubcommand: %q\nflag: %q", commandOut.String(), flagOut.String())
	}
}

func TestRootHelpListsSubcommands(t *testing.T) {
	var stderr bytes.Buffer
	if err := run([]string{"-h"}, &bytes.Buffer{}, &stderr); err != nil {
		t.Fatalf("help: %v", err)
	}
	for _, command := range []string{"import", "config", "version"} {
		if !strings.Contains(stderr.String(), command) {
			t.Fatalf("help does not list %q:\n%s", command, stderr.String())
		}
	}
}

func TestConfigValidateValidDefault(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"config", "validate"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("expected valid default config, got error: %v", err)
	}
	if !strings.Contains(stdout.String(), "configuration valid") {
		t.Errorf("expected 'configuration valid' in stdout, got: %q", stdout.String())
	}
}

func TestConfigValidateValidCustomFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"config", "validate",
		"-mode=solo",
		"-data-dir=/tmp/test-cairn",
		"-listen=:9090",
		"-instance-name=My Instance",
		"-base-url=https://status.example.com",
		"-trusted-proxy=10.0.0.1,192.168.1.0/24",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("expected valid custom config, got error: %v", err)
	}
	if !strings.Contains(stdout.String(), "configuration valid") {
		t.Errorf("expected 'configuration valid' in stdout, got: %q", stdout.String())
	}
}

func TestConfigValidateInvalidFields(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantErrMsg string
	}{
		{
			name:       "empty listen address",
			args:       []string{"config", "validate", "-listen="},
			wantErrMsg: "--listen must not be empty",
		},
		{
			name:       "empty data dir",
			args:       []string{"config", "validate", "-data-dir="},
			wantErrMsg: "--data-dir must not be empty",
		},
		{
			name:       "unknown mode",
			args:       []string{"config", "validate", "-mode=invalid"},
			wantErrMsg: "unknown --mode \"invalid\"",
		},
		{
			name:       "probe mode (Phase 4 not built)",
			args:       []string{"config", "validate", "-mode=probe"},
			wantErrMsg: "--mode=probe is Phase 4 work",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(tt.args, &stdout, &stderr)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErrMsg)
			}
			if !strings.Contains(err.Error(), tt.wantErrMsg) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErrMsg)
			}
		})
	}
}

func TestConfigValidateUnexpectedArgument(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"config", "validate", "unexpected"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for unexpected argument, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("error = %q, want it to mention unexpected argument", err.Error())
	}
}

func TestConfigUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"config", "unknown"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for unknown config subcommand, got nil")
	}
	if !strings.Contains(stderr.String(), "usage: cairn config validate") {
		t.Errorf("expected usage in stderr, got: %q", stderr.String())
	}
}

func TestConfigHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"config", "validate", "-h"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("expected -h to succeed, got: %v", err)
	}
	if !strings.Contains(stderr.String(), "usage: cairn config validate") {
		t.Errorf("expected usage in stderr, got: %q", stderr.String())
	}
}

func seedTestKuma(t *testing.T, dir string) string {
	t.Helper()

	path := filepath.Join(dir, "kuma.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open kuma.db: %v", err)
	}
	defer func() { _ = db.Close() }()

	schema := `CREATE TABLE monitor (
		id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(150), description TEXT,
		active BOOLEAN DEFAULT 1, type VARCHAR(20), url TEXT, hostname VARCHAR(255), port INTEGER,
		interval INTEGER DEFAULT 60, retry_interval INTEGER DEFAULT 60, resend_interval INTEGER DEFAULT 0,
		timeout DOUBLE DEFAULT 0, maxretries INTEGER DEFAULT 0, upside_down BOOLEAN DEFAULT 0,
		parent INTEGER, keyword VARCHAR(255), invert_keyword BOOLEAN DEFAULT 0,
		accepted_statuscodes_json TEXT DEFAULT '["200-299"]', method TEXT DEFAULT 'GET',
		basic_auth_user TEXT, basic_auth_pass TEXT, auth_method VARCHAR(250),
		ignore_tls BOOLEAN DEFAULT 0, max_redirects INTEGER DEFAULT 10,
		dns_resolve_type VARCHAR(5), docker_container VARCHAR(255), push_token VARCHAR(20),
		proxy_id INTEGER);
	CREATE TABLE tag (id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(255), color VARCHAR(20));
	CREATE TABLE monitor_tag (id INTEGER PRIMARY KEY AUTOINCREMENT, monitor_id INTEGER, tag_id INTEGER, value TEXT);
	CREATE TABLE notification (id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(255),
		active BOOLEAN DEFAULT 1, is_default BOOLEAN DEFAULT 0, config TEXT);
	CREATE TABLE monitor_notification (id INTEGER PRIMARY KEY AUTOINCREMENT, monitor_id INTEGER, notification_id INTEGER);
	CREATE TABLE status_page (id INTEGER PRIMARY KEY AUTOINCREMENT, slug VARCHAR(255), title VARCHAR(255),
		description TEXT, theme VARCHAR(30), published BOOLEAN DEFAULT 1, password VARCHAR(255));
	CREATE TABLE ` + "`group`" + ` (id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(255), status_page_id INTEGER, weight INTEGER DEFAULT 1000);
	CREATE TABLE monitor_group (id INTEGER PRIMARY KEY AUTOINCREMENT, monitor_id INTEGER, group_id INTEGER, weight INTEGER DEFAULT 1000);
	CREATE TABLE heartbeat (id INTEGER PRIMARY KEY AUTOINCREMENT, important BOOLEAN DEFAULT 0,
		monitor_id INTEGER, status SMALLINT, msg TEXT, time DATETIME, ping INTEGER);
	INSERT INTO monitor (id, name, type, url, interval) VALUES (1, 'Prod Service', 'http', 'https://example.com/health', 60);`

	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("seed test kuma: %v", err)
	}
	return path
}

func TestImportKumaReportJSONStdout(t *testing.T) {
	kumaDB := seedTestKuma(t, t.TempDir())
	dataDir := filepath.Join(t.TempDir(), "cairn-data")

	var stdout, stderr bytes.Buffer
	err := run([]string{
		"import", "kuma",
		"-data-dir=" + dataDir,
		"-dry-run",
		"-report-json=-",
		kumaDB,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run import kuma -report-json -: %v (stderr: %s)", err, stderr.String())
	}

	outStr := stdout.String()
	if strings.Contains(outStr, "Dry run — nothing was written.") {
		t.Errorf("expected text table to be omitted on stdout, got: %s", outStr)
	}

	var report model.ImportReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal json report from stdout: %v\noutput: %s", err, outStr)
	}

	if !report.DryRun {
		t.Errorf("report.DryRun = false, want true")
	}
	if report.State != model.ImportSucceeded {
		t.Errorf("report.State = %q, want %q", report.State, model.ImportSucceeded)
	}
	if report.Summary["monitor"].Imported != 1 {
		t.Errorf("summary.Imported = %d, want 1", report.Summary["monitor"].Imported)
	}
}

func TestImportKumaReportJSONFile(t *testing.T) {
	kumaDB := seedTestKuma(t, t.TempDir())
	dataDir := filepath.Join(t.TempDir(), "cairn-data")
	jsonPath := filepath.Join(t.TempDir(), "report.json")

	var stdout, stderr bytes.Buffer
	err := run([]string{
		"import", "kuma",
		"-data-dir=" + dataDir,
		"-dry-run",
		"-report-json=" + jsonPath,
		kumaDB,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run import kuma -report-json <file>: %v (stderr: %s)", err, stderr.String())
	}

	outStr := stdout.String()
	if !strings.Contains(outStr, "Dry run — nothing was written.") {
		t.Errorf("expected stdout to contain human table, got: %s", outStr)
	}

	dataBytes, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read report json: %v", err)
	}

	var report model.ImportReport
	if err := json.Unmarshal(dataBytes, &report); err != nil {
		t.Fatalf("unmarshal json report from file: %v", err)
	}

	if !report.DryRun {
		t.Errorf("report.DryRun = false, want true")
	}
	if report.State != model.ImportSucceeded {
		t.Errorf("report.State = %q, want %q", report.State, model.ImportSucceeded)
	}
	if report.Summary["monitor"].Imported != 1 {
		t.Errorf("summary.Imported = %d, want 1", report.Summary["monitor"].Imported)
	}
}
