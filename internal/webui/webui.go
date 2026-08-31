// Package webui serves the phone-facing PWA.
//
// The assets are embedded rather than read from disk so the daemon stays one
// self-contained binary: an installed copy cannot drift from the version that
// was built, and there is no asset path to get wrong in packaging.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets
var assets embed.FS

// Handler serves the UI at the root.
func Handler() http.Handler {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		// Unreachable: the embed directive above guarantees the directory
		// exists at build time.
		panic(err)
	}

	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Vendored libraries are content-addressed by their pinned version in
		// the filename, but index.html and app.js change on every build, and a
		// phone holding a stale app.js against a new API is a confusing bug to
		// receive a report about.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
