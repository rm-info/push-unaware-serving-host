// Package api exposes the HTTP API used by client PWAs.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/rm-info/push-unaware-serving-host/internal/httpx"
	"github.com/rm-info/push-unaware-serving-host/internal/keyreq"
	"github.com/rm-info/push-unaware-serving-host/internal/portal"
	"github.com/rm-info/push-unaware-serving-host/internal/pushsend"
	"github.com/rm-info/push-unaware-serving-host/internal/schedule"
	"github.com/rm-info/push-unaware-serving-host/internal/store"
)

// Server holds the API dependencies.
type Server struct {
	Store          *store.Store
	VAPIDPublicKey string
	// AllowedPushHosts restricts subscription endpoints; empty = any https host.
	AllowedPushHosts []string
	// KeyRequests enables the key request routes when non-nil.
	KeyRequests    *keyreq.Service
	TrustedProxies []netip.Prefix
	Log            *slog.Logger
	Now            func() time.Time
}

var opaqueIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

const maxBody = 256 << 10

// Handler returns the HTTP handler for the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /v1/vapid-public-key", s.vapidKey)

	mux.Handle("POST /v1/subscriptions", s.withApp(s.registerSubscription))
	mux.Handle("PUT /v1/subscription", s.withSub(s.updateSubscription))
	mux.Handle("DELETE /v1/subscription", s.withSub(s.deleteSubscription))

	mux.Handle("GET /v1/reminders", s.withSub(s.listReminders))
	mux.Handle("PUT /v1/reminders", s.withSub(s.replaceReminders))
	mux.Handle("PUT /v1/reminders/{id}", s.withSub(s.putReminder))
	mux.Handle("DELETE /v1/reminders/{id}", s.withSub(s.deleteReminder))

	if s.KeyRequests != nil {
		mux.HandleFunc("POST /v1/key-requests", s.createKeyRequest)
		mux.HandleFunc("GET /v1/key-requests/status", s.keyRequestStatus)
	}

	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such route")
	})
	return s.cors(mux)
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// --- middleware -------------------------------------------------------------

// cors answers preflights for origins allowed by at least one app; the
// per-app origin check happens once the API key is known (withApp).
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && strings.HasPrefix(r.URL.Path, "/v1/") {
			ok, err := s.Store.OriginAllowedByAnyApp(r.Context(), origin)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
				return
			}
			w.Header().Add("Vary", "Origin")
			if ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Max-Age", "86400")
			}
			if r.Method == http.MethodOptions {
				if ok {
					w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE")
					w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-App-Key")
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type appHandler func(w http.ResponseWriter, r *http.Request, app store.App)
type subHandler func(w http.ResponseWriter, r *http.Request, app store.App, sub store.Subscription)

func (s *Server) withApp(h appHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-App-Key")
		if key == "" {
			writeError(w, http.StatusUnauthorized, "missing_app_key", "X-App-Key header required")
			return
		}
		app, err := s.Store.AppByKey(r.Context(), key)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid_app_key", "unknown app key")
			return
		}
		if err != nil {
			s.internal(w, err)
			return
		}
		if app.Disabled {
			writeError(w, http.StatusForbidden, "app_disabled", "this app key has been disabled by the server operator")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !store.OriginAllowed(app.Origins, o) {
			writeError(w, http.StatusForbidden, "origin_not_allowed", "origin not allowed for this app")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		h(w, r, app)
	})
}

func (s *Server) withSub(h subHandler) http.Handler {
	return s.withApp(func(w http.ResponseWriter, r *http.Request, app store.App) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeError(w, http.StatusUnauthorized, "missing_token", "Authorization: Bearer <subscription token> required")
			return
		}
		sub, err := s.Store.SubscriptionByToken(r.Context(), app.ID, token)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid_token", "unknown subscription token")
			return
		}
		if err != nil {
			s.internal(w, err)
			return
		}
		h(w, r, app, sub)
	})
}

