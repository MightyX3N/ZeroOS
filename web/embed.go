// Package web holds the desktop web UI, built into the ZeroOS program.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// FS returns the web UI files.
func FS() fs.FS {
	sub, _ := fs.Sub(files, "static")
	return sub
}
