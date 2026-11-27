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
// the public allowlist (AllowedOnPortalHost); every other host gets 404. Only
// r.Host is consulted, never a forwarding header. Each denial logs one Info line.
func HostRouting(instanceHost string, lookup Lookup, next http.Handler) http.Handler {
	instanceHost = NormalizeHost(instanceHost)
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
			deny(w, r, h, "invalid_host")
			return
		}
		business, ok, err := lookup(r.Context(), h)
		if err != nil {
			slog.ErrorContext(r.Context(), "portal host lookup failed", "host", h, "err", err)
			httpx.WriteJSON(w, http.StatusServiceUnavailable, httpx.ErrorBody{Code: "UNAVAILABLE", Message: "temporarily unavailable"})
			return
		}
		if !ok {
			deny(w, r, h, "unknown_host")
			return
		}
		if r.URL.Path != path.Clean(r.URL.Path) || r.URL.RawPath != "" || !AllowedOnPortalHost(r.Method, r.URL.Path) {
			deny(w, r, h, "not_allowlisted")
			return
		}
		next.ServeHTTP(w, r.WithContext(httpx.WithPortalBusiness(r.Context(), business)))
	})
}

// deny answers 404 with no detail and logs why. The path goes through
// httpx.SafePath so capability tokens never reach the log.
func deny(w http.ResponseWriter, r *http.Request, host, reason string) {
	slog.InfoContext(r.Context(), "portal host request denied",
		"host", truncate(host, 253),
		"method", r.Method,
		"path", truncate(httpx.SafePath(r), 256),
		"reason", reason,
	)
	httpx.WriteError(w, r, errs.ErrNotFound)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func isMethod(m string, allowed ...string) bool {
	for _, a := range allowed {
		if m == a {
			return true
		}
	}
	return false
}

// oneSegment reports whether p is prefix followed by exactly one non-empty path
// segment (no further "/"). Deeper paths would reach the web app's fallback
// route, which leads to the operator sign-in screen.
func oneSegment(p, prefix string) bool {
	rest, ok := strings.CutPrefix(p, prefix)
	return ok && rest != "" && !strings.Contains(rest, "/")
}

var staticExts = []string{".js", ".css", ".ico", ".svg", ".png", ".woff2"}

// AllowedOnPortalHost is the complete set of requests a custom hostname may make.
// A wiring test walks the real router against it, so a route added under one of
// these prefixes fails CI until it is listed there.
func AllowedOnPortalHost(method, p string) bool {
	switch {
	case oneSegment(p, "/p/"), oneSegment(p, "/m/s/"):
		return isMethod(method, http.MethodGet, http.MethodHead)
	case oneSegment(p, "/m/confirm/"):
		return isMethod(method, http.MethodGet, http.MethodHead, http.MethodPost)
	case strings.HasPrefix(p, "/api/v1/feedback/public/"), strings.HasPrefix(p, "/api/v1/mailing/public/"):
		return isMethod(method, http.MethodGet, http.MethodPost, http.MethodOptions)
	case p == "/a.js":
		// Served by the analytics module with an unbound site key; never on a custom host.
		return false
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
