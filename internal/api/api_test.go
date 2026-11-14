package api

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rm-info/push-unaware-serving-host/internal/pushsend"
	"github.com/rm-info/push-unaware-serving-host/internal/store"
)

type tenv struct {
	t   *testing.T
	srv *httptest.Server
	st  *store.Store
	now time.Time
}

func setup(t *testing.T) *tenv {
	st, err := store.Open(filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatal(err)
	}
	e := &tenv{t: t, st: st, now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	s := &Server{Store: st, VAPIDPublicKey: "PUBKEY", AllowedPushHosts: pushsend.DefaultPushHosts,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return e.now }}
	e.srv = httptest.NewServer(s.Handler())
	t.Cleanup(func() { e.srv.Close(); st.Close() })
	return e
}

func (e *tenv) app(name string, origins []string, maxSubs, maxRems int) string {
	_, key, err := e.st.CreateApp(context.Background(), name, origins, maxSubs, maxRems)
	if err != nil {
		e.t.Fatal(err)
	}
	return key
}

type resp struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func (e *tenv) do(method, path string, headers map[string]string, body string) resp {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	out := resp{status: r.StatusCode, header: r.Header, raw: string(b)}
	_ = json.Unmarshal(b, &out.body)
	return out
}

func subJSON(endpoint string) string {
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	a := make([]byte, 16)
	rand.Read(a)
	return `{"endpoint":"` + endpoint + `","expirationTime":null,"keys":{"p256dh":"` +
		base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()) + `","auth":"` + base64.RawURLEncoding.EncodeToString(a) + `"}}`
}

func (e *tenv) register(appKey, endpoint string) string {
	e.t.Helper()
	r := e.do("POST", "/v1/subscriptions", map[string]string{"X-App-Key": appKey}, subJSON(endpoint))
	if r.status != 201 {
		e.t.Fatalf("register: %d %s", r.status, r.raw)
	}
	return r.body["token"].(string)
}

func hdr(appKey, token string) map[string]string {
	return map[string]string{"X-App-Key": appKey, "Authorization": "Bearer " + token}
}

const daily = `{"schedule":{"type":"daily","time":"08:00","tz":"Europe/Paris"}}`

func TestPublicRoutes(t *testing.T) {
	e := setup(t)
	if r := e.do("GET", "/healthz", nil, ""); r.status != 200 || r.body["status"] != "ok" {
		t.Fatalf("health %d %s", r.status, r.raw)
	}
	r := e.do("GET", "/v1/vapid-public-key", nil, "")
	if r.status != 200 || r.body["publicKey"] != "PUBKEY" || !strings.Contains(r.header.Get("Cache-Control"), "public") {
		t.Fatalf("vapid %d %s %v", r.status, r.raw, r.header)
	}
	if r := e.do("GET", "/v1/nope", nil, ""); r.status != 404 {
		t.Fatal(r.status)
	}
}

func TestReminderLifecycle(t *testing.T) {
	e := setup(t)
	key := e.app("a", nil, 10, 10)
	tok := e.register(key, "https://web.push.apple.com/abc")

	r := e.do("PUT", "/v1/reminders/11111111-2222-3333-4444-555555555555", hdr(key, tok), daily)
	if r.status != 201 || r.body["next_run_at"] != "2026-11-15T07:00:00Z" {
		t.Fatalf("put: %d %s", r.status, r.raw)
	}
	if r := e.do("PUT", "/v1/reminders/11111111-2222-3333-4444-555555555555", hdr(key, tok), daily); r.status != 200 {
		t.Fatalf("update: %d", r.status)
	}
	r = e.do("GET", "/v1/reminders", hdr(key, tok), "")
	list := r.body["reminders"].([]any)
	if r.status != 200 || len(list) != 1 {
		t.Fatalf("list: %s", r.raw)
	}
	if sched := list[0].(map[string]any)["schedule"].(map[string]any); sched["tz"] != "Europe/Paris" {
		t.Fatalf("schedule not echoed: %s", r.raw)
	}

	// Bulk replace; past one-shot reminders are silently dropped.
	r = e.do("PUT", "/v1/reminders", hdr(key, tok), `{"reminders":[
		{"id":"a","schedule":{"type":"once","at":"2026-11-14T13:00:00+01:00"}},
		{"id":"b","schedule":{"type":"once","at":"2026-11-14T14:00:00+01:00"}}]}`)
	if r.status != 200 || len(r.body["reminders"].([]any)) != 1 {
		t.Fatalf("replace: %d %s", r.status, r.raw)
	}
	if r := e.do("DELETE", "/v1/reminders/b", hdr(key, tok), ""); r.status != 204 {
		t.Fatalf("delete: %d", r.status)
	}
	if r := e.do("DELETE", "/v1/reminders/b", hdr(key, tok), ""); r.status != 404 {
		t.Fatalf("delete again: %d", r.status)
	}

	// Subscription update keeps the token; delete forgets everything.
	if r := e.do("PUT", "/v1/subscription", hdr(key, tok), subJSON("https://web.push.apple.com/new")); r.status != 204 {
		t.Fatalf("update sub: %d %s", r.status, r.raw)
	}
	if r := e.do("DELETE", "/v1/subscription", hdr(key, tok), ""); r.status != 204 {
		t.Fatalf("delete sub: %d", r.status)
	}
	if r := e.do("GET", "/v1/reminders", hdr(key, tok), ""); r.status != 401 {
		t.Fatalf("token still valid: %d", r.status)
	}
}

