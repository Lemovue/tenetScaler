// Package web embeds the dashboard's static files (HTML, CSS, JS).
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var files embed.FS

// Handler serves the embedded dashboard files.
func Handler() http.Handler {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err) // the embed directive guarantees "static" exists
	}
	return http.FileServer(http.FS(sub))
}
