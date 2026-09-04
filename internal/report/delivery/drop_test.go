package delivery

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// The drop: one run's files put into a bucket for a recipient.
//
// These tests run against an httptest server standing in for the object store,
// so what they assert is the delivery's behaviour — which keys, which bytes,
// which outcome on failure. The wire protocol underneath is proven separately in
// internal/s3, against AWS's own published signature vector; asserting it again
// here would be asserting the same thing twice and would make these tests fail
// for a reason that has nothing to do with delivery.

// bucketStub records PUTs and can be made to refuse them.
type bucketStub struct {
	mu     sync.Mutex
	put    map[string][]byte
	status int

	*httptest.Server
}

func newBucket() *bucketStub {
	b := &bucketStub{put: map[string][]byte{}, status: http.StatusOK}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.status != http.StatusOK {
			w.WriteHeader(b.status)
			_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code></Error>`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		b.put[r.URL.Path] = body
		w.WriteHeader(http.StatusOK)
	}))
	return b
}

func (b *bucketStub) keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.put))
	for k := range b.put {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (b *bucketStub) body(key string) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.put[key]
}

// openSecret is the vault stand-in. It returns the plaintext it was built with
// and asserts nothing about the envelope, because AAD binding is the vault's
// property and is tested where the vault is.
type openSecret struct {
	secret string
	err    error
}

func (o openSecret) Open(_, _, _ []byte) ([]byte, error) {
	if o.err != nil {
		return nil, o.err
	}
	return []byte(o.secret), nil
}

func s3Target(bucketURL string, formats ...string) model.ReportScheduleDelivery {
	t := target(model.ReportDeliveryS3, map[string]any{
		"bucket":        "client-reports",
		"prefix":        "acme",
		"region":        "ap-southeast-2",
		"endpoint":      bucketURL,
		"path_style":    true,
		"access_key_id": "AKIAIOSFODNN7EXAMPLE",
	}, formats...)
	t.SecretsSealed = []byte("sealed")
	return t
}

// TestADropUploadsEveryArtifactUnderAReadableKey.
//
// The key layout is the assertion. A drop's reader is a person or somebody's
// pipeline, so the path is named from the template and the period — the opposite
// choice from the mirror, whose reader is a restore and which therefore
// reproduces the artifact-id path on disk.
func TestADropUploadsEveryArtifactUnderAReadableKey(t *testing.T) {
	t.Parallel()

	bucket := newBucket()
	defer bucket.Close()

	s, f := fixture(s3Target(bucket.URL))
	d := dispatcher(t, s, f).WithDrops(openSecret{secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"})

	if err := d.Deliver(context.Background(), s.run.ID, now); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// Case is preserved by `slug`, which strips rather than lower-cases: the key
	// is read by a person, and "Acme-monthly" is what they called the template.
	want := []string{
		"/client-reports/acme/Acme-monthly/2026-03/Acme-monthly-2026-03.csv",
		"/client-reports/acme/Acme-monthly/2026-03/Acme-monthly-2026-03.pdf",
	}
	if got := bucket.keys(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("uploaded keys:\n got %v\nwant %v", got, want)
	}
	if string(bucket.body(want[1])) != "%PDF-1" {
		t.Errorf("the PDF's bytes did not arrive: %q", bucket.body(want[1]))
	}

	logged := s.log()
	if len(logged) != 1 || logged[0].Outcome != model.DeliverySucceeded {
		t.Fatalf("delivery log = %+v, want one success", logged)
	}
	// The target is described by bucket and prefix and never by credential.
	if !strings.Contains(logged[0].Target, "client-reports") {
		t.Errorf("target = %q, want it to name the bucket", logged[0].Target)
	}
}

// A template named `../../etc` has nowhere to go, because the key is sanitised by
// the same slug the mail attachment uses.
//
// This is one of the two places in the subsystem where a user-supplied string
// reaches a path — the artifact's own on-disk path avoids the problem by not
// using one at all — so the sanitising is what makes it safe rather than a
// convention that it is not abused.
func TestADropKeyCannotEscapeItsPrefix(t *testing.T) {
	t.Parallel()

	bucket := newBucket()
	defer bucket.Close()

	s, f := fixture(s3Target(bucket.URL))
	s.template.Name = "../../etc/passwd"
	d := dispatcher(t, s, f).WithDrops(openSecret{secret: "secret"})

	if err := d.Deliver(context.Background(), s.run.ID, now); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	for _, key := range bucket.keys() {
		if strings.Contains(key, "..") {
			t.Errorf("key %q escapes its prefix", key)
		}
		if !strings.HasPrefix(key, "/client-reports/acme/") {
			t.Errorf("key %q is outside the configured bucket and prefix", key)
		}
	}
}

// A bucket that refuses the upload is a delivery failure, retried like any other
// — and the provider's own message travels, because 'AccessDenied' and
// 'NoSuchBucket' send an operator to two different screens.
func TestARefusedUploadIsRetriedAndRecordedWithTheProvidersMessage(t *testing.T) {
	t.Parallel()

	bucket := newBucket()
	bucket.status = http.StatusForbidden
	defer bucket.Close()

	s, f := fixture(s3Target(bucket.URL))
	d := dispatcher(t, s, f).WithDrops(openSecret{secret: "secret"})

	if err := d.Deliver(context.Background(), s.run.ID, now); err == nil {
		t.Error("a refused upload was reported as success")
	}

	logged := s.log()
	if len(logged) != MaxAttempts {
		t.Fatalf("%d rows, want one per attempt (%d)", len(logged), MaxAttempts)
	}
	for _, row := range logged {
		if row.Outcome != model.DeliveryFailed {
			t.Errorf("outcome = %q, want failed", row.Outcome)
		}
		if !strings.Contains(row.Error, "AccessDenied") {
			t.Errorf("reason = %q, want the provider's own message", row.Error)
		}
	}
}

// A credential that will not open is permanent, not transient. The envelope is
// AAD-bound to its row, so a failure is either a key problem or a relocated
// ciphertext, and neither is fixed by trying again fifteen seconds later.
func TestACredentialThatWillNotOpenIsNotRetried(t *testing.T) {
	t.Parallel()

	bucket := newBucket()
	defer bucket.Close()

	s, f := fixture(s3Target(bucket.URL))
	d := dispatcher(t, s, f).WithDrops(openSecret{err: errors.New("cipher: message authentication failed")})

	_ = d.Deliver(context.Background(), s.run.ID, now)

	logged := s.log()
	if len(logged) != 1 {
		t.Fatalf("%d rows, want 1 — an unopenable credential does not become openable", len(logged))
	}
	if logged[0].Outcome != model.DeliveryFailed {
		t.Errorf("outcome = %q, want failed", logged[0].Outcome)
	}
	if len(bucket.keys()) != 0 {
		t.Error("something was uploaded with a credential that could not be opened")
	}
}

// An incompletely configured drop is refused before anything is attempted, with
// the missing field named — the same discipline the settings surface applies to
// the mirror.
func TestAnIncompleteDropIsRefusedBeforeItIsAttempted(t *testing.T) {
	t.Parallel()

	incomplete := target(model.ReportDeliveryS3, map[string]any{"bucket": "client-reports"})
	incomplete.SecretsSealed = []byte("sealed")

	s, f := fixture(incomplete)
	d := dispatcher(t, s, f).WithDrops(openSecret{secret: "secret"})
	_ = d.Deliver(context.Background(), s.run.ID, now)

	logged := s.log()
	if len(logged) != 1 {
		t.Fatalf("%d rows, want 1 — an incomplete configuration does not complete itself", len(logged))
	}
	if !strings.Contains(logged[0].Error, "region") {
		t.Errorf("reason = %q, want it to name the missing field", logged[0].Error)
	}
}
