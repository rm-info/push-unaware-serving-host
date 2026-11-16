// Package keyreq implements API key requests from third parties: submit a
// request, then check it (and claim the key once approved) with the status
// token. Used by both the HTML form and the JSON API.
package keyreq

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/rm-info/push-unaware-serving-host/internal/httpx"
	"github.com/rm-info/push-unaware-serving-host/internal/store"
	"github.com/rm-info/push-unaware-serving-host/internal/validate"
)

// Errors returned to requesters.
var (
	ErrRateLimited = errors.New("too many requests from your address, try again later")
	ErrQueueFull   = errors.New("the request queue is full, try again later")
	ErrNameTaken   = errors.New("this application name is already taken")
	ErrBadToken    = errors.New("unknown status token")
)

// InvalidError is a validation failure.
type InvalidError struct{ Err error }

func (e InvalidError) Error() string { return e.Err.Error() }
func (e InvalidError) Unwrap() error { return e.Err }

// Raw is a request as submitted.
type Raw struct {
	Name        string   `json:"name"`
	Contact     string   `json:"contact"`
	Origins     []string `json:"origins"`
	Description string   `json:"description"`
}

// Service handles key requests.
type Service struct {
	Store      *store.Store
	Log        *slog.Logger
	MaxPending int
	submit     *httpx.Limiter
	check      *httpx.Limiter
}

// New creates the service: 5 submissions and 60 checks per hour and IP.
func New(st *store.Store, log *slog.Logger, maxPending int) *Service {
	return &Service{Store: st, Log: log, MaxPending: maxPending,
		submit: httpx.NewLimiter(5, time.Hour), check: httpx.NewLimiter(60, time.Hour)}
}

// Validate checks and normalizes a request.
func Validate(r Raw) (store.NewKeyRequest, error) {
	var in store.NewKeyRequest
	var err error
	in.Name = strings.TrimSpace(r.Name)
	if err = validate.AppName(in.Name); err != nil {
		return in, InvalidError{err}
	}
	if in.Contact, err = validate.Contact(r.Contact); err != nil {
		return in, InvalidError{err}
	}
	if in.Origins, err = validate.Origins(strings.Join(r.Origins, " "), true); err != nil {
		return in, InvalidError{err}
	}
	if in.Description, err = validate.Text(r.Description, "description", 1000, true); err != nil {
		return in, InvalidError{err}
	}
	return in, nil
}

// Submit queues a request and returns the requester's status token.
func (s *Service) Submit(ctx context.Context, clientIP string, r Raw) (string, error) {
	in, err := Validate(r)
	if err != nil {
		return "", err
	}
	if !s.submit.Allow(clientIP) {
		return "", ErrRateLimited
	}
	_, token, err := s.Store.CreateKeyRequest(ctx, in, s.MaxPending)
	switch {
	case errors.Is(err, store.ErrTooManyPending):
		return "", ErrQueueFull
	case errors.Is(err, store.ErrExists):
		return "", ErrNameTaken
	case err != nil:
		return "", err
	}
	if n, err := s.Store.CountPendingKeyRequests(ctx); err == nil {
		s.Log.Info("new API key request awaiting review", "pending", n)
	}
	return token, nil
}

// Check returns the request's state; the first check after approval
// carries the API key.
func (s *Service) Check(ctx context.Context, clientIP, token string) (store.KeyClaim, error) {
	if !s.check.Allow(clientIP) {
		return store.KeyClaim{}, ErrRateLimited
	}
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "req_") || len(token) > 100 {
		return store.KeyClaim{}, ErrBadToken
	}
	c, err := s.Store.ClaimKeyRequest(ctx, token)
	if errors.Is(err, store.ErrNotFound) {
		return c, ErrBadToken
	}
	return c, err
}
