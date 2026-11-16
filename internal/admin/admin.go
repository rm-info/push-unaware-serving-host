// Package admin serves the operator web UI: review key requests, create,
// edit, rotate, disable and delete applications.
//
// Security model: accounts are created with the CLI only; passwords are
// bcrypt-hashed; sessions live in an HttpOnly, Secure, SameSite=Strict
// cookie and are stored hashed with a 12h lifetime; every state-changing
// request needs the session's CSRF token and is refused when the browser
// flags it as cross-site; failed logins are rate limited per IP and per
// username; pages run without JavaScript under a strict CSP.
package admin

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/rm-info/push-unaware-serving-host/internal/httpx"
	"github.com/rm-info/push-unaware-serving-host/internal/store"
	"github.com/rm-info/push-unaware-serving-host/internal/ui"
	"github.com/rm-info/push-unaware-serving-host/internal/validate"
)

// Default quotas proposed for new applications.
const (
	DefaultMaxSubscriptions = 10000
	DefaultMaxReminders     = 100
)

const bcryptCost = 12

// HashPassword hashes an admin password.
func HashPassword(p string) (string, error) {
	if err := validate.Password(p); err != nil {
		return "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(p), bcryptCost)
	return string(h), err
}

// dummyHash makes unknown-username logins cost as much as real ones.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not-a-real-password"), bcryptCost)

// Config configures the admin UI.
type Config struct {
	Store          *store.Store
	Log            *slog.Logger
	TrustedProxies []netip.Prefix
	// SecureCookie marks the session cookie Secure (and uses the __Host-
	// prefix). Only disable for plain-HTTP access on a trusted network.
	SecureCookie    bool
	SessionTTL      time.Duration
	RequestsEnabled bool
	PublicURL       string // optional; derived from the request otherwise
}

// Server is the admin UI.
type Server struct {
	cfg       Config
	loginIP   *httpx.Limiter
	loginUser *httpx.Limiter
}

// New creates the admin UI.
func New(cfg Config) *Server {
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	return &Server{
		cfg:       cfg,
		loginIP:   httpx.NewLimiter(10, 15*time.Minute),
		loginUser: httpx.NewLimiter(20, 15*time.Minute),
	}
}

func (s *Server) cookieName() string {
	if s.cfg.SecureCookie {
		return "__Host-push_admin"
	}
	return "push_admin"
}

// Handler returns the routes, all under /admin/.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/login", s.loginPage)
	mux.HandleFunc("POST /admin/login", s.login)
	mux.Handle("POST /admin/logout", s.auth(s.logout))

	mux.Handle("GET /admin/{$}", s.auth(s.dashboard))
	mux.Handle("GET /admin/requests/{id}", s.auth(s.requestPage))
	mux.Handle("POST /admin/requests/{id}/approve", s.auth(s.approve))
	mux.Handle("POST /admin/requests/{id}/reject", s.auth(s.reject))

	mux.Handle("GET /admin/apps/new", s.auth(s.newAppPage))
	mux.Handle("POST /admin/apps/new", s.auth(s.createApp))
	mux.Handle("GET /admin/apps/{name}", s.auth(s.appPage))
	mux.Handle("POST /admin/apps/{name}/update", s.auth(s.updateApp))
	mux.Handle("POST /admin/apps/{name}/rotate", s.auth(s.rotateKey))
	mux.Handle("POST /admin/apps/{name}/disable", s.auth(s.setDisabled(true)))
	mux.Handle("POST /admin/apps/{name}/enable", s.auth(s.setDisabled(false)))
	mux.Handle("POST /admin/apps/{name}/delete", s.auth(s.deleteApp))

	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusSeeOther)
	})
	mux.HandleFunc("/admin/", func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, http.StatusNotFound, "Not found", "This page does not exist.")
	})
	return mux
}

// --- authentication ---------------------------------------------------------

type handler func(w http.ResponseWriter, r *http.Request, sess store.Session)

