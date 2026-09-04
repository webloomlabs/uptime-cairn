package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// SigV4, in the four steps the specification names, and no more than four.
//
// ADR-008 item 10 chose this over a vendor SDK, and the sentence that decided it
// is worth restating where the code is: a canonical request, a string to sign, a
// four-step key derivation and one header, against a dependency tree that would
// dwarf the rest of go.mod for a client that touches four verbs.
//
// **Everything here is exact-match or nothing.** A signature is not approximately
// correct — a byte of difference in the canonical request produces a 403 with no
// indication of which byte, which is why the canonicalisation rules below are
// written out rather than approximated with url.Values.Encode and hoped for.

// unsignedPayload is what a streaming upload would send. We do not stream: every
// artifact is already in memory by the time it reaches this package, because it
// was rendered there, so the payload hash is real on every request.
//
// The distinction matters for a reason beyond tidiness — SigV4 with a real
// payload hash authenticates the body, so a proxy that rewrites bytes in transit
// is caught by the signature rather than by a checksum nobody compares.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sign adds the Authorization header for one request.
//
// `payloadHash` is hex SHA-256 of the body, which the caller has already computed
// — it goes on the wire as x-amz-content-sha256 as well as into the signature, so
// computing it twice would be computing it twice over a hundred megabytes.
func sign(req *http.Request, cfg Config, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	signed, canonicalHeaders := canonicalHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonicalHeaders,
		signed,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, cfg.Region, "s3", "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	// The four-step derivation. Each step keys the next, which is what makes a
	// leaked signing key useful for one day, one region and one service rather
	// than for the account.
	key := hmacSHA256([]byte("AWS4"+cfg.SecretAccessKey), []byte(dateStamp))
	key = hmacSHA256(key, []byte(cfg.Region))
	key = hmacSHA256(key, []byte("s3"))
	key = hmacSHA256(key, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 "+
		"Credential="+cfg.AccessKeyID+"/"+scope+", "+
		"SignedHeaders="+signed+", "+
		"Signature="+signature)
}

// canonicalHeaders returns the signed-header list and the block that goes into
// the canonical request.
//
// **Which headers are signed is a decision, not an accident.** Host always, every
// `x-amz-*` header, and `content-type` and `date` where the caller set one.
//
// Signing by the `x-amz-` prefix rather than from a fixed list is what makes
// server-side encryption work with no second code path: Put sets the header and
// it is signed here because of its name. The two non-`x-amz-` headers are
// included because a provider may echo them into the response and because the
// published SigV4 vectors sign them, which is what lets the test below check this
// implementation against AWS's own arithmetic rather than against itself.
//
// Anything else Go's transport adds on the way out — User-Agent, Accept-Encoding
// — is deliberately unsigned. SignedHeaders names exactly what went into the
// signature, so an unsigned header is not a hole: it is a header the server is
// told not to authenticate, which is the honest description of one this process
// did not choose.
func canonicalHeaders(req *http.Request) (signed string, block string) {
	names := make([]string, 0, len(req.Header)+1)
	values := make(map[string]string, len(req.Header)+1)

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	names = append(names, "host")
	values["host"] = host

	for name, vs := range req.Header {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-amz-") && lower != "content-type" && lower != "date" {
			continue
		}
		names = append(names, lower)
		values[lower] = collapseSpaces(strings.Join(vs, ","))
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(values[name])
		b.WriteByte('\n')
	}
	return strings.Join(names, ";"), b.String()
}

// canonicalURI is the path, each segment percent-encoded to S3's rules.
//
// **Not url.EscapedPath.** S3 wants the path encoded with the unreserved set of
// RFC 3986 and nothing else, so a key containing `+`, `=` or `:` — all of which
// Go leaves alone in a path — signs differently from the way it is transmitted
// unless the encoding is done here. Artifact keys are hex ids and slashes today,
// so this is defensive rather than load-bearing; it stops being defensive the
// first time a prefix contains a space.
func canonicalURI(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}
	segments := strings.Split(u.Path, "/")
	for i, seg := range segments {
		segments[i] = encodeSegment(seg)
	}
	return strings.Join(segments, "/")
}

// canonicalQuery sorts parameters by name and encodes them the same way.
//
// url.Values.Encode sorts by key but encodes a space as `+`, which SigV4 rejects,
// so this does its own pass.
func canonicalQuery(u *url.URL) string {
	query := u.Query()
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		vs := append([]string(nil), query[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, encodeSegment(k)+"="+encodeSegment(v))
		}
	}
	return strings.Join(parts, "&")
}

// encodeSegment percent-encodes everything outside RFC 3986's unreserved set.
func encodeSegment(s string) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

// collapseSpaces trims and folds internal whitespace runs, which is what the
// header canonicalisation rule asks for.
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