// --- public routes ----------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) vapidKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, http.StatusOK, map[string]string{"publicKey": s.VAPIDPublicKey})
}

// --- subscriptions ----------------------------------------------------------

// subscriptionBody is PushSubscription.toJSON() as produced by browsers.
type subscriptionBody struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	ExpirationTime any `json:"expirationTime,omitempty"` // accepted, ignored
}

func (s *Server) readSubscription(w http.ResponseWriter, r *http.Request) (subscriptionBody, bool) {
	var b subscriptionBody
	if !readJSON(w, r, &b) {
		return b, false
	}
	if err := pushsend.ValidateEndpoint(b.Endpoint, s.AllowedPushHosts); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_endpoint", err.Error())
		return b, false
	}
	if err := pushsend.ValidateKeys(b.Keys.P256dh, b.Keys.Auth); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_keys", err.Error())
		return b, false
	}
	return b, true
}

func (s *Server) registerSubscription(w http.ResponseWriter, r *http.Request, app store.App) {
	b, ok := s.readSubscription(w, r)
	if !ok {
		return
	}
	token, created, err := s.Store.RegisterSubscription(r.Context(), app, b.Endpoint, b.Keys.P256dh, b.Keys.Auth)
	if errors.Is(err, store.ErrQuotaExceeded) {
		writeError(w, http.StatusTooManyRequests, "quota_exceeded", "too many subscriptions for this app")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"token": token, "created": created})
}