func (s *Server) auth(h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(s.cookieName())
		var sess store.Session
		if err == nil {
			sess, err = s.cfg.Store.SessionByToken(r.Context(), c.Value)
		}
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, http.ErrNoCookie) {
				s.internal(w, err)
				return
			}
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			} else {
				s.fail(w, http.StatusForbidden, "Session expired", "Your session has expired: sign in again.")
			}
			return
		}
		if r.Method != http.MethodGet {
			if httpx.CrossSite(r) {
				s.fail(w, http.StatusForbidden, "Forbidden", "Cross-site request refused.")
				return
			}
			_ = r.ParseForm()
			if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.CSRFToken)) != 1 {
				s.fail(w, http.StatusForbidden, "Forbidden", "Invalid form token: reload the page and try again.")
				return
			}
		}
		h(w, r, sess)
	})
}

func (s *Server) insecure(r *http.Request) bool {
	if !s.cfg.SecureCookie || r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		return false
	}
	host := r.Host
	if h, _, err := splitHost(host); err == nil {
		host = h
	}
	return host != "localhost" && host != "127.0.0.1" && host != "::1"
}

func splitHost(hostport string) (string, string, error) {
	u, err := url.Parse("//" + hostport)
	if err != nil {
		return "", "", err
	}
	return u.Hostname(), u.Port(), nil
}

type loginData struct {
	NoAdmins bool
	Insecure bool
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	n, err := s.cfg.Store.CountAdmins(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	ui.Render(w, status, "admin_login", ui.Page{Title: "Sign in", Error: errMsg,
		Data: loginData{NoAdmins: n == 0, Insecure: s.insecure(r)}})
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.renderLogin(w, r, http.StatusOK, "")
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if httpx.CrossSite(r) {
		s.fail(w, http.StatusForbidden, "Forbidden", "Cross-site request refused.")
		return
	}
	ip := httpx.ClientIP(r, s.cfg.TrustedProxies)
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	if s.loginIP.Blocked(ip) || s.loginUser.Blocked(username) {
		s.cfg.Log.Warn("admin login throttled")
		s.renderLogin(w, r, http.StatusTooManyRequests, "Too many failed attempts. Try again in 15 minutes.")
		return
	}
	adm, err := s.cfg.Store.AdminByUsername(r.Context(), username)
	hash := dummyHash
	if err == nil {
		hash = []byte(adm.PasswordHash)
	} else if !errors.Is(err, store.ErrNotFound) {
		s.internal(w, err)
		return
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil || err != nil {
		s.loginIP.Add(ip)
		s.loginUser.Add(username)
		s.cfg.Log.Warn("admin login failed")
		s.renderLogin(w, r, http.StatusUnauthorized, "Wrong username or password.")
		return
	}
	s.loginIP.Reset(ip)
	s.loginUser.Reset(username)
	token, sess, err := s.cfg.Store.CreateSession(r.Context(), adm.ID, s.cfg.SessionTTL)
	if err != nil {
		s.internal(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: token, Path: "/", Expires: sess.ExpiresAt,
		HttpOnly: true, Secure: s.cfg.SecureCookie, SameSite: http.SameSiteStrictMode,
	})
	s.audit(r.Context(), adm.Username, "login", "", "")
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, sess store.Session) {
	if c, err := r.Cookie(s.cookieName()); err == nil {
		_ = s.cfg.Store.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.SecureCookie, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// --- pages ------------------------------------------------------------------

var flashes = map[string]string{
	"approved": "Request approved. The requester can now retrieve the key with their status token.",
	"rejected": "Request rejected.",
	"updated":  "Application updated.",
	"disabled": "Application disabled: its key is refused and its pushes are paused.",
	"enabled":  "Application enabled.",
	"deleted":  "Application deleted.",
}

func (s *Server) page(r *http.Request, sess store.Session, title string, data any) ui.Page {
	return ui.Page{Title: title, Admin: sess.Username, CSRF: sess.CSRFToken, Flash: flashes[r.URL.Query().Get("ok")],
		BaseURL: s.baseURL(r), Data: data}
}

type dashboardData struct {
	Pending, Decided []store.KeyRequest
	Apps             []store.AppStats
	Audit            []store.AuditEntry
	RequestsEnabled  bool
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request, sess store.Session) {
	ctx := r.Context()
	var d dashboardData
	var err error
	if d.Pending, err = s.cfg.Store.ListKeyRequests(ctx, store.RequestPending); err != nil {
		s.internal(w, err)
		return
	}
	all, err := s.cfg.Store.ListKeyRequests(ctx, "")
	if err != nil {
		s.internal(w, err)
		return
	}
	for _, kr := range all {
		if kr.Status != store.RequestPending && len(d.Decided) < 10 {
			d.Decided = append(d.Decided, kr)
		}
	}
	if d.Apps, err = s.cfg.Store.ListApps(ctx); err != nil {
		s.internal(w, err)
		return
	}
	if d.Audit, err = s.cfg.Store.ListAudit(ctx, 20); err != nil {
		s.internal(w, err)
		return
	}
	d.RequestsEnabled = s.cfg.RequestsEnabled
	ui.Render(w, http.StatusOK, "admin_dashboard", s.page(r, sess, "Dashboard", d))
}

type appForm struct {
	Name, Origins, Contact, Description string
	MaxSubs, MaxRems                    int
}

type requestData struct {
	Request store.KeyRequest
	Form    appForm
}

func (s *Server) loadRequest(w http.ResponseWriter, r *http.Request) (store.KeyRequest, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, http.StatusNotFound, "Not found", "No such request.")
		return store.KeyRequest{}, false
	}
	kr, err := s.cfg.Store.KeyRequestByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.fail(w, http.StatusNotFound, "Not found", "No such request.")
		return kr, false
	}
	if err != nil {
		s.internal(w, err)
		return kr, false
	}
	return kr, true
}

func (s *Server) requestPage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	kr, ok := s.loadRequest(w, r)
	if !ok {
		return
	}
	form := appForm{Name: kr.Name, Origins: strings.Join(kr.Origins, "\n"), MaxSubs: DefaultMaxSubscriptions, MaxRems: DefaultMaxReminders}
	ui.Render(w, http.StatusOK, "admin_request", s.page(r, sess, "Request "+kr.Name, requestData{Request: kr, Form: form}))
}

