package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Key request statuses.
const (
	RequestPending  = "pending"
	RequestApproved = "approved"
	RequestRejected = "rejected"
)

// ErrTooManyPending is returned when the pending-request queue is full.
var ErrTooManyPending = errors.New("too many pending requests")

// KeyRequest is a third party's request for an API key.
type KeyRequest struct {
	ID           int64
	Name         string
	Contact      string
	Origins      []string
	Description  string
	Status       string
	Reason       string // rejection reason, shown to the requester
	AppID        int64  // set once approved (0 if the app was deleted since)
	AppName      string
	CreatedAt    time.Time
	DecidedAt    time.Time
	DecidedBy    string
	KeyClaimedAt time.Time
}

// NewKeyRequest is the input for a key request.
type NewKeyRequest struct {
	Name        string
	Contact     string
	Origins     []string
	Description string
}

// CreateKeyRequest queues a request and returns its ID and the requester's
// status token (stored hashed only).
func (s *Store) CreateKeyRequest(ctx context.Context, in NewKeyRequest, maxPending int) (int64, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM key_requests WHERE status = 'pending'`).Scan(&pending); err != nil {
		return 0, "", err
	}
	if pending >= maxPending {
		return 0, "", ErrTooManyPending
	}
	var taken bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM apps WHERE name = ?)`, in.Name).Scan(&taken); err != nil {
		return 0, "", err
	}
	if taken {
		return 0, "", ErrExists
	}
	token := newSecret("req_")
	res, err := tx.ExecContext(ctx,
		`INSERT INTO key_requests(name, contact, origins, description, status_token_hash, created_at) VALUES (?,?,?,?,?,?)`,
		in.Name, in.Contact, strings.Join(in.Origins, " "), in.Description, hashSecret(token), s.now().Unix())
	if err != nil {
		return 0, "", err
	}
	id, _ := res.LastInsertId()
	return id, token, tx.Commit()
}

const requestCols = `r.id, r.name, r.contact, r.origins, r.description, r.status, r.reason,
	COALESCE(r.app_id, 0), COALESCE(a.name, ''), r.created_at, COALESCE(r.decided_at, 0), r.decided_by, COALESCE(r.key_claimed_at, 0)`

const requestFrom = ` FROM key_requests r LEFT JOIN apps a ON a.id = r.app_id `

func scanRequest(row interface{ Scan(...any) error }) (KeyRequest, error) {
	var r KeyRequest
	var origins string
	var created, decided, claimed int64
	err := row.Scan(&r.ID, &r.Name, &r.Contact, &origins, &r.Description, &r.Status, &r.Reason,
		&r.AppID, &r.AppName, &created, &decided, &r.DecidedBy, &claimed)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.Origins = strings.Fields(origins)
	r.CreatedAt = time.Unix(created, 0).UTC()
	if decided > 0 {
		r.DecidedAt = time.Unix(decided, 0).UTC()
	}
	if claimed > 0 {
		r.KeyClaimedAt = time.Unix(claimed, 0).UTC()
	}
	return r, nil
}

// KeyRequestByID returns one request.
func (s *Store) KeyRequestByID(ctx context.Context, id int64) (KeyRequest, error) {
	return scanRequest(s.db.QueryRowContext(ctx, `SELECT `+requestCols+requestFrom+`WHERE r.id = ?`, id))
}

// ListKeyRequests returns requests with the given status ("" = all), oldest
// pending first, otherwise newest first.
func (s *Store) ListKeyRequests(ctx context.Context, status string) ([]KeyRequest, error) {
	q := `SELECT ` + requestCols + requestFrom
	var args []any
	if status != "" {
		q += `WHERE r.status = ? `
		args = append(args, status)
	}
	if status == RequestPending {
		q += `ORDER BY r.created_at, r.id`
	} else {
		q += `ORDER BY r.created_at DESC, r.id DESC LIMIT 200`
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyRequest
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ApproveKeyRequest creates the application described by in (the operator
// may have edited the requested values) without a key: the requester claims
// it with their status token.
func (s *Store) ApproveKeyRequest(ctx context.Context, id int64, in NewApp, actor string) (App, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return App{}, err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM key_requests WHERE id = ?`, id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return App{}, ErrNotFound
		}
		return App{}, err
	}
	if status != RequestPending {
		return App{}, ErrNotPending
	}
	app, err := s.insertApp(ctx, tx, in, "")
	if err != nil {
		return App{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE key_requests SET status = 'approved', app_id = ?, decided_at = ?, decided_by = ? WHERE id = ?`,
		app.ID, s.now().Unix(), actor, id); err != nil {
		return App{}, err
	}
	return app, tx.Commit()
}

// ErrNotPending is returned when deciding on an already decided request.
var ErrNotPending = errors.New("request is not pending")

// RejectKeyRequest rejects a pending request; reason is shown to the
// requester.
func (s *Store) RejectKeyRequest(ctx context.Context, id int64, reason, actor string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE key_requests SET status = 'rejected', reason = ?, decided_at = ?, decided_by = ? WHERE id = ? AND status = 'pending'`,
		reason, s.now().Unix(), actor, id)
	if err := affectedOne(res, err); err != nil {
		if errors.Is(err, ErrNotFound) {
			if _, e := s.KeyRequestByID(ctx, id); e == nil {
				return ErrNotPending
			}
		}
		return err
	}
	return nil
}

// KeyClaim is what a requester sees when checking their request.
type KeyClaim struct {
	Request KeyRequest
	// Key is set exactly once: the first check after approval.
	Key string
	// AlreadyClaimed is true when the key was issued earlier (to the
	// requester, or by an operator rotating it).
	AlreadyClaimed bool
}

// ClaimKeyRequest looks up a request by status token. If it is approved and
// its key was never issued, a key is generated now, stored hashed and
// returned: it is never stored in clear, and never shown again.
func (s *Store) ClaimKeyRequest(ctx context.Context, token string) (KeyClaim, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return KeyClaim{}, err
	}
	defer tx.Rollback()
	r, err := scanRequest(tx.QueryRowContext(ctx, `SELECT `+requestCols+requestFrom+`WHERE r.status_token_hash = ?`, hashSecret(token)))
	if err != nil {
		return KeyClaim{}, err
	}
	c := KeyClaim{Request: r}
	if r.Status != RequestApproved || r.AppID == 0 {
		return c, nil
	}
	if !r.KeyClaimedAt.IsZero() {
		c.AlreadyClaimed = true
		return c, nil
	}
	key := newSecret("app_")
	res, err := tx.ExecContext(ctx, `UPDATE apps SET key_hash = ? WHERE id = ? AND key_hash IS NULL`, hashSecret(key), r.AppID)
	if err != nil {
		return KeyClaim{}, err
	}
	now := s.now().Unix()
	if _, err := tx.ExecContext(ctx, `UPDATE key_requests SET key_claimed_at = ? WHERE id = ?`, now, r.ID); err != nil {
		return KeyClaim{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		c.AlreadyClaimed = true // operator already issued a key
	} else {
		c.Key = key
		c.Request.KeyClaimedAt = time.Unix(now, 0).UTC()
	}
	return c, tx.Commit()
}

// CountPendingKeyRequests returns the number of requests awaiting a decision.
func (s *Store) CountPendingKeyRequests(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM key_requests WHERE status = 'pending'`).Scan(&n)
	return n, err
}
