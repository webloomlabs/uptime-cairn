package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The HTTP target drives the real thing.
//
// Everything below exists to make one measurement honest: the SQLite target can
// tell you the schema is right, and it cannot tell you the product works. Only a
// running engine has a scheduler that can fall behind, a worker pool that can
// shed, a result buffer that can fill, and an alerting queue that can drop —
// and every one of those failures is invisible from the database, because the
// symptom is a row that was never written.
//
// So this target starts a real `cairn`, creates real monitors through the real
// API pointed at a target this process serves, and then mostly *watches*. The
// sustained-write number is observed rather than driven, because the engine's
// rate is set by arithmetic — N monitors on an I-second interval produce N/I
// results a second and there is nothing else to check — and the interesting
// question is whether it achieves that rate rather than how fast it could go.

const (
	// setupPassword is fixed and local. This engine is spawned into a temporary
	// directory, listens on loopback, and is killed at the end of the run; the
	// credential exists because the API requires one, not because it protects
	// anything.
	setupPassword = "load-test-correct-horse"

	// createWorkers bounds concurrent monitor creation. SQLite has a single
	// writer, so more than a handful of concurrent POSTs queue on it rather than
	// going faster — but a handful does hide the per-request overhead that would
	// otherwise dominate.
	createWorkers = 8

	// monitorInterval is the 20-second floor, which is the whole claim: the
	// project promises 5,000 monitors at this interval, and anything longer
	// measures an easier problem.
	monitorInterval = 20
	monitorTimeout  = 5
)

// HTTPTarget drives /api/v1 against a running engine.
type HTTPTarget struct {
	BaseURL string

	// Binary, when set, is a `cairn` the harness starts and stops itself. A gate
	// that depends on somebody having remembered to start a server by hand is a
	// gate that measures whatever that server happened to be doing.
	Binary  string
	DataDir string
	Verbose bool

	client *http.Client
	key    string

	engine    *exec.Cmd
	engineLog *os.File
	spawned   bool

	// checked serves the endpoint every monitor points at, and can be made to
	// fail on command. It is in this process so the partition is instantaneous
	// and total: the engine sees every one of its targets go at once, which is
	// the burst worth measuring.
	checked  *http.Server
	checkedL net.Listener
	healthy  atomic.Bool
	requests atomic.Uint64

	// sink receives outbound webhook deliveries, so the alerting queue is
	// exercised rather than assumed.
	sink      *http.Server
	sinkL     net.Listener
	delivered atomic.Int64
}

func (t *HTTPTarget) Name() string {
	if t.Binary != "" {
		return "http(engine started by the harness)"
	}
	return fmt.Sprintf("http(%s)", t.BaseURL)
}

// Setup starts the engine if asked to, authenticates, and creates the workload
// through the API.
//
// rollupHours is ignored, and deliberately: history on this target is whatever
// the engine has actually produced. Seeding rollups behind its back would
// measure a read path over rows the product never wrote, which is the one way
// this whole exercise could quietly become worthless.
func (t *HTTPTarget) Setup(ctx context.Context, w *Workload, _ int) error {
	// A cookie jar because first-run setup answers with a session, and the API
	// key this needs cannot be minted without one — the bootstrap is deliberately
	// a browser's path, since a fresh install has no key to make a key with.
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	t.client = &http.Client{
		Timeout: 30 * time.Second,
		Jar:     jar,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: createWorkers * 2,
			MaxConnsPerHost:     createWorkers * 2,
		},
	}

	if err := t.startChecked(); err != nil {
		return err
	}
	if err := t.startSink(); err != nil {
		return err
	}
	if t.Binary != "" {
		if err := t.startEngine(ctx); err != nil {
			return err
		}
	}
	if err := t.waitReady(ctx); err != nil {
		return err
	}
	if err := t.authenticate(ctx); err != nil {
		return err
	}
	if err := t.createTaxonomy(ctx, w); err != nil {
		return err
	}
	if err := t.createWebhook(ctx); err != nil {
		return err
	}
	if err := t.createMonitors(ctx, w); err != nil {
		return err
	}
	return t.findDeepCursor(ctx, w)
}

