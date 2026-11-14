// Package web embeds the demo PWA served under /demo/ when enabled.
package web

import "embed"

// Demo holds the demo PWA (files under demo/).
//
//go:embed demo
var Demo embed.FS
