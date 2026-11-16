package admin_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rm-info/push-unaware-serving-host/internal/admin"
	"github.com/rm-info/push-unaware-serving-host/internal/api"
	"github.com/rm-info/push-unaware-serving-host/internal/httpx"
	"github.com/rm-info/push-unaware-serving-host/internal/keyreq"
	"github.com/rm-info/push-unaware-serving-host/internal/portal"
	"github.com/rm-info/push-unaware-serving-host/internal/store"
	"github.com/rm-info/push-unaware-serving-host/internal/ui"
)

type env struct {
	t      *testing.T
	srv    *httptest.Server
	st     *store.Store
	client *http.Client
}

func setup(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxies := httpx.DefaultTrustedProxies
	svc := keyreq.New(st, log, 100)
	mux := http.NewServeMux()
	mux.Handle("/", (&api.Server{Store: st, KeyRequests: svc, TrustedProxies: proxies, Log: log}).Handler())
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", ui.Static()))
	p := (&portal.Server{Service: svc, Log: log, TrustedProxies: proxies}).Handler()
	mux.Handle("/request-key", p)
	mux.Handle("/request-key/", p)
	adm := admin.New(admin.Config{Store: st, Log: log, TrustedProxies: proxies, SecureCookie: true, RequestsEnabled: true}).Handler()
	mux.Handle("/admin", adm)
	mux.Handle("/admin/", adm)

	e := &env{t: t, st: st}
	e.srv = httptest.NewTLSServer(mux)
	e.client = e.newClient()
	t.Cleanup(func() { e.srv.Close(); st.Close() })
	return e
}

