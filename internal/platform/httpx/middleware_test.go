package httpx

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/auth"
)

func TestRequireAuthRejectsAnonymous(t *testing.T) {
	h := RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous request: want 401, got %d", rec.Code)
	}
}

func TestAuthToPrincipalThenRequireAuth(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	ring, _ := auth.NewKeyRing("manyforge", "manyforge-api", "k1", priv, map[string]ed25519.PublicKey{"k1": pub})
	pid := uuid.New()
	tok, _ := ring.Sign(pid, time.Hour, time.Now())

	var seen uuid.UUID
	var ok bool
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, ok = PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	chain := AuthToPrincipal(ring)(RequireAuth(final))

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated request: want 200, got %d", rec.Code)
	}
	if !ok || seen != pid {
		t.Errorf("principal in context: want %s (ok), got %s (ok=%v)", pid, seen, ok)
	}
}

func TestRequestIDSetsHeader(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RequestIDFromContext(r.Context()) == "" {
			t.Error("request id missing from context")
		}
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("X-Request-Id header not set")
	}
}
