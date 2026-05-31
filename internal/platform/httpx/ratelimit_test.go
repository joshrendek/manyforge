package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
)

func TestRateLimitMiddleware(t *testing.T) {
	// rate=0 (no refill), burst=2 -> the third request for a key is rejected.
	lim := ratelimit.NewTokenBucket(0, 2)
	var served int
	h := httpx.RateLimit(lim, func(r *http.Request) string { return r.Header.Get("X-Key") })(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			served++
			w.WriteHeader(http.StatusOK)
		}),
	)

	call := func(key string) int {
		req := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
		req.Header.Set("X-Key", key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if c := call("a"); c != http.StatusOK {
		t.Fatalf("1st request: want 200, got %d", c)
	}
	if c := call("a"); c != http.StatusOK {
		t.Fatalf("2nd request: want 200, got %d", c)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	req.Header.Set("X-Key", "a")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd request: want 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("429 should set Retry-After")
	}
	if served != 2 {
		t.Errorf("handler should have run twice (3rd blocked), ran %d", served)
	}

	// A different key has its own bucket and is still allowed.
	if c := call("b"); c != http.StatusOK {
		t.Errorf("different key should be allowed, got %d", c)
	}
}
