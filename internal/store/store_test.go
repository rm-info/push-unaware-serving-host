package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mkApp(t *testing.T, s *Store, name string, maxSubs, maxRems int) (App, string) {
	t.Helper()
	app, key, err := s.CreateApp(context.Background(), NewApp{Name: name, MaxSubscriptions: maxSubs, MaxRemindersPerSub: maxRems})
	if err != nil {
		t.Fatal(err)
	}
	return app, key
}

func TestAppKeys(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	app, key := mkApp(t, s, "a", 10, 10)
	got, err := s.AppByKey(ctx, key)
	if err != nil || got.ID != app.ID {
		t.Fatalf("AppByKey: %v %v", got, err)
	}
	if _, _, err := s.CreateApp(ctx, NewApp{Name: "a", MaxSubscriptions: 1, MaxRemindersPerSub: 1}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate app: %v", err)
	}
	newKey, err := s.RotateAppKey(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppByKey(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatal("old key still valid")
	}
	if _, err := s.AppByKey(ctx, newKey); err != nil {
		t.Fatal(err)
	}
}

func TestSubscriptionIsolationAndQuota(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a, _ := mkApp(t, s, "a", 1, 2)
	b, _ := mkApp(t, s, "b", 5, 5)

	tokA, created, err := s.RegisterSubscription(ctx, a, "https://push/1", "k", "x")
	if err != nil || !created {
		t.Fatal(err)
	}
	// Same endpoint, other app: independent subscription.
	tokB, created, err := s.RegisterSubscription(ctx, b, "https://push/1", "k", "x")
	if err != nil || !created {
		t.Fatal(err)
	}
	// A token is only valid for its own app.
	if _, err := s.SubscriptionByToken(ctx, b.ID, tokA); !errors.Is(err, ErrNotFound) {
		t.Fatal("token of app a accepted by app b")
	}
	subB, err := s.SubscriptionByToken(ctx, b.ID, tokB)
	if err != nil {
		t.Fatal(err)
	}
	// Quota: app a allows one subscription.
	if _, _, err := s.RegisterSubscription(ctx, a, "https://push/2", "k", "x"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("quota: %v", err)
	}
	// Re-registering rotates the token, keeps reminders.
	subA, _ := s.SubscriptionByToken(ctx, a.ID, tokA)
	next := time.Now().Add(time.Hour)
	if _, err := s.PutReminder(ctx, subA.ID, 2, NewReminder{OpaqueID: "r1", Schedule: "{}", NextRunAt: next}); err != nil {
		t.Fatal(err)
	}
	tokA2, created, err := s.RegisterSubscription(ctx, a, "https://push/1", "k2", "x2")
	if err != nil || created || tokA2 == tokA {
		t.Fatalf("re-register: created=%v err=%v", created, err)
	}
	if _, err := s.SubscriptionByToken(ctx, a.ID, tokA); !errors.Is(err, ErrNotFound) {
		t.Fatal("old token still valid")
	}
	if rs, _ := s.ListReminders(ctx, subA.ID); len(rs) != 1 {
		t.Fatalf("reminders lost on re-register: %d", len(rs))
	}
	if rs, _ := s.ListReminders(ctx, subB.ID); len(rs) != 0 {
		t.Fatal("reminder leaked to other app")
	}
}

func TestRemindersQuotaReplaceAndCascade(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a, _ := mkApp(t, s, "a", 10, 2)
	tok, _, _ := s.RegisterSubscription(ctx, a, "https://push/1", "k", "x")
	sub, _ := s.SubscriptionByToken(ctx, a.ID, tok)
	at := time.Unix(2000000000, 0)
	put := func(id string) error {
		_, err := s.PutReminder(ctx, sub.ID, a.MaxRemindersPerSub, NewReminder{OpaqueID: id, Schedule: "{}", NextRunAt: at})
		return err
	}
	if err := put("1"); err != nil {
		t.Fatal(err)
	}
	if err := put("2"); err != nil {
		t.Fatal(err)
	}
	if err := put("1"); err != nil { // update, not a new one
		t.Fatal(err)
	}
	if err := put("3"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("quota: %v", err)
	}
	if err := s.ReplaceReminders(ctx, sub.ID, 2, []NewReminder{{OpaqueID: "x", Schedule: "{}", NextRunAt: at}}); err != nil {
		t.Fatal(err)
	}
	rs, _ := s.ListReminders(ctx, sub.ID)
	if len(rs) != 1 || rs[0].OpaqueID != "x" || !rs[0].NextRunAt.Equal(at) {
		t.Fatalf("replace: %+v", rs)
	}
	if err := s.ReplaceReminders(ctx, sub.ID, 5, []NewReminder{{OpaqueID: "y", NextRunAt: at}, {OpaqueID: "y", NextRunAt: at}}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate ids: %v", err)
	}
	if rs, _ := s.ListReminders(ctx, sub.ID); len(rs) != 1 || rs[0].OpaqueID != "x" {
		t.Fatal("failed replace was not rolled back")
	}
	if err := s.DeleteReminder(ctx, sub.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// Deleting the app cascades to everything.
	if err := s.DeleteApp(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	due, err := s.DueReminders(ctx, at.Add(time.Hour), 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("cascade: %d %v", len(due), err)
	}
}

func TestDueAndAdvance(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a, _ := mkApp(t, s, "a", 10, 10)
	tok, _, _ := s.RegisterSubscription(ctx, a, "https://push/1", "k", "x")
	sub, _ := s.SubscriptionByToken(ctx, a.ID, tok)
	now := time.Unix(2000000000, 0)
	s.PutReminder(ctx, sub.ID, 10, NewReminder{OpaqueID: "past", Schedule: "{}", NextRunAt: now.Add(-time.Minute)})
	s.PutReminder(ctx, sub.ID, 10, NewReminder{OpaqueID: "future", Schedule: "{}", NextRunAt: now.Add(time.Minute)})
	due, err := s.DueReminders(ctx, now, 10)
	if err != nil || len(due) != 1 || due[0].OpaqueID != "past" || due[0].Sub.Endpoint != "https://push/1" {
		t.Fatalf("due: %+v %v", due, err)
	}
	if err := s.RetryAt(ctx, due[0].ID, now.Add(30*time.Second), 1); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.DueReminders(ctx, now, 10); len(due) != 0 {
		t.Fatal("retry not deferred")
	}
	if err := s.Advance(ctx, due[0].ID, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	rs, _ := s.ListReminders(ctx, sub.ID)
	if len(rs) != 2 || rs[1].OpaqueID != "past" || rs[1].Attempts != 0 {
		t.Fatalf("advance: %+v", rs)
	}
	if err := s.Advance(ctx, due[0].ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if rs, _ := s.ListReminders(ctx, sub.ID); len(rs) != 1 {
		t.Fatal("advance to zero did not delete")
	}
}
