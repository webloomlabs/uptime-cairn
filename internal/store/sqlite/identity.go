package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

// CountUsers answers "is first-run setup still required?". It is the only thing
// standing between an unconfigured install and an open door, so it is a count
// rather than a flag someone can forget to set.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// CreateUser inserts an account. The unique index on (org_id, email) is what
// makes "create the first administrator" safe against two simultaneous callers:
// the second one loses on the constraint rather than on a check-then-act race.
func (s *Store) CreateUser(ctx context.Context, u model.User) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (
			id, org_id, email, name, password_hash, role, active,
			totp_secret, totp_enabled_at, timezone, locale, last_login_at,
			created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID[:], u.OrgID[:], u.Email, nullString(u.Name), u.PasswordHash, u.Role,
		boolToInt(u.Active), nullBytes(u.TOTPSecret), nullMillis(u.TOTPEnabledAt),
		nullString(u.Timezone), nullString(u.Locale), nullMillis(u.LastLoginAt),
		millis(u.CreatedAt), millis(u.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

const userColumns = `id, org_id, email, name, password_hash, role, active,
	totp_secret, totp_enabled_at, timezone, locale, last_login_at, created_at, updated_at`

// UserByEmail looks up a login. Email is stored lowercased so that uniqueness
// means what a user expects it to mean.
func (s *Store) UserByEmail(ctx context.Context, orgID model.ID, email string) (model.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE org_id = ? AND email = ?`, orgID[:], email)
	return scanUser(row)
}

// UserByID looks up the account behind a session.
func (s *Store) UserByID(ctx context.Context, id model.ID) (model.User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id[:])
	return scanUser(row)
}

// SetUserTOTP writes the encrypted secret and the confirmation timestamp
// together, because a secret with no timestamp is an unconfirmed enrolment and
// the two must never disagree.
func (s *Store) SetUserTOTP(ctx context.Context, id model.ID, secret []byte, enabledAt *time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET totp_secret = ?, totp_enabled_at = ?, updated_at = ? WHERE id = ?`,
		nullBytes(secret), nullMillis(enabledAt), millis(time.Now().UTC()), id[:])
	if err != nil {
		return fmt.Errorf("update totp: %w", err)
	}
	return nil
}

// TouchUserLogin records a successful login.
func (s *Store) TouchUserLogin(ctx context.Context, id model.ID, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, millis(at), id[:])
	return err
}

func scanUser(row scanner) (model.User, error) {
	var (
		u                        model.User
		id, orgID, totpSecret    []byte
		name, timezone, locale   sql.NullString
		totpEnabledAt, lastLogin sql.NullInt64
		active                   int64
		createdAt, updatedAt     int64
	)
	if err := row.Scan(&id, &orgID, &u.Email, &name, &u.PasswordHash, &u.Role, &active,
		&totpSecret, &totpEnabledAt, &timezone, &locale, &lastLogin, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.User{}, store.ErrNotFound
		}
		return model.User{}, err
	}

	copy(u.ID[:], id)
	copy(u.OrgID[:], orgID)
	u.Name = name.String
	u.Active = active == 1
	if len(totpSecret) > 0 {
		u.TOTPSecret = append([]byte(nil), totpSecret...)
	}
	u.TOTPEnabledAt = nullableTime(totpEnabledAt)
	u.Timezone = timezone.String
	u.Locale = locale.String
	u.LastLoginAt = nullableTime(lastLogin)
	u.CreatedAt = fromMillis(createdAt)
	u.UpdatedAt = fromMillis(updatedAt)
	return u, nil
}

// ---------------------------------------------------------------------------
// Recovery codes
// ---------------------------------------------------------------------------

// ReplaceRecoveryCodes swaps the whole set, which is what confirming TOTP does.
// Issuing new codes must invalidate the old ones, or a leaked printout stays
// live forever.
func (s *Store) ReplaceRecoveryCodes(ctx context.Context, userID model.ID, hashes [][]byte) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM user_recovery_codes WHERE user_id = ?`, userID[:]); err != nil {
		return fmt.Errorf("clear recovery codes: %w", err)
	}
	now := millis(time.Now().UTC())
	for _, hash := range hashes {
		id := model.NewID()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO user_recovery_codes (id, user_id, code_hash, created_at) VALUES (?,?,?,?)`,
			id[:], userID[:], hash, now); err != nil {
			return fmt.Errorf("insert recovery code: %w", err)
		}
	}
	return tx.Commit()
}

