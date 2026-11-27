//go:build contract

package main

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/clidist"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/portaldomain"
)

func portalWiringRing(t *testing.T) *auth.KeyRing {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	ring, err := auth.NewKeyRing("manyforge", "manyforge-api", "k1", priv,
		map[string]ed25519.PublicKey{"k1": pub})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	return ring
}

// The portal-domain management routes must sit inside both RequireAuth and the
// inbox.manage permission group. Handler tests mount Routes directly and the
// drift test uses no-op middleware, so neither notices if the mount moves.
func TestPortalDomainRoutesAreInboxManageGated(t *testing.T) {
	ring := portalWiringRing(t)
	tok, err := ring.Sign(uuid.MustParse("00000000-0000-4000-8000-000000000003"), time.Hour, time.Now())
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	const base = "/api/v1/businesses/00000000-0000-4000-8000-000000000001/portal-domains"
	const domain = "/00000000-0000-4000-8000-000000000002"
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, base},
		{http.MethodPost, base},
		{http.MethodPost, base + domain + "/verify"},
		{http.MethodDelete, base + domain},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var inboxManageRan bool
			h := testHandlers()
			h.inboxManage = func(http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					inboxManageRan = true
					// Deny, so the zero-value handler beneath is never reached.
					w.WriteHeader(http.StatusNotFound)
				})
			}
			mux := httpx.NewRouter(ring)
			mountAPIRoutes(mux, h)

			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if !inboxManageRan || rec.Code != http.StatusNotFound {
				t.Fatalf("SECURITY REGRESSION: portal-domain route bypassed inbox.manage (ran=%v status=%d)", inboxManageRan, rec.Code)
			}

			inboxManageRan = false
			anon := httptest.NewRecorder()
			mux.ServeHTTP(anon, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`)))
			if anon.Code != http.StatusUnauthorized {
				t.Fatalf("SECURITY REGRESSION: portal-domain route without a token: status %d, want 401", anon.Code)
			}
			if inboxManageRan {
				t.Fatal("inbox.manage ran before authentication")
			}
		})
	}
}

// Walk the real router and assert exactly which routes the custom-hostname
// allowlist admits. Two allowlist rows are path prefixes; without this a route
// added under them, or a new single-segment root route ending in an asset
// extension, would answer on every custom hostname with no failing test.
// Adding to the expected list is a security decision: the route must be public
// and bound to the hostname's business (see the portal custom domains design).
func TestPortalHostAllowlistAgainstRouter(t *testing.T) {
	ring := portalWiringRing(t)
	h := testHandlers()
	mux := httpx.NewRouter(ring)
	mountAPIRoutes(mux, h)
	// The other root-level registrations main makes beside mountAPIRoutes.
	h.account.BrowserPasskeyRoutes(mux, noop)
	(&clidist.Handler{Dir: t.TempDir()}).Routes(mux)

	want := []string{
		"GET /m/confirm/{token}",
		"POST /m/confirm/{token}",
		"GET /api/v1/feedback/public/{key}/posts",
		"POST /api/v1/feedback/public/{key}/posts",
		"OPTIONS /api/v1/feedback/public/{key}/posts",
		"POST /api/v1/feedback/public/{key}/posts/{postID}/votes",
		"OPTIONS /api/v1/feedback/public/{key}/posts/{postID}/votes",
		"POST /api/v1/mailing/public/{key}/subscribe",
		"OPTIONS /api/v1/mailing/public/{key}/subscribe",
		"GET /api/v1/mailing/public/{key}/brand",
	}

	var got []string
	walked := 0
	rootLevel := 0
	walk := func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		walked++
		if !strings.HasPrefix(route, "/api/") {
			rootLevel++
		}
		if portaldomain.AllowedOnPortalHost(strings.ToUpper(method), concretePath(route)) {
			got = append(got, strings.ToUpper(method)+" "+route)
		}
		return nil
	}
	if err := chi.Walk(mux, walk); err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	if walked < 100 || rootLevel < 5 {
		t.Fatalf("walk saw %d routes (%d root-level); the router was not built as in production", walked, rootLevel)
	}

	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("SECURITY: the custom-hostname allowlist admits a different route set than reviewed.\ngot:\n  %s\nwant:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// concretePath turns a chi pattern into a request path: each {param} becomes a
// plain segment and a trailing wildcard becomes one more segment.
func concretePath(pattern string) string {
	segs := strings.Split(pattern, "/")
	for i, s := range segs {
		switch {
		case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"):
			segs[i] = "x"
		case s == "*":
			segs[i] = "x"
		}
	}
	return strings.Join(segs, "/")
}
