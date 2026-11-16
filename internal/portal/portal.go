// Package portal serves the public HTML pages where third parties request an
// API key and later retrieve it.
package portal

import (
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"

	"github.com/rm-info/push-unaware-serving-host/internal/admin"
	"github.com/rm-info/push-unaware-serving-host/internal/httpx"
	"github.com/rm-info/push-unaware-serving-host/internal/keyreq"
	"github.com/rm-info/push-unaware-serving-host/internal/store"
	"github.com/rm-info/push-unaware-serving-host/internal/ui"
)

// Server serves /request-key.
type Server struct {
	Service        *keyreq.Service
	Log            *slog.Logger
	TrustedProxies []netip.Prefix
	PublicURL      string
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /request-key", s.form)
	mux.HandleFunc("POST /request-key", s.submit)
	mux.HandleFunc("GET /request-key/status", s.statusForm)
	mux.HandleFunc("POST /request-key/status", s.status)
	return mux
}

type formData struct{ Name, Contact, Origins, Description string }

func (s *Server) form(w http.ResponseWriter, r *http.Request) {
	ui.Render(w, http.StatusOK, "portal_request", ui.Page{Title: "Request an API key", Data: formData{}})
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		s.fail(w, http.StatusBadRequest, "Invalid form.")
		return
	}
	f := formData{
		Name:        r.PostFormValue("name"),
		Contact:     r.PostFormValue("contact"),
		Origins:     r.PostFormValue("origins"),
		Description: r.PostFormValue("description"),
	}
	if r.PostFormValue("website") != "" { // honeypot: bots fill every field
		ui.Render(w, http.StatusOK, "portal_submitted", ui.Page{Title: "Request submitted", Data: map[string]string{"Token": "req_" + strings.Repeat("x", 43)}})
		return
	}
	token, err := s.Service.Submit(r.Context(), httpx.ClientIP(r, s.TrustedProxies), keyreq.Raw{
		Name: f.Name, Contact: f.Contact, Origins: strings.Fields(strings.ReplaceAll(f.Origins, ",", " ")), Description: f.Description,
	})
	if err != nil {
		status, msg := userError(err)
		if status == http.StatusInternalServerError {
			s.Log.Error("key request", "err", err)
		}
		ui.Render(w, status, "portal_request", ui.Page{Title: "Request an API key", Error: msg, Data: f})
		return
	}
	ui.Render(w, http.StatusOK, "portal_submitted", ui.Page{Title: "Request submitted", Data: map[string]string{"Token": token}})
}

func (s *Server) statusForm(w http.ResponseWriter, r *http.Request) {
	ui.Render(w, http.StatusOK, "portal_status_form", ui.Page{Title: "Check a request"})
}

type statusData struct{ Claim store.KeyClaim }

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	c, err := s.Service.Check(r.Context(), httpx.ClientIP(r, s.TrustedProxies), r.PostFormValue("token"))
	if err != nil {
		status, msg := userError(err)
		if status == http.StatusInternalServerError {
			s.Log.Error("key request status", "err", err)
		}
		ui.Render(w, status, "portal_status_form", ui.Page{Title: "Check a request", Error: msg})
		return
	}
	ui.Render(w, http.StatusOK, "portal_status", ui.Page{Title: "Request " + c.Request.Name,
		BaseURL: admin.BaseURL(s.PublicURL, r), Data: statusData{Claim: c}})
}

// userError maps service errors to an HTTP status and a safe message.
func userError(err error) (int, string) {
	var inv keyreq.InvalidError
	switch {
	case errors.As(err, &inv):
		return http.StatusUnprocessableEntity, inv.Error()
	case errors.Is(err, keyreq.ErrRateLimited):
		return http.StatusTooManyRequests, err.Error()
	case errors.Is(err, keyreq.ErrQueueFull):
		return http.StatusServiceUnavailable, err.Error()
	case errors.Is(err, keyreq.ErrNameTaken):
		return http.StatusConflict, err.Error()
	case errors.Is(err, keyreq.ErrBadToken):
		return http.StatusNotFound, err.Error()
	}
	return http.StatusInternalServerError, "Internal error, try again later."
}

// UserError is exported for the JSON API.
func UserError(err error) (int, string) { return userError(err) }

func (s *Server) fail(w http.ResponseWriter, status int, msg string) {
	ui.Render(w, status, "error", ui.Page{Title: "Error", Data: msg})
}
