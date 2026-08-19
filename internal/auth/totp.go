package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 fixes SHA-1 for TOTP; every authenticator app implements that and nothing else
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP parameters, all of them fixed by what authenticator apps actually
// implement rather than by preference: RFC 6238 with SHA-1, six digits, a
// thirty-second step.
const (
	totpDigits = 6
	totpPeriod = 30 * time.Second
	totpSecret = 20 // bytes, the RFC 4226 recommendation

	// totpSkew accepts the neighbouring steps, which covers a phone clock a few
	// seconds out and a user typing the last digit as the code rolls over.
	// Wider windows trade real security for convenience nobody asked for.
	totpSkew = 1
)

// NewTOTPSecret returns a fresh shared secret.
func NewTOTPSecret() ([]byte, error) {
	secret := make([]byte, totpSecret)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("auth: totp secret: %w", err)
	}
	return secret, nil
}

// EncodeTOTPSecret renders the secret as unpadded base32, which is the form
// every authenticator expects to be typed or scanned.
func EncodeTOTPSecret(secret []byte) string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
}

// ProvisioningURI builds the otpauth:// URI a QR code encodes.
func ProvisioningURI(issuer, account string, secret []byte) string {
	query := url.Values{}
	query.Set("secret", EncodeTOTPSecret(secret))
	query.Set("issuer", issuer)
	query.Set("algorithm", "SHA1")
	query.Set("digits", fmt.Sprint(totpDigits))
	query.Set("period", fmt.Sprint(int(totpPeriod.Seconds())))

	label := url.PathEscape(issuer + ":" + account)
	return "otpauth://totp/" + label + "?" + query.Encode()
}

// VerifyTOTP reports whether code is valid for secret at time now.
func VerifyTOTP(secret []byte, code string, now time.Time) bool {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return false
	}

	counter := now.Unix() / int64(totpPeriod.Seconds())
	for offset := -totpSkew; offset <= totpSkew; offset++ {
		// Constant-time compare so a wrong code cannot be narrowed down by
		// timing which prefix matched.
		if subtleEqualString(code, totpAt(secret, counter+int64(offset))) {
			return true
		}
	}
	return false
}

// totpAt is HOTP (RFC 4226) over the time-based counter.
func totpAt(secret []byte, counter int64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(counter))

	mac := hmac.New(sha1.New, secret)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	// Dynamic truncation: the low nibble of the last byte picks the offset.
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff

	mod := uint32(1)
	for range totpDigits {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, value%mod)
}

func subtleEqualString(a, b string) bool {
	return EqualTokens([]byte(a), []byte(b))
}
