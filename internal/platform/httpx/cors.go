package httpx

import (
	"net/http"
	"strings"

	"github.com/manyforge/manyforge/internal/platform/weborigin"
)

// PublicIngestCORS enables exact-origin browser access only for the public routes
// that install it. Origin is metadata, not authentication. Invalid registration
// configuration panics rather than silently weakening an enabled policy.
func PublicIngestCORS(allowedOrigins []string, instanceOrigin string, methods ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(allowedOrigins)+1)
	if len(allowedOrigins) > 0 {
		for i := 0; i <= len(allowedOrigins); i++ {
			raw := instanceOrigin
			if i < len(allowedOrigins) {
				raw = allowedOrigins[i]
			}
			origin, err := weborigin.Normalize(raw)
			if err != nil {
				panic("httpx.PublicIngestCORS: invalid origin configuration")
			}
			allowed[origin] = struct{}{}
		}
	}
	allowedMethods := make(map[string]struct{}, len(methods))
	for _, method := range methods {
		allowedMethods[method] = struct{}{}
	}
	methodHeader := strings.Join(methods, ", ")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			preflight := r.Method == http.MethodOptions
			if len(allowed) == 0 {
				if preflight {
					corsNotAllowed(w)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			mergeVary(w.Header(), "Origin")
			if preflight {
				mergeVary(w.Header(), "Access-Control-Request-Method", "Access-Control-Request-Headers")
			}
			origins := r.Header.Values("Origin")
			if len(origins) == 0 && !preflight {
				next.ServeHTTP(w, r)
				return
			}
			// Bound parsing independently of the server's overall header limit.
			if len(origins) != 1 || len(origins[0]) > 2048 {
				originNotAllowed(w)
				return
			}
			origin, err := weborigin.FromHeader(origins)
			if _, ok := allowed[origin]; err != nil || !ok {
				originNotAllowed(w)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After")
			if preflight {
				requestedMethods := r.Header.Values("Access-Control-Request-Method")
				if len(requestedMethods) != 1 {
					corsNotAllowed(w)
					return
				}
				if _, ok := allowedMethods[requestedMethods[0]]; !ok || !publicIngestHeadersAllowed(r.Header.Values("Access-Control-Request-Headers")) {
					corsNotAllowed(w)
					return
				}
				w.Header().Set("Access-Control-Allow-Methods", methodHeader)
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
				w.Header().Set("Access-Control-Max-Age", "300")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func publicIngestHeadersAllowed(values []string) bool {
	if len(values) > 16 {
		return false
	}
	size := 0
	for _, value := range values {
		size += len(value)
		if size > 1024 {
			return false
		}
		for _, name := range strings.Split(value, ",") {
			if !strings.EqualFold(strings.TrimSpace(name), "Content-Type") {
				return false
			}
		}
	}
	return true
}

func originNotAllowed(w http.ResponseWriter) {
	WriteJSON(w, http.StatusForbidden, ErrorBody{Code: "ORIGIN_NOT_ALLOWED", Message: "origin not allowed"})
}

func corsNotAllowed(w http.ResponseWriter) {
	WriteJSON(w, http.StatusForbidden, ErrorBody{Code: "CORS_NOT_ALLOWED", Message: "CORS request not allowed"})
}

// mergeVary preserves all prior middleware tokens, including Vary: *.
func mergeVary(header http.Header, names ...string) {
	for _, name := range names {
		found := false
		for _, value := range header.Values("Vary") {
			for _, token := range strings.Split(value, ",") {
				if strings.EqualFold(strings.TrimSpace(token), name) || strings.TrimSpace(token) == "*" {
					found = true
				}
			}
		}
		if !found {
			header.Add("Vary", name)
		}
	}
}
