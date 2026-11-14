// Package scheduler fires due reminders.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/rm-info/push-unaware-serving-host/internal/pushsend"
	"github.com/rm-info/push-unaware-serving-host/internal/schedule"
	"github.com/rm-info/push-unaware-serving-host/internal/store"
)

// Scheduler periodically sends the pushes whose time has come.
type Scheduler struct {
	Store  *store.Store
	Sender pushsend.Sender
	// Grace is how late an occurrence may still be sent (server downtime,
	// retries). Older occurrences are skipped, not sent.
	Grace time.Duration
	// Interval between scans.
	Interval time.Duration
	// Workers is the number of concurrent sends.
	Workers int
	Log     *slog.Logger
	Now     func() time.Time
}

// Stats counts what one pass did. Counts only: nothing identifying is logged.
type Stats struct {
	Sent, Retried, Skipped, Failed, Gone int
}

func (s Stats) total() int { return s.Sent + s.Retried + s.Skipped + s.Failed + s.Gone }

// Payload is the JSON sent to the service worker. It never contains more
// than the client's opaque ID and the scheduled time.
type Payload struct {
	ID string `json:"id"` // the client's opaque reminder ID
	T  int64  `json:"t"`  // scheduled occurrence, unix seconds (UTC)
}

const batchSize = 500

// Run scans every Interval until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		s.logPass(s.RunOnce(ctx))
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Scheduler) logPass(st Stats, err error) {
	if err != nil {
		s.Log.Error("scheduler pass failed", "err", err)
	}
	if st.total() > 0 {
		s.Log.Info("scheduler pass", "sent", st.Sent, "retried", st.Retried, "skipped", st.Skipped, "failed", st.Failed, "gone", st.Gone)
	}
}

// RunOnce sends everything that is due now.
func (s *Scheduler) RunOnce(ctx context.Context) (Stats, error) {
	var total Stats
	seen := map[int64]bool{} // never handle a reminder twice in one pass
	for ctx.Err() == nil {
		now := s.now()
		due, err := s.Store.DueReminders(ctx, now, batchSize)
		if err != nil {
			return total, err
		}
		var batch []store.Due
		for _, d := range due {
			if !seen[d.ID] {
				seen[d.ID] = true
				batch = append(batch, d)
			}
		}
		if len(batch) == 0 {
			return total, nil
		}
		st := s.process(ctx, batch)
		total.Sent += st.Sent
		total.Retried += st.Retried
		total.Skipped += st.Skipped
		total.Failed += st.Failed
		total.Gone += st.Gone
	}
	return total, ctx.Err()
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

type outcome int

const (
	sent outcome = iota
	retried
	skipped
	failed
	gone
)

func (s *Scheduler) process(ctx context.Context, batch []store.Due) Stats {
	workers := s.Workers
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan store.Due)
	results := make(chan outcome)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range jobs {
				results <- s.handle(ctx, d)
			}
		}()
	}
	go func() {
		for _, d := range batch {
			jobs <- d
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	var st Stats
	for o := range results {
		switch o {
		case sent:
			st.Sent++
		case retried:
			st.Retried++
		case skipped:
			st.Skipped++
		case failed:
			st.Failed++
		case gone:
			st.Gone++
		}
	}
	return st
}

func (s *Scheduler) handle(ctx context.Context, d store.Due) outcome {
	now := s.now()
	sched, err := schedule.Parse([]byte(d.Schedule))
	if err != nil { // cannot happen: validated on write
		s.Log.Error("corrupt schedule, deleting reminder", "reminder", d.ID, "err", err)
		_ = s.Store.Advance(ctx, d.ID, time.Time{})
		return failed
	}
	advance := func() {
		next, ok := sched.Next(now)
		if !ok {
			next = time.Time{}
		}
		if err := s.Store.Advance(ctx, d.ID, next); err != nil {
			s.Log.Error("advance reminder", "reminder", d.ID, "err", err)
		}
	}

	if now.Sub(d.NextRunAt) > s.Grace {
		advance() // missed (server was down): skip rather than fire late
		return skipped
	}

	payload, _ := json.Marshal(Payload{ID: d.OpaqueID, T: d.NextRunAt.Unix()})
	res, err := s.Sender.Send(ctx, pushsend.Message{
		Endpoint: d.Sub.Endpoint,
		P256dh:   d.Sub.P256dh,
		Auth:     d.Sub.Auth,
		Payload:  payload,
		Topic:    pushsend.Topic(d.OpaqueID),
	})

	switch {
	case err == nil && res.Status >= 200 && res.Status < 300:
		advance()
		return sent
	case err == nil && (res.Status == 404 || res.Status == 410):
		// Subscription expired or revoked: drop it with all its reminders.
		if err := s.Store.DeleteSubscription(ctx, d.Sub.ID); err != nil {
			s.Log.Error("delete dead subscription", "subscription", d.Sub.ID, "err", err)
		}
		return gone
	case err != nil || res.Status == 429 || res.Status >= 500:
		attempts := d.Attempts + 1
		at := now.Add(backoff(attempts))
		if res.RetryAfter > 0 && now.Add(res.RetryAfter).After(at) {
			at = now.Add(res.RetryAfter)
		}
		if at.Sub(d.NextRunAt) > s.Grace {
			s.Log.Warn("giving up on occurrence", "reminder", d.ID, "attempts", attempts, "status", res.Status, "err", errString(err))
			advance()
			return failed
		}
		if err := s.Store.RetryAt(ctx, d.ID, at, attempts); err != nil {
			s.Log.Error("schedule retry", "reminder", d.ID, "err", err)
		}
		return retried
	default:
		// Other 4xx (bad payload, VAPID rejected...): retrying won't help.
		s.Log.Warn("push rejected", "reminder", d.ID, "status", res.Status)
		advance()
		return failed
	}
}

// backoff returns 30s, 1m, 2m, 4m... capped at 10 minutes.
func backoff(attempt int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < attempt && d < 10*time.Minute; i++ {
		d *= 2
	}
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	// url.Error embeds the endpoint URL: keep it out of the logs.
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op + ": " + ue.Err.Error()
	}
	return err.Error()
}
