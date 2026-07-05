//go:build ui_embed

package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

// distFS embeds the built Angular SPA (web/dist/manyforge-web/browser,
// copied into ./dist by the Dockerfile build stage) into the binary.
// "all:dist" is required (not "dist/**") to recursively include nested
// asset directories and files that begin with "_" or ".".
//
//go:embed all:dist
var distFS embed.FS

// Handler returns the embedded SPA's static handler. The bool is always true
// in a ui_embed build — see embed_stub.go for the non-ui_embed counterpart.
func Handler() (http.Handler, bool) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, false
	}
	return newSPAHandler(sub), true
}
