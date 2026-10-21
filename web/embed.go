// Package web embeds the admin console so the binary is self-contained.
package web

import "embed"

// AdminFS holds the single-file admin UI.
//
//go:embed admin/index.html
var AdminFS embed.FS