// ConsumeRecoveryCode marks a code used and reports whether it was valid and
// unused. Single-use is enforced by the UPDATE's own WHERE clause, so two
// simultaneous attempts cannot both succeed.
func (s *Store) ConsumeRecoveryCode(ctx context.Context, userID model.ID, hash []byte) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE user_recovery_codes SET used_at = ? WHERE user_id = ? AND code_hash = ? AND used_at IS NULL`,
		millis(time.Now().UTC()), userID[:], hash)
	if err != nil {
		return false, fmt.Errorf("consume recovery code: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// CreateSession stores a login.
func (s *Store) CreateSession(ctx context.Context, sess model.Session) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, user_id, token_hash, csrf_token_hash, expires_at, created_at, last_seen_at, ip, user_agent)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		sess.ID[:], sess.UserID[:], sess.TokenHash, sess.CSRFTokenHash,
		millis(sess.ExpiresAt), millis(sess.CreatedAt), nullMillis(sess.LastSeenAt),
		nullString(sess.IP), nullString(sess.UserAgent))
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// SessionByTokenHash resolves a cookie. Expired rows are treated as absent here
// rather than deleted on the read path: a request should not depend on a write
// succeeding, and DeleteExpiredSessions sweeps them.
func (s *Store) SessionByTokenHash(ctx context.Context, hash []byte, now time.Time) (model.Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, token_hash, csrf_token_hash, expires_at, created_at, last_seen_at, ip, user_agent
		FROM sessions WHERE token_hash = ? AND expires_at > ?`, hash, millis(now))

	var (
		sess              model.Session
		id, userID        []byte
		lastSeen          sql.NullInt64
		expiresAt, create int64
		ip, agent         sql.NullString
	)
	if err := row.Scan(&id, &userID, &sess.TokenHash, &sess.CSRFTokenHash,
		&expiresAt, &create, &lastSeen, &ip, &agent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Session{}, store.ErrNotFound
		}
		return model.Session{}, err
	}

	copy(sess.ID[:], id)
	copy(sess.UserID[:], userID)
	sess.ExpiresAt = fromMillis(expiresAt)
	sess.CreatedAt = fromMillis(create)
	sess.LastSeenAt = nullableTime(lastSeen)
	sess.IP = ip.String
	sess.UserAgent = agent.String
	return sess, nil
}

// TouchSession records activity.
func (s *Store) TouchSession(ctx context.Context, id model.ID, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id = ?`, millis(at), id[:])
	return err
}

// DeleteSession is logout.
func (s *Store) DeleteSession(ctx context.Context, id model.ID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id[:])
	return err
}

// DeleteUserSessions ends every session for an account, which is what a
// credential change should do — otherwise disabling TOTP leaves the sessions
// that were established before it intact.
func (s *Store) DeleteUserSessions(ctx context.Context, userID model.ID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID[:])
	return err
}

// DeleteExpiredSessions sweeps. Cheap, indexed, and run on a timer.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, millis(now))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// API keys
// ---------------------------------------------------------------------------

const apiKeyColumns = `id, org_id, name, prefix, key_hash, scopes, expires_at,
	last_used_at, revoked_at, created_by, created_at, updated_at`

// CreateAPIKey stores a key by its hash. The plaintext never reaches this layer.
func (s *Store) CreateAPIKey(ctx context.Context, k model.APIKey) error {
	scopes, err := json.Marshal(k.Scopes)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (`+apiKeyColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		k.ID[:], k.OrgID[:], k.Name, k.Prefix, k.KeyHash, string(scopes),
		nullMillis(k.ExpiresAt), nullMillis(k.LastUsedAt), nullMillis(k.RevokedAt),
		nullID(k.CreatedBy), millis(k.CreatedAt), millis(k.UpdatedAt)); err != nil {
		return fmt.Errorf("insert api key: %w", err)
	}
	return nil
}

// APIKeyByHash is the authentication lookup. Revoked and expired keys are
// returned rather than filtered, so the caller can tell "no such key" from
// "this key is dead" — the second deserves a different message.
func (s *Store) APIKeyByHash(ctx context.Context, hash []byte) (model.APIKey, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE key_hash = ?`, hash)
	return scanAPIKey(row)
}

// GetAPIKey reads one by id.
func (s *Store) GetAPIKey(ctx context.Context, id model.ID) (model.APIKey, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE id = ?`, id[:])
	return scanAPIKey(row)
}