// parseAppForm validates the shared application fields.
func parseAppForm(r *http.Request, originsRequired bool) (appForm, store.NewApp, error) {
	f := appForm{
		Name:        strings.TrimSpace(r.PostFormValue("name")),
		Origins:     r.PostFormValue("origins"),
		Contact:     strings.TrimSpace(r.PostFormValue("contact")),
		Description: r.PostFormValue("description"),
	}
	f.MaxSubs, _ = strconv.Atoi(r.PostFormValue("max_subscriptions"))
	f.MaxRems, _ = strconv.Atoi(r.PostFormValue("max_reminders"))
	in := store.NewApp{Name: f.Name, MaxSubscriptions: f.MaxSubs, MaxRemindersPerSub: f.MaxRems}
	var err error
	if r.PostForm.Has("name") {
		if err = validate.AppName(f.Name); err != nil {
			return f, in, userErr(err)
		}
	}
	if in.Origins, err = validate.Origins(f.Origins, originsRequired); err != nil {
		return f, in, userErr(err)
	}
	if err = validate.Quota(f.MaxSubs, "max devices"); err != nil {
		return f, in, userErr(err)
	}
	if err = validate.Quota(f.MaxRems, "max reminders per device"); err != nil {
		return f, in, userErr(err)
	}
	if f.Contact != "" {
		if in.Contact, err = validate.Contact(f.Contact); err != nil {
			return f, in, userErr(err)
		}
	}
	if in.Description, err = validate.Text(f.Description, "description", 1000, false); err != nil {
		return f, in, userErr(err)
	}
	return f, in, nil
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request, sess store.Session) {
	kr, ok := s.loadRequest(w, r)
	if !ok {
		return
	}
	form, in, err := parseAppForm(r, false)
	if err == nil {
		in.Contact, in.Description = kr.Contact, kr.Description
		_, err = s.cfg.Store.ApproveKeyRequest(r.Context(), kr.ID, in, sess.Username)
		if errors.Is(err, store.ErrExists) {
			err = userErr(fmt.Errorf("an application named %q already exists: pick another name", in.Name))
		} else if errors.Is(err, store.ErrNotPending) {
			err = userErr(errors.New("this request was already decided"))
		}
	}
	if err != nil {
		if !isUserError(err) {
			s.internal(w, err)
			return
		}
		p := s.page(r, sess, "Request "+kr.Name, requestData{Request: kr, Form: form})
		p.Error = err.Error()
		ui.Render(w, http.StatusUnprocessableEntity, "admin_request", p)
		return
	}
	s.audit(r.Context(), sess.Username, "approve request", in.Name, fmt.Sprintf("request #%d", kr.ID))
	http.Redirect(w, r, "/admin/?ok=approved", http.StatusSeeOther)
}

