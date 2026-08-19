package model

import "time"

// Roles. Only owner occurs in Phase 1 — the rest are in the schema and in the
// scope table from the start so Phase 3's RBAC is a feature rather than a
// migration of every existing row (ADR-003).
const (
	RoleOwner     = "owner"
	RoleAdmin     = "admin"
	RoleEditor    = "editor"
	RoleResponder = "responder"
	RoleViewer    = "viewer"
	RoleBilling   = "billing"
)

// User is an account that can log in.
type User struct {
	ID     ID
	OrgID  ID
	Email  string
	Name   string
	Role   string
	Active bool

	// PasswordHash is argon2id in its encoded form, parameters included.
	PasswordHash string

	// TOTPSecret is the encrypted envelope, not the secret. It is decrypted only
	// at verification time, and never leaves this process (data model §12).
	TOTPSecret []byte

	// TOTPEnabledAt is null when enrolment was started and never confirmed,
	// which is not the same as TOTP being off: an unconfirmed secret must never
	// gate a login.
	TOTPEnabledAt *time.Time

	Timezone    string
	Locale      string
	LastLoginAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TOTPEnabled reports whether two-factor is active for this account.
func (u User) TOTPEnabled() bool { return u.TOTPEnabledAt != nil }

// Session is a browser login. The cookie carries the token; only its hash is
// stored, so a database leak does not hand over live sessions.
type Session struct {
	ID            ID
	UserID        ID
	TokenHash     []byte
	CSRFTokenHash []byte
	ExpiresAt     time.Time
	CreatedAt     time.Time
	LastSeenAt    *time.Time
	IP            string
	UserAgent     string
}

// APIKey is a scoped, expiring, revocable credential for automation. The key
// material exists in exactly one response and nowhere else.
type APIKey struct {
	ID      ID
	OrgID   ID
	Name    string
	Prefix  string
	KeyHash []byte
	Scopes  []string

	ExpiresAt *time.Time
	// LastUsedAt is the signal for finding keys safe to revoke, which is why it
	// is worth the write at all.
	LastUsedAt *time.Time
	// RevokedAt is soft, so a revoked key's audit entries stay resolvable.
	RevokedAt *time.Time
	CreatedBy *ID
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Usable reports whether the key may authenticate a request now.
func (k APIKey) Usable(now time.Time) bool {
	if k.RevokedAt != nil {
		return false
	}
	return k.ExpiresAt == nil || k.ExpiresAt.After(now)
}
