package site

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, s *Server, path string) (*http.Response, string) {
	t.Helper()
	s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://push.test"+path, nil))
	return rec.Result(), rec.Body.String()
}

func TestHome(t *testing.T) {
	res, body := get(t, &Server{KeyRequests: true, PublicURL: "https://push.example.org/"}, "/")
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("status %d, type %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Error("missing CSP")
	}
	for _, want := range []string{"Unaware Serving Host", `href="/request-key"`, `href="/llms.txt"`, "https://push.example.org/llms.txt"} {
		if !strings.Contains(body, want) {
			t.Errorf("home page lacks %q", want)
		}
	}

	_, body = get(t, &Server{}, "/")
	if strings.Contains(body, `href="/request-key"`) {
		t.Error("request form linked while key requests are disabled")
	}
	if !strings.Contains(body, "http://push.test/llms.txt") {
		t.Error("base URL not derived from the request")
	}
}

func TestLLMs(t *testing.T) {
	res, body := get(t, &Server{KeyRequests: true, PublicURL: "https://push.example.org"}, "/llms.txt")
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("status %d, type %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	for _, want := range []string{
		"# PUSH — Push Unaware Serving Host at https://push.example.org",
		"POST https://push.example.org/v1/key-requests",
		"## Integration guide",
		"## HTTP API reference",
		"### 0. Prerequisites", // demoted from "## "
		"### Authentication",
		"export class PushClient", // client source
		"addEventListener('push'", // service worker source
	} {
		if !strings.Contains(body, want) {
			t.Errorf("llms.txt lacks %q", want)
		}
	}

	if cc := res.Header.Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("configured public URL: Cache-Control %q", cc)
	}

	res, body = get(t, &Server{}, "/llms.txt")
	if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("host-derived base URL must not be cached, got %q", cc)
	}
	if !strings.Contains(body, "API key requests: **closed**") || strings.Contains(body, "curl -sS -X POST") {
		t.Error("llms.txt does not reflect closed key requests")
	}
}

func TestClient(t *testing.T) {
	res, body := get(t, &Server{}, "/push-client.js")
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("status %d, type %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if !strings.Contains(body, "export class PushClient") {
		t.Error("not the client")
	}
}

func TestDemote(t *testing.T) {
	in := "# Title\n\ntext\n```sh\n# a shell comment\n```\n## Sub"
	want := "text\n```sh\n# a shell comment\n```\n### Sub"
	if got := demote(in); got != want {
		t.Errorf("demote:\n%s\nwant:\n%s", got, want)
	}
}
