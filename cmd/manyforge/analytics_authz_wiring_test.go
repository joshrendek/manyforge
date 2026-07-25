//go:build contract

package main

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// TestAnalyticsSummaryIsPermissionGated asserts the analytics read route is mounted INSIDE the
// telemetry.read middleware group in the production router.
//
// This gap is invisible to the other tests by construction: the integration environment mounts
// ReadRoutes directly and injects a principal, and the OpenAPI-drift harness substitutes no-op
// middleware. Both would keep passing if the route were moved out of its permission group, which
// would expose one tenant's traffic data to any authenticated principal.
//
// Rather than assert on wiring by inspection, this mounts the real mountAPIRoutes with a
// middleware that RECORDS whether it ran, and drives an actual request through it.
func TestAnalyticsSummaryIsPermissionGated(t *testing.T) {
	var telemetryReadRan bool
	recording := func(flag *bool) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				*flag = true
				// Deny, so the handler beneath is never reached and cannot mask a wiring bug.
				w.WriteHeader(http.StatusNotFound)
			})
		}
	}

	pub, priv, _ := ed25519.GenerateKey(nil)
	ring, err := auth.NewKeyRing("manyforge", "manyforge-api", "k1", priv,
		map[string]ed25519.PublicKey{"k1": pub})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	// The FULL production handler set, with only the gate under test swapped for a recorder — so
	// this exercises the same mountAPIRoutes wiring main uses, not a hand-built subset.
	h := testHandlers()
	h.telemetryRead = recording(&telemetryReadRan)

	mux := httpx.NewRouter(ring)
	mountAPIRoutes(mux, h)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/businesses/00000000-0000-4000-8000-000000000001/analytics/summary?client_id=00000000-0000-4000-8000-000000000002",
		nil)
	// A token good enough to clear RequireAuth; the permission gate is what is under test.
	tok, terr := ring.Sign(uuid.MustParse("00000000-0000-4000-8000-000000000003"), time.Hour, time.Now())
	if terr != nil {
		t.Fatalf("sign token: %v", terr)
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if !telemetryReadRan {
		t.Fatal("SECURITY REGRESSION: the analytics summary route did not pass through the " +
			"telemetry.read middleware. It is mounted outside its permission group, so any " +
			"authenticated principal could read another business's traffic data.")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("the permission gate did not short-circuit the request: got %d, body %q",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}
