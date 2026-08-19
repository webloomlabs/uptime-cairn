package auth

import (
	"strings"
	"testing"
	"time"
)

func TestPasswordHashing(t *testing.T) {
	t.Parallel()

	hash, err := HashPassword("correct-horse-battery")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("hash = %q, want the PHC encoded form", hash)
	}
	// The parameters travel with the hash, so raising the cost later leaves
	// existing accounts able to log in.
	if !strings.Contains(hash, "m=19456,t=2,p=1") {
		t.Errorf("hash = %q, want it to record its parameters", hash)
	}

	ok, err := VerifyPassword("correct-horse-battery", hash)
	if err != nil || !ok {
		t.Errorf("VerifyPassword(correct) = %v, %v; want true, nil", ok, err)
	}

	ok, err = VerifyPassword("correct-horse-batteru", hash)
	if err != nil || ok {
		t.Errorf("VerifyPassword(wrong) = %v, %v; want false, nil", ok, err)
	}
}

// Two hashes of one password must differ, or the salt is not doing its job and
// a stolen table becomes a lookup exercise.
func TestPasswordHashesAreSalted(t *testing.T) {
	t.Parallel()

	first, _ := HashPassword("same-password-twice")
	second, _ := HashPassword("same-password-twice")
	if first == second {
		t.Error("two hashes of the same password are identical: the salt is missing")
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"", "plaintext", "$argon2id$v=19$broken", "$bcrypt$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA"} {
		if _, err := VerifyPassword("whatever", bad); err == nil {
			t.Errorf("VerifyPassword against %q returned no error", bad)
		}
	}
}

func TestTOTPRoundTrip(t *testing.T) {
	t.Parallel()

	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)

	code := totpAt(secret, now.Unix()/30)
	if !VerifyTOTP(secret, code, now) {
		t.Error("a freshly generated code did not verify")
	}
	if len(code) != totpDigits {
		t.Errorf("code = %q, want %d digits", code, totpDigits)
	}

	// One step either side is accepted: a phone clock a few seconds out, or a
	// user typing the last digit as the code rolls over.
	if !VerifyTOTP(secret, code, now.Add(25*time.Second)) {
		t.Error("the previous step's code was rejected inside the skew window")
	}
	// Two steps out is not.
	if VerifyTOTP(secret, code, now.Add(90*time.Second)) {
		t.Error("a code two steps old was accepted; the skew window is too wide")
	}
	if VerifyTOTP(secret, "000000", now) && code != "000000" {
		t.Error("an arbitrary code verified")
	}
}

// The URI is what a QR code encodes, and getting a parameter wrong means an
// authenticator that silently produces codes nothing accepts.
func TestProvisioningURI(t *testing.T) {
	t.Parallel()

	uri := ProvisioningURI("Cairn Dev", "someone@example.com", []byte("12345678901234567890"))
	for _, want := range []string{"otpauth://totp/", "algorithm=SHA1", "digits=6", "period=30", "issuer=Cairn+Dev"} {
		if !strings.Contains(uri, want) {
			t.Errorf("uri = %q, want it to contain %q", uri, want)
		}
	}
}

func TestScopeImplication(t *testing.T) {
	t.Parallel()

	// write implies read on the same resource, and nothing implies anything
	// across resources.
	cases := []struct {
		held, want Scope
		grants     bool
	}{
		{ScopeMonitorsWrite, ScopeMonitorsRead, true},
		{ScopeMonitorsWrite, ScopeMonitorsWrite, true},
		{ScopeMonitorsRead, ScopeMonitorsWrite, false},
		{ScopeMonitorsWrite, ScopeHeartbeatsRead, false},
		{ScopeAPIKeysWrite, ScopeAPIKeysRead, true},
		{ScopeStatusPagesWrite, ScopeStatusPagesRead, true},
	}
	for _, tc := range cases {
		if got := (Set{tc.held}).Grants(tc.want); got != tc.grants {
			t.Errorf("Set{%s}.Grants(%s) = %v, want %v", tc.held, tc.want, got, tc.grants)
		}
	}
}

// Covers is what stops a key minting a stronger key than its creator holds.
func TestScopeCovers(t *testing.T) {
	t.Parallel()

	creator := Set{ScopeAPIKeysWrite, ScopeMonitorsRead}
	if creator.Covers(Set{ScopeMonitorsWrite}) {
		t.Error("a creator holding only monitors:read covered monitors:write")
	}
	if !creator.Covers(Set{ScopeMonitorsRead, ScopeAPIKeysRead}) {
		t.Error("a creator did not cover scopes it holds, read implied by write included")
	}
}

func TestRoleScopes(t *testing.T) {
	t.Parallel()

	if !ScopesFor("owner").Grants(ScopeAPIKeysWrite) {
		t.Error("owner cannot manage API keys")
	}
	if ScopesFor("viewer").Grants(ScopeMonitorsWrite) {
		t.Error("viewer can write monitors")
	}
	if ScopesFor("nonsense") != nil {
		t.Error("an unknown role was granted scopes")
	}
}

func TestAPIKeyShape(t *testing.T) {
	t.Parallel()

	key, prefix, err := NewAPIKey()
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	if !strings.HasPrefix(key, KeyPrefix) {
		t.Errorf("key = %q, want the %q namespace", key, KeyPrefix)
	}
	if !strings.HasPrefix(key, prefix) || len(prefix) != len(KeyPrefix)+4 {
		t.Errorf("prefix = %q, want the first four characters after the namespace", prefix)
	}

	// The prefix is stored in listings, so it must not be enough to reconstruct
	// the key.
	if len(key) < 40 {
		t.Errorf("key is %d characters, too short to be 256 bits of randomness", len(key))
	}

	other, _, _ := NewAPIKey()
	if other == key {
		t.Error("two generated keys were identical")
	}
}

func TestRecoveryCodes(t *testing.T) {
	t.Parallel()

	codes, err := NewRecoveryCodes(10)
	if err != nil {
		t.Fatalf("NewRecoveryCodes: %v", err)
	}
	if len(codes) != 10 {
		t.Fatalf("got %d codes, want 10", len(codes))
	}

	seen := map[string]bool{}
	for _, code := range codes {
		if seen[code] {
			t.Errorf("duplicate recovery code %q", code)
		}
		seen[code] = true
		if len(code) != 14 { // xxxx-xxxx-xxxx
			t.Errorf("code %q is %d characters, want 14", code, len(code))
		}
		// Characters that are misread off a screen are excluded on purpose.
		if strings.ContainsAny(code, "01ilo") {
			t.Errorf("code %q contains an ambiguous character", code)
		}
	}
}

func TestBearerToken(t *testing.T) {
	t.Parallel()

	if token, ok := BearerToken("Bearer cairn_abc"); !ok || token != "cairn_abc" {
		t.Errorf("BearerToken = %q, %v; want cairn_abc, true", token, ok)
	}
	for _, bad := range []string{"", "cairn_abc", "Basic dXNlcjpwYXNz", "Bearer "} {
		if _, ok := BearerToken(bad); ok {
			t.Errorf("BearerToken(%q) accepted", bad)
		}
	}
}
