package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrationFromV1KeepsDataAndForeignKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push.db")
	// Build a v1 database as the first release created it.
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		migrations[0],
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO meta VALUES ('schema_version', '1')`,
		`INSERT INTO apps(id, name, key_hash, origins, max_subscriptions, max_reminders_per_sub, created_at) VALUES (1, 'old', '` + hashSecret("app_old") + `', 'https://a.test', 5, 6, 1700000000)`,
		`INSERT INTO subscriptions(id, app_id, endpoint, p256dh, auth, token_hash, created_at) VALUES (1, 1, 'https://push/1', 'k', 'a', 'th', 1700000000)`,
		`INSERT INTO reminders(subscription_id, opaque_id, schedule, next_run_at, attempt_at) VALUES (1, 'r', '{}', 1, 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	app, err := s.AppByKey(ctx, "app_old")
	if err != nil || app.Name != "old" || !app.HasKey || app.Disabled || app.MaxSubscriptions != 5 || app.Origins[0] != "https://a.test" {
		t.Fatalf("app after migration: %+v %v", app, err)
	}
	var v string
	s.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v)
	if v != "2" {
		t.Fatalf("schema_version = %s", v)
	}
	// Cascades still work on the rebuilt apps table.
	if err := s.DeleteApp(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM subscriptions) + (SELECT COUNT(*) FROM reminders)`).Scan(&n)
	if n != 0 {
		t.Fatalf("cascade broken after migration: %d rows left", n)
	}
	// Reopening is a no-op.
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`UPDATE meta SET value = '99' WHERE key = 'schema_version'`)
	s.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("opened a database from the future")
	}
}

func TestKeyRequestLifecycle(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	in := NewKeyRequest{Name: "cool-app", Contact: "dev@app.test", Origins: []string{"https://app.test"}, Description: "pills"}

	id, token, err := s.CreateKeyRequest(ctx, in, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Pending: nothing to claim.
	c, err := s.ClaimKeyRequest(ctx, token)
	if err != nil || c.Request.Status != RequestPending || c.Key != "" {
		t.Fatalf("pending claim: %+v %v", c, err)
	}
	if _, err := s.ClaimKeyRequest(ctx, "req_wrong"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// Queue limit.
	s.CreateKeyRequest(ctx, NewKeyRequest{Name: "b", Contact: "x@y.test"}, 2)
	if _, _, err := s.CreateKeyRequest(ctx, NewKeyRequest{Name: "c", Contact: "x@y.test"}, 2); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("queue limit: %v", err)
	}

	app, err := s.ApproveKeyRequest(ctx, id, NewApp{Name: "cool-app", Origins: in.Origins, MaxSubscriptions: 10, MaxRemindersPerSub: 5, Contact: in.Contact}, "alice")
	if err != nil || app.HasKey {
		t.Fatalf("approve: %+v %v", app, err)
	}
	if _, err := s.ApproveKeyRequest(ctx, id, NewApp{Name: "other"}, "alice"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("double approve: %v", err)
	}
	// Name now taken for new requests.
	if _, _, err := s.CreateKeyRequest(ctx, NewKeyRequest{Name: "cool-app"}, 10); !errors.Is(err, ErrExists) {
		t.Fatalf("name taken: %v", err)
	}

	c, err = s.ClaimKeyRequest(ctx, token)
	if err != nil || c.Key == "" || c.Request.AppName != "cool-app" {
		t.Fatalf("claim: %+v %v", c, err)
	}
	got, err := s.AppByKey(ctx, c.Key)
	if err != nil || got.ID != app.ID {
		t.Fatalf("claimed key does not work: %v", err)
	}
	// Only once.
	c2, err := s.ClaimKeyRequest(ctx, token)
	if err != nil || c2.Key != "" || !c2.AlreadyClaimed {
		t.Fatalf("second claim: %+v %v", c2, err)
	}
	if _, err := s.AppByKey(ctx, c.Key); err != nil {
		t.Fatal("second claim changed the key")
	}

	r, _ := s.KeyRequestByID(ctx, id)
	if r.Status != RequestApproved || r.DecidedBy != "alice" || r.KeyClaimedAt.IsZero() {
		t.Fatalf("request after claim: %+v", r)
	}
}

func TestRotateBeforeClaimBlocksClaim(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	id, token, _ := s.CreateKeyRequest(ctx, NewKeyRequest{Name: "app", Contact: "a@b.test"}, 10)
	s.ApproveKeyRequest(ctx, id, NewApp{Name: "app", MaxSubscriptions: 1, MaxRemindersPerSub: 1}, "op")
	key, err := s.RotateAppKey(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.ClaimKeyRequest(ctx, token)
	if err != nil || c.Key != "" || !c.AlreadyClaimed {
		t.Fatalf("claim after rotate: %+v %v", c, err)
	}
	if _, err := s.AppByKey(ctx, key); err != nil {
		t.Fatal("operator key lost")
	}
}

func TestRejectAndPurge(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Unix(2000000000, 0)
	s.now = func() time.Time { return now }
	id, token, _ := s.CreateKeyRequest(ctx, NewKeyRequest{Name: "spam", Contact: "a@b.test"}, 10)
	if err := s.RejectKeyRequest(ctx, id, "not a PWA", "op"); err != nil {
		t.Fatal(err)
	}
	if err := s.RejectKeyRequest(ctx, id, "again", "op"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("double reject: %v", err)
	}
	if err := s.RejectKeyRequest(ctx, 999, "", "op"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	c, _ := s.ClaimKeyRequest(ctx, token)
	if c.Request.Status != RequestRejected || c.Request.Reason != "not a PWA" || c.Key != "" {
		t.Fatalf("%+v", c)
	}
	pid, _, _ := s.CreateKeyRequest(ctx, NewKeyRequest{Name: "waiting", Contact: "a@b.test"}, 10)
	s.Audit(ctx, "op", "x", "", "")

	now = now.Add(400 * 24 * time.Hour)
	if err := s.Purge(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.KeyRequestByID(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("old rejected request kept")
	}
	if _, err := s.KeyRequestByID(ctx, pid); err != nil {
		t.Fatal("pending request purged")
	}
	if a, _ := s.ListAudit(ctx, 10); len(a) != 0 {
		t.Fatal("old audit kept")
	}
}

func TestAdminSessions(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Unix(2000000000, 0)
	s.now = func() time.Time { return now }
	if err := s.CreateAdmin(ctx, "alice", "hash1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAdmin(ctx, "alice", "x"); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	a, _ := s.AdminByUsername(ctx, "alice")
	tok, sess, err := s.CreateSession(ctx, a.ID, time.Hour)
	if err != nil || sess.CSRFToken == "" {
		t.Fatal(err)
	}
	got, err := s.SessionByToken(ctx, tok)
	if err != nil || got.Username != "alice" || got.CSRFToken != sess.CSRFToken {
		t.Fatalf("%+v %v", got, err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := s.SessionByToken(ctx, tok); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired session accepted")
	}
	now = now.Add(-2 * time.Hour)
	// Password change ends sessions.
	if err := s.SetAdminPassword(ctx, "alice", "hash2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionByToken(ctx, tok); !errors.Is(err, ErrNotFound) {
		t.Fatal("session survived password change")
	}
	tok, _, _ = s.CreateSession(ctx, a.ID, time.Hour)
	if err := s.DeleteAdmin(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionByToken(ctx, tok); !errors.Is(err, ErrNotFound) {
		t.Fatal("session survived account deletion")
	}
}

func TestDisabledAppPausesPushes(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a, key := mkApp(t, s, "a", 10, 10)
	tok, _, _ := s.RegisterSubscription(ctx, a, "https://push/1", "k", "x")
	sub, _ := s.SubscriptionByToken(ctx, a.ID, tok)
	at := time.Unix(2000000000, 0)
	s.PutReminder(ctx, sub.ID, 10, NewReminder{OpaqueID: "r", Schedule: "{}", NextRunAt: at})

	if err := s.SetAppDisabled(ctx, "a", true); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.DueReminders(ctx, at, 10); len(due) != 0 {
		t.Fatal("disabled app's reminder is due")
	}
	if app, _ := s.AppByKey(ctx, key); !app.Disabled {
		t.Fatal("not flagged disabled")
	}
	if ok, _ := s.OriginAllowedByAnyApp(ctx, "https://x.test"); ok {
		t.Fatal("disabled app still allows CORS origins")
	}
	s.SetAppDisabled(ctx, "a", false)
	if due, _ := s.DueReminders(ctx, at, 10); len(due) != 1 {
		t.Fatal("re-enabled app's reminder not due")
	}
	if err := s.SetAppDisabled(ctx, "nope", true); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
