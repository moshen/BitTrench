// Package webui serves the single-page UI, compiled into the binary with
// go:embed so there is one file to deploy and no way for the assets to drift
// from the binary that serves them.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets
var assets embed.FS

// Register mounts the UI at the root of a mux.
//
// Every response carries Cache-Control: no-store. That is not caution for its
// own sake - stale cached JS previously resurrected bugs that had already been
// fixed, which is a miserable thing to debug because the server is right and
// the browser is wrong.
func Register(mux *http.ServeMux) error {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		return err
	}
	server := http.FileServer(http.FS(sub))
	mux.Handle("/", noStore(server))
	return nil
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