func (s *Server) updateSubscription(w http.ResponseWriter, r *http.Request, app store.App, sub store.Subscription) {
	b, ok := s.readSubscription(w, r)
	if !ok {
		return
	}
	err := s.Store.UpdateSubscription(r.Context(), sub.ID, b.Endpoint, b.Keys.P256dh, b.Keys.Auth)
	if errors.Is(err, store.ErrExists) {
		writeError(w, http.StatusConflict, "endpoint_exists", "this endpoint is already registered with another token")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteSubscription(w http.ResponseWriter, r *http.Request, app store.App, sub store.Subscription) {
	if err := s.Store.DeleteSubscription(r.Context(), sub.ID); err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- reminders --------------------------------------------------------------

type reminderOut struct {
	ID        string          `json:"id"`
	Schedule  json.RawMessage `json:"schedule"`
	NextRunAt string          `json:"next_run_at"`
}

type reminderIn struct {
	ID       string          `json:"id,omitempty"`
	Schedule json.RawMessage `json:"schedule"`
}

// prepare validates a reminder and computes its first occurrence.
func (s *Server) prepare(id string, raw json.RawMessage) (store.NewReminder, string, error) {
	if !opaqueIDRe.MatchString(id) {
		return store.NewReminder{}, "invalid_id", errors.New("id must be 1-128 characters among A-Z a-z 0-9 _ -")
	}
	if len(raw) == 0 {
		return store.NewReminder{}, "invalid_schedule", errors.New(`"schedule" is required`)
	}
	sched, err := schedule.Parse(raw)
	if err != nil {
		return store.NewReminder{}, "invalid_schedule", err
	}
	next, ok := sched.Next(s.now())
	if !ok {
		return store.NewReminder{}, "no_occurrence", errors.New("schedule has no future occurrence")
	}
	return store.NewReminder{OpaqueID: id, Schedule: string(sched.JSON()), NextRunAt: next}, "", nil
}

func (s *Server) putReminder(w http.ResponseWriter, r *http.Request, app store.App, sub store.Subscription) {
	var in reminderIn
	if !readJSON(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	if in.ID != "" && in.ID != id {
		writeError(w, http.StatusUnprocessableEntity, "invalid_id", "body id does not match URL")
		return
	}
	nr, code, err := s.prepare(id, in.Schedule)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, code, err.Error())
		return
	}
	created, err := s.Store.PutReminder(r.Context(), sub.ID, app.MaxRemindersPerSub, nr)
	if errors.Is(err, store.ErrQuotaExceeded) {
		writeError(w, http.StatusTooManyRequests, "quota_exceeded", "too many reminders for this subscription")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, toOut(nr.OpaqueID, nr.Schedule, nr.NextRunAt))
}

func (s *Server) replaceReminders(w http.ResponseWriter, r *http.Request, app store.App, sub store.Subscription) {
	var in struct {
		Reminders []reminderIn `json:"reminders"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Reminders == nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_body", `"reminders" array is required (may be empty)`)
		return
	}
	rs := make([]store.NewReminder, 0, len(in.Reminders))
	for i, ri := range in.Reminders {
		nr, code, err := s.prepare(ri.ID, ri.Schedule)
		if code == "no_occurrence" {
			continue // already past: nothing to schedule
		}
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": code, "message": err.Error(), "index": i})
			return
		}
		rs = append(rs, nr)
	}
	err := s.Store.ReplaceReminders(r.Context(), sub.ID, app.MaxRemindersPerSub, rs)
	switch {
	case errors.Is(err, store.ErrQuotaExceeded):
		writeError(w, http.StatusTooManyRequests, "quota_exceeded", "too many reminders for this subscription")
		return
	case errors.Is(err, store.ErrExists):
		writeError(w, http.StatusUnprocessableEntity, "duplicate_id", err.Error())
		return
	case err != nil:
		s.internal(w, err)
		return
	}
	s.listReminders(w, r, app, sub)
}

func (s *Server) deleteReminder(w http.ResponseWriter, r *http.Request, app store.App, sub store.Subscription) {
	err := s.Store.DeleteReminder(r.Context(), sub.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such reminder")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listReminders(w http.ResponseWriter, r *http.Request, app store.App, sub store.Subscription) {
	rs, err := s.Store.ListReminders(r.Context(), sub.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]reminderOut, 0, len(rs))
	for _, rm := range rs {
		out = append(out, toOut(rm.OpaqueID, rm.Schedule, rm.NextRunAt))
	}
	writeJSON(w, http.StatusOK, map[string]any{"reminders": out})
}

func toOut(id, sched string, next time.Time) reminderOut {
	return reminderOut{ID: id, Schedule: json.RawMessage(sched), NextRunAt: next.UTC().Format(time.RFC3339)}
}

// --- key requests -----------------------------------------------------------

func (s *Server) createKeyRequest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var in keyreq.Raw
	if !readJSON(w, r, &in) {
		return
	}
	token, err := s.KeyRequests.Submit(r.Context(), httpx.ClientIP(r, s.TrustedProxies), in)
	if err != nil {
		s.keyRequestError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": store.RequestPending, "status_token": token})
}

func (s *Server) keyRequestStatus(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	c, err := s.KeyRequests.Check(r.Context(), httpx.ClientIP(r, s.TrustedProxies), token)
	if err != nil {
		s.keyRequestError(w, err)
		return
	}
	out := map[string]any{"name": c.Request.Name, "status": c.Request.Status, "created_at": c.Request.CreatedAt.Format(time.RFC3339)}
	if c.Request.Reason != "" {
		out["reason"] = c.Request.Reason
	}
	if c.Request.Status == store.RequestApproved {
		out["app"] = c.Request.AppName
		out["key_issued"] = c.Key != "" || c.AlreadyClaimed
	}
	if c.Key != "" {
		out["api_key"] = c.Key // returned exactly once
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) keyRequestError(w http.ResponseWriter, err error) {
	status, msg := portal.UserError(err)
	codes := map[int]string{422: "invalid_request", 429: "rate_limited", 503: "queue_full", 409: "name_taken", 404: "unknown_token"}
	code, ok := codes[status]
	if !ok {
		s.internal(w, err)
		return
	}
	writeError(w, status, code, msg)
}

// --- helpers ----------------------------------------------------------------

func (s *Server) internal(w http.ResponseWriter, err error) {
	s.Log.Error("request failed", "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON body")
		}
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_json", "unexpected data after JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}