// startEngine spawns a cairn into a fresh data directory.
func (t *HTTPTarget) startEngine(ctx context.Context) error {
	if t.DataDir == "" {
		dir, err := os.MkdirTemp("", "cairn-loadtest-*")
		if err != nil {
			return fmt.Errorf("temp data dir: %w", err)
		}
		t.DataDir = dir
	}
	if err := os.MkdirAll(t.DataDir, 0o750); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}

	port, err := freePort()
	if err != nil {
		return err
	}
	t.BaseURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	logPath := filepath.Join(t.DataDir, "engine.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("engine log: %w", err)
	}
	t.engineLog = logFile

	//nolint:gosec // the binary path is an operator-supplied flag on their own machine
	cmd := exec.CommandContext(ctx, t.Binary,
		"--data-dir", t.DataDir,
		"--listen", fmt.Sprintf("127.0.0.1:%d", port),
		"--instance-name", "Load Test")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", t.Binary, err)
	}
	t.engine = cmd
	t.spawned = true

	fmt.Printf("engine started: %s (pid %d, log %s)\n", t.BaseURL, cmd.Process.Pid, logPath)
	return nil
}

func (t *HTTPTarget) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.BaseURL+"/healthz", nil)
		if err != nil {
			return err
		}
		resp, err := t.client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s did not become ready within 30s; is a cairn running there? "+
		"pass -cairn <path to binary> to have the harness start one", t.BaseURL)
}

// authenticate completes first-run setup and mints an API key.
//
// A key rather than the session it just obtained: a bearer token needs no CSRF
// header and no cookie jar, which is what an automated client would use, and
// measuring the cookie path would be measuring the browser's case for no reason.
func (t *HTTPTarget) authenticate(ctx context.Context) error {
	var status struct {
		SetupRequired bool `json:"setup_required"`
	}
	if err := t.call(ctx, http.MethodGet, "/api/v1/setup/status", nil, &status, nil); err != nil {
		return fmt.Errorf("setup status: %w", err)
	}

	var session struct {
		CSRFToken string `json:"csrf_token"`
	}
	if status.SetupRequired {
		body := map[string]string{"email": "loadtest@example.test", "password": setupPassword}
		if err := t.call(ctx, http.MethodPost, "/api/v1/setup", body, &session, nil); err != nil {
			return fmt.Errorf("first-run setup: %w", err)
		}
	} else {
		body := map[string]string{"email": "loadtest@example.test", "password": setupPassword}
		if err := t.call(ctx, http.MethodPost, "/api/v1/auth/login", body, &session, nil); err != nil {
			return fmt.Errorf("login: %w (the engine already has an administrator this harness does not know)", err)
		}
	}

	var created struct {
		Key string `json:"key"`
	}
	request := map[string]any{
		"name": "load-test",
		"scopes": []string{
			"monitors:read", "monitors:write", "heartbeats:read",
			"groups:read", "groups:write", "tags:read", "tags:write",
			"webhooks:read", "webhooks:write", "metrics:read",
		},
	}
	headers := map[string]string{"X-Cairn-CSRF-Token": session.CSRFToken}
	if err := t.call(ctx, http.MethodPost, "/api/v1/api-keys", request, &created, headers); err != nil {
		return fmt.Errorf("mint api key: %w", err)
	}
	t.key = created.Key
	return nil
}

// createTaxonomy creates the groups and tags, then re-points every monitor at
// the identifiers the server assigned.
//
// The remapping is the part worth being careful about. The generator invented
// ids and stamped copies of them onto each monitor; replacing the slice entries
// alone would leave 5,000 monitors referencing groups that do not exist, and the
// filter scenarios would then return nothing and pass. Mapping old to new rather
// than recomputing the assignment keeps this correct if the generator's
// distribution rule ever changes.
func (t *HTTPTarget) createTaxonomy(ctx context.Context, w *Workload) error {
	remap := map[string][]byte{}

	for i := range w.Groups {
		var out struct {
			ID string `json:"id"`
		}
		body := map[string]any{"name": fmt.Sprintf("group-%04d", i)}
		if err := t.call(ctx, http.MethodPost, "/api/v1/groups", body, &out, nil); err != nil {
			return fmt.Errorf("create group %d: %w", i, err)
		}
		id, err := parseUUID(out.ID)
		if err != nil {
			return err
		}
		remap[hexUUID(w.Groups[i])] = id
		w.Groups[i] = id
	}
	for i := range w.Tags {
		var out struct {
			ID string `json:"id"`
		}
		body := map[string]any{"name": fmt.Sprintf("tag-%04d", i)}
		if err := t.call(ctx, http.MethodPost, "/api/v1/tags", body, &out, nil); err != nil {
			return fmt.Errorf("create tag %d: %w", i, err)
		}
		id, err := parseUUID(out.ID)
		if err != nil {
			return err
		}
		remap[hexUUID(w.Tags[i])] = id
		w.Tags[i] = id
	}

	for i := range w.Monitors {
		m := &w.Monitors[i]
		if replacement, ok := remap[hexUUID(m.GroupID)]; ok {
			m.GroupID = replacement
		}
		for j, tag := range m.TagIDs {
			if replacement, ok := remap[hexUUID(tag)]; ok {
				m.TagIDs[j] = replacement
			}
		}
	}
	return nil
}