func (s *Server) reject(w http.ResponseWriter, r *http.Request, sess store.Session) {
	kr, ok := s.loadRequest(w, r)
	if !ok {
		return
	}
	reason, err := validate.Text(r.PostFormValue("reason"), "reason", 500, false)
	err = userErr(err)
	if err == nil {
		err = s.cfg.Store.RejectKeyRequest(r.Context(), kr.ID, reason, sess.Username)
		if errors.Is(err, store.ErrNotPending) {
			err = userErr(errors.New("this request was already decided"))
		}
	}
	if err != nil {
		if !isUserError(err) {
			s.internal(w, err)
			return
		}
		p := s.page(r, sess, "Request "+kr.Name, requestData{Request: kr,
			Form: appForm{Name: kr.Name, Origins: strings.Join(kr.Origins, "\n"), MaxSubs: DefaultMaxSubscriptions, MaxRems: DefaultMaxReminders}})
		p.Error = err.Error()
		ui.Render(w, http.StatusUnprocessableEntity, "admin_request", p)
		return
	}
	s.audit(r.Context(), sess.Username, "reject request", kr.Name, fmt.Sprintf("request #%d", kr.ID))
	http.Redirect(w, r, "/admin/?ok=rejected", http.StatusSeeOther)
}

func (s *Server) newAppPage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	ui.Render(w, http.StatusOK, "admin_app_new", s.page(r, sess, "New application",
		appForm{MaxSubs: DefaultMaxSubscriptions, MaxRems: DefaultMaxReminders}))
}

type keyData struct{ App, Key string }

func (s *Server) createApp(w http.ResponseWriter, r *http.Request, sess store.Session) {
	form, in, err := parseAppForm(r, false)
	var key string
	if err == nil {
		_, key, err = s.cfg.Store.CreateApp(r.Context(), in)
		if errors.Is(err, store.ErrExists) {
			err = userErr(fmt.Errorf("an application named %q already exists", in.Name))
		}
	}
	if err != nil {
		if !isUserError(err) {
			s.internal(w, err)
			return
		}
		p := s.page(r, sess, "New application", form)
		p.Error = err.Error()
		ui.Render(w, http.StatusUnprocessableEntity, "admin_app_new", p)
		return
	}
	s.audit(r.Context(), sess.Username, "create app", in.Name, "")
	ui.Render(w, http.StatusOK, "admin_key", s.page(r, sess, "API key", keyData{App: in.Name, Key: key}))
}

type appData struct {
	App                      store.App
	Subscriptions, Reminders int
}

func (s *Server) loadApp(w http.ResponseWriter, r *http.Request) (appData, bool) {
	apps, err := s.cfg.Store.ListApps(r.Context())
	if err != nil {
		s.internal(w, err)
		return appData{}, false
	}
	for _, a := range apps {
		if a.Name == r.PathValue("name") {
			return appData{App: a.App, Subscriptions: a.Subscriptions, Reminders: a.Reminders}, true
		}
	}
	s.fail(w, http.StatusNotFound, "Not found", "No such application.")
	return appData{}, false
}

func (s *Server) appPage(w http.ResponseWriter, r *http.Request, sess store.Session) {
	d, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	ui.Render(w, http.StatusOK, "admin_app", s.page(r, sess, d.App.Name, d))
}

