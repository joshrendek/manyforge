package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/auth"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyPrincipal
)

// RequestID assigns a correlation id to each request (header + context).
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.NewString()
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFromContext returns the correlation id, if any.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

// Recover converts a panic into a 500 instead of crashing the server.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.ErrorContext(r.Context(), "panic recovered", "panic", rec, "path", r.URL.Path)
				WriteJSON(w, http.StatusInternalServerError, ErrorBody{Code: "INTERNAL", Message: "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// RequestLogger logs each request with its correlation id and duration.
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.InfoContext(r.Context(), "request",
			"method", r.Method, "path", r.URL.Path,
			"request_id", RequestIDFromContext(r.Context()),
			"dur_ms", time.Since(start).Milliseconds())
	})
}

// AuthToPrincipal parses a Bearer access token (if present) and stores the
// principal id in context. It does NOT reject unauthenticated requests — that is
// RequireAuth's job — so public endpoints can share the chain.
func AuthToPrincipal(ring *auth.KeyRing) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				if pid, err := ring.Parse(strings.TrimPrefix(h, "Bearer ")); err == nil {
					r = r.WithContext(context.WithValue(r.Context(), ctxKeyPrincipal, pid))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// PrincipalFromContext returns the authenticated principal id, if any.
func PrincipalFromContext(ctx context.Context) (uuid.UUID, bool) {
	pid, ok := ctx.Value(ctxKeyPrincipal).(uuid.UUID)
	return pid, ok
}

// WithPrincipal returns a context carrying principalID, as AuthToPrincipal would.
// Exported for handler tests that bypass the auth middleware.
func WithPrincipal(ctx context.Context, principalID uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxKeyPrincipal, principalID)
}

// RequireAuth rejects requests without a valid principal (401).
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := PrincipalFromContext(r.Context()); !ok {
			WriteJSON(w, http.StatusUnauthorized, ErrorBody{Code: "UNAUTHORIZED", Message: "authentication required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