// createWebhook points the engine's outbound deliveries at this process.
//
// The partition phase is the reason. A burst that marks several thousand
// monitors down inside one scheduler tick is exactly what the delivery queue is
// sized against, and until something counted the deliveries on the other end
// that size was an argument.
func (t *HTTPTarget) createWebhook(ctx context.Context) error {
	body := map[string]any{
		"name":   "load-test sink",
		"url":    fmt.Sprintf("http://127.0.0.1:%d/", t.sinkL.Addr().(*net.TCPAddr).Port),
		"events": []string{"monitor.down", "monitor.up"},
	}
	return t.call(ctx, http.MethodPost, "/api/v1/webhooks", body, nil, nil)
}

// createMonitors creates the workload through the real write path, and adopts
// the identifiers the server assigned.
//
// The generator's ids are discarded here, which is the one place this target
// deviates from the SQLite one. The server mints UUIDv7s of its own, and a
// scenario that filtered by a tag id the harness invented would return nothing
// and pass.
func (t *HTTPTarget) createMonitors(ctx context.Context, w *Workload) error {
	type result struct {
		index int
		id    []byte
		when  time.Time
		err   error
	}

	jobs := make(chan int)
	results := make(chan result, createWorkers)

	var wg sync.WaitGroup
	for range createWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				m := w.Monitors[i]

				var out struct {
					ID        string    `json:"id"`
					UpdatedAt time.Time `json:"updated_at"`
				}
				body := map[string]any{
					"name": m.Name,
					// Every monitor is an HTTP check against this process. The
					// workload's type mix is right for the schema target, where
					// type only affects a column; here a `dns` monitor would
					// resolve a name that does not exist and measure the
					// resolver's timeout instead of the engine.
					"type": "http",
					// A path per monitor, so the endpoint can fail for a
					// realistic fraction of them permanently. Without that every
					// monitor on this target would be up and the status filter
					// would measure a query that matches nothing — which is the
					// one plan the data model's §6.2 hypothesis is not about.
					"config":           map[string]any{"url": t.checkedURL(i, m.Status)},
					"interval_seconds": monitorInterval,
					"timeout_seconds":  monitorTimeout,
					// No retries: one failed check is one transition, which is
					// what makes time-to-detect a number rather than a range.
					"retries":  0,
					"group_id": hexUUID(m.GroupID),
					"tag_ids":  hexUUIDs(m.TagIDs),
					// Explicitly empty rather than absent: absent attaches the
					// default channels, and a run whose alerting depends on what
					// somebody configured earlier is not repeatable.
					"notification_channel_ids": []string{},
				}
				err := t.call(ctx, http.MethodPost, "/api/v1/monitors", body, &out, nil)
				if err != nil {
					results <- result{index: i, err: err}
					continue
				}
				id, err := parseUUID(out.ID)
				results <- result{index: i, id: id, when: out.UpdatedAt, err: err}
			}
		}()
	}

	go func() {
		for i := range w.Monitors {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	// Read before and after, because the creation rate on its own has never been
	// enough to act on. The gate has reported for two revisions that creating
	// monitors slows as the install grows, and "slow" and "queued" want opposite
	// fixes: work that got harder wants a cheaper query, a queue wants the thing
	// in front of it moved. The wait counter is the one that says which.
	beforePool, _ := t.Counters(ctx)

	start := time.Now()
	done := 0
	for res := range results {
		if res.err != nil {
			return fmt.Errorf("create monitor %d: %w", res.index, res.err)
		}
		w.Monitors[res.index].ID = res.id
		w.Monitors[res.index].UpdatedAt = res.when
		done++
		if t.Verbose && done%500 == 0 {
			fmt.Printf("  created %d/%d monitors\n", done, len(w.Monitors))
		}
	}

	elapsed := time.Since(start)
	fmt.Printf("created %d monitors through the API in %s (%.0f/sec)\n",
		len(w.Monitors), elapsed.Round(time.Millisecond), float64(len(w.Monitors))/elapsed.Seconds())

	if afterPool, err := t.Counters(ctx); err == nil {
		waits := afterPool.WriterWaits - beforePool.WriterWaits
		seconds := afterPool.WriterWaitSeconds - beforePool.WriterWaitSeconds
		// Skipped rather than printed as zero when the engine reports no pool
		// series at all: "nothing queued" and "nobody is counting" are different
		// claims and only one of them is evidence.
		if afterPool.WriterWaits > 0 || beforePool.WriterWaits > 0 || seconds > 0 {
			// Summed across the writers, not wall clock, so it can exceed the
			// elapsed time and that is not a bug: eight goroutines queueing for
			// half a second each is four seconds of waiting inside one second of
			// run. The per-statement mean is the figure to read.
			var each time.Duration
			if waits > 0 {
				each = time.Duration(seconds / float64(waits) * float64(time.Second))
			}
			fmt.Printf("  %d statements queued for the write connection, %s in total, %s each\n",
				waits, time.Duration(seconds*float64(time.Second)).Round(time.Millisecond),
				each.Round(time.Microsecond))
		}
	}
	return nil
}

// findDeepCursor pages to the middle of the collection and keeps the token.
//
// It cannot be computed: the API's cursor is opaque on purpose, so that nobody
// builds one by hand and then depends on the encoding. Walking there once is
// both the honest way to get it and a check that paging works at all.
func (t *HTTPTarget) findDeepCursor(ctx context.Context, w *Workload) error {
	const pageSize = 100
	target := len(w.Monitors) / 2

	var cursor *Cursor
	for seen := 0; seen < target; {
		res, err := t.ListMonitors(ctx, ListQuery{Limit: pageSize, Cursor: cursor})
		if err != nil {
			return fmt.Errorf("walk to deep cursor: %w", err)
		}
		seen += res.Rows
		if res.Next == nil {
			break
		}
		cursor = res.Next
	}
	w.DeepCursor = cursor
	return nil
}

// MeasureWrites observes the rate the engine achieves.
//
// Two counter reads, seconds apart, from the endpoint an operator scrapes. The
// harness writes nothing: there is no API for writing heartbeats, by design, and
// a back door that let the harness push rows would measure a path the product
// does not have.
//
// The expected rate is arithmetic — monitors divided by interval — and it is a
// ceiling the engine cannot exceed because there is nothing else to check. So
// the assertion is "achieves what the schedule implies", and the failure it
// catches is an engine quietly falling behind: shedding checks, or running them
// late, or unable to write fast enough to keep up with its own scheduler.
func (t *HTTPTarget) MeasureWrites(ctx context.Context, w *Workload, seconds int) (WriteResult, error) {
	out := WriteResult{
		Method:   "observed: the engine's own heartbeat counter over the window",
		Expected: float64(len(w.Monitors)) / float64(monitorInterval),
	}
	if seconds <= 0 {
		return out, nil
	}

	// Wait for the engine to reach steady state before measuring, rather than
	// sleeping a fixed interval and hoping.
	//
	// This matters more than it looks. Seeding 5,000 monitors saturates the
	// single writer for two minutes, during which the probe keeps checking and
	// buffers results it cannot deliver. Afterwards it drains that backlog at
	// roughly twice the steady-state rate — and a fixed warm-up measured the
	// drain, reporting 499 heartbeats a second against a schedule implying 250
	// and inviting the reader to conclude the engine was running ahead of its
	// work when it was catching up on it. Rows counted by check time said
	// 250/sec throughout; rows counted by write time said 500. Both were true.
	//
	// Steady state is defined as the observed rate settling near the rate the
	// schedule implies, which is the property being waited for rather than a
	// proxy for it. Buffer depth was the obvious proxy and the wrong one: a busy
	// probe always holds the results produced since its last acknowledgement, so
	// "buffer empty" never happens and waiting for it hangs.
	warmup, err := t.waitForSteadyState(ctx, out.Expected)
	if err != nil {
		return out, err
	}

	before, err := t.Counters(ctx)
	if err != nil {
		return out, err
	}
	requestsBefore := t.requests.Load()
	start := time.Now()

	select {
	case <-ctx.Done():
		return out, ctx.Err()
	case <-time.After(time.Duration(seconds) * time.Second):
	}

	after, err := t.Counters(ctx)
	if err != nil {
		return out, err
	}
	requestsAfter := t.requests.Load()

	elapsed := time.Since(start).Seconds()
	out.Rate = float64(after.HeartbeatsWritten-before.HeartbeatsWritten) / elapsed
	out.TargetRequests = requestsAfter - requestsBefore

	// The window just measured is guaranteed to hold heartbeats, which is what
	// the history scenario needs. Fixed at setup it would have been a range the
	// engine had not reached yet.
	w.HistoryFrom = start.Add(-warmup)
	w.HistoryTo = time.Now().UTC()
	out.Shed = after.ProbeShedResults - before.ProbeShedResults +
		after.ProbeSkippedChecks - before.ProbeSkippedChecks
	out.Rejected = after.ResultsRejected - before.ResultsRejected

	ingested := after.ResultsIngested - before.ResultsIngested
	if written := after.HeartbeatsWritten - before.HeartbeatsWritten; ingested > written {
		out.Redelivered = ingested - written
	}

	fmt.Printf("engine wrote %d heartbeats in %.1fs = %.1f/sec (schedule implies %.1f/sec); the checked endpoint saw %d requests, %d results were redelivered\n",
		after.HeartbeatsWritten-before.HeartbeatsWritten, elapsed, out.Rate, out.Expected,
		out.TargetRequests, out.Redelivered)
	fmt.Printf("  counters: heartbeats %d -> %d, ingested %d -> %d, probe checks started %d -> %d\n",
		before.HeartbeatsWritten, after.HeartbeatsWritten,
		before.ResultsIngested, after.ResultsIngested,
		before.ProbeChecksStarted, after.ProbeChecksStarted)
	return out, nil
}

func (t *HTTPTarget) ListMonitors(ctx context.Context, q ListQuery) (ListResult, error) {
	params := make([]string, 0, 5)
	if q.Limit > 0 {
		params = append(params, "limit="+strconv.Itoa(q.Limit))
	}
	if q.Cursor != nil && q.Cursor.Token != "" {
		params = append(params, "cursor="+q.Cursor.Token)
	}
	if q.Status != "" {
		params = append(params, "status="+q.Status)
	}
	if len(q.TagID) > 0 {
		params = append(params, "tag_id="+hexUUID(q.TagID))
	}
	if len(q.GroupID) > 0 {
		params = append(params, "group_id="+hexUUID(q.GroupID))
	}
	if q.Include != "" {
		params = append(params, "include="+q.Include)
	}
	if q.HeartbeatsLimit > 0 {
		params = append(params, "heartbeats_limit="+strconv.Itoa(q.HeartbeatsLimit))
	}

	var page struct {
		Data       []json.RawMessage `json:"data"`
		Pagination struct {
			NextCursor *string `json:"next_cursor"`
		} `json:"pagination"`
	}
	// The raw body is what the assertion needs, not the decoded shape: the
	// invariant is about bytes on the wire reaching a browser, and a decoded
	// struct has already thrown that number away.
	raw, err := t.callRaw(ctx, http.MethodGet, "/api/v1/monitors?"+strings.Join(params, "&"))
	if err != nil {
		return ListResult{}, err
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return ListResult{}, err
	}

	out := ListResult{Rows: len(page.Data), Bytes: len(raw)}
	if page.Pagination.NextCursor != nil {
		out.Next = &Cursor{Token: *page.Pagination.NextCursor}
	}
	return out, nil
}

// callRaw is call, keeping the body.
func (t *HTTPTarget) callRaw(ctx context.Context, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, t.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if t.key != "" {
		req.Header.Set("Authorization", "Bearer "+t.key)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, truncate(string(raw), 400))
	}
	return raw, nil
}

