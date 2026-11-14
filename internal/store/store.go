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

const schema = `
CREATE TABLE IF NOT EXISTS apps (
	id                    INTEGER PRIMARY KEY,
	name                  TEXT    NOT NULL UNIQUE,
	key_hash              TEXT    NOT NULL UNIQUE,
	origins               TEXT    NOT NULL DEFAULT '',
	max_subscriptions     INTEGER NOT NULL,
	max_reminders_per_sub INTEGER NOT NULL,
	created_at            INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS subscriptions (
	id         INTEGER PRIMARY KEY,
	app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
	endpoint   TEXT    NOT NULL,
	p256dh     TEXT    NOT NULL,
	auth       TEXT    NOT NULL,
	token_hash TEXT    NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	UNIQUE (app_id, endpoint)
);
CREATE TABLE IF NOT EXISTS reminders (
	id              INTEGER PRIMARY KEY,
	subscription_id INTEGER NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
	opaque_id       TEXT    NOT NULL,
	schedule        TEXT    NOT NULL,
	next_run_at     INTEGER NOT NULL, -- occurrence being fired (unix seconds, UTC)
	attempt_at      INTEGER NOT NULL, -- when to (re)try sending it
	attempts        INTEGER NOT NULL DEFAULT 0,
	UNIQUE (subscription_id, opaque_id)
);
CREATE INDEX IF NOT EXISTS reminders_attempt_at ON reminders(attempt_at);
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
INSERT OR IGNORE INTO meta(key, value) VALUES ('schema_version', '1');
`

// Store wraps the SQLite database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// App is a client application allowed to use the server.
type App struct {
	ID                 int64
	Name               string
	Origins            []string // allowed browser origins; empty = any
	MaxSubscriptions   int
	MaxRemindersPerSub int
	CreatedAt          time.Time
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
	if _, err := db.Exec(schema); err != nil {
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

// --- apps -------------------------------------------------------------------

// CreateApp registers an application and returns its API key. The key is
// only stored hashed: it cannot be shown again.
func (s *Store) CreateApp(ctx context.Context, name string, origins []string, maxSubs, maxRemPerSub int) (App, string, error) {
	key := newSecret("app_")
	now := s.now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO apps(name, key_hash, origins, max_subscriptions, max_reminders_per_sub, created_at) VALUES (?,?,?,?,?,?)`,
		name, hashSecret(key), strings.Join(origins, " "), maxSubs, maxRemPerSub, now.Unix())
	if err != nil {
		if isUnique(err) {
			return App{}, "", ErrExists
		}
		return App{}, "", err
	}
	id, _ := res.LastInsertId()
	return App{ID: id, Name: name, Origins: origins, MaxSubscriptions: maxSubs, MaxRemindersPerSub: maxRemPerSub, CreatedAt: now.Truncate(time.Second)}, key, nil
}

// RotateAppKey replaces an application's API key.
func (s *Store) RotateAppKey(ctx context.Context, name string) (string, error) {
	key := newSecret("app_")
	res, err := s.db.ExecContext(ctx, `UPDATE apps SET key_hash = ? WHERE name = ?`, hashSecret(key), name)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}
	return key, nil
}

// UpdateApp changes an application's origins and quotas.
func (s *Store) UpdateApp(ctx context.Context, a App) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE apps SET origins = ?, max_subscriptions = ?, max_reminders_per_sub = ? WHERE name = ?`,
		strings.Join(a.Origins, " "), a.MaxSubscriptions, a.MaxRemindersPerSub, a.Name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteApp removes an application with all its subscriptions and reminders.
func (s *Store) DeleteApp(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM apps WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const appCols = `id, name, origins, max_subscriptions, max_reminders_per_sub, created_at`

func scanApp(row interface{ Scan(...any) error }) (App, error) {
	var a App
	var origins string
	var created int64
	if err := row.Scan(&a.ID, &a.Name, &origins, &a.MaxSubscriptions, &a.MaxRemindersPerSub, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return App{}, ErrNotFound
		}
		return App{}, err
	}
	a.Origins = strings.Fields(origins)
	a.CreatedAt = time.Unix(created, 0).UTC()
	return a, nil
}

// AppByKey finds the application owning an API key.
func (s *Store) AppByKey(ctx context.Context, key string) (App, error) {
	return scanApp(s.db.QueryRowContext(ctx, `SELECT `+appCols+` FROM apps WHERE key_hash = ?`, hashSecret(key)))
}

// AppByName finds an application by name.
func (s *Store) AppByName(ctx context.Context, name string) (App, error) {
	return scanApp(s.db.QueryRowContext(ctx, `SELECT `+appCols+` FROM apps WHERE name = ?`, name))
}

// AppStats are counters shown by the admin CLI.
type AppStats struct {
	App
	Subscriptions int
	Reminders     int
}

// ListApps returns all applications with counters.
func (s *Store) ListApps(ctx context.Context) ([]AppStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+appCols+` FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var apps []AppStats
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		apps = append(apps, AppStats{App: a})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range apps {
		err := s.db.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM subscriptions WHERE app_id = ?),
			(SELECT COUNT(*) FROM reminders r JOIN subscriptions s ON s.id = r.subscription_id WHERE s.app_id = ?)`,
			apps[i].ID, apps[i].ID).Scan(&apps[i].Subscriptions, &apps[i].Reminders)
		if err != nil {
			return nil, err
		}
	}
	return apps, nil
}

// OriginAllowedByAnyApp reports whether a browser origin may call the API at
// all (used for CORS preflight, which carries no API key).
func (s *Store) OriginAllowedByAnyApp(ctx context.Context, origin string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT origins FROM apps`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var origins string
		if err := rows.Scan(&origins); err != nil {
			return false, err
		}
		if OriginAllowed(strings.Fields(origins), origin) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// OriginAllowed reports whether origin is in the list; an empty list allows
// any origin.
func OriginAllowed(origins []string, origin string) bool {
	if len(origins) == 0 {
		return true
	}
	for _, o := range origins {
		if o == "*" || strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
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
		 FROM reminders r JOIN subscriptions s ON s.id = r.subscription_id
		 WHERE r.attempt_at <= ? ORDER BY r.attempt_at LIMIT ?`, now.Unix(), limit)
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
