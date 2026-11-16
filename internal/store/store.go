// Package store persists applications, push subscriptions and reminders in
// SQLite. It stores nothing but what is needed to fire a push at the right
// time: no reminder text, no user identity.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Errors returned by the store.
var (
	ErrNotFound      = errors.New("not found")
	ErrExists        = errors.New("already exists")
	ErrQuotaExceeded = errors.New("quota exceeded")
)

// Store wraps the SQLite database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Subscription is a browser push subscription (one device + one app).
type Subscription struct {
	ID       int64
	AppID    int64
	Endpoint string
	P256dh   string
	Auth     string
}

// Reminder is a scheduled push.
type Reminder struct {
	ID             int64
	SubscriptionID int64
	OpaqueID       string
	Schedule       string // normalized JSON
	NextRunAt      time.Time
	AttemptAt      time.Time
	Attempts       int
}

// Due is a reminder that must be sent now, with its subscription.
type Due struct {
	Reminder
	Sub Subscription
}

// NewReminder is the input for creating or replacing a reminder.
type NewReminder struct {
	OpaqueID  string
	Schedule  string
	NextRunAt time.Time
}

// Open opens (and creates if needed) the database at path. Use ":memory:"
// for tests.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	if path == ":memory:" {
		dsn = "file::memory:?_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One process, one writer: a single connection avoids SQLITE_BUSY and
	// keeps :memory: databases consistent.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database is usable.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// --- secrets ---------------------------------------------------------------

func newSecret(prefix string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// --- subscriptions ----------------------------------------------------------

// RegisterSubscription stores a push subscription for an app and returns a
// new management token. Registering an endpoint that already exists for the
// app updates its keys and rotates the token (holding the subscription
// itself proves ownership of the device); its reminders are kept.
func (s *Store) RegisterSubscription(ctx context.Context, app App, endpoint, p256dh, auth string) (token string, created bool, err error) {
	token = newSecret("sub_")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE subscriptions SET p256dh = ?, auth = ?, token_hash = ? WHERE app_id = ? AND endpoint = ?`,
		p256dh, auth, hashSecret(token), app.ID, endpoint)
	if err != nil {
		return "", false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM subscriptions WHERE app_id = ?`, app.ID).Scan(&count); err != nil {
			return "", false, err
		}
		if count >= app.MaxSubscriptions {
			return "", false, ErrQuotaExceeded
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO subscriptions(app_id, endpoint, p256dh, auth, token_hash, created_at) VALUES (?,?,?,?,?,?)`,
			app.ID, endpoint, p256dh, auth, hashSecret(token), s.now().Unix()); err != nil {
			return "", false, err
		}
		created = true
	}
	return token, created, tx.Commit()
}

// SubscriptionByToken finds an app's subscription from its management token.
func (s *Store) SubscriptionByToken(ctx context.Context, appID int64, token string) (Subscription, error) {
	var sub Subscription
	err := s.db.QueryRowContext(ctx,
		`SELECT id, app_id, endpoint, p256dh, auth FROM subscriptions WHERE token_hash = ? AND app_id = ?`,
		hashSecret(token), appID).Scan(&sub.ID, &sub.AppID, &sub.Endpoint, &sub.P256dh, &sub.Auth)
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrNotFound
	}
	return sub, err
}

// UpdateSubscription replaces the endpoint and keys of a subscription (after
// a browser "pushsubscriptionchange"), keeping its token and reminders.
func (s *Store) UpdateSubscription(ctx context.Context, id int64, endpoint, p256dh, auth string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions SET endpoint = ?, p256dh = ?, auth = ? WHERE id = ?`, endpoint, p256dh, auth, id)
	if isUnique(err) {
		return ErrExists
	}
	return err
}

