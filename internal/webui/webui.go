// Package webui serves the single-page UI, compiled into the binary with
// go:embed so there is one file to deploy and no way for the assets to drift
// from the binary that serves them.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed assets
var assets embed.FS

// assetTypes pins the media type of everything the UI ships.
//
// Without this, http.FileServer asks the platform, and Go's mime package
// consults the Windows registry, where .js and .css are routinely mapped to
// text/plain. A browser sent text/plain for a script refuses to run it, and the
// UI loads as unstyled HTML that does nothing. Pinning the four types the page
// actually uses takes the platform out of it; http.ServeContent keeps a
// Content-Type that is already set, and an error response overwrites it.
var assetTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".svg":  "image/svg+xml",
}

// Register mounts the UI at the root of a mux.
//
// Every response carries Cache-Control: no-store. That is not caution for its
// own sake: stale cached JS previously resurrected bugs that had already been
// fixed, which is a miserable thing to debug because the server is right and
// the browser is wrong.
func Register(mux *http.ServeMux) error {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		return err
	}
	server := http.FileServer(http.FS(sub))
	mux.Handle("/", headers(server))
	return nil
}

func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if ct := assetTypes[strings.ToLower(path.Ext(r.URL.Path))]; ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		next.ServeHTTP(w, r)
	})
}
