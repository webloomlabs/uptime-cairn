package s3

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The credentials, bucket and instant AWS publishes its worked examples with.
// They authenticate against nothing; they exist so the arithmetic below can be
// checked against somebody else's answer.
const (
	exampleKeyID  = "AKIAIOSFODNN7EXAMPLE"
	exampleSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

func exampleTime() time.Time {
	return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
}

// TestSignMatchesPublishedVector checks this signer against AWS's own "Example:
// PUT Object" walkthrough.
//
// **This is the only test in the package that can find a wrong signature.**
// Every other assertion here compares this code against itself, which a
// consistent misreading of the specification would pass — and a signature is not
// approximately correct: one byte of difference in the canonical request
// produces a 403 that names no byte. So the expected value below comes from
// AWS's documentation rather than from a previous run of this code.
func TestSignMatchesPublishedVector(t *testing.T) {
	const (
		payloadHash = "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072"
		want        = "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"
	)

	req, err := http.NewRequest(http.MethodPut,
		"https://examplebucket.s3.amazonaws.com/test%24file.text", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The vector signs date and x-amz-storage-class alongside the two headers
	// every request carries. Both reach the signature by the ordinary rules —
	// `date` by name and the storage class by its `x-amz-` prefix — which is the
	// property being checked as much as the digest is.
	req.Header.Set("Date", "Fri, 24 May 2013 00:00:00 GMT")
	req.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")

	cfg := Config{
		Bucket:          "examplebucket",
		Region:          "us-east-1",
		AccessKeyID:     exampleKeyID,
		SecretAccessKey: exampleSecret,
	}
	sign(req, cfg, payloadHash, exampleTime())

	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, "Signature="+want) {
		t.Errorf("signature does not match the published vector\n got: %s\nwant Signature=%s", auth, want)
	}
	if !strings.Contains(auth, "SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class") {
		t.Errorf("signed header list does not match the published vector: %s", auth)
	}
	if !strings.Contains(auth, "Credential="+exampleKeyID+"/20130524/us-east-1/s3/aws4_request") {
		t.Errorf("credential scope does not match the published vector: %s", auth)
	}
}

// TestCanonicalURIEncodesReservedCharacters is the reason canonicalURI does not
// use url.EscapedPath: Go leaves `+`, `=` and `:` alone in a path, and S3 signs
// them encoded. A key that transmits one way and signs the other is a 403 whose
// cause is invisible.
func TestCanonicalURIEncodesReservedCharacters(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/2026/09/a+b.pdf", "/2026/09/a%2Bb.pdf"},
		{"/reports/q1 final.csv", "/reports/q1%20final.csv"},
		{"/a=b/c:d.json", "/a%3Db/c%3Ad.json"},
		{"/plain/artifact.pdf", "/plain/artifact.pdf"},
	} {
		req, err := http.NewRequest(http.MethodGet, "https://example.com"+tc.in, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := canonicalURI(req.URL); got != tc.want {
			t.Errorf("canonicalURI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestConfigKeyJoinsPrefix(t *testing.T) {
	for _, tc := range []struct{ prefix, key, want string }{
		{"", "2026/09/a.pdf", "2026/09/a.pdf"},
		{"cairn", "2026/09/a.pdf", "cairn/2026/09/a.pdf"},
		{"/cairn/", "/2026/09/a.pdf", "cairn/2026/09/a.pdf"},
		{"cairn/reports", "a.pdf", "cairn/reports/a.pdf"},
	} {
		if got := (Config{Prefix: tc.prefix}).Key(tc.key); got != tc.want {
			t.Errorf("Key(%q) with prefix %q = %q, want %q", tc.key, tc.prefix, got, tc.want)
		}
	}
}

// TestAddressingStyles covers the switch that exists because a self-hosted MinIO
// has neither wildcard DNS nor a certificate covering <bucket>.<host>.
func TestAddressingStyles(t *testing.T) {
	virtual := New(Config{Bucket: "artifacts", Region: "us-east-1"}, nil)
	got, err := virtual.objectURL("2026/09/a.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://artifacts.s3.us-east-1.amazonaws.com/2026/09/a.pdf"; got != want {
		t.Errorf("virtual-host URL = %q, want %q", got, want)
	}

	path := New(Config{
		Bucket: "artifacts", Region: "us-east-1",
		Endpoint: "http://minio.internal:9000", PathStyle: true,
	}, nil)
	got, err = path.objectURL("2026/09/a.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://minio.internal:9000/artifacts/2026/09/a.pdf"; got != want {
		t.Errorf("path-style URL = %q, want %q", got, want)
	}
}

// TestValidateNamesTheMissingField. An operator who half-configures a mirror
// should be told which field on the settings page, not handed a 403 at 09:00 on
// the first of the month.
func TestValidateNamesTheMissingField(t *testing.T) {
	err := Config{Bucket: "b"}.Validate()
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	for _, field := range []string{"region", "access_key_id", "secret_access_key"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error does not name %s: %v", field, err)
		}
	}

	full := Config{Bucket: "b", Region: "r", AccessKeyID: "k", SecretAccessKey: "s"}
	if err := full.Validate(); err != nil {
		t.Errorf("complete configuration rejected: %v", err)
	}
	if err := (Config{
		Bucket: "b", Region: "r", AccessKeyID: "k", SecretAccessKey: "s",
		ServerSideEncryption: "rot13",
	}).Validate(); err == nil {
		t.Error("an unknown server-side encryption mode was accepted")
	}
}

// TestPutSignsAndSendsWhatItSays exercises the whole request path against a
// server that inspects it, which is what catches a header that is set after
// signing — a class of bug the signature test cannot see because it never sends.
func TestPutSignsAndSendsWhatItSays(t *testing.T) {
	var got *http.Request
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		body = buf
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(Config{
		Bucket: "artifacts", Prefix: "cairn", Region: "ap-southeast-2",
		Endpoint: server.URL, PathStyle: true,
		AccessKeyID: exampleKeyID, SecretAccessKey: exampleSecret,
		ServerSideEncryption: "AES256",
	}, server.Client())

	payload := []byte("a rendered report")
	if err := client.Put(context.Background(), "2026/09/a.pdf", payload, "application/pdf"); err != nil {
		t.Fatalf("put: %v", err)
	}

	if got.URL.Path != "/artifacts/cairn/2026/09/a.pdf" {
		t.Errorf("path = %q, want the bucket and prefix in it", got.URL.Path)
	}
	if string(body) != string(payload) {
		t.Errorf("body = %q, want %q", body, payload)
	}
	if got.Header.Get("X-Amz-Server-Side-Encryption") != "AES256" {
		t.Error("server-side encryption header was not passed through")
	}
	if got.Header.Get("X-Amz-Content-Sha256") != hexSHA256(payload) {
		t.Error("payload digest header does not describe the body")
	}
	// Every header the signature claims has to be one the request actually
	// carries. A SignedHeaders list naming a header that was never sent is the
	// exact shape of the bug that only shows up against a real provider.
	auth := got.Header.Get("Authorization")
	_, list, _ := strings.Cut(auth, "SignedHeaders=")
	list, _, _ = strings.Cut(list, ",")
	for _, name := range strings.Split(list, ";") {
		if name == "host" {
			continue
		}
		if got.Header.Get(name) == "" {
			t.Errorf("signature covers %q but the request does not carry it", name)
		}
	}
}

// TestProviderErrorCarriesItsOwnMessage. "403" alone sends an operator to a
// search engine; "SignatureDoesNotMatch" sends them to their credentials.
func TestProviderErrorCarriesItsOwnMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Error><Code>SignatureDoesNotMatch</Code></Error>`))
	}))
	defer server.Close()

	client := New(Config{
		Bucket: "artifacts", Region: "us-east-1", Endpoint: server.URL, PathStyle: true,
		AccessKeyID: exampleKeyID, SecretAccessKey: exampleSecret,
	}, server.Client())

	err := client.Put(context.Background(), "a.pdf", []byte("x"), "application/pdf")
	if err == nil {
		t.Fatal("a 403 was reported as success")
	}
	if !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Errorf("error does not carry the provider's message: %v", err)
	}
}

// TestNotFoundIsDistinguishable, because Delete tolerates a missing object and
// Get must not: retention runs after a restore from a backup taken before the
// artifact existed.
func TestNotFoundIsDistinguishable(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchBucket</Code></Error>`))
	}))
	defer server.Close()

	client := New(Config{
		Bucket: "artifacts", Region: "us-east-1", Endpoint: server.URL, PathStyle: true,
		AccessKeyID: exampleKeyID, SecretAccessKey: exampleSecret,
	}, server.Client())

	if _, err := client.Get(context.Background(), "gone.pdf"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get on a missing object: want ErrNotFound, got %v", err)
	}
	if err := client.Delete(context.Background(), "gone.pdf"); err != nil {
		t.Errorf("Delete of a missing object should not be an error, got %v", err)
	}

	// **A PUT into a bucket that does not exist must not report "object not
	// found".** That names the one thing that was never supposed to be there and
	// sends an operator to look for a file instead of at their bucket name. A
	// live run against MinIO produced exactly that message, which is why this
	// assertion exists.
	err := client.Put(context.Background(), "a.pdf", []byte("x"), "application/pdf")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "NoSuchBucket") {
		t.Errorf("the 404 does not carry the provider's message: %v", err)
	}
}

