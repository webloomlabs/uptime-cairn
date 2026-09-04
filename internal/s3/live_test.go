package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
)

// A live round-trip against a real S3-compatible server.
//
// # Why this exists when the vector test already passes
//
// TestSignMatchesPublishedVector proves the arithmetic against AWS's own worked
// example, which is the strongest assertion in the package — but it proves the
// signature for *one request AWS wrote down*. It cannot catch a header this
// client sets on a path the vector does not cover, an addressing style that
// resolves to a hostname no certificate matches, or a provider that is stricter
// than the specification in some corner. Those are found by sending, and only by
// sending.
//
// # Why it is opt-in
//
// It needs a server, so it skips unless one is named. `go test ./...` on a laptop
// with nothing running must stay green — a test that fails for want of
// infrastructure trains people to ignore failures, which costs more than the
// coverage is worth.
//
//	docker run -d -p 9000:9000 \
//	  -e MINIO_ROOT_USER=cairnaccesskey -e MINIO_ROOT_PASSWORD=cairnsecretkey123 \
//	  quay.io/minio/minio:latest server /data
//
//	CAIRN_S3_TEST_ENDPOINT=http://127.0.0.1:9000 \
//	CAIRN_S3_TEST_BUCKET=cairn-artifacts \
//	CAIRN_S3_TEST_ACCESS_KEY=cairnaccesskey \
//	CAIRN_S3_TEST_SECRET_KEY=cairnsecretkey123 \
//	  go test ./internal/s3/ -run Live -v
//
// The bucket must exist: creating one is a fifth verb this client deliberately
// does not have, because nothing in the product creates buckets.
func liveConfig(t *testing.T) Config {
	t.Helper()

	endpoint := os.Getenv("CAIRN_S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("set CAIRN_S3_TEST_ENDPOINT (and _BUCKET, _ACCESS_KEY, _SECRET_KEY) to run this against a real server")
	}
	cfg := Config{
		Bucket:          os.Getenv("CAIRN_S3_TEST_BUCKET"),
		Prefix:          "cairn-live-test",
		Region:          cmp(os.Getenv("CAIRN_S3_TEST_REGION"), "us-east-1"),
		Endpoint:        endpoint,
		PathStyle:       os.Getenv("CAIRN_S3_TEST_VIRTUAL_HOST") == "",
		AccessKeyID:     os.Getenv("CAIRN_S3_TEST_ACCESS_KEY"),
		SecretAccessKey: os.Getenv("CAIRN_S3_TEST_SECRET_KEY"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the live test configuration is incomplete: %v", err)
	}
	return cfg
}

func cmp(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// TestLiveRoundTrip exercises all four verbs against a real server.
//
// The assertion that matters is the middle one: the bytes that come back are the
// bytes that went out. A signature that is wrong fails before that, and a client
// that corrupts a large body fails at it.
func TestLiveRoundTrip(t *testing.T) {
	client := New(liveConfig(t), nil)
	ctx := context.Background()

	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	key := "roundtrip-" + hex.EncodeToString(suffix) + ".bin"

	// Deliberately not a round number and not small: a body that spans several
	// TCP segments is where a client that signs a length it does not send fails.
	payload := make([]byte, 1<<20+7)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	if err := client.Put(ctx, key, payload, "application/octet-stream"); err != nil {
		t.Fatalf("put: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Delete(context.Background(), key); err != nil {
			t.Errorf("cleanup delete: %v", err)
		}
	})

	size, err := client.Head(ctx, key)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if size != int64(len(payload)) {
		t.Errorf("head reports %d bytes, want %d", size, len(payload))
	}

	got, err := client.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("the bytes that came back are not the bytes that went out (%d vs %d)", len(got), len(payload))
	}

	if err := client.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := client.Head(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: want ErrNotFound, got %v", err)
	}
}

// A key with characters Go and S3 encode differently. This is the case
// canonicalURI exists for, and it can only be confirmed against a server that
// checks the signature it was sent.
func TestLiveAwkwardKey(t *testing.T) {
	client := New(liveConfig(t), nil)
	ctx := context.Background()

	const key = "awkward/a+b c=d/report:2026.json"
	if err := client.Put(ctx, key, []byte(`{"ok":true}`), "application/json"); err != nil {
		t.Fatalf("put: %v", err)
	}
	t.Cleanup(func() { _ = client.Delete(context.Background(), key) })

	got, err := client.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Errorf("got %q", got)
	}
}

// A wrong secret must be refused by the server rather than accepted, which is
// what confirms the signature is being checked at all — a test suite that only
// ever sends valid signatures cannot tell a verified one from an ignored one.
func TestLiveWrongCredentialIsRefused(t *testing.T) {
	cfg := liveConfig(t)
	cfg.SecretAccessKey = "definitely-not-the-secret"
	client := New(cfg, nil)

	err := client.Put(context.Background(), "should-not-exist.bin", []byte("x"), "application/octet-stream")
	if err == nil {
		t.Fatal("a request signed with the wrong secret was accepted")
	}
	t.Logf("refused, as it must be: %v", err)
}
