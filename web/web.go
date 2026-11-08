// Package web embeds the built single-page app. Build it with
// `npm run build` (or `make web`) before building the Go binary; without a
// build the server answers UI routes with a short notice instead.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:build
var build embed.FS

// Dist returns the built UI files.
func Dist() fs.FS {
	sub, err := fs.Sub(build, "build")
	if err != nil {
		panic(err)
	}
	return sub
}