func (e *env) newClient() *http.Client {
	// srv.Client() always returns the same *http.Client: copy it so each
	// client has its own cookie jar.
	jar, _ := cookiejar.New(nil)
	return &http.Client{Transport: e.srv.Client().Transport, Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

type page struct {
	status int
	header http.Header
	body   string
}

func (e *env) req(c *http.Client, method, path string, form url.Values, hdr map[string]string) page {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r, _ := http.NewRequest(method, e.srv.URL+path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	resp, err := c.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return page{resp.StatusCode, resp.Header, string(b)}
}

func (e *env) get(path string) page { return e.req(e.client, "GET", path, nil, nil) }
func (e *env) post(path string, form url.Values) page {
	return e.req(e.client, "POST", path, form, nil)
}

var (
	csrfRe  = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	keyRe   = regexp.MustCompile(`app_[A-Za-z0-9_-]{43}`)
	tokenRe = regexp.MustCompile(`req_[A-Za-z0-9_-]{43}`)
)

func find(t *testing.T, re *regexp.Regexp, body string) string {
	t.Helper()
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("%s not found in:\n%s", re, body)
	}
	return m[len(m)-1]
}

func (e *env) addAdmin(user, pass string) {
	h, err := admin.HashPassword(pass)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.CreateAdmin(context.Background(), user, h); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) login(user, pass string) string {
	e.t.Helper()
	p := e.post("/admin/login", url.Values{"username": {user}, "password": {pass}})
	if p.status != http.StatusSeeOther {
		e.t.Fatalf("login: %d %s", p.status, p.body)
	}
	return find(e.t, csrfRe, e.get("/admin/").body)
}

func (e *env) apiStatus(key string) (int, string) {
	r, _ := http.NewRequest("GET", e.srv.URL+"/v1/reminders", nil)
	r.Header.Set("X-App-Key", key)
	resp, err := e.client.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestFullRequestApprovalFlow(t *testing.T) {
	e := setup(t)
	e.addAdmin("alice", "correct horse battery")

	// 1. A third party submits a request through the public form.
	p := e.get("/request-key")
	if p.status != 200 || !strings.Contains(p.header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("form: %d %v", p.status, p.header)
	}
	p = e.post("/request-key", url.Values{
		"name": {"pill-app"}, "contact": {"dev@pill.test"}, "origins": {"https://pill.test\nhttps://www.pill.test"},
		"description": {"Reminders <script>alert(1)</script>"},
	})
	if p.status != 200 {
		t.Fatalf("submit: %d %s", p.status, p.body)
	}
	token := find(t, tokenRe, p.body)

	p = e.post("/request-key/status", url.Values{"token": {token}})
	if p.status != 200 || !strings.Contains(p.body, "pending review") {
		t.Fatalf("status pending: %d %s", p.status, p.body)
	}

	// 2. Admin pages require a session.
	if p = e.get("/admin/"); p.status != http.StatusSeeOther || p.header.Get("Location") != "/admin/login" {
		t.Fatalf("unauthenticated dashboard: %d", p.status)
	}
	if p = e.post("/admin/requests/1/approve", url.Values{}); p.status != http.StatusForbidden {
		t.Fatalf("unauthenticated POST: %d", p.status)
	}

	// 3. Sign in: hardened cookie.
	p = e.post("/admin/login", url.Values{"username": {"alice"}, "password": {"wrong password!!"}})
	if p.status != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", p.status)
	}
	p = e.post("/admin/login", url.Values{"username": {"alice"}, "password": {"correct horse battery"}})
	ck := p.header.Get("Set-Cookie")
	for _, want := range []string{"__Host-push_admin=", "HttpOnly", "Secure", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(ck, want) {
			t.Fatalf("cookie %q lacks %s", ck, want)
		}
	}
	dash := e.get("/admin/")
	if dash.status != 200 || !strings.Contains(dash.body, "pill-app") || !strings.Contains(dash.body, "dev@pill.test") {
		t.Fatalf("dashboard: %d %s", dash.status, dash.body)
	}
	csrf := find(t, csrfRe, dash.body)

	// Escaping: the description is shown as text.
	rp := e.get("/admin/requests/1")
	if strings.Contains(rp.body, "<script>alert(1)</script>") || !strings.Contains(rp.body, "&lt;script&gt;") {
		t.Fatal("description not escaped")
	}

	approve := url.Values{"name": {"pill-app"}, "origins": {"https://pill.test"}, "max_subscriptions": {"50"}, "max_reminders": {"20"}}

	// 4. CSRF protections.
	if p = e.post("/admin/requests/1/approve", approve); p.status != http.StatusForbidden {
		t.Fatalf("missing csrf: %d", p.status)
	}
	withCSRF := url.Values{"csrf": {csrf}}
	for k, v := range approve {
		withCSRF[k] = v
	}
	if p = e.req(e.client, "POST", "/admin/requests/1/approve", withCSRF, map[string]string{"Sec-Fetch-Site": "cross-site"}); p.status != http.StatusForbidden {
		t.Fatalf("cross-site: %d", p.status)
	}
	// Validation error is shown in the form, nothing created.
	bad := url.Values{"csrf": {csrf}, "name": {"Bad Name"}, "origins": {""}, "max_subscriptions": {"1"}, "max_reminders": {"1"}}
	if p = e.post("/admin/requests/1/approve", bad); p.status != http.StatusUnprocessableEntity || !strings.Contains(p.body, "lowercase") {
		t.Fatalf("invalid approve: %d %s", p.status, p.body)
	}

	// 5. Approve.
	if p = e.post("/admin/requests/1/approve", withCSRF); p.status != http.StatusSeeOther {
		t.Fatalf("approve: %d %s", p.status, p.body)
	}
	if p = e.post("/admin/requests/1/approve", withCSRF); p.status != http.StatusUnprocessableEntity {
		t.Fatalf("double approve: %d", p.status)
	}
	app, err := e.st.AppByName(context.Background(), "pill-app")
	if err != nil || app.HasKey || app.MaxSubscriptions != 50 || len(app.Origins) != 1 || app.Contact != "dev@pill.test" {
		t.Fatalf("approved app: %+v %v", app, err)
	}

	// 6. The requester claims the key, once.
	anon := e.newClient()
	p = e.req(anon, "POST", "/request-key/status", url.Values{"token": {token}}, nil)
	key := find(t, keyRe, p.body)
	if !strings.Contains(p.header.Get("Cache-Control"), "no-store") {
		t.Fatal("key page cacheable")
	}
	if s, b := e.apiStatus(key); s != 401 || !strings.Contains(b, "missing_token") { // key accepted, device token missing
		t.Fatalf("claimed key: %d %s", s, b)
	}
	p = e.req(anon, "POST", "/request-key/status", url.Values{"token": {token}}, nil)
	if keyRe.MatchString(p.body) || !strings.Contains(p.body, "already issued") {
		t.Fatal("key shown twice")
	}

	// 7. Revoke (disable) and restore.
	if p = e.post("/admin/apps/pill-app/disable", url.Values{"csrf": {csrf}}); p.status != http.StatusSeeOther {
		t.Fatalf("disable: %d %s", p.status, p.body)
	}
	if s, b := e.apiStatus(key); s != 403 || !strings.Contains(b, "app_disabled") {
		t.Fatalf("disabled key: %d %s", s, b)
	}
	e.post("/admin/apps/pill-app/enable", url.Values{"csrf": {csrf}})
	if s, _ := e.apiStatus(key); s != 401 {
		t.Fatalf("re-enabled key: %d", s)
	}

	// 8. Rotate: new key shown to the admin, old key dead.
	p = e.post("/admin/apps/pill-app/rotate", url.Values{"csrf": {csrf}})
	newKey := find(t, keyRe, p.body)
	if newKey == key {
		t.Fatal("key not rotated")
	}
	if s, b := e.apiStatus(key); s != 401 || !strings.Contains(b, "invalid_app_key") {
		t.Fatalf("old key after rotate: %d %s", s, b)
	}

	// 9. Delete needs the exact name.
	if p = e.post("/admin/apps/pill-app/delete", url.Values{"csrf": {csrf}, "confirm": {"pill"}}); p.status != http.StatusUnprocessableEntity {
		t.Fatalf("delete without confirm: %d", p.status)
	}
	if p = e.post("/admin/apps/pill-app/delete", url.Values{"csrf": {csrf}, "confirm": {"pill-app"}}); p.status != http.StatusSeeOther {
		t.Fatalf("delete: %d", p.status)
	}
	if s, _ := e.apiStatus(newKey); s != 401 {
		t.Fatal("deleted app's key still works")
	}

	// 10. Audit trail and logout.
	dash = e.get("/admin/")
	for _, a := range []string{"approve request", "disable app", "enable app", "rotate key", "delete app", "login"} {
		if !strings.Contains(dash.body, a) {
			t.Errorf("audit lacks %q", a)
		}
	}
	e.post("/admin/logout", url.Values{"csrf": {csrf}})
	if p = e.get("/admin/"); p.status != http.StatusSeeOther {
		t.Fatal("session survived logout")
	}
}

func TestRejectAndCreate(t *testing.T) {
	e := setup(t)
	e.addAdmin("bob", "another long password")
	p := e.post("/request-key", url.Values{"name": {"spam-app"}, "contact": {"s@spam.test"}, "origins": {"https://spam.test"}, "description": {"buy now"}})
	token := find(t, tokenRe, p.body)
	csrf := e.login("bob", "another long password")

	if p = e.post("/admin/requests/1/reject", url.Values{"csrf": {csrf}, "reason": {"Not a PWA."}}); p.status != http.StatusSeeOther {
		t.Fatalf("reject: %d %s", p.status, p.body)
	}
	p = e.post("/request-key/status", url.Values{"token": {token}})
	if !strings.Contains(p.body, "rejected") || !strings.Contains(p.body, "Not a PWA.") || keyRe.MatchString(p.body) {
		t.Fatalf("rejected status: %s", p.body)
	}

	// Direct creation by the admin shows the key once.
	p = e.post("/admin/apps/new", url.Values{"csrf": {csrf}, "name": {"internal"}, "origins": {"https://in.test"},
		"max_subscriptions": {"5"}, "max_reminders": {"5"}})
	key := find(t, keyRe, p.body)
	if s, _ := e.apiStatus(key); s != 401 {
		t.Fatalf("created key: %d", s)
	}
	if p = e.post("/admin/apps/new", url.Values{"csrf": {csrf}, "name": {"internal"}, "max_subscriptions": {"5"}, "max_reminders": {"5"}}); p.status != http.StatusUnprocessableEntity || !strings.Contains(p.body, "already exists") {
		t.Fatalf("duplicate: %d", p.status)
	}
}

func TestLoginThrottling(t *testing.T) {
	e := setup(t)
	e.addAdmin("carol", "the real password")
	p := e.get("/admin/login")
	if strings.Contains(p.body, "No administrator account") {
		t.Fatal("setup notice shown although an admin exists")
	}
	for i := 0; i < 10; i++ {
		e.post("/admin/login", url.Values{"username": {"carol"}, "password": {"guess"}})
	}
	p = e.post("/admin/login", url.Values{"username": {"carol"}, "password": {"the real password"}})
	if p.status != http.StatusTooManyRequests {
		t.Fatalf("not throttled: %d", p.status)
	}
	// Unknown usernames fail the same way.
	e2 := setup(t)
	if p = e2.get("/admin/login"); !strings.Contains(p.body, "No administrator account") {
		t.Fatal("no setup notice")
	}
	if p = e2.post("/admin/login", url.Values{"username": {"ghost"}, "password": {"whatever"}}); p.status != http.StatusUnauthorized {
		t.Fatalf("unknown user: %d", p.status)
	}
}

func TestPublicFormValidationAndHoneypot(t *testing.T) {
	e := setup(t)
	cases := []url.Values{
		{"name": {"x"}, "contact": {"a@b.test"}, "origins": {"https://a.test"}, "description": {"d"}},
		{"name": {"good-name"}, "contact": {"nope"}, "origins": {"https://a.test"}, "description": {"d"}},
		{"name": {"good-name"}, "contact": {"a@b.test"}, "origins": {"http://a.test"}, "description": {"d"}},
		{"name": {"good-name"}, "contact": {"a@b.test"}, "origins": {""}, "description": {"d"}},
		{"name": {"good-name"}, "contact": {"a@b.test"}, "origins": {"https://a.test"}, "description": {""}},
	}
	for _, f := range cases {
		if p := e.post("/request-key", f); p.status != http.StatusUnprocessableEntity {
			t.Errorf("%v: %d", f, p.status)
		}
	}
	p := e.post("/request-key", url.Values{"name": {"bot-app"}, "contact": {"a@b.test"}, "origins": {"https://a.test"}, "description": {"d"}, "website": {"http://spam"}})
	if p.status != 200 {
		t.Fatal(p.status)
	}
	if n, _ := e.st.CountPendingKeyRequests(context.Background()); n != 0 {
		t.Fatal("honeypot request stored")
	}
	// Rate limit: 5 per hour per IP.
	for i := 0; i < 5; i++ {
		e.post("/request-key", url.Values{"name": {"app-" + string(rune('a'+i))}, "contact": {"a@b.test"}, "origins": {"https://a.test"}, "description": {"d"}})
	}
	if p = e.post("/request-key", url.Values{"name": {"app-z"}, "contact": {"a@b.test"}, "origins": {"https://a.test"}, "description": {"d"}}); p.status != http.StatusTooManyRequests {
		t.Fatalf("rate limit: %d", p.status)
	}
}
