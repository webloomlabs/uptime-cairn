package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// fakeUploader stands in for the bucket. It records what it was handed and can
// be made to fail, which is the only behaviour these tests need from an object
// store — the wire protocol is proven against AWS's own vector in internal/s3.
type fakeUploader struct {
	mu    sync.Mutex
	puts  map[string]string
	fail  error
	calls int
}

func newUploader() *fakeUploader { return &fakeUploader{puts: map[string]string{}} }

// For makes the fake its own MirrorSource, so a test that does not care about
// resolution hands the uploader straight to WithMirror. `settings` is ignored
// because these tests are about what happens after the mirror is resolved;
// resolution has its own tests below.
func (u *fakeUploader) For(model.ReportStorageSettings) Uploader { return u }

func (u *fakeUploader) Upload(_ context.Context, relPath string, data []byte, contentType string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	if u.fail != nil {
		return u.fail
	}
	u.puts[relPath] = contentType
	_ = data
	return nil
}

func (u *fakeUploader) uploaded() map[string]string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make(map[string]string, len(u.puts))
	for k, v := range u.puts {
		out[k] = v
	}
	return out
}

// TestTheMirrorCopiesEveryArtifactUnderItsLocalPath.
//
// The path identity is the assertion, not an incidental. `2026/09/<id>.pdf` in
// the bucket and on disk is what lets an operator restore by copying a tree into
// place instead of reconstructing a naming rule out of the database.
func TestTheMirrorCopiesEveryArtifactUnderItsLocalPath(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON, model.FormatCSV)
	run.ReportTemplateID = s.template.ID
	files := newFiles()
	uploads := newUploader()

	if err := New(s, files, Options{}).WithMirror(uploads, nil).Execute(t.Context(), run, now); err != nil {
		t.Fatalf("execute: %v", err)
	}

	state, failure, artifacts := s.outcome()
	if state != model.RunSucceeded {
		t.Fatalf("state = %q (%s), want succeeded", state, failure)
	}

	put := uploads.uploaded()
	if len(put) != len(artifacts) {
		t.Fatalf("uploaded %d objects for %d artifacts", len(put), len(artifacts))
	}
	for _, a := range artifacts {
		if _, ok := put[a.Path]; !ok {
			t.Errorf("artifact %s was filed at %q locally and never uploaded under that key", a.Format, a.Path)
		}
		// Created pending and moved on by the upload, so the row never claims an
		// offsite copy that has not been attempted.
		if a.MirrorState != model.MirrorPending {
			t.Errorf("%s artifact was created with mirror state %q, want pending", a.Format, a.MirrorState)
		}
		if got := s.mirrorStateOf(a.ID); got != model.MirrorUploaded {
			t.Errorf("%s artifact ended in mirror state %q, want uploaded", a.Format, got)
		}
	}
	// The content type travels, so an object opened from a bucket console is a
	// PDF rather than a download of octets.
	for _, a := range artifacts {
		if want := mirrorContentType(a.Format); put[a.Path] != want {
			t.Errorf("%s artifact uploaded as %q, want %q", a.Format, put[a.Path], want)
		}
	}
}

// **The property the whole design rests on** (ADR-008 item 9): a mirror failure
// is recorded and does not degrade the run.
//
// A report that rendered, filed and delivered has not failed because a bucket was
// unreachable. Marking it partial would train an operator to ignore the state
// that also means "the PDF did not render", which is the state that actually
// needs them.
func TestAFailedUploadDoesNotDegradeTheRun(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON, model.FormatCSV)
	run.ReportTemplateID = s.template.ID
	files := newFiles()
	uploads := newUploader()
	uploads.fail = errors.New("s3: put 2026/04/x.json: 403 Forbidden: <Code>SignatureDoesNotMatch</Code>")

	if err := New(s, files, Options{}).WithMirror(uploads, nil).Execute(t.Context(), run, now); err != nil {
		t.Fatalf("execute: %v", err)
	}

	state, failure, artifacts := s.outcome()
	if state != model.RunSucceeded {
		t.Errorf("a bucket failure degraded the run to %q (%s); local storage is the source of truth", state, failure)
	}
	for _, a := range artifacts {
		if a.State != model.ArtifactRendered {
			t.Errorf("%s artifact state = %q; a mirror failure must not change it", a.Format, a.State)
		}
		if got := s.mirrorStateOf(a.ID); got != model.MirrorFailed {
			t.Errorf("%s artifact ended in mirror state %q, want failed", a.Format, got)
		}
	}

	// The bytes are still readable locally, which is the sentence "never a read
	// path" means in practice.
	if data, ok := files.bytes(model.FormatJSON); !ok || len(data) == 0 {
		t.Error("the artifact is not readable locally after a mirror failure")
	}
}

