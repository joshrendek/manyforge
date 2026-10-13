package main

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
)

// SECREVIEW-M1: the mailing provider webhooks must NOT sit behind the low
// end-user ingest limiter (provider egress IPs are shared by every tenant). They
// get their own pre-auth per-IP ceiling, while the generic inbox webhook keeps
// the ingest limiter.
func TestMailingProviderWebhooksBypassIngestLimitButKeepOwnCeiling(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	ring, err := auth.NewKeyRing("manyforge", "manyforge-api", "k1", priv, map[string]ed25519.PublicKey{"k1": pub})
	if err != nil {
		t.Fatal(err)
	}
	deny := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Test-Limiter", "ingest")
			w.WriteHeader(http.StatusTooManyRequests)
		})
	}
	const ceiling = 3
	h := testHandlers()
	h.ingestLimit = deny // an exhausted end-user ingest bucket
	h.mailingHookLimit = httpx.RateLimit(ratelimit.NewTokenBucket(0.0001, ceiling),
		func(r *http.Request) string { return ratelimit.ClientIP(r, nil) })
	mux := httpx.NewRouter(ring)
	mountAPIRoutes(mux, h)

	post := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.RemoteAddr = "198.51.100.7:443" // one shared provider egress IP
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}

	// The generic inbox webhook keeps the ingest limiter.
	if w := post("/api/v1/inbound/email/test"); w.Code != http.StatusTooManyRequests || w.Header().Get("X-Test-Limiter") != "ingest" {
		t.Fatalf("inbox webhook status = %d limiter=%q; want the ingest limiter's 429", w.Code, w.Header().Get("X-Test-Limiter"))
	}

	// The mailing provider webhooks are not behind it: unsigned requests reach the
	// handler (401 from the zero-DB handler) until the pre-auth ceiling trips.
	paths := []string{
		"/api/v1/inbound/mailing/" + uuid.NewString() + "/resend",
		"/api/v1/inbound/mailing/" + uuid.NewString() + "/ses",
		"/api/v1/inbound/mailing/" + uuid.NewString() + "/resend",
	}
	for i, p := range paths {
		w := post(p)
		if w.Header().Get("X-Test-Limiter") == "ingest" {
			t.Fatalf("request %d to %s hit the end-user ingest limiter", i, p)
		}
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("request %d to %s status = %d, want 401 (reached the handler)", i, p, w.Code)
		}
	}
	w := post("/api/v1/inbound/mailing/" + uuid.NewString() + "/resend")
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "RATE_LIMITED") {
		t.Fatalf("over the pre-auth ceiling: status = %d body=%s, want 429 RATE_LIMITED", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Test-Limiter") == "ingest" {
		t.Fatal("the pre-auth ceiling must be the mailing webhook limiter, not ingestLimit")
	}
}