func TestValidation(t *testing.T) {
	e := setup(t)
	key := e.app("a", nil, 10, 10)
	tok := e.register(key, "https://fcm.googleapis.com/fcm/send/x")
	cases := []struct {
		method, path, body string
		want               int
		code               string
	}{
		{"PUT", "/v1/reminders/bad%20id", daily, 422, "invalid_id"},
		{"PUT", "/v1/reminders/" + strings.Repeat("x", 129), daily, 422, "invalid_id"},
		{"PUT", "/v1/reminders/ok", `{"schedule":{"type":"daily","time":"08:00"}}`, 422, "invalid_schedule"},
		{"PUT", "/v1/reminders/ok", `{"schedule":{"type":"once","at":"2020-01-01T00:00:00Z"}}`, 422, "no_occurrence"},
		{"PUT", "/v1/reminders/ok", `{}`, 422, "invalid_schedule"},
		{"PUT", "/v1/reminders/ok", `{"id":"other",` + daily[1:], 422, "invalid_id"},
		{"PUT", "/v1/reminders/ok", `{`, 400, "invalid_json"},
		{"PUT", "/v1/reminders/ok", daily + `{}`, 400, "invalid_json"},
		{"PUT", "/v1/reminders", `{}`, 422, "invalid_body"},
		{"PUT", "/v1/reminders", `{"reminders":[{"id":"x","schedule":{"type":"daily","time":"08:00","tz":"UTC"}},{"id":"x","schedule":{"type":"daily","time":"09:00","tz":"UTC"}}]}`, 422, "duplicate_id"},
		{"PUT", "/v1/reminders/ok", `{"schedule":"` + strings.Repeat("x", 300<<10) + `"}`, 413, "body_too_large"},
	}
	for _, c := range cases {
		r := e.do(c.method, c.path, hdr(key, tok), c.body)
		if r.status != c.want || r.body["error"] != c.code {
			t.Errorf("%s %s %.60s: got %d %s, want %d %s", c.method, c.path, c.body, r.status, r.raw, c.want, c.code)
		}
	}

	// Endpoints outside known push services are refused (no SSRF).
	for _, ep := range []string{"https://169.254.169.254/latest", "http://fcm.googleapis.com/x", "https://localhost/x"} {
		r := e.do("POST", "/v1/subscriptions", map[string]string{"X-App-Key": key}, subJSON(ep))
		if r.status != 422 || r.body["error"] != "invalid_endpoint" {
			t.Errorf("endpoint %s: %d %s", ep, r.status, r.raw)
		}
	}
	r := e.do("POST", "/v1/subscriptions", map[string]string{"X-App-Key": key},
		`{"endpoint":"https://fcm.googleapis.com/x","keys":{"p256dh":"abc","auth":"def"}}`)
	if r.status != 422 || r.body["error"] != "invalid_keys" {
		t.Errorf("bad keys: %d %s", r.status, r.raw)
	}
}

