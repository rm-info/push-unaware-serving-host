package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// App is a client application allowed to use the server.
type App struct {
	ID                 int64
	Name               string
	Origins            []string // allowed browser origins; empty = any
	MaxSubscriptions   int
	MaxRemindersPerSub int
	Disabled           bool   // API access refused and pushes paused
	HasKey             bool   // false until an approved requester claims the key
	Contact            string // how to reach the app's maintainer (operator-facing)
	Description        string
	CreatedAt          time.Time
}

// NewApp is the input for creating an application.
type NewApp struct {
	Name               string
	Origins            []string
	MaxSubscriptions   int
	MaxRemindersPerSub int
	Contact            string
	Description        string
}

// CreateApp registers an application and returns its API key. The key is
// only stored hashed: it cannot be shown again.
func (s *Store) CreateApp(ctx context.Context, in NewApp) (App, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return App{}, "", err
	}
	defer tx.Rollback()
	key := newSecret("app_")
	app, err := s.insertApp(ctx, tx, in, hashSecret(key))
	if err != nil {
		return App{}, "", err
	}
	return app, key, tx.Commit()
}

// insertApp inserts an app; keyHash may be "" for an app whose key will be
// claimed later.
func (s *Store) insertApp(ctx context.Context, tx *sql.Tx, in NewApp, keyHash string) (App, error) {
	now := s.now().UTC()
	var kh any
	if keyHash != "" {
		kh = keyHash
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO apps(name, key_hash, origins, max_subscriptions, max_reminders_per_sub, contact, description, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		in.Name, kh, strings.Join(in.Origins, " "), in.MaxSubscriptions, in.MaxRemindersPerSub, in.Contact, in.Description, now.Unix())
	if err != nil {
		if isUnique(err) {
			return App{}, ErrExists
		}
		return App{}, err
	}
	id, _ := res.LastInsertId()
	return App{ID: id, Name: in.Name, Origins: in.Origins, MaxSubscriptions: in.MaxSubscriptions,
		MaxRemindersPerSub: in.MaxRemindersPerSub, HasKey: keyHash != "", Contact: in.Contact,
		Description: in.Description, CreatedAt: now.Truncate(time.Second)}, nil
}

// RotateAppKey replaces an application's API key; the old one stops working
// immediately.
func (s *Store) RotateAppKey(ctx context.Context, name string) (string, error) {
	key := newSecret("app_")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRowContext(ctx, `UPDATE apps SET key_hash = ? WHERE name = ? RETURNING id`, hashSecret(key), name).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	// A requester who has not claimed a key yet must not get another one.
	if _, err := tx.ExecContext(ctx,
		`UPDATE key_requests SET key_claimed_at = ? WHERE app_id = ? AND key_claimed_at IS NULL`, s.now().Unix(), id); err != nil {
		return "", err
	}
	return key, tx.Commit()
}

// UpdateApp changes an application's origins and quotas.
func (s *Store) UpdateApp(ctx context.Context, a App) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE apps SET origins = ?, max_subscriptions = ?, max_reminders_per_sub = ? WHERE name = ?`,
		strings.Join(a.Origins, " "), a.MaxSubscriptions, a.MaxRemindersPerSub, a.Name)
	return affectedOne(res, err)
}

// SetAppDisabled disables (revokes) or re-enables an application. A disabled
// app's API calls are refused and its pushes are paused; occurrences missed
// meanwhile are skipped by the scheduler's grace rule.
func (s *Store) SetAppDisabled(ctx context.Context, name string, disabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE apps SET disabled = ? WHERE name = ?`, disabled, name)
	return affectedOne(res, err)
}

// DeleteApp removes an application with all its subscriptions and reminders.
func (s *Store) DeleteApp(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM apps WHERE name = ?`, name)
	return affectedOne(res, err)
}

func affectedOne(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const appCols = `id, name, origins, max_subscriptions, max_reminders_per_sub, disabled, key_hash IS NOT NULL, contact, description, created_at`

func scanApp(row interface{ Scan(...any) error }) (App, error) {
	var a App
	var origins string
	var created int64
	if err := row.Scan(&a.ID, &a.Name, &origins, &a.MaxSubscriptions, &a.MaxRemindersPerSub,
		&a.Disabled, &a.HasKey, &a.Contact, &a.Description, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return App{}, ErrNotFound
		}
		return App{}, err
	}
	a.Origins = strings.Fields(origins)
	a.CreatedAt = time.Unix(created, 0).UTC()
	return a, nil
}

// AppByKey finds the application owning an API key (disabled or not: the
// caller decides).
func (s *Store) AppByKey(ctx context.Context, key string) (App, error) {
	return scanApp(s.db.QueryRowContext(ctx, `SELECT `+appCols+` FROM apps WHERE key_hash = ?`, hashSecret(key)))
}

// AppByName finds an application by name.
func (s *Store) AppByName(ctx context.Context, name string) (App, error) {
	return scanApp(s.db.QueryRowContext(ctx, `SELECT `+appCols+` FROM apps WHERE name = ?`, name))
}

// AppStats are counters shown to operators.
type AppStats struct {
	App
	Subscriptions int
	Reminders     int
}

// ListApps returns all applications with counters.
func (s *Store) ListApps(ctx context.Context) ([]AppStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+appCols+`,
		(SELECT COUNT(*) FROM subscriptions s WHERE s.app_id = apps.id),
		(SELECT COUNT(*) FROM reminders r JOIN subscriptions s ON s.id = r.subscription_id WHERE s.app_id = apps.id)
		FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var apps []AppStats
	for rows.Next() {
		var a AppStats
		var origins string
		var created int64
		if err := rows.Scan(&a.ID, &a.Name, &origins, &a.MaxSubscriptions, &a.MaxRemindersPerSub,
			&a.Disabled, &a.HasKey, &a.Contact, &a.Description, &created, &a.Subscriptions, &a.Reminders); err != nil {
			return nil, err
		}
		a.Origins = strings.Fields(origins)
		a.CreatedAt = time.Unix(created, 0).UTC()
		apps = append(apps, a)
	}
	return apps, rows.Err()
}

// OriginAllowedByAnyApp reports whether a browser origin may call the API at
// all (used for CORS preflight, which carries no API key).
func (s *Store) OriginAllowedByAnyApp(ctx context.Context, origin string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT origins FROM apps WHERE disabled = 0`)
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
