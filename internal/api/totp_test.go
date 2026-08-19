package api

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // the test has to speak the same algorithm RFC 6238 fixes
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// generateTOTP is an independent implementation of the code an authenticator
// app would produce. Deliberately not calling into internal/auth: a test that
// reuses the implementation it is checking proves only that the code is
// self-consistent.
func generateTOTP(t *testing.T, secret string) string {
	t.Helper()

	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}

	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(time.Now().Unix()/30))

	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1_000_000)
}

func TestTOTPEnrolmentAndLogin(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	owner := newClient(t, server)
	owner.setup()

	resp, enrolment := owner.do(http.MethodPost, "/api/v1/auth/totp", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enrol = %d, want 200 (%v)", resp.StatusCode, enrolment)
	}
	secret, _ := enrolment["secret"].(string)
	if secret == "" {
		t.Fatal("enrolment returned no secret")
	}
	if uri, _ := enrolment["provisioning_uri"].(string); uri == "" {
		t.Error("enrolment returned no provisioning URI")
	}

	// Enrolment alone must not gate logins: an abandoned enrolment would
	// otherwise lock the account.
	before := newClient(t, server)
	resp, _ = before.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "owner@example.com", "password": "correct-horse-battery",
	})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("login during unconfirmed enrolment = %d, want 200", resp.StatusCode)
	}

	resp, _ = owner.do(http.MethodPost, "/api/v1/auth/totp/confirm", map[string]string{"totp_code": "000000"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("confirm with a wrong code = %d, want 422", resp.StatusCode)
	}

	resp, confirmed := owner.do(http.MethodPost, "/api/v1/auth/totp/confirm",
		map[string]string{"totp_code": generateTOTP(t, secret)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm = %d, want 200 (%v)", resp.StatusCode, confirmed)
	}
	codes, _ := confirmed["recovery_codes"].([]any)
	if len(codes) != recoveryCodeCount {
		t.Fatalf("got %d recovery codes, want %d", len(codes), recoveryCodeCount)
	}

	// Now the password alone is not enough, and the client is told what to send.
	fresh := newClient(t, server)
	resp, problem := fresh.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "owner@example.com", "password": "correct-horse-battery",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login without a code = %d, want 401", resp.StatusCode)
	}
	if problem["type"] != errorBase+"totp-required" {
		t.Errorf("type = %v, want the totp-required problem", problem["type"])
	}

	resp, _ = fresh.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "owner@example.com", "password": "correct-horse-battery",
		"totp_code": generateTOTP(t, secret),
	})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("login with a valid code = %d, want 200", resp.StatusCode)
	}

	// A wrong password with a right code is still a failure.
	resp, _ = newClient(t, server).do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "owner@example.com", "password": "wrong", "totp_code": generateTOTP(t, secret),
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("login with a wrong password and a valid code = %d, want 401", resp.StatusCode)
	}

	recovery, _ := codes[0].(string)
	byRecovery := newClient(t, server)
	resp, _ = byRecovery.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "owner@example.com", "password": "correct-horse-battery", "recovery_code": recovery,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login with a recovery code = %d, want 200", resp.StatusCode)
	}

	// Single use, and the second attempt proves the row is consumed rather than
	// merely checked.
	resp, _ = newClient(t, server).do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "owner@example.com", "password": "correct-horse-battery", "recovery_code": recovery,
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("reusing a recovery code = %d, want 401", resp.StatusCode)
	}
}

// Disabling the second factor needs proof the caller still holds it. A stolen
// session must not be enough to remove the protection on the account.
func TestTOTPDisableNeedsPasswordAndCode(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	owner := newClient(t, server)
	owner.setup()

	_, enrolment := owner.do(http.MethodPost, "/api/v1/auth/totp", nil)
	secret, _ := enrolment["secret"].(string)
	owner.do(http.MethodPost, "/api/v1/auth/totp/confirm", map[string]string{"totp_code": generateTOTP(t, secret)})

	resp, _ := owner.do(http.MethodDelete, "/api/v1/auth/totp", map[string]string{
		"password": "correct-horse-battery",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("disable without a code = %d, want 422", resp.StatusCode)
	}

	resp, _ = owner.do(http.MethodDelete, "/api/v1/auth/totp", map[string]string{
		"password": "wrong-password", "totp_code": generateTOTP(t, secret),
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("disable with a wrong password = %d, want 401", resp.StatusCode)
	}

	resp, _ = owner.do(http.MethodDelete, "/api/v1/auth/totp", map[string]string{
		"password": "correct-horse-battery", "totp_code": generateTOTP(t, secret),
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("disable = %d, want 204", resp.StatusCode)
	}

	// And the account logs in with the password alone again.
	resp, _ = newClient(t, server).do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "owner@example.com", "password": "correct-horse-battery",
	})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("login after disabling TOTP = %d, want 200", resp.StatusCode)
	}
}
