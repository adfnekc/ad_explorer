// Package webui carries the built frontend inside the binary.
//
// The assets are embedded rather than read from disk so the gateway is one file
// to deploy, and so the version of the UI always matches the version of the
// server that is talking to it.
package webui

import (
	"embed"
	"io/fs"
)

// The build writes the Vite output into dist. The all: prefix includes files
// whose names begin with an underscore or a dot, which Vite emits.
//
//go:embed all:dist
var embedded embed.FS

// Assets returns the frontend, rooted so that index.html is at the top.
func Assets() (fs.FS, error) { return fs.Sub(embedded, "dist") }
