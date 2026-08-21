package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// kumaUpload writes a small Uptime Kuma database and returns its bytes, ready
// to be posted.
func kumaUpload(t *testing.T, statements ...string) []byte {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kuma.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	schema := []string{
		`CREATE TABLE monitor (
			id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(150), active BOOLEAN DEFAULT 1,
			type VARCHAR(20), url TEXT, hostname VARCHAR(255), port INTEGER,
			interval INTEGER DEFAULT 60, retry_interval INTEGER DEFAULT 60, timeout DOUBLE DEFAULT 0,
			maxretries INTEGER DEFAULT 0, upside_down BOOLEAN DEFAULT 0, parent INTEGER,
			keyword VARCHAR(255), accepted_statuscodes_json TEXT DEFAULT '["200-299"]')`,
		`CREATE TABLE tag (id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(255), color VARCHAR(20))`,
		`CREATE TABLE monitor_tag (id INTEGER PRIMARY KEY AUTOINCREMENT, monitor_id INTEGER, tag_id INTEGER, value TEXT)`,
	}
	for _, stmt := range append(schema, statements...) {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			t.Fatalf("seed: %v", err)
		}
	}
	_ = db.Close()

	raw, err := readFile(path)
	if err != nil {
		t.Fatalf("read kuma.db: %v", err)
	}
	return raw
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// postImport uploads databases and returns the accepted job.
func postImport(t *testing.T, c *client, options map[string]any, databases ...[]byte) map[string]any {
	t.Helper()

	var body bytes.Buffer
	form := multipart.NewWriter(&body)

	if options != nil {
		encoded, err := json.Marshal(options)
		if err != nil {
			t.Fatal(err)
		}
		part, err := form.CreateFormField("options")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(encoded); err != nil {
			t.Fatal(err)
		}
	}
	for i, database := range databases {
		part, err := form.CreateFormFile("files", "kuma-"+string(rune('a'+i))+".db")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(database); err != nil {
			t.Fatal(err)
		}
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		c.base+"/api/v1/imports/kuma", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	c.authorise(req)

	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	var job map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &job)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("import = %d, want 202 (%s)", resp.StatusCode, raw)
	}
	return job
}

// awaitJob polls until the import finishes.
func awaitJob(t *testing.T, c *client, id string) map[string]any {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, job := c.do(http.MethodGet, "/api/v1/imports/"+id, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get import job = %d (%v)", resp.StatusCode, job)
		}
		switch job["state"] {
		case "succeeded", "partial", "failed":
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the import did not finish within the deadline")
	return nil
}

// The guided flow, end to end. It runs asynchronously because an import of a
// Kuma install with a year of history is minutes of work, and a request held
// open for minutes dies at the first proxy between the browser and here.
func TestTheGuidedImportRunsAndReportsWhatItDid(t *testing.T) {
	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	database := kumaUpload(t,
		`INSERT INTO monitor (id, name, type, url, interval) VALUES (1, 'Checkout', 'http', 'https://shop.example.com/', 60)`,
		`INSERT INTO monitor (id, name, type, hostname, port, interval) VALUES (2, 'Broker', 'mqtt', 'mqtt.example.com', 1883, 60)`,
		`INSERT INTO tag (id, name, color) VALUES (1, 'critical', '#dc2626')`,
		`INSERT INTO monitor_tag (monitor_id, tag_id) VALUES (1, 1)`,
	)

	accepted := postImport(t, c, nil, database)
	if accepted["state"] != "queued" {
		t.Errorf("state = %v, want queued", accepted["state"])
	}

	job := awaitJob(t, c, accepted["id"].(string))

	// Partial, not succeeded: the mqtt monitor did not come across, and
	// "succeeded" has to mean this install now monitors what the old one did.
	if job["state"] != "partial" {
		t.Errorf("state = %v, want partial (%v)", job["state"], job["summary"])
	}

	// Every source entity appears exactly once. That is the guarantee.
	entries := job["entries"].([]any)
	seen := map[string]string{}
	for _, entry := range entries {
		e := entry.(map[string]any)
		key := e["entity_type"].(string) + "/" + e["source_name"].(string)
		if previous, twice := seen[key]; twice {
			t.Errorf("%s appears twice in the report (%s and %s)", key, previous, e["result"])
		}
		seen[key] = e["result"].(string)
	}
	if seen["monitor/Checkout"] != "imported" {
		t.Errorf("Checkout = %q", seen["monitor/Checkout"])
	}
	if seen["monitor/Broker"] != "unsupported" {
		t.Errorf("Broker = %q, want unsupported", seen["monitor/Broker"])
	}
	if seen["tag/critical"] != "imported" {
		t.Errorf("critical = %q", seen["tag/critical"])
	}

	// The sources census: what the file held, before anything was written.
	sources := job["sources"].([]any)
	if len(sources) != 1 {
		t.Fatalf("%d sources, want 1", len(sources))
	}

	// And the monitors are actually here, through the ordinary API.
	_, monitors := c.do(http.MethodGet, "/api/v1/monitors", nil)
	rows := monitors["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("%d monitors after the import, want 1", len(rows))
	}
	if rows[0].(map[string]any)["enabled"] != false {
		t.Error("the imported monitor is already checking; imports arrive paused for review")
	}
}

// A dry run reports and writes nothing, which is what makes an import something
// a cautious person will run at all.
func TestAGuidedDryRunWritesNothing(t *testing.T) {
	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	database := kumaUpload(t,
		`INSERT INTO monitor (id, name, type, url, interval) VALUES (1, 'Checkout', 'http', 'https://shop.example.com/', 60)`)

	accepted := postImport(t, c, map[string]any{"dry_run": true}, database)
	job := awaitJob(t, c, accepted["id"].(string))

	if job["dry_run"] != true {
		t.Errorf("dry_run = %v", job["dry_run"])
	}
	if len(job["entries"].([]any)) == 0 {
		t.Error("a dry run produced no report, which is its entire output")
	}

	_, monitors := c.do(http.MethodGet, "/api/v1/monitors", nil)
	if rows := monitors["data"].([]any); len(rows) != 0 {
		t.Errorf("a dry run created %d monitors", len(rows))
	}
}

// Nothing to import is a validation error naming the field, not a 500 three
// layers away.
func TestAnImportWithNoDatabaseIsRefused(t *testing.T) {
	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		c.base+"/api/v1/imports/kuma", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	c.authorise(req)

	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("an empty import = %d, want 422", resp.StatusCode)
	}
}

// A file that is not a Kuma database fails the job with an explanation, rather
// than importing zero of everything and calling it a success.
func TestAFileThatIsNotAKumaDatabaseFailsWithAnExplanation(t *testing.T) {
	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	accepted := postImport(t, c, nil, []byte("this is not a database"))
	job := awaitJob(t, c, accepted["id"].(string))

	if job["state"] != "failed" {
		t.Fatalf("state = %v, want failed", job["state"])
	}
	failure, _ := job["error"].(string)
	if failure == "" {
		t.Error("the job failed with no explanation")
	}
}
