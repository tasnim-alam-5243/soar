//go:build ui

package webui

import (
	"embed"
	"io/fs"
)

// dist is the Vite build of web/ (make web-build writes it here).
//
//go:embed all:dist
var dist embed.FS

func embedded() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	return sub
}
