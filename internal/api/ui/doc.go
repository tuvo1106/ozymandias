// Package ui embeds the built web UI (web/, built by `make web` into
// ./dist) into the ozyd binary, so a single file serves everything.
//
// Only dist/.gitkeep is tracked in git. Without a UI build, the embedded
// filesystem holds just that placeholder and api.UI serves a page explaining
// how to build it. `go build` and `go test` never depend on Node.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the embedded UI build, rooted at the directory that holds
// index.html.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil { // unreachable: "dist" is a valid, embedded path
		panic(err)
	}
	return sub
}