// DeleteSubscription removes a subscription and all its reminders.
func (s *Store) DeleteSubscription(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM subscriptions WHERE id = ?`, id)
	return err
}

// --- reminders --------------------------------------------------------------

// PutReminder creates or replaces a reminder, identified by its opaque ID
// within the subscription.
func (s *Store) PutReminder(ctx context.Context, subID int64, maxPerSub int, r NewReminder) (created bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE reminders SET schedule = ?, next_run_at = ?, attempt_at = ?, attempts = 0 WHERE subscription_id = ? AND opaque_id = ?`,
		r.Schedule, r.NextRunAt.Unix(), r.NextRunAt.Unix(), subID, r.OpaqueID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM reminders WHERE subscription_id = ?`, subID).Scan(&count); err != nil {
			return false, err
		}
		if count >= maxPerSub {
			return false, ErrQuotaExceeded
		}
		if err := insertReminder(ctx, tx, subID, r); err != nil {
			return false, err
		}
		created = true
	}
	return created, tx.Commit()
}

// ReplaceReminders atomically replaces all reminders of a subscription.
func (s *Store) ReplaceReminders(ctx context.Context, subID int64, maxPerSub int, rs []NewReminder) error {
	if len(rs) > maxPerSub {
		return ErrQuotaExceeded
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM reminders WHERE subscription_id = ?`, subID); err != nil {
		return err
	}
	for _, r := range rs {
		if err := insertReminder(ctx, tx, subID, r); err != nil {
			if isUnique(err) {
				return fmt.Errorf("%w: duplicate id %q", ErrExists, r.OpaqueID)
			}
			return err
		}
	}
	return tx.Commit()
}

func insertReminder(ctx context.Context, tx *sql.Tx, subID int64, r NewReminder) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO reminders(subscription_id, opaque_id, schedule, next_run_at, attempt_at) VALUES (?,?,?,?,?)`,
		subID, r.OpaqueID, r.Schedule, r.NextRunAt.Unix(), r.NextRunAt.Unix())
	return err
}

// DeleteReminder removes one reminder.
func (s *Store) DeleteReminder(ctx context.Context, subID int64, opaqueID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM reminders WHERE subscription_id = ? AND opaque_id = ?`, subID, opaqueID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const reminderCols = `r.id, r.subscription_id, r.opaque_id, r.schedule, r.next_run_at, r.attempt_at, r.attempts`

// ListReminders returns the reminders of a subscription, soonest first.
func (s *Store) ListReminders(ctx context.Context, subID int64) ([]Reminder, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+reminderCols+` FROM reminders r WHERE r.subscription_id = ? ORDER BY r.next_run_at, r.opaque_id`, subID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reminder
	for rows.Next() {
		var r Reminder
		var next, attempt int64
		if err := rows.Scan(&r.ID, &r.SubscriptionID, &r.OpaqueID, &r.Schedule, &next, &attempt, &r.Attempts); err != nil {
			return nil, err
		}
		r.NextRunAt, r.AttemptAt = time.Unix(next, 0).UTC(), time.Unix(attempt, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// DueReminders returns up to limit reminders whose attempt time is reached.
func (s *Store) DueReminders(ctx context.Context, now time.Time, limit int) ([]Due, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+reminderCols+`, s.id, s.app_id, s.endpoint, s.p256dh, s.auth
		 FROM reminders r
		 JOIN subscriptions s ON s.id = r.subscription_id
		 JOIN apps a ON a.id = s.app_id
		 WHERE r.attempt_at <= ? AND a.disabled = 0
		 ORDER BY r.attempt_at LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Due
	for rows.Next() {
		var d Due
		var next, attempt int64
		if err := rows.Scan(&d.ID, &d.SubscriptionID, &d.OpaqueID, &d.Schedule, &next, &attempt, &d.Attempts,
			&d.Sub.ID, &d.Sub.AppID, &d.Sub.Endpoint, &d.Sub.P256dh, &d.Sub.Auth); err != nil {
			return nil, err
		}
		d.NextRunAt, d.AttemptAt = time.Unix(next, 0).UTC(), time.Unix(attempt, 0).UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

// Advance moves a reminder to its next occurrence, or deletes it when there
// is none (next is zero).
func (s *Store) Advance(ctx context.Context, id int64, next time.Time) error {
	if next.IsZero() {
		_, err := s.db.ExecContext(ctx, `DELETE FROM reminders WHERE id = ?`, id)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE reminders SET next_run_at = ?, attempt_at = ?, attempts = 0 WHERE id = ?`, next.Unix(), next.Unix(), id)
	return err
}

// RetryAt schedules another delivery attempt for the current occurrence.
func (s *Store) RetryAt(ctx context.Context, id int64, at time.Time, attempts int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE reminders SET attempt_at = ?, attempts = ? WHERE id = ?`, at.Unix(), attempts, id)
	return err
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