// ListAPIKeys pages on the same (updated_at, id) cursor as everything else.
func (s *Store) ListAPIKeys(ctx context.Context, after *store.Cursor, limit int) ([]model.APIKey, bool, error) {
	query := `SELECT ` + apiKeyColumns + ` FROM api_keys`
	args := []any{}
	if after != nil {
		query += ` WHERE (updated_at, id) < (?, ?)`
		args = append(args, millis(after.UpdatedAt), after.ID[:])
	}
	query += ` ORDER BY updated_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list api keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// UpdateAPIKey changes name, scopes, and expiry. The key material is not
// touched, because rotating a secret in place would leave every holder of the
// old value silently unauthenticated.
func (s *Store) UpdateAPIKey(ctx context.Context, k model.APIKey) error {
	scopes, err := json.Marshal(k.Scopes)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET name = ?, scopes = ?, expires_at = ?, updated_at = ? WHERE id = ?`,
		k.Name, string(scopes), nullMillis(k.ExpiresAt), millis(k.UpdatedAt), k.ID[:])
	if err != nil {
		return fmt.Errorf("update api key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// RevokeAPIKey is soft and immediate: the row stays so audit entries remain
// resolvable, and authentication checks revoked_at on every request.
func (s *Store) RevokeAPIKey(ctx context.Context, id model.ID, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = ?, updated_at = ? WHERE id = ? AND revoked_at IS NULL`,
		millis(at), millis(at), id[:])
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// TouchAPIKey records use. The caller throttles this to at most once a minute
// per key: written per request, every authenticated call becomes a write, and
// on SQLite that is a write lock on the hottest path in the API.
func (s *Store) TouchAPIKey(ctx context.Context, id model.ID, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`, millis(at), id[:])
	return err
}

func scanAPIKey(row scanner) (model.APIKey, error) {
	var (
		k                          model.APIKey
		id, orgID, createdBy       []byte
		scopes                     string
		expires, lastUsed, revoked sql.NullInt64
		createdAt, updatedAt       int64
	)
	if err := row.Scan(&id, &orgID, &k.Name, &k.Prefix, &k.KeyHash, &scopes,
		&expires, &lastUsed, &revoked, &createdBy, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.APIKey{}, store.ErrNotFound
		}
		return model.APIKey{}, err
	}

	copy(k.ID[:], id)
	copy(k.OrgID[:], orgID)
	if err := json.Unmarshal([]byte(scopes), &k.Scopes); err != nil {
		return model.APIKey{}, fmt.Errorf("api key %s has unreadable scopes: %w", k.ID, err)
	}
	k.ExpiresAt = nullableTime(expires)
	k.LastUsedAt = nullableTime(lastUsed)
	k.RevokedAt = nullableTime(revoked)
	k.CreatedBy = idFromBytes(createdBy)
	k.CreatedAt = fromMillis(createdAt)
	k.UpdatedAt = fromMillis(updatedAt)
	return k, nil
}

// ---------------------------------------------------------------------------
// Encryption keys
// ---------------------------------------------------------------------------

// WrappedKey is a data key as stored: sealed with the root key, which never
// appears in the database (data model §12.3).
type WrappedKey struct {
	Version uint32
	Wrapped []byte
}

// EncryptionKeys returns every data key that has not been retired.
func (s *Store) EncryptionKeys(ctx context.Context) ([]WrappedKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT version, wrapped_dek FROM encryption_keys WHERE retired_at IS NULL ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read encryption keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WrappedKey
	for rows.Next() {
		var k WrappedKey
		if err := rows.Scan(&k.Version, &k.Wrapped); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// InsertEncryptionKey records a new data key version.
func (s *Store) InsertEncryptionKey(ctx context.Context, k WrappedKey, algorithm string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO encryption_keys (version, wrapped_dek, algorithm, created_at) VALUES (?,?,?,?)`,
		k.Version, k.Wrapped, algorithm, millis(at))
	if err != nil {
		return fmt.Errorf("insert encryption key: %w", err)
	}
	return nil
}

// HasEncryptedData reports whether any ciphertext exists. It decides the one
// dangerous startup case: a missing root key with encrypted rows present must
// stop the process, not quietly generate a replacement.
func (s *Store) HasEncryptedData(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE totp_secret IS NOT NULL`).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM encryption_keys`).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// generalSettings is the section this build reads. It is a struct rather than a
// map so that a typo in a key is a compile error rather than a setting that
// silently does nothing.
type generalSettings struct {
	InstanceName string `json:"instance_name,omitempty"`
}

// InstanceName returns the operator-chosen name, or "" when unset.
func (s *Store) InstanceName(ctx context.Context, orgID model.ID) (string, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT general FROM settings WHERE org_id = ?`, orgID[:]).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("read settings: %w", err)
	}

	var general generalSettings
	if err := json.Unmarshal([]byte(raw), &general); err != nil {
		return "", fmt.Errorf("settings.general is unreadable: %w", err)
	}
	return general.InstanceName, nil
}

// SetInstanceName writes the name chosen at setup. It merges into the section
// rather than replacing it, because a later release will put more in there and
// a blind overwrite would quietly drop it.
func (s *Store) SetInstanceName(ctx context.Context, orgID model.ID, name string) error {
	current, err := s.InstanceName(ctx, orgID)
	if err != nil {
		return err
	}
	if current == name {
		return nil
	}

	encoded, err := json.Marshal(generalSettings{InstanceName: name})
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO settings (org_id, general, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (org_id) DO UPDATE SET general = excluded.general, updated_at = excluded.updated_at`,
		orgID[:], string(encoded), millis(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	return nil
}
