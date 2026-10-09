// Package webui embeds the built frontend (frontend/ → internal/webui/dist).
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built SPA rooted at index.html. Built reports false when the
// frontend has not been built and only the placeholder is embedded.
func FS() (assets fs.FS, built bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	_, err = fs.Stat(sub, "index.html")
	return sub, err == nil
}
