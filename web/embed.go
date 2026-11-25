// Package web embeds the built client UI (web/dist) into the
// remote-client binary — the single-static-binary story. `npm run build`
// (the Makefile's web-build target, and the release workflow) produces
// dist/; a committed .gitkeep keeps `go build` working before the first
// npm build, and the ui package serves a fallback page when index.html
// is missing.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// Dist returns the built UI's root filesystem.
func Dist() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("web: embedded dist missing: " + err.Error())
	}
	return sub
}