// MeasureLive opens browser update streams and reports what they carried.
//
// This is ADR-004's live half under the same load the rest of the gate runs at,
// and the assertion it enables is the one the 5,000-monitor figure cannot make:
// a monitor nobody is watching must cost a connected browser nothing. Every
// stream here subscribes to a page's worth of ids and nothing else, so at
// 5,000 monitors on a 20-second interval the engine is producing 250 results a
// second while each client should be receiving on the order of its own page
// divided by the interval — and if it is receiving 250, the channel is not
// scoped and the whole design has quietly become a broadcast.
//
// Foreign counts the failure directly: a diff for a monitor this stream never
// subscribed to. One is a bug, not a tolerance.
func (t *HTTPTarget) MeasureLive(ctx context.Context, w *Workload, clients, scoped, seconds int) (LiveResult, error) {
	out := LiveResult{Clients: clients, Scoped: scoped}
	if clients <= 0 || seconds <= 0 || len(w.Monitors) == 0 {
		return out, nil
	}
	if scoped > len(w.Monitors) {
		scoped = len(w.Monitors)
	}
	out.Scoped = scoped

	window, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()

	var (
		mu      sync.Mutex
		updates int
		foreign int
		total   int
		wg      sync.WaitGroup
	)

	started := time.Now()
	for c := 0; c < clients; c++ {
		// Each client watches a different slice, so the measurement is not one
		// page of monitors observed several times — which would let a broadcast
		// implementation pass by accident.
		offset := (c * scoped) % len(w.Monitors)
		ids := make([]string, 0, scoped)
		want := make(map[string]bool, scoped)
		for i := 0; i < scoped; i++ {
			id := hexUUID(w.Monitors[(offset+i)%len(w.Monitors)].ID)
			ids = append(ids, id)
			want[id] = true
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			n, bad, bytes := t.readStream(window, ids, want)
			mu.Lock()
			updates += n
			foreign += bad
			total += bytes
			mu.Unlock()
		}()
	}
	wg.Wait()

	out.Updates = updates
	out.Foreign = foreign
	out.Bytes = total
	out.Seconds = time.Since(started).Seconds()
	return out, nil
}