// TestNoMirrorLeavesNoQueue. An install that has not configured one must not
// accumulate artifacts in a state that reads as "waiting to be uploaded", because
// the retry pass would then have a backlog it can never drain.
func TestNoMirrorLeavesNoQueue(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON)
	run.ReportTemplateID = s.template.ID

	if err := New(s, newFiles(), Options{}).Execute(t.Context(), run, now); err != nil {
		t.Fatalf("execute: %v", err)
	}

	_, _, artifacts := s.outcome()
	for _, a := range artifacts {
		if a.MirrorState != "" {
			t.Errorf("%s artifact has mirror state %q on an install with no mirror", a.Format, a.MirrorState)
		}
		if got := s.mirrorStateOf(a.ID); got != "" {
			t.Errorf("a mirror outcome was recorded with no mirror configured: %q", got)
		}
	}
}

// TestNewMirrorRefusesAnIncompleteConfiguration.
//
// Enablement and completeness are separate questions, and the settings surface
// already refuses the combination. Reaching it here means a hand-edited row or an
// older build, and the recovery is not to upload to a half-configured bucket.
func TestNewMirrorRefusesAnIncompleteConfiguration(t *testing.T) {
	t.Parallel()

	if m := NewMirror(model.ReportStorageSettings{MirrorEnabled: false, Bucket: "b", Region: "r", AccessKeyID: "k"}, "s"); m != nil {
		t.Error("a disabled mirror produced a client")
	}
	if m := NewMirror(model.ReportStorageSettings{MirrorEnabled: true, Bucket: "b"}, ""); m != nil {
		t.Error("an incomplete mirror produced a client")
	}
	m := NewMirror(model.ReportStorageSettings{
		MirrorEnabled: true, Bucket: "b", Region: "us-east-1", AccessKeyID: "k",
	}, "secret")
	if m == nil {
		t.Fatal("a complete mirror produced no client")
	}

	// A nil Mirror is the supported shape for an install with none, and calling
	// through it must say so rather than panic — the runner holds this as an
	// interface and the nil check is on the interface, so this is the belt to
	// that brace.
	var absent *Mirror
	if err := absent.Upload(context.Background(), "a.pdf", nil, ""); err == nil ||
		!strings.Contains(err.Error(), "no mirror") {
		t.Errorf("nil mirror upload: %v", err)
	}
}

// --- resolution -------------------------------------------------------------

// plainOpener returns the plaintext for any envelope. AAD binding is the vault's
// property and is tested where the vault is; what matters here is what the
// provider does with what it gets back.
type plainOpener struct {
	secret string
	err    error
	calls  int
}

func (o *plainOpener) Open(_, _, _ []byte) ([]byte, error) {
	o.calls++
	if o.err != nil {
		return nil, o.err
	}
	return []byte(o.secret), nil
}

func configured() model.ReportStorageSettings {
	return model.ReportStorageSettings{
		MirrorEnabled: true, Bucket: "artifacts", Region: "us-east-1",
		AccessKeyID: "key", SecretAccessKeySealed: []byte("sealed"),
	}
}

