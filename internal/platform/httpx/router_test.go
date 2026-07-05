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
// routes by specificity (it's a trie), not registration order. The "/*"
// catch-all is registered FIRST here — before any of the specific ops/API
// routes — precisely so the test cannot pass merely because static routes
// happened to be added before the wildcard. If chi resolved by
// first-registered-wins instead of trie specificity, every specific route
// below would incorrectly fall through to the catch-all; asserting each
// reaches its own distinct handler body proves it doesn't. Only a path that
// matches none of "/healthz", "/readyz", "/metrics", or "/api/v1/..." may
// hit the catch-all.
func TestRouter_CatchAllDoesNotShadowSpecificRoutes(t *testing.T) {
	ring, err := auth.NewDevKeyRing("manyforge", "manyforge-api")
	if err != nil {
		t.Fatalf("NewDevKeyRing: %v", err)
	}

	mux := NewRouter(ring)

	// Registered FIRST — mirrors none of main.go's mount order on purpose.
	// This proves chi resolves "/*" by trie specificity, not by which route
	// was added to the mux first.
	mux.Handle("/*", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("spa"))
	}))

	// Specific ops + API routes registered AFTER the catch-all.
	mux.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("healthz-ok")) })
	mux.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("readyz-ok")) })
	mux.Handle("/metrics", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("metrics-ok"))
	}))
	mux.Route("/api/v1", func(r chi.Router) {
		r.Get("/ping", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("pong")) })
	})

	cases := []struct {
		path string
		want string
	}{
		{"/healthz", "healthz-ok"},
		{"/readyz", "readyz-ok"},
		{"/metrics", "metrics-ok"},
		{"/api/v1/ping", "pong"},
		{"/some/spa/route", "spa"}, // genuinely unmatched path falls through to the catch-all
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
