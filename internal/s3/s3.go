// Package s3 is a minimal S3-compatible object client over the standard library.
//
// ADR-008 item 10 takes this trade deliberately: SigV4 for PUT, GET, HEAD and
// DELETE is a canonical request, a string to sign, a four-step HMAC derivation
// and one header — a few hundred lines of our own code against a vendor SDK's
// dependency tree, for a client that touches four verbs.
//
// Three compatibility details are part of the decision rather than discoveries,
// and each is a configuration field below:
//
//   - **Path-style addressing is selectable.** MinIO, Garage and Ceph commonly
//     need it; AWS prefers virtual-host style. "S3-compatible" means very little
//     without the choice.
//   - **A region is required for the signature** even where the provider ignores
//     it, because it is an input to the key derivation rather than routing.
//   - **The endpoint is overridable**, which is the other half of what makes
//     "S3-compatible" mean anything.
//
// **Static credentials only** (item 11). No instance profiles, no STS, no
// credential chain: those are AWS-specific paths with their own refresh and
// failure modes, and an install that needs one can say so in its own phase.
//
// This package has two callers and they must not be confused with each other.
// The **mirror** is a durability copy of every artifact, and its failure is
// recorded rather than fatal. The **drop** is a delivery target for one run's
// files. They share this client and nothing else (ADR-008 item 9,
// PHASE-2-PLAN §4.2).
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config is one bucket and the credentials to reach it.
//
// Assembled from the report_storage settings section for the mirror, and from a
// schedule delivery target's config plus its sealed secret for the drop. The
// same shape either way, which is what "they share a client" means in practice.
type Config struct {
	Bucket string

	// Prefix is prepended to every key, with any leading or trailing slashes
	// normalised away by Key. A bucket shared with something else is the common
	// case, not the exotic one.
	Prefix string

	// Region is required. Not because every provider routes on it — most
	// S3-compatible providers ignore it entirely — but because it is an input to
	// the signing key, so a wrong one produces a 403 rather than a redirect.
	Region string

	// Endpoint overrides the AWS hostname: MinIO, R2, Backblaze, Garage, Ceph.
	// Empty means s3.<region>.amazonaws.com.
	Endpoint string

	// PathStyle puts the bucket in the path rather than in the hostname.
	PathStyle bool

	AccessKeyID     string
	SecretAccessKey string

	// ServerSideEncryption is passed through as x-amz-server-side-encryption
	// when set — "AES256" or "aws:kms". Client-side encryption is not
	// implemented and ADR-008 item 16 records that as a decision rather than an
	// omission: artifacts are not encrypted at rest, consistently with the
	// monitor names and uptime figures already in plaintext in the database.
	ServerSideEncryption string
}

// Validate reports what is missing, naming the field rather than the concept.
//
// Called before a request rather than at construction so that an operator who
// half-configures a mirror gets one clear error on save instead of an
// authentication failure at 09:00 on the first of the month.
func (c Config) Validate() error {
	var missing []string
	if strings.TrimSpace(c.Bucket) == "" {
		missing = append(missing, "bucket")
	}
	if strings.TrimSpace(c.Region) == "" {
		// Named explicitly because it is the field an operator on MinIO will
		// leave blank, reasonably, having never needed one before.
		missing = append(missing, "region (required for the request signature even where the provider ignores it)")
	}
	if strings.TrimSpace(c.AccessKeyID) == "" {
		missing = append(missing, "access_key_id")
	}
	if c.SecretAccessKey == "" {
		missing = append(missing, "secret_access_key")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s", ErrNotConfigured, strings.Join(missing, ", "))
	}
	if c.Endpoint != "" {
		if _, err := url.Parse(c.Endpoint); err != nil {
			return fmt.Errorf("endpoint is not a URL: %w", err)
		}
	}
	switch c.ServerSideEncryption {
	case "", "AES256", "aws:kms":
	default:
		return fmt.Errorf("server_side_encryption must be AES256 or aws:kms, not %q", c.ServerSideEncryption)
	}
	return nil
}

// Key joins the configured prefix to an object key.
func (c Config) Key(key string) string {
	prefix := strings.Trim(c.Prefix, "/")
	key = strings.TrimPrefix(key, "/")
	if prefix == "" {
		return key
	}
	return prefix + "/" + key
}