// TestUnconfiguredNeverReachesTheNetwork. A half-configured mirror must fail on
// the settings page's terms rather than as a connection error to a hostname
// assembled out of empty strings.
func TestUnconfiguredNeverReachesTheNetwork(t *testing.T) {
	client := New(Config{Bucket: "artifacts"}, &http.Client{
		Transport: refuseTransport{t},
	})
	if err := client.Put(context.Background(), "a.pdf", []byte("x"), ""); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("want ErrNotConfigured, got %v", err)
	}
}

type refuseTransport struct{ t *testing.T }

func (r refuseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.t.Error("an unconfigured client attempted a request")
	return nil, errors.New("refused")
}

// TestProviderMessageIsReadable.
//
// A live run against MinIO produced an error whose first hundred characters were
// the XML preamble, with `NoSuchBucket` pushed past where anybody would read it.
// The parsing is by hand on purpose: a truncated body still has to yield whatever
// was in it, and a strict parser returns nothing for one that was cut in half.
func TestProviderMessageIsReadable(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, body, want string }{
		{
			"code and message",
			`<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchBucket</Code>` +
				`<Message>The specified bucket does not exist</Message></Error>`,
			"NoSuchBucket: The specified bucket does not exist",
		},
		{
			"code only",
			`<?xml version="1.0"?><Error><Code>SignatureDoesNotMatch</Code></Error>`,
			"SignatureDoesNotMatch",
		},
		{
			// Cut off by the byte cap mid-document. The code was already read, so
			// it is still the answer.
			"truncated after the code",
			`<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>You do not`,
			"AccessDenied",
		},
		{
			"not xml at all",
			"upstream connect error",
			"upstream connect error",
		},
	} {
		if got := providerMessage([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
