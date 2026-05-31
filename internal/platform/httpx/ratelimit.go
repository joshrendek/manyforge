package httpx

import (
	"net/http"

	"github.com/manyforge/manyforge/internal/platform/ratelimit"
)

// RateLimit throttles abuse-sensitive endpoints (FR-029). When the limiter has no
// token for the key derived from the request, it responds 429 with a Retry-After
// hint instead of invoking the handler. key is typically a per-IP function built
// from ratelimit.ClientIP so spoofed X-Forwarded-For headers can't evade it.
func RateLimit(limiter ratelimit.Limiter, key func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !limiter.Allow(key(r)) {
				w.Header().Set("Retry-After", "1")
				WriteJSON(w, http.StatusTooManyRequests, ErrorBody{Code: "RATE_LIMITED", Message: "too many requests"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
