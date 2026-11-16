package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Admin is an operator account for the web admin UI.
type Admin struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}

// CreateAdmin adds an operator account. passwordHash is computed by the
// caller (bcrypt).
func (s *Store) CreateAdmin(ctx context.Context, username, passwordHash string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO admins(username, password_hash, created_at) VALUES (?,?,?)`,
		username, passwordHash, s.now().Unix())
	if isUnique(err) {
		return ErrExists
	}
	return err
}

// SetAdminPassword changes a password and ends all of that admin's sessions.
func (s *Store) SetAdminPassword(ctx context.Context, username, passwordHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRowContext(ctx, `UPDATE admins SET password_hash = ? WHERE username = ? RETURNING id`, passwordHash, username).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_sessions WHERE admin_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteAdmin removes an account (and its sessions).
func (s *Store) DeleteAdmin(ctx context.Context, username string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM admins WHERE username = ?`, username)
	return affectedOne(res, err)
}

// AdminByUsername finds an account.
func (s *Store) AdminByUsername(ctx context.Context, username string) (Admin, error) {
	var a Admin
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id, username, password_hash, created_at FROM admins WHERE username = ?`, username).
		Scan(&a.ID, &a.Username, &a.PasswordHash, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	a.CreatedAt = time.Unix(created, 0).UTC()
	return a, err
}

// ListAdmins returns all accounts (without password hashes).
func (s *Store) ListAdmins(ctx context.Context) ([]Admin, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, username, created_at FROM admins ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Admin
	for rows.Next() {
		var a Admin
		var created int64
		if err := rows.Scan(&a.ID, &a.Username, &created); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountAdmins returns the number of operator accounts.
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admins`).Scan(&n)
	return n, err
}

// Session is a logged-in admin.
type Session struct {
	AdminID   int64
	Username  string
	CSRFToken string
	ExpiresAt time.Time
}

// CreateSession opens a session and returns its cookie token.
func (s *Store) CreateSession(ctx context.Context, adminID int64, ttl time.Duration) (token string, sess Session, err error) {
	token = newSecret("")
	sess = Session{AdminID: adminID, CSRFToken: newSecret(""), ExpiresAt: s.now().Add(ttl).UTC().Truncate(time.Second)}
	_, err = s.db.ExecContext(ctx, `INSERT INTO admin_sessions(token_hash, admin_id, csrf_token, expires_at) VALUES (?,?,?,?)`,
		hashSecret(token), adminID, sess.CSRFToken, sess.ExpiresAt.Unix())
	return token, sess, err
}

// SessionByToken returns a valid (non-expired) session.
func (s *Store) SessionByToken(ctx context.Context, token string) (Session, error) {
	var sess Session
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT s.admin_id, a.username, s.csrf_token, s.expires_at
		FROM admin_sessions s JOIN admins a ON a.id = s.admin_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, hashSecret(token), s.now().Unix()).
		Scan(&sess.AdminID, &sess.Username, &sess.CSRFToken, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return sess, ErrNotFound
	}
	sess.ExpiresAt = time.Unix(exp, 0).UTC()
	return sess, err
}

// DeleteSession ends a session (logout).
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE token_hash = ?`, hashSecret(token))
	return err
}

// AuditEntry is one operator action.
type AuditEntry struct {
	At     time.Time
	Actor  string
	Action string
	Target string
	Detail string
}

// Audit records an operator action. Never pass end-user data here.
func (s *Store) Audit(ctx context.Context, actor, action, target, detail string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_log(at, actor, action, target, detail) VALUES (?,?,?,?,?)`,
		s.now().Unix(), actor, action, target, detail)
	return err
}

// ListAudit returns the most recent entries.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, actor, action, target, detail FROM audit_log ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		if err := rows.Scan(&at, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// Purge deletes data that is no longer needed: expired sessions, decided
// requests older than 30 days (rejected, or approved and claimed), audit
// entries older than a year. Pending and unclaimed requests are kept.
func (s *Store) Purge(ctx context.Context) error {
	now := s.now()
	month := now.Add(-30 * 24 * time.Hour).Unix()
	stmts := []struct {
		q    string
		args []any
	}{
		{`DELETE FROM admin_sessions WHERE expires_at <= ?`, []any{now.Unix()}},
		{`DELETE FROM key_requests WHERE status = 'rejected' AND decided_at < ?`, []any{month}},
		{`DELETE FROM key_requests WHERE status = 'approved' AND (key_claimed_at < ? OR app_id IS NULL AND decided_at < ?)`, []any{month, month}},
		{`DELETE FROM audit_log WHERE at < ?`, []any{now.Add(-365 * 24 * time.Hour).Unix()}},
	}
	for _, st := range stmts {
		if _, err := s.db.ExecContext(ctx, st.q, st.args...); err != nil {
			return err
		}
	}
	return nil
}