// **The correction this type exists for.** An operator who enables the mirror at
// 09:00 must not have to restart the instance for it to reach the next report.
//
// The first cut built the client at start-up, which meant the settings surface
// accepted the change and nothing was ever uploaded — the exact drift the
// per-run settings read exists to close, reintroduced one layer up.
func TestTheMirrorFollowsSettingsWithoutARestart(t *testing.T) {
	t.Parallel()

	opener := &plainOpener{secret: "s3cret"}
	provider := NewMirrorProvider(opener, model.SentinelOrgID, nil)

	// Off, as every install starts.
	if got := provider.For(model.ReportStorageSettings{}); got != nil {
		t.Fatal("a mirror was resolved for an install that has not configured one")
	}
	// Enabled, with no restart in between.
	if got := provider.For(configured()); got == nil {
		t.Fatal("enabling the mirror did not take effect")
	}
	// And off again.
	if got := provider.For(model.ReportStorageSettings{}); got != nil {
		t.Error("disabling the mirror did not take effect")
	}
}

// The client is cached while the configuration is unchanged, so consecutive runs
// share one connection pool rather than opening a handshake per artifact — at
// fifty reports on the first of the month that is the difference between one
// connection and hundreds.
func TestTheResolvedMirrorIsCachedUntilTheConfigurationChanges(t *testing.T) {
	t.Parallel()

	opener := &plainOpener{secret: "s3cret"}
	provider := NewMirrorProvider(opener, model.SentinelOrgID, nil)

	first := provider.For(configured())
	if provider.For(configured()) != first {
		t.Error("an unchanged configuration produced a second client")
	}
	if opener.calls != 1 {
		t.Errorf("the credential was opened %d times for one configuration", opener.calls)
	}

	// **A rotated credential must produce a new client.** The cache key includes
	// the sealed envelope precisely so that rotating a key is not a cache hit on
	// the old one — which would keep uploading with a credential the operator
	// believes they have replaced.
	rotated := configured()
	rotated.SecretAccessKeySealed = []byte("a different envelope")
	if provider.For(rotated) == first {
		t.Error("a rotated credential reused the client built from the old one")
	}

	// So must a changed endpoint.
	moved := configured()
	moved.Endpoint = "https://minio.internal:9000"
	if provider.For(moved) == first {
		t.Error("a changed endpoint reused the old client")
	}
}

// A credential that will not open leaves the mirror off with the reason logged,
// rather than taking monitoring down over a durability copy — and it is logged
// once rather than once per artifact of every report.
func TestACredentialThatWillNotOpenDisablesTheMirrorQuietly(t *testing.T) {
	t.Parallel()

	opener := &plainOpener{err: errors.New("cipher: message authentication failed")}
	provider := NewMirrorProvider(opener, model.SentinelOrgID, nil)

	for range 5 {
		if got := provider.For(configured()); got != nil {
			t.Fatal("a mirror was resolved from a credential that would not open")
		}
	}
	if opener.calls != 1 {
		t.Errorf("the failing credential was opened %d times; it is cached as absent", opener.calls)
	}
}

// The fingerprint must never carry a live credential: it outlives the request in
// a cache key, and the envelope is what is available before it is opened anyway.
func TestTheCacheKeyCarriesNoPlaintext(t *testing.T) {
	t.Parallel()

	settings := configured()
	settings.SecretAccessKeySealed = []byte("sealed-envelope-bytes")
	key := fingerprint(settings)
	for _, secret := range []string{"sealed-envelope-bytes", "s3cret"} {
		if strings.Contains(key, secret) {
			t.Errorf("the cache key contains %q", secret)
		}
	}
	if !strings.Contains(key, "artifacts") {
		t.Error("the cache key does not distinguish buckets")
	}
}

// A nil provider is the supported shape for a build not running one.
func TestANilProviderResolvesToNoMirror(t *testing.T) {
	t.Parallel()

	var absent *MirrorProvider
	if got := absent.For(configured()); got != nil {
		t.Error("a nil provider resolved a mirror")
	}
}
