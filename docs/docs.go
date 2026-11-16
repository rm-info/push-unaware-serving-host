// Package docs embeds the integrator documentation so the server can publish
// it (see internal/site: /llms.txt). The Markdown files stay the single source.
package docs

import _ "embed"

// Integration is docs/integration.md.
//
//go:embed integration.md
var Integration string

// API is docs/api.md.
//
//go:embed api.md
var API string
