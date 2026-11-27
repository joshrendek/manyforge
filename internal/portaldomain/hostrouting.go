package portaldomain

import (
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// HostRouting confines requests by the Host they arrived on. The instance host and
// internal hosts pass through untouched; a verified custom hostname reaches only
// the public allowlist below; every other host gets 404. Only r.Host is consulted,
// never a forwarding header.
func HostRouting(instanceHost string, lookup Lookup, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if instanceHost == "" {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		h := NormalizeHost(r.Host)
		if h == instanceHost || IsInternalHost(h) {
			next.ServeHTTP(w, r)
			return
		}
		if !ValidHostname(h) {
			httpx.WriteError(w, r, errs.ErrNotFound)
			return
		}
		business, ok, err := lookup(r.Context(), h)
		if err != nil {
			slog.ErrorContext(r.Context(), "portal host lookup failed", "host", h, "err", err)
			httpx.WriteJSON(w, http.StatusServiceUnavailable, httpx.ErrorBody{Code: "UNAVAILABLE", Message: "temporarily unavailable"})
			return
		}
		if !ok {
			httpx.WriteError(w, r, errs.ErrNotFound)
			return
		}
		if r.URL.Path != path.Clean(r.URL.Path) || r.URL.RawPath != "" || !allowedOnPortalHost(r.Method, r.URL.Path) {
			httpx.WriteError(w, r, errs.ErrNotFound)
			return
		}
		next.ServeHTTP(w, r.WithContext(httpx.WithPortalBusiness(r.Context(), business)))
	})
}

func isMethod(m string, allowed ...string) bool {
	for _, a := range allowed {
		if m == a {
			return true
		}
	}
	return false
}

// hasTail reports whether p starts with prefix and has at least one more character.
func hasTail(p, prefix string) bool {
	return len(p) > len(prefix) && strings.HasPrefix(p, prefix)
}

var staticExts = []string{".js", ".css", ".ico", ".svg", ".png", ".woff2"}

// allowedOnPortalHost is the complete set of requests a custom hostname may make.
func allowedOnPortalHost(method, p string) bool {
	switch {
	case hasTail(p, "/p/"), hasTail(p, "/m/s/"):
		return isMethod(method, http.MethodGet, http.MethodHead)
	case hasTail(p, "/m/confirm/"):
		return isMethod(method, http.MethodGet, http.MethodHead, http.MethodPost)
	case strings.HasPrefix(p, "/api/v1/feedback/public/"), strings.HasPrefix(p, "/api/v1/mailing/public/"):
		return isMethod(method, http.MethodGet, http.MethodPost, http.MethodOptions)
	case p == "/a.js":
		return isMethod(method, http.MethodGet, http.MethodHead)
	case p == "/a/e":
		return isMethod(method, http.MethodPost, http.MethodOptions)
	case strings.HasPrefix(p, "/media/"):
		return isMethod(method, http.MethodGet, http.MethodHead)
	}
	// Single-segment static asset: exactly one "/", at position 0.
	if strings.LastIndexByte(p, '/') == 0 && strings.IndexByte(p, '/') == 0 && isMethod(method, http.MethodGet, http.MethodHead) {
		for _, ext := range staticExts {
			if len(p) > 1+len(ext) && strings.HasSuffix(p, ext) {
				return true
			}
		}
	}
	return false
}
