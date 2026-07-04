// Package webui serves the built Angular SPA (web/dist/manyforge-web/browser)
// same-origin with the API, so the SPA's relative /api/v1 calls resolve
// without CORS. The SPA build is only embedded into the binary when built
// with the ui_embed tag (see embed.go / embed_stub.go); newSPAHandler itself
// has no build tag so it can be unit tested against an in-memory fs.FS
// without a real frontend build.
package webui

import (
	"bytes"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

const indexFile = "index.html"

// newSPAHandler serves static files from fsys and falls back to index.html
// for client-side routes so Angular's router can take over. A request path
// with a ".." segment is rejected (404) before it is ever resolved against
// fsys. A request for what looks like an asset (its path has a file
// extension) that isn't found returns a real 404 instead of falling back to
// index.html.
func newSPAHandler(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if containsDotDot(r.URL.Path) {
			http.NotFound(w, r)
			return
		}

		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "."
		}

		if serveFile(w, r, fsys, name) {
			return
		}

		// name == "." is the SPA root ("/"), which is never a real file — it
		// always falls back to index.html. Anything else with a file
		// extension is an asset request; a miss there is a genuine 404 and
		// must not fall back to index.html.
		if name != "." && path.Ext(name) != "" && !acceptsHTML(r) {
			http.NotFound(w, r)
			return
		}

		if !serveFile(w, r, fsys, indexFile) {
			http.NotFound(w, r)
		}
	})
}

// containsDotDot reports whether p has a literal ".." path segment.
func containsDotDot(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func acceptsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// serveFile writes the named file's contents from fsys, returning false
// (writing nothing) if it doesn't exist or is a directory, so the caller can
// decide how to fall back.
func serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) bool {
	info, err := fs.Stat(fsys, name)
	if err != nil || info.IsDir() {
		return false
	}
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return false
	}

	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeContent(w, r, name, info.ModTime(), bytes.NewReader(data))
	return true
}
