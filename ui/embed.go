// Package ui provides the embedded SPA assets built by Vite.
// Run `npm run build` in the ui/ directory before `go build` to populate dist/.
package ui

import "embed"

// DistFS holds the built SPA files from dist/.
// The embed directive is relative to this file's directory (ui/).
//
//go:embed dist
var DistFS embed.FS
