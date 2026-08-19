package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
)

// KeyPrefix is the namespace for API keys. Probe credentials live in a
// different one and can do nothing on the REST API (ADR-005 decision 8); a
// prefix on the wire is what makes a leaked key identifiable in a log or a
// secret scanner.
const KeyPrefix = "cairn_"

// tokenBytes is 256 bits of randomness. These are looked up by hash, so the only
// defence is that guessing one is infeasible.
const tokenBytes = 32

// NewToken returns a fresh opaque token, URL-safe and unpadded.
func NewToken() (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// NewAPIKey returns a key and the prefix stored beside its hash. The prefix is
// non-secret and exists so an operator can recognise a key in a listing without
// the listing being able to authenticate as it.
func NewAPIKey() (key string, prefix string, err error) {
	token, err := NewToken()
	if err != nil {
		return "", "", err
	}
	key = KeyPrefix + token
	return key, key[:len(KeyPrefix)+4], nil
}

// HashToken is sha256, not argon2id, and the difference is deliberate: these
// tokens are 256 random bits, so there is no dictionary to slow down, and an
// expensive hash on the authentication path would turn every API call into a
// CPU cost. Passwords are the opposite case, and use argon2id.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// EqualTokens compares in constant time.
func EqualTokens(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// BearerToken pulls the credential out of an Authorization header.
func BearerToken(header string) (string, bool) {
	value, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	return value, true
}

// recoveryAlphabet omits characters that are misread when copied off a screen:
// no 0/O, no 1/I/l.
const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// NewRecoveryCodes returns single-use codes for when the authenticator is gone.
// Shown once, stored hashed, and consumed individually — which is why each one
// gets its own row and its own used_at.
func NewRecoveryCodes(count int) ([]string, error) {
	codes := make([]string, 0, count)
	for range count {
		var b strings.Builder
		for group := range 3 {
			if group > 0 {
				b.WriteByte('-')
			}
			for range 4 {
				pick := make([]byte, 1)
				if _, err := rand.Read(pick); err != nil {
					return nil, fmt.Errorf("auth: recovery code: %w", err)
				}
				b.WriteByte(recoveryAlphabet[int(pick[0])%len(recoveryAlphabet)])
			}
		}
		codes = append(codes, b.String())
	}
	return codes, nil
}
