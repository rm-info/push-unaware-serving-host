// Package ui renders the server-side HTML pages (admin UI and public
// key-request pages). Pages use no JavaScript and no inline styles, so they
// run under a strict Content-Security-Policy.
package ui

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rm-info/push-unaware-serving-host/internal/httpx"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Page is the data passed to every template.
type Page struct {
	Title   string
	Admin   string // logged-in admin username, "" otherwise
	CSRF    string
	Flash   string
	Error   string
	BaseURL string // public base URL of this server
	Data    any
}

var funcs = template.FuncMap{
	"date": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.UTC().Format("2006-01-02 15:04 UTC")
	},
	"join":  strings.Join,
	"lines": func(ss []string) string { return strings.Join(ss, "\n") },
}

var pages = map[string]*template.Template{}

func init() {
	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		panic(err)
	}
	for _, n := range names {
		base := strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".html")
		if base == "layout" {
			continue
		}
		pages[base] = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", n))
	}
}

// Render writes a full HTML page with security headers.
func Render(w http.ResponseWriter, status int, name string, p Page) {
	t, ok := pages[name]
	if !ok {
		panic("ui: unknown page " + name)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", p); err != nil {
		slog.Error("render page", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httpx.SecureHTML(w)
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// Static serves the stylesheet and icons under /assets/.
func Static() http.Handler {
	sub, _ := fs.Sub(staticFS, "static")
	fsrv := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fsrv.ServeHTTP(w, r)
	})
}
