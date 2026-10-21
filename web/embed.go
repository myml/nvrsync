// Package web contains the embedded frontend assets served by the HTTP API.
package web

import (
	"embed"
	"io/fs"
)

// FS holds the frontend build output. The assets directory is embedded
// recursively so hashed chunks and nested directories are all included.
//
//go:embed all:assets
var FS embed.FS

// Sub returns the frontend assets rooted at the "assets" directory. The
// returned filesystem is what the API serves at / and /assets/.
func Sub() fs.FS {
	sub, err := fs.Sub(FS, "assets")
	if err != nil {
		// assets is embedded at build time, so fs.Sub can only fail if the
		// embed directive itself is broken.
		panic("web: cannot sub embedded assets: " + err.Error())
	}
	return sub
}