// ErrNotConfigured means the configuration is incomplete. Distinguished from a
// request failure because it is fixed on the settings page rather than by
// retrying — the same distinction the delivery dispatcher draws between a skip
// and a transient fault.
var ErrNotConfigured = errors.New("s3: not configured")

// ErrNotFound is a 404 from the provider.
//
// Its text says "not found" rather than "object not found" on purpose: a PUT into
// a bucket that does not exist is also a 404, and naming the object there points
// an operator at the one thing that was never supposed to be present. What is
// missing is said by the provider's own message, which travels with this.
var ErrNotFound = errors.New("s3: not found")

// Client performs signed requests against one bucket.
type Client struct {
	cfg  Config
	http *http.Client

	// now is the clock, injectable because a signature is a function of the
	// timestamp and a test that cannot fix it can only assert that signing
	// produced something.
	now func() time.Time
}

// DefaultTimeout bounds one object request.
//
// Generous by the standards of this codebase because the object at the far end
// may be the hundred-megabyte CSV ADR-008 item 7 anticipates, and a mirror that
// times out on the artifact it most needs to protect is worse than one that
// waits.
const DefaultTimeout = 5 * time.Minute

// New returns a client. A nil http.Client gets one with DefaultTimeout.
func New(cfg Config, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{cfg: cfg, http: hc, now: time.Now}
}

// Put uploads one object.
//
// `contentType` goes on the wire so that a browser opening the object from a
// bucket console sees a PDF rather than a download of octets.
func (c *Client) Put(ctx context.Context, key string, body []byte, contentType string) error {
	if err := c.cfg.Validate(); err != nil {
		return err
	}

	req, err := c.request(ctx, http.MethodPut, key, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.cfg.ServerSideEncryption != "" {
		// Signed by virtue of its prefix rather than by a special case in the
		// signer, which is why canonicalHeaders takes every x-amz-* header.
		req.Header.Set("X-Amz-Server-Side-Encryption", c.cfg.ServerSideEncryption)
	}

	resp, err := c.do(req, hexSHA256(body))
	if err != nil {
		return err
	}
	defer drain(resp)
	return statusError("put", c.cfg.Key(key), resp)
}

// Get downloads one object. The bytes are returned whole rather than streamed
// because every caller in this codebase has the whole object in memory already —
// the mirror is written from a rendered artifact and read back only by a
// verification that hashes it.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	if err := c.cfg.Validate(); err != nil {
		return nil, err
	}

	req, err := c.request(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req, emptyPayloadHash)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if err := statusError("get", c.cfg.Key(key), resp); err != nil {
		return nil, err
	}
	return io.ReadAll(resp.Body)
}

// Head reports an object's size, and is how "is the mirror actually holding
// this?" is answered without transferring it.
func (c *Client) Head(ctx context.Context, key string) (int64, error) {
	if err := c.cfg.Validate(); err != nil {
		return 0, err
	}

	req, err := c.request(ctx, http.MethodHead, key, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.do(req, emptyPayloadHash)
	if err != nil {
		return 0, err
	}
	defer drain(resp)
	if err := statusError("head", c.cfg.Key(key), resp); err != nil {
		return 0, err
	}
	return resp.ContentLength, nil
}

// Delete removes one object.
//
// A delete of an absent object is not an error, matching both S3's own behaviour
// and the local store's Remove — retention runs after a restore from a backup
// taken before the artifact existed, and a sweep that failed on the first such
// key would stop reclaiming anything behind it.
func (c *Client) Delete(ctx context.Context, key string) error {
	if err := c.cfg.Validate(); err != nil {
		return err
	}

	req, err := c.request(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req, emptyPayloadHash)
	if err != nil {
		return err
	}
	defer drain(resp)
	if err := statusError("delete", c.cfg.Key(key), resp); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

func (c *Client) request(ctx context.Context, method, key string, body io.Reader) (*http.Request, error) {
	endpoint, err := c.objectURL(key)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("s3: build request: %w", err)
	}
	return req, nil
}

func (c *Client) do(req *http.Request, payloadHash string) (*http.Response, error) {
	sign(req, c.cfg, payloadHash, c.now())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3: %s %s: %w", req.Method, redactedTarget(req.URL), err)
	}
	return resp, nil
}

