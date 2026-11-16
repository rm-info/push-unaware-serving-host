package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rm-info/push-unaware-serving-host/internal/pushsend"
	"github.com/rm-info/push-unaware-serving-host/internal/store"
)

type fakeSender struct {
	mu     sync.Mutex
	status map[string]int // endpoint -> status (default 201)
	err    error
	sent   []pushsend.Message
}

func (f *fakeSender) Send(_ context.Context, m pushsend.Message) (pushsend.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	if f.err != nil {
		return pushsend.Result{}, f.err
	}
	if s, ok := f.status[m.Endpoint]; ok {
		return pushsend.Result{Status: s}, nil
	}
	return pushsend.Result{Status: 201}, nil
}

type env struct {
	st    *store.Store
	sched *Scheduler
	snd   *fakeSender
	now   time.Time
	sub   store.Subscription
	app   store.App
}

func setup(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, snd: &fakeSender{status: map[string]int{}}, now: time.Date(2026, 11, 14, 7, 0, 0, 0, time.UTC)}
	e.sched = &Scheduler{Store: st, Sender: e.snd, Grace: time.Hour, Workers: 4,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return e.now }}
	ctx := context.Background()
	e.app, _, _ = st.CreateApp(ctx, store.NewApp{Name: "a", MaxSubscriptions: 100, MaxRemindersPerSub: 100})
	e.sub = e.addSub(t, "https://push/1")
	return e
}

func (e *env) addSub(t *testing.T, endpoint string) store.Subscription {
	ctx := context.Background()
	tok, _, err := e.st.RegisterSubscription(ctx, e.app, endpoint, "k", "a")
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := e.st.SubscriptionByToken(ctx, e.app.ID, tok)
	return sub
}

func (e *env) put(t *testing.T, sub store.Subscription, id, sched string, next time.Time) {
	if _, err := e.st.PutReminder(context.Background(), sub.ID, 100, store.NewReminder{OpaqueID: id, Schedule: sched, NextRunAt: next}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) run(t *testing.T) Stats {
	st, err := e.sched.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (e *env) list(t *testing.T, sub store.Subscription) []store.Reminder {
	rs, err := e.st.ListReminders(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

const daily8 = `{"type":"daily","time":"08:00","tz":"Europe/Paris"}`

func TestOnceSentThenDeleted(t *testing.T) {
	e := setup(t)
	e.put(t, e.sub, "once-1", `{"type":"once","at":"2026-11-14T07:00:00Z"}`, e.now)
	if st := e.run(t); st.Sent != 1 {
		t.Fatalf("stats %+v", st)
	}
	var p Payload
	if err := json.Unmarshal(e.snd.sent[0].Payload, &p); err != nil || p.ID != "once-1" || p.T != e.now.Unix() {
		t.Fatalf("payload %s", e.snd.sent[0].Payload)
	}
	if len(e.list(t, e.sub)) != 0 {
		t.Fatal("once reminder not deleted")
	}
	if st := e.run(t); st.Sent != 0 {
		t.Fatal("sent twice")
	}
}

func TestRecurringAdvances(t *testing.T) {
	e := setup(t)
	e.put(t, e.sub, "d", daily8, e.now) // 08:00 Paris = 07:00Z
	e.put(t, e.sub, "later", daily8, e.now.Add(time.Hour))
	if st := e.run(t); st.Sent != 1 {
		t.Fatalf("stats %+v", st)
	}
	rs := e.list(t, e.sub)
	if len(rs) != 2 {
		t.Fatal(rs)
	}
	for _, r := range rs {
		if r.OpaqueID == "d" && !r.NextRunAt.Equal(e.now.Add(24*time.Hour)) {
			t.Fatalf("next = %s", r.NextRunAt)
		}
	}
}

func TestGoneDeletesSubscription(t *testing.T) {
	e := setup(t)
	other := e.addSub(t, "https://push/2")
	e.put(t, e.sub, "a", daily8, e.now)
	e.put(t, e.sub, "b", daily8, e.now.Add(time.Hour)) // not due, must still go
	e.put(t, other, "c", daily8, e.now)
	e.snd.status["https://push/1"] = 410
	st := e.run(t)
	if st.Gone != 1 || st.Sent != 1 {
		t.Fatalf("stats %+v", st)
	}
	if len(e.list(t, e.sub)) != 0 {
		t.Fatal("reminders of dead subscription remain")
	}
	if len(e.list(t, other)) != 1 {
		t.Fatal("other subscription affected")
	}
}

func TestRetryThenGiveUp(t *testing.T) {
	e := setup(t)
	e.put(t, e.sub, "r", daily8, e.now)
	e.snd.status["https://push/1"] = 503
	start := e.now
	retries := 0
	for i := 0; i < 50; i++ {
		st := e.run(t)
		retries += st.Retried
		if st.Failed == 1 {
			break
		}
		rs := e.list(t, e.sub)
		e.now = rs[0].AttemptAt
	}
	if retries < 3 {
		t.Fatalf("only %d retries", retries)
	}
	rs := e.list(t, e.sub)
	if len(rs) != 1 || !rs[0].NextRunAt.Equal(start.Add(24*time.Hour)) || rs[0].Attempts != 0 {
		t.Fatalf("after give-up: %+v", rs)
	}
	if e.now.Sub(start) > time.Hour {
		t.Fatalf("retried past grace: %s", e.now.Sub(start))
	}
}

func TestTransportErrorRetries(t *testing.T) {
	e := setup(t)
	e.put(t, e.sub, "r", daily8, e.now)
	e.snd.err = errors.New("connection refused")
	if st := e.run(t); st.Retried != 1 {
		t.Fatalf("stats %+v", st)
	}
	e.snd.err = nil
	e.now = e.now.Add(31 * time.Second)
	if st := e.run(t); st.Sent != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestPermanentRejectSkipsOccurrence(t *testing.T) {
	e := setup(t)
	e.put(t, e.sub, "r", daily8, e.now)
	e.snd.status["https://push/1"] = 400
	if st := e.run(t); st.Failed != 1 {
		t.Fatalf("stats %+v", st)
	}
	if rs := e.list(t, e.sub); len(rs) != 1 || !rs[0].NextRunAt.Equal(e.now.Add(24*time.Hour)) {
		t.Fatalf("%+v", rs)
	}
}

func TestMissedOccurrencesSkipped(t *testing.T) {
	e := setup(t)
	// Server was down for 3 days.
	e.put(t, e.sub, "d", daily8, e.now)
	e.put(t, e.sub, "o", `{"type":"once","at":"2026-11-14T07:00:00Z"}`, e.now)
	e.put(t, e.sub, "recent", `{"type":"once","at":"2026-11-17T06:30:00Z"}`, time.Date(2026, 11, 17, 6, 30, 0, 0, time.UTC))
	e.now = e.now.Add(72 * time.Hour) // 2026-11-17T07:00Z
	st := e.run(t)
	if st.Skipped != 2 || st.Sent != 1 {
		t.Fatalf("stats %+v", st)
	}
	rs := e.list(t, e.sub)
	if len(rs) != 1 || rs[0].OpaqueID != "d" || !rs[0].NextRunAt.Equal(e.now.Add(24*time.Hour)) {
		t.Fatalf("%+v", rs)
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for i, w := range want {
		if got := backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %s, want %s", i+1, got, w)
		}
	}
}
