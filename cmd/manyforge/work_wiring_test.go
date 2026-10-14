package main

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// TestWorkRoutesArePermissionGated drives a request through the production mountAPIRoutes for
// every Work Board route and records which gate ran. The drift test and the integration tests
// both bypass the permission middleware, so a route moved into the wrong group (e.g. token
// management under work.read) would otherwise go unnoticed. The sync route must instead sit
// behind its pre-auth limiter and outside RequireAuth (it authenticates an mfw_ token).
func TestWorkRoutesArePermissionGated(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	ring, err := auth.NewKeyRing("manyforge", "manyforge-api", "k1", priv, map[string]ed25519.PublicKey{"k1": pub})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	tok, err := ring.Sign(uuid.New(), time.Hour, time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	var ran string
	recording := func(name string) func(http.Handler) http.Handler {
		return func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				ran = name
				w.WriteHeader(http.StatusTeapot) // deny: the handler beneath must not mask wiring
			})
		}
	}
	h := testHandlers()
	h.workRead = recording("work.read")
	h.workWrite = recording("work.write")
	h.workSyncLimit = recording("sync-limit")
	mux := httpx.NewRouter(ring)
	mountAPIRoutes(mux, h)

	const biz = "/api/v1/businesses/00000000-0000-4000-8000-000000000001/work"
	cases := []struct {
		method, path, gate string
		jwt                bool
	}{
		{http.MethodGet, biz + "/summary", "work.read", true},
		{http.MethodGet, biz + "/projects", "work.read", true},
		{http.MethodGet, biz + "/projects/manyforge", "work.read", true},
		{http.MethodGet, biz + "/projects/manyforge/tasks", "work.read", true},
		{http.MethodPost, biz + "/projects", "work.write", true},
		{http.MethodPost, biz + "/projects/manyforge/tasks", "work.write", true},
		{http.MethodPatch, biz + "/projects/manyforge/tasks/mf-abc123", "work.write", true},
		{http.MethodGet, biz + "/projects/manyforge/tokens", "work.write", true},
		{http.MethodPost, biz + "/projects/manyforge/tokens", "work.write", true},
		{http.MethodDelete, biz + "/projects/manyforge/tokens/00000000-0000-4000-8000-000000000002", "work.write", true},
		{http.MethodPost, "/api/v1/work/sync", "sync-limit", false},
	}
	for _, c := range cases {
		ran = ""
		req := httptest.NewRequest(c.method, c.path, nil)
		if c.jwt {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if ran != c.gate || rec.Code != http.StatusTeapot {
			t.Errorf("%s %s: gate %q (status %d), want %q", c.method, c.path, ran, rec.Code, c.gate)
		}
	}
}