// readStream consumes one SSE stream until the context expires.
func (t *HTTPTarget) readStream(ctx context.Context, ids []string, want map[string]bool) (updates, foreign, bytes int) {
	path := t.BaseURL + "/api/v1/live?monitor_ids=" + strings.Join(ids, ",")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return 0, 0, 0
	}
	req.Header.Set("Authorization", "Bearer "+t.key)
	req.Header.Set("Accept", "text/event-stream")

	// Its own client: the shared one has a response-header timeout tuned for
	// request/response, and a stream that stays open for the window would trip
	// it.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, 0
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, 0
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var event string
	for scanner.Scan() {
		line := scanner.Text()
		bytes += len(line) + 1
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && event == "monitor":
			var diff struct {
				MonitorID string `json:"monitor_id"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &diff); err != nil {
				continue
			}
			updates++
			if !want[diff.MonitorID] {
				foreign++
			}
		}
	}
	return updates, foreign, bytes
}

func (t *HTTPTarget) Membership(ctx context.Context, q ListQuery) (MembershipResult, error) {
	var params []string
	if q.Status != "" {
		params = append(params, "status="+q.Status)
	}
	if len(q.TagID) > 0 {
		params = append(params, "tag_id="+hexUUID(q.TagID))
	}
	if len(q.GroupID) > 0 {
		params = append(params, "group_id="+hexUUID(q.GroupID))
	}

	var out struct {
		Version int64 `json:"version"`
		Count   int64 `json:"count"`
	}
	path := "/api/v1/monitors/membership"
	if len(params) > 0 {
		path += "?" + strings.Join(params, "&")
	}
	if err := t.call(ctx, http.MethodGet, path, nil, &out, nil); err != nil {
		return MembershipResult{}, err
	}
	return MembershipResult{Version: out.Version, Count: out.Count}, nil
}

// History reads the real endpoint over the range the run has actually produced.
//
// `resolution=auto` is what the dashboard sends, and it is the interesting case:
// the server picks the coarsest tier that yields a sensible number of buckets,
// and reads raw where raw still covers the range. A load test that asked for a
// specific tier would measure a query the product only issues when a user goes
// looking.
func (t *HTTPTarget) History(ctx context.Context, monitorID []byte, from, to time.Time) (int, error) {
	path := fmt.Sprintf("/api/v1/monitors/%s/history?from=%s&to=%s&resolution=auto",
		hexUUID(monitorID),
		from.UTC().Format(time.RFC3339),
		to.UTC().Format(time.RFC3339))

	var out struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := t.call(ctx, http.MethodGet, path, nil, &out, nil); err != nil {
		return 0, err
	}
	return len(out.Data), nil
}

// Partition flips every monitored endpoint at once.
//
// One switch rather than stopping the listener, because a refused connection and
// a 500 exercise different paths and the 500 is the one that produces a clean,
// instantaneous transition for the whole fleet. Stopping the listener would also
// make recovery a race against the engine's in-flight checks.
func (t *HTTPTarget) Partition(_ context.Context, healthy bool) error {
	t.healthy.Store(healthy)
	return nil
}

// Counters reads /metrics and pulls out the series the gate asserts on.
func (t *HTTPTarget) Counters(ctx context.Context) (EngineCounters, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.BaseURL+"/metrics", nil)
	if err != nil {
		return EngineCounters{}, err
	}
	req.Header.Set("Authorization", "Bearer "+t.key)

	resp, err := t.client.Do(req)
	if err != nil {
		return EngineCounters{}, fmt.Errorf("scrape /metrics: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return EngineCounters{}, fmt.Errorf("scrape /metrics: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return EngineCounters{}, err
	}
	return parseMetrics(string(body)), nil
}

func (t *HTTPTarget) Deliveries() int { return int(t.delivered.Load()) }

// parseMetrics reads the exposition format far enough to find the series that
// matter. Deliberately not a full parser: it takes the last value for each
// metric name, which is right because every series read here is either
// unlabelled or has exactly one probe reporting it in solo mode.
func parseMetrics(body string) EngineCounters {
	wanted := map[string]*uint64{}
	var out EngineCounters
	wanted["cairn_heartbeats_written_total"] = &out.HeartbeatsWritten
	wanted["cairn_results_ingested_total"] = &out.ResultsIngested
	wanted["cairn_results_rejected_total"] = &out.ResultsRejected
	wanted["cairn_alerts_published_total"] = &out.AlertsPublished
	wanted["cairn_alerts_dropped_total"] = &out.AlertsDropped
	wanted["cairn_probe_shed_results_total"] = &out.ProbeShedResults
	wanted["cairn_probe_skipped_checks_total"] = &out.ProbeSkippedChecks
	wanted["cairn_probe_checks_started_total"] = &out.ProbeChecksStarted
	wanted["cairn_probe_due_queue_depth"] = &out.ProbeDueQueueDepth
	wanted["cairn_probe_buffered_results"] = &out.ProbeBufferedItems
	wanted["cairn_webhook_events_dropped_total"] = &out.WebhookEventsDropped

	// Matched on the full series including its label, unlike everything above.
	// The pool series exist once per pool and the writer's is the one that
	// answers "was this queued"; stripping the label the way the rest of this
	// function does would leave whichever pool was printed last.
	wanted[`cairn_db_pool_wait_total{pool="writer"}`] = &out.WriterWaits

	var waitSeconds float64
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			continue
		}
		if name == `cairn_db_pool_wait_seconds_total{pool="writer"}` {
			waitSeconds = n
		}
		into, ok := wanted[name]
		if !ok {
			// Labels are otherwise ignored. In solo mode there is one probe and
			// one of each series, so this is exact; with several probes it would
			// keep whichever came last, which is why the gate's probe assertions
			// are about shedding rather than about a per-probe total.
			if brace := strings.IndexByte(name, '{'); brace >= 0 {
				into, ok = wanted[name[:brace]]
			}
			if !ok {
				continue
			}
		}
		if n >= 0 {
			*into = uint64(n)
		}
	}
	out.WriterWaitSeconds = waitSeconds
	return out
}

func (t *HTTPTarget) Close() error {
	if t.engine != nil && t.engine.Process != nil {
		_ = t.engine.Process.Kill()
		_ = t.engine.Wait()
	}
	if t.engineLog != nil {
		_ = t.engineLog.Close()
	}
	if t.checked != nil {
		_ = t.checked.Close()
	}
	if t.sink != nil {
		_ = t.sink.Close()
	}
	return nil
}

// startChecked serves the endpoint every monitor watches.
func (t *HTTPTarget) startChecked() error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for checked endpoint: %w", err)
	}
	t.checkedL = listener
	t.healthy.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.requests.Add(1)

		// Two ways to fail. A monitor whose path says "down" always fails, which
		// reproduces the workload's status distribution. A partition fails
		// everything, which is the burst.
		//
		// A 500 rather than a hang, in both cases. A timeout would make
		// time-to-detect a measurement of the timeout the harness itself chose.
		if !t.healthy.Load() || strings.HasPrefix(r.URL.Path, "/check/down/") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("failing"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	t.checked = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = t.checked.Serve(listener) }()
	return nil
}

// checkedURL gives each monitor its own path, and encodes in that path whether
// this one is meant to be permanently failing.
//
// The workload's status distribution is ~95% up, which is what makes `down` a
// selective filter and therefore what makes the index on it worth having. That
// distribution has to survive onto this target or the status scenarios measure
// an empty result set.
func (t *HTTPTarget) checkedURL(index int, status string) string {
	suffix := "up"
	if status == "down" {
		suffix = "down"
	}
	return fmt.Sprintf("http://127.0.0.1:%d/check/%s/%d",
		t.checkedL.Addr().(*net.TCPAddr).Port, suffix, index)
}

// startSink receives the engine's outbound webhook deliveries.
func (t *HTTPTarget) startSink() error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for webhook sink: %w", err)
	}
	t.sinkL = listener

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Drained rather than ignored: an unread body keeps the connection from
		// being reused, and the engine would then measure connection setup.
		_, _ = io.Copy(io.Discard, r.Body)
		t.delivered.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	t.sink = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = t.sink.Serve(listener) }()
	return nil
}

// call performs one API request. Errors carry the response body, because a 422
// from the monitor write path names the field and that is the whole reason the
// API returns problem documents.
func (t *HTTPTarget) call(ctx context.Context, method, path string, body, into any, headers map[string]string) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, t.BaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if t.key != "" {
		req.Header.Set("Authorization", "Bearer "+t.key)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, truncate(string(raw), 400))
	}
	if into == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, into)
}

// freePort asks the kernel for one and gives it straight back. A race in
// principle; in practice the engine binds it milliseconds later, and the
// alternative is a hardcoded port that collides with whatever else the machine
// is running.
func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	return port, listener.Close()
}

// hexUUID renders a 16-byte id in the dashed form the API speaks.
func hexUUID(b []byte) string {
	if len(b) != 16 {
		return ""
	}
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

func hexUUIDs(ids [][]byte) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, hexUUID(id))
	}
	return out
}

func parseUUID(s string) ([]byte, error) {
	stripped := strings.ReplaceAll(s, "-", "")
	b, err := hex.DecodeString(stripped)
	if err != nil || len(b) != 16 {
		return nil, fmt.Errorf("malformed identifier %q from the API", s)
	}
	return b, nil
}

// waitForSteadyState waits until the engine's write rate settles at the rate its
// schedule implies.
//
// Two consecutive windows within tolerance, because one can land across a
// scheduler tick boundary and read low for a reason that has nothing to do with
// backlog. The ceiling exists so a genuinely overloaded engine produces a
// measurement and a finding rather than a hang — "it never caught up" is the
// most important thing this harness could ever report, and it cannot report it
// from inside a loop.
func (t *HTTPTarget) waitForSteadyState(ctx context.Context, expected float64) (time.Duration, error) {
	const (
		window    = 5 * time.Second
		ceiling   = 5 * time.Minute
		tolerance = 0.25
		required  = 2
	)

	fmt.Printf("waiting for the engine to reach steady state before measuring...\n")
	start := time.Now()
	settled := 0

	previous, err := t.Counters(ctx)
	if err != nil {
		return 0, err
	}
	last := time.Now()

	for {
		select {
		case <-ctx.Done():
			return time.Since(start), ctx.Err()
		case <-time.After(window):
		}

		current, err := t.Counters(ctx)
		if err != nil {
			return time.Since(start), err
		}
		elapsed := time.Since(last).Seconds()
		rate := float64(current.HeartbeatsWritten-previous.HeartbeatsWritten) / elapsed
		previous, last = current, time.Now()

		if expected > 0 && rate <= expected*(1+tolerance) && rate >= expected*(1-tolerance) {
			settled++
		} else {
			settled = 0
		}
		if settled >= required {
			fmt.Printf("  settled at %.0f/sec after %s\n", rate, time.Since(start).Round(time.Second))
			return time.Since(start), nil
		}
		if time.Since(start) > ceiling {
			fmt.Printf("  still at %.0f/sec against %.0f/sec after %s; measuring anyway and reporting it\n",
				rate, expected, ceiling)
			return time.Since(start), nil
		}
	}
}
