// Package webui holds the browser dashboard's static files, served by wispd
// at /ui/ (internal/server/ui.go). There is no build step: plain HTML, CSS and
// ES modules, plus a vendored xterm.js for the terminal.
package webui

import "embed"

//go:embed static
var Static embed.FS
