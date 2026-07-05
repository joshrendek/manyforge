//go:build !ui_embed

package webui

import "net/http"

// Handler returns (nil, false): without the ui_embed build tag there is no
// embedded frontend build, so callers (main.go) skip registering the SPA
// catch-all route. This keeps `make test` and local `go run` working without
// a built frontend.
func Handler() (http.Handler, bool) {
	return nil, false
}