// objectURL builds the request URL in whichever addressing style is configured.
//
// The two styles differ in one place and it is the one that breaks quietly:
// virtual-host puts the bucket in the hostname, which requires DNS for
// <bucket>.<endpoint> and TLS certificates that cover it. That is why path-style
// is selectable rather than inferred — a self-hosted MinIO has neither, and the
// failure without the switch is a TLS error that names a hostname the operator
// never typed.
func (c *Client) objectURL(key string) (string, error) {
	host := c.cfg.Endpoint
	scheme := "https"
	if host == "" {
		host = "s3." + c.cfg.Region + ".amazonaws.com"
	}
	if parsed, err := url.Parse(host); err == nil && parsed.Scheme != "" {
		scheme = parsed.Scheme
		host = parsed.Host
		if host == "" {
			return "", fmt.Errorf("s3: endpoint %q has no host", c.cfg.Endpoint)
		}
	}

	full := c.cfg.Key(key)
	if full == "" {
		return "", errors.New("s3: empty object key")
	}

	// Encoded here as well as in the signer, and with the same function, because
	// the transmitted path and the signed path have to be byte-identical.
	segments := strings.Split(full, "/")
	for i, seg := range segments {
		segments[i] = encodeSegment(seg)
	}
	path := strings.Join(segments, "/")

	if c.cfg.PathStyle {
		return scheme + "://" + host + "/" + encodeSegment(c.cfg.Bucket) + "/" + path, nil
	}
	return scheme + "://" + encodeSegment(c.cfg.Bucket) + "." + host + "/" + path, nil
}

// statusError turns a non-2xx into an error carrying the provider's own message.
//
// The body is included, truncated, because S3-compatible providers put the
// actionable part there — "SignatureDoesNotMatch", "NoSuchBucket",
// "AccessDenied" — and an error that says only "403" sends an operator to a
// search engine instead of to their bucket policy.
func statusError(op, key string, resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	detail := providerMessage(snippet)
	if detail == "" {
		detail = resp.Status
	}

	// A 404 is still ErrNotFound, because Delete tolerates a missing object and
	// Get must distinguish one — but **the provider's message travels with it**.
	//
	// Without that, a PUT into a bucket that does not exist reports "object not
	// found", naming the one thing that was never supposed to be there and
	// sending an operator to look for a file instead of at their bucket name.
	// The provider says `NoSuchBucket`; there is no reason to replace that with
	// a worse sentence.
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s %s: %s", ErrNotFound, op, key, detail)
	}
	return fmt.Errorf("s3: %s %s: %s: %s", op, key, resp.Status, detail)
}

// providerMessage pulls the useful sentence out of an S3 error document.
//
// Every S3-compatible provider answers with the same XML shape — a `<Code>` and
// usually a `<Message>` — and the raw body is mostly preamble. A live run against
// MinIO produced an error whose first hundred characters were
// `<?xml version="1.0" encoding="UTF-8"?>`, with the one word that mattered
// pushed past where anybody would read it.
//
// Parsed by hand rather than with encoding/xml: the whole grammar needed is two
// tags, the input is a byte-capped snippet that may be truncated mid-document,
// and a strict parser would return nothing at all for a body that was cut in
// half — which is precisely the case where a human still wants whatever was
// there. Falls back to the collapsed body when neither tag is present, because a
// provider that answers with something else has still said something.
func providerMessage(body []byte) string {
	code := between(string(body), "<Code>", "</Code>")
	message := between(string(body), "<Message>", "</Message>")
	switch {
	case code != "" && message != "":
		return code + ": " + message
	case code != "":
		return code
	case message != "":
		return message
	default:
		return collapseSpaces(string(body))
	}
}

func between(s, open, close string) string {
	start := strings.Index(s, open)
	if start < 0 {
		return ""
	}
	rest := s[start+len(open):]
	end := strings.Index(rest, close)
	if end < 0 {
		return ""
	}
	return collapseSpaces(rest[:end])
}

// redactedTarget is what an error may name. The bucket and host are not secret;
// the credential is not in the URL because this client signs by header rather
// than by query string, so there is nothing to strip — stated here so that a
// later change to presigned URLs does not quietly start logging one.
func redactedTarget(u *url.URL) string {
	return u.Scheme + "://" + u.Host + u.Path
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}
