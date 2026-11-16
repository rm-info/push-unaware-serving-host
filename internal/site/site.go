// Package site serves the public front of a PUSH server: a landing page for
// humans (/), a single-file documentation for AI coding agents (/llms.txt)
// and the reusable browser client (/push-client.js).
package site

import (
	"bytes"
	_ "embed"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"text/template"

	"github.com/rm-info/push-unaware-serving-host/docs"
	"github.com/rm-info/push-unaware-serving-host/internal/admin"
	"github.com/rm-info/push-unaware-serving-host/internal/ui"
	"github.com/rm-info/push-unaware-serving-host/web"
)

// SourceURL is where the project's code and documentation live.
const SourceURL = "https://github.com/rm-info/push-unaware-serving-host"

//go:embed llms.txt.tmpl
var llmsSrc string

var llmsTmpl = template.Must(template.New("llms").Parse(llmsSrc))

// Server serves the public pages.
type Server struct {
	KeyRequests bool   // public key requests are enabled
	Demo        bool   // the demo PWA is served under /demo/
	PublicURL   string // configured public base URL, "" = derived from requests
	Log         *slog.Logger
}

// Handler returns the routes. Mount it on "GET /{$}", "GET /llms.txt" and
// "GET /push-client.js".
func (s *Server) Handler() http.Handler {
	client := mustRead("demo/push-client.js")
	sw := mustRead("demo/sw.js")
	integration, api := demote(docs.Integration), demote(docs.API)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /llms.txt", func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		err := llmsTmpl.Execute(&buf, map[string]any{
			"BaseURL": admin.BaseURL(s.PublicURL, r), "KeyRequests": s.KeyRequests, "Demo": s.Demo,
			"Integration": integration, "API": api,
			"Client": strings.TrimSpace(client), "ServiceWorker": strings.TrimSpace(sw),
		})
		if err != nil {
			s.Log.Error("render llms.txt", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(buf.Bytes())
	})
	mux.HandleFunc("GET /push-client.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write([]byte(client))
	})
	return mux
}

type homeData struct {
	KeyRequests bool
	Demo        bool
	SourceURL   string
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ui.Render(w, http.StatusOK, "site_home", ui.Page{
		Title:   "Scheduled Web Push for PWAs",
		BaseURL: admin.BaseURL(s.PublicURL, r),
		Data:    homeData{KeyRequests: s.KeyRequests, Demo: s.Demo, SourceURL: SourceURL},
	})
}

func mustRead(name string) string {
	b, err := fs.ReadFile(web.Demo, name)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// demote prepares an embedded document to nest under a "##" section: its
// leading "# Title" line is dropped (the section heading replaces it) and the
// other ATX headings outside fenced code blocks shift one level down.
func demote(md string) string {
	md = strings.TrimSpace(md)
	if strings.HasPrefix(md, "# ") {
		_, md, _ = strings.Cut(md, "\n")
		md = strings.TrimSpace(md)
	}
	lines := strings.Split(md, "\n")
	inFence := false
	for i, l := range lines {
		if strings.HasPrefix(l, "```") {
			inFence = !inFence
			continue
		}
		if !inFence && strings.HasPrefix(l, "#") {
			lines[i] = "#" + l
		}
	}
	return strings.Join(lines, "\n")
}