func (s *Server) updateApp(w http.ResponseWriter, r *http.Request, sess store.Session) {
	d, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	_, in, err := parseAppForm(r, false)
	if err == nil {
		app := d.App
		app.Origins, app.MaxSubscriptions, app.MaxRemindersPerSub = in.Origins, in.MaxSubscriptions, in.MaxRemindersPerSub
		err = s.cfg.Store.UpdateApp(r.Context(), app)
		if err != nil {
			s.internal(w, err)
			return
		}
	}
	if err != nil {
		p := s.page(r, sess, d.App.Name, d)
		p.Error = err.Error()
		ui.Render(w, http.StatusUnprocessableEntity, "admin_app", p)
		return
	}
	s.audit(r.Context(), sess.Username, "update app", d.App.Name,
		fmt.Sprintf("origins=%s max_devices=%d max_reminders=%d", strings.Join(in.Origins, " "), in.MaxSubscriptions, in.MaxRemindersPerSub))
	http.Redirect(w, r, "/admin/apps/"+url.PathEscape(d.App.Name)+"?ok=updated", http.StatusSeeOther)
}

func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request, sess store.Session) {
	name := r.PathValue("name")
	key, err := s.cfg.Store.RotateAppKey(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		s.fail(w, http.StatusNotFound, "Not found", "No such application.")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	s.audit(r.Context(), sess.Username, "rotate key", name, "")
	ui.Render(w, http.StatusOK, "admin_key", s.page(r, sess, "API key", keyData{App: name, Key: key}))
}

func (s *Server) setDisabled(disabled bool) handler {
	return func(w http.ResponseWriter, r *http.Request, sess store.Session) {
		name := r.PathValue("name")
		err := s.cfg.Store.SetAppDisabled(r.Context(), name, disabled)
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, http.StatusNotFound, "Not found", "No such application.")
			return
		}
		if err != nil {
			s.internal(w, err)
			return
		}
		action, flash := "enable app", "enabled"
		if disabled {
			action, flash = "disable app", "disabled"
		}
		s.audit(r.Context(), sess.Username, action, name, "")
		http.Redirect(w, r, "/admin/apps/"+url.PathEscape(name)+"?ok="+flash, http.StatusSeeOther)
	}
}

func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request, sess store.Session) {
	d, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	if r.PostFormValue("confirm") != d.App.Name {
		p := s.page(r, sess, d.App.Name, d)
		p.Error = "Confirmation does not match the application name: nothing was deleted."
		ui.Render(w, http.StatusUnprocessableEntity, "admin_app", p)
		return
	}
	if err := s.cfg.Store.DeleteApp(r.Context(), d.App.Name); err != nil {
		s.internal(w, err)
		return
	}
	s.audit(r.Context(), sess.Username, "delete app", d.App.Name,
		fmt.Sprintf("%d devices, %d reminders", d.Subscriptions, d.Reminders))
	http.Redirect(w, r, "/admin/?ok=deleted", http.StatusSeeOther)
}

// --- helpers ----------------------------------------------------------------

// userError is a validation or conflict message shown in the form; any
// other error is an internal failure.
type userError struct{ error }

func userErr(err error) error {
	if err == nil {
		return nil
	}
	return userError{err}
}

func isUserError(err error) bool {
	var ue userError
	return errors.As(err, &ue)
}

func (s *Server) baseURL(r *http.Request) string {
	return BaseURL(s.cfg.PublicURL, r)
}

// BaseURL returns the configured public URL or derives it from the request.
func BaseURL(configured string, r *http.Request) string {
	if configured != "" {
		return strings.TrimSuffix(configured, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) audit(ctx context.Context, actor, action, target, detail string) {
	if err := s.cfg.Store.Audit(ctx, actor, action, target, detail); err != nil {
		s.cfg.Log.Error("audit log", "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, status int, title, msg string) {
	ui.Render(w, status, "error", ui.Page{Title: title, Data: msg})
}

func (s *Server) internal(w http.ResponseWriter, err error) {
	s.cfg.Log.Error("admin request failed", "err", err)
	s.fail(w, http.StatusInternalServerError, "Error", "Internal error. See the server logs.")
}
