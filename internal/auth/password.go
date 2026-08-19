// Package auth implements the credentials side of the API: password hashing,
// opaque tokens, TOTP, and scopes.
//
// One rule runs through it, from data model §12.1: hash what you verify,
// encrypt what you replay. Passwords, session tokens, API keys, and recovery
// codes are only ever compared, so they are hashed and are not recoverable by
// anyone, including us. A TOTP secret has to be fed to the algorithm on every
// login, so it is encrypted instead.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters. PHASE-1-PLAN.md §4.1 names argon2id specifically; these
// are the OWASP-recommended settings, chosen to be affordable on the smallest
// hardware this product targets — 19 MiB per login is nothing on a server and
// survivable on a Raspberry Pi.
//
// They are encoded into every hash, so raising them later is a one-line change
// that leaves existing hashes verifying exactly as before.
const (
	argonMemory  = 19 * 1024 // KiB
	argonTime    = 2
	argonThreads = 1
	argonSaltLen = 16
	argonKeyLen  = 32
)

// MinPasswordLength is the floor the spec sets. Length, not composition rules:
// a 12-character passphrase beats eight characters of punctuation theatre.
const MinPasswordLength = 12

// ErrInvalidHash means the stored hash is not in the encoded form this package
// writes — a corrupted row or a hash from another tool.
var ErrInvalidHash = errors.New("auth: password hash is not in the expected encoded form")

// HashPassword returns the PHC-style encoded hash, parameters included.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: salt: %w", err)
	}

	sum := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum)), nil
}

// VerifyPassword reports whether the password matches, using the parameters
// recorded in the hash rather than the current constants — otherwise raising the
// cost would lock out every existing account.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrInvalidHash
	}
	if version != argon2.Version {
		return false, fmt.Errorf("auth: hash uses argon2 version %d, this build has %d", version, argon2.Version)
	}

	var memory uint32
	var time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrInvalidHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrInvalidHash
	}

	got := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(want)))

	// Constant time: a comparison that returns early leaks how much of the hash
	// matched, one byte at a time.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
