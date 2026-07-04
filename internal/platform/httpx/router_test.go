package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/manyforge/manyforge/internal/platform/auth"
)

// TestRouter_CatchAllDoesNotShadowSpecificRoutes pins the routing-precedence
// invariant Task 1.2 (same-origin SPA embedding) relies on: chi resolves
// routes by specificity (it's a trie), not registration order. A "/*"
// catch-all mounted LAST — exactly as main.go mounts the SPA handler after
// the ops routes and mountAPIRoutes — must still lose to more specific
// routes like "/healthz" or "/api/v1/...", and only ever serve genuinely
// unmatched paths.
func TestRouter_CatchAllDoesNotShadowSpecificRoutes(t *testing.T) {
	ring, err := auth.NewDevKeyRing("manyforge", "manyforge-api")
	if err != nil {
		t.Fatalf("NewDevKeyRing: %v", err)
	}

	mux := NewRouter(ring)
	mux.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.Route("/api/v1", func(r chi.Router) {
		r.Get("/ping", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("pong")) })
	})

	// Registered LAST, mirroring main.go's SPA mount order.
	mux.Handle("/*", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("spa"))
	}))

	cases := []struct {
		path string
		want string
	}{
		{"/healthz", "ok"},
		{"/api/v1/ping", "pong"},
		{"/tickets/123", "spa"}, // unmatched path falls through to the catch-all
	}

	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if got := rec.Body.String(); got != tc.want {
			t.Errorf("%s: body = %q, want %q", tc.path, got, tc.want)
		}
	}
}