func TestAuthAndIsolation(t *testing.T) {
	e := setup(t)
	keyA := e.app("a", nil, 10, 10)
	keyB := e.app("b", nil, 10, 10)
	tokA := e.register(keyA, "https://web.push.apple.com/1")
	e.do("PUT", "/v1/reminders/secret", hdr(keyA, tokA), daily)

	if r := e.do("GET", "/v1/reminders", nil, ""); r.status != 401 || r.body["error"] != "missing_app_key" {
		t.Fatal(r.raw)
	}
	if r := e.do("GET", "/v1/reminders", map[string]string{"X-App-Key": "app_nope"}, ""); r.status != 401 || r.body["error"] != "invalid_app_key" {
		t.Fatal(r.raw)
	}
	if r := e.do("GET", "/v1/reminders", map[string]string{"X-App-Key": keyA}, ""); r.status != 401 || r.body["error"] != "missing_token" {
		t.Fatal(r.raw)
	}
	// App B cannot use app A's device token.
	if r := e.do("GET", "/v1/reminders", hdr(keyB, tokA), ""); r.status != 401 || r.body["error"] != "invalid_token" {
		t.Fatal(r.raw)
	}
	if r := e.do("DELETE", "/v1/reminders/secret", hdr(keyB, tokA), ""); r.status != 401 {
		t.Fatal(r.raw)
	}
	// Same device registered with app B sees nothing of app A.
	tokB := e.register(keyB, "https://web.push.apple.com/1")
	if r := e.do("GET", "/v1/reminders", hdr(keyB, tokB), ""); len(r.body["reminders"].([]any)) != 0 {
		t.Fatal("leak between apps:", r.raw)
	}
	if r := e.do("DELETE", "/v1/reminders/secret", hdr(keyB, tokB), ""); r.status != 404 {
		t.Fatal(r.status)
	}
}

func TestQuotas(t *testing.T) {
	e := setup(t)
	key := e.app("a", nil, 1, 1)
	tok := e.register(key, "https://web.push.apple.com/1")
	if r := e.do("POST", "/v1/subscriptions", map[string]string{"X-App-Key": key}, subJSON("https://web.push.apple.com/2")); r.status != 429 {
		t.Fatalf("subs quota: %d", r.status)
	}
	// Re-registering the same device is not a new subscription.
	r := e.do("POST", "/v1/subscriptions", map[string]string{"X-App-Key": key}, subJSON("https://web.push.apple.com/1"))
	if r.status != 200 || r.body["created"] != false {
		t.Fatalf("re-register: %d %s", r.status, r.raw)
	}
	tok2 := r.body["token"].(string)
	if tok2 == tok {
		t.Fatal("token not rotated")
	}
	e.do("PUT", "/v1/reminders/one", hdr(key, tok2), daily)
	if r := e.do("PUT", "/v1/reminders/two", hdr(key, tok2), daily); r.status != 429 {
		t.Fatalf("reminders quota: %d", r.status)
	}
}

func TestCORS(t *testing.T) {
	e := setup(t)
	key := e.app("a", []string{"https://app.one"}, 10, 10)
	e.app("b", []string{"https://app.two"}, 10, 10)

	pre := func(origin string) resp {
		return e.do("OPTIONS", "/v1/reminders", map[string]string{"Origin": origin, "Access-Control-Request-Method": "PUT"}, "")
	}
	if r := pre("https://app.one"); r.status != 204 || r.header.Get("Access-Control-Allow-Origin") != "https://app.one" ||
		!strings.Contains(r.header.Get("Access-Control-Allow-Headers"), "X-App-Key") {
		t.Fatalf("preflight: %d %v", r.status, r.header)
	}
	if r := pre("https://evil.site"); r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("evil origin allowed")
	}
	// Origin allowed for app b only: app a's key refuses it.
	r := e.do("POST", "/v1/subscriptions", map[string]string{"X-App-Key": key, "Origin": "https://app.two"}, subJSON("https://web.push.apple.com/1"))
	if r.status != 403 || r.body["error"] != "origin_not_allowed" {
		t.Fatalf("cross-app origin: %d %s", r.status, r.raw)
	}
	r = e.do("POST", "/v1/subscriptions", map[string]string{"X-App-Key": key, "Origin": "https://app.one"}, subJSON("https://web.push.apple.com/1"))
	if r.status != 201 || r.header.Get("Access-Control-Allow-Origin") != "https://app.one" {
		t.Fatalf("own origin: %d %v", r.status, r.header)
	}
}
