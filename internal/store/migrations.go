package store

import (
	"database/sql"
	"fmt"
	"strconv"
)

// migrations are applied in order; migrations[i] brings the schema to
// version i+1. Never edit a released migration: append a new one.
var migrations = []string{
	// v1: initial schema.
	`
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
`,
	// v2: key requests, admin accounts, app disabling, nullable key (an
	// approved app has no key until the requester claims it).
	`
CREATE TABLE apps_new (
	id                    INTEGER PRIMARY KEY,
	name                  TEXT    NOT NULL UNIQUE,
	key_hash              TEXT    UNIQUE,
	origins               TEXT    NOT NULL DEFAULT '',
	max_subscriptions     INTEGER NOT NULL,
	max_reminders_per_sub INTEGER NOT NULL,
	disabled              INTEGER NOT NULL DEFAULT 0,
	contact               TEXT    NOT NULL DEFAULT '',
	description           TEXT    NOT NULL DEFAULT '',
	created_at            INTEGER NOT NULL
);
INSERT INTO apps_new (id, name, key_hash, origins, max_subscriptions, max_reminders_per_sub, created_at)
	SELECT id, name, key_hash, origins, max_subscriptions, max_reminders_per_sub, created_at FROM apps;
DROP TABLE apps;
ALTER TABLE apps_new RENAME TO apps;

CREATE TABLE key_requests (
	id                INTEGER PRIMARY KEY,
	name              TEXT    NOT NULL,
	contact           TEXT    NOT NULL,
	origins           TEXT    NOT NULL,
	description       TEXT    NOT NULL,
	status            TEXT    NOT NULL DEFAULT 'pending', -- pending | approved | rejected
	reason            TEXT    NOT NULL DEFAULT '',
	status_token_hash TEXT    NOT NULL UNIQUE,
	app_id            INTEGER REFERENCES apps(id) ON DELETE SET NULL,
	created_at        INTEGER NOT NULL,
	decided_at        INTEGER,
	decided_by        TEXT    NOT NULL DEFAULT '',
	key_claimed_at    INTEGER
);
CREATE INDEX key_requests_status ON key_requests(status, created_at);

CREATE TABLE admins (
	id            INTEGER PRIMARY KEY,
	username      TEXT    NOT NULL UNIQUE,
	password_hash TEXT    NOT NULL,
	created_at    INTEGER NOT NULL
);
CREATE TABLE admin_sessions (
	token_hash TEXT    PRIMARY KEY,
	admin_id   INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
	csrf_token TEXT    NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE TABLE audit_log (
	id     INTEGER PRIMARY KEY,
	at     INTEGER NOT NULL,
	actor  TEXT    NOT NULL,
	action TEXT    NOT NULL,
	target TEXT    NOT NULL DEFAULT '',
	detail TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_at ON audit_log(at);
`,
}

// SchemaVersion is the schema version this build expects.
var SchemaVersion = len(migrations)

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return err
	}
	var v string
	version := 0
	switch err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v); err {
	case nil:
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("bad schema_version %q", v)
		}
		version = n
	case sql.ErrNoRows:
	default:
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema v%d is newer than this binary (v%d): upgrade PUSH", version, len(migrations))
	}
	if version == len(migrations) {
		return nil
	}

	// Table rebuilds need foreign keys off; this is a no-op inside a
	// transaction, so toggle it around. The pool has a single connection.
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer db.Exec(`PRAGMA foreign_keys = ON`)

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := version; i < len(migrations); i++ {
		if _, err := tx.Exec(migrations[i]); err != nil {
			return fmt.Errorf("v%d: %w", i+1, err)
		}
	}
	rows, err := tx.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	bad := rows.Next()
	rows.Close()
	if bad {
		return fmt.Errorf("foreign key check failed after migration")
	}
	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES ('schema_version', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.Itoa(len(migrations))); err != nil {
		return err
	}
	return tx.Commit()
}
