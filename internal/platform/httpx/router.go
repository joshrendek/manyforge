package httpx

import (
	"github.com/go-chi/chi/v5"

	"github.com/manyforge/manyforge/internal/platform/auth"
)

// NewRouter builds the base chi router with the standard middleware chain.
// Feature modules mount their routes onto the returned mux; protected routes
// add RequireAuth / RequirePermission.
func NewRouter(ring *auth.KeyRing) *chi.Mux {
	mux := chi.NewRouter()
	mux.Use(RequestID)
	mux.Use(Recover)
	mux.Use(RequestLogger)
	mux.Use(AuthToPrincipal(ring))
	return mux
}
