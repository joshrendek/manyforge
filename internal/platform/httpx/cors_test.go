package httpx_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/manyforge/manyforge/internal/feedback"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
	"github.com/manyforge/manyforge/internal/telemetry"
)

const corsSite = "https://customer.example"
const corsInstance = "https://instance.example"

func corsRequest(handler http.Handler, method string, headers http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/public/key", strings.NewReader("a simple write"))
	r.Header = headers
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func requireCORSError(t *testing.T, w *httptest.ResponseRecorder, code string) {
	t.Helper()
	var body httpx.ErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusForbidden || body.Code != code {
		t.Fatalf("response = %d %+v, want 403 %s", w.Code, body, code)
	}
}

func hasCORSHeaderToken(headers http.Header, header, token string) bool {
	for _, value := range headers.Values(header) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func TestPublicIngestCORSOriginBoundary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		origins []string
	}{
		{"denied", []string{"https://denied.example"}},
		{"opaque", []string{"null"}},
		{"wildcard", []string{"*"}},
		{"duplicate", []string{corsSite, corsSite}},
		{"joined", []string{corsSite + ", " + corsSite}},
		{"empty", []string{""}},
		{"path", []string{corsSite + "/page"}},
		{"userinfo", []string{"https://user@customer.example"}},
		{"oversized", []string{"https://" + strings.Repeat("a", 2048)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutations := 0
			h := httpx.PublicIngestCORS([]string{corsSite}, corsInstance, http.MethodPost)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				mutations++
			}))
			w := corsRequest(h, http.MethodPost, http.Header{"Origin": tc.origins, "Content-Type": {"text/plain"}})
			requireCORSError(t, w, "ORIGIN_NOT_ALLOWED")
			if mutations != 0 || w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("denied simple write reached handler or exposed CORS: mutations=%d headers=%v", mutations, w.Header())
			}
		})
	}
	for _, origin := range []string{corsSite, corsInstance, ""} {
		mutations := 0
		h := httpx.PublicIngestCORS([]string{corsSite}, corsInstance, http.MethodPost)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mutations++
			w.WriteHeader(http.StatusAccepted)
		}))
		headers := make(http.Header)
		if origin != "" {
			headers.Set("Origin", origin)
		}
		w := corsRequest(h, http.MethodPost, headers)
		if w.Code != http.StatusAccepted || mutations != 1 || w.Header().Get("Access-Control-Allow-Origin") != origin {
			t.Fatalf("allowed origin %q: status=%d mutations=%d headers=%v", origin, w.Code, mutations, w.Header())
		}
	}
}

func TestPublicIngestCORSDisabled(t *testing.T) {
	called := 0
	h := httpx.PublicIngestCORS(nil, "", http.MethodPost)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusAccepted)
	}))
	for _, origin := range []string{corsSite, "null", ""} {
		w := corsRequest(h, http.MethodPost, http.Header{"Origin": {origin}})
		if w.Code != http.StatusAccepted || w.Header().Get("Access-Control-Allow-Origin") != "" || len(w.Header().Values("Vary")) != 0 {
			t.Fatalf("disabled policy changed actual response: %d %v", w.Code, w.Header())
		}
	}
	w := corsRequest(h, http.MethodOptions, http.Header{"Origin": {corsSite}, "Access-Control-Request-Method": {"POST"}})
	requireCORSError(t, w, "CORS_NOT_ALLOWED")
	if called != 3 || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("disabled preflight reached handler or allowed CORS: called=%d headers=%v", called, w.Header())
	}
}

func TestPublicIngestCORSPreflightSyntax(t *testing.T) {
	for _, tc := range []struct {
		name    string
		methods []string
		headers []string
	}{
		{"missing-method", nil, nil},
		{"duplicate-method", []string{"POST", "POST"}, nil},
		{"joined-method", []string{"POST, GET"}, nil},
		{"wrong-method", []string{"DELETE"}, nil},
		{"case-sensitive-method", []string{"post"}, nil},
		{"authorization", []string{"POST"}, []string{"Authorization"}},
		{"feedback-signature", []string{"POST"}, []string{"X-Feedback-Signature"}},
		{"telemetry-signature", []string{"POST"}, []string{"X-Telemetry-Signature"}},
		{"empty-header", []string{"POST"}, []string{""}},
		{"empty-list-item", []string{"POST"}, []string{"Content-Type,"}},
		{"oversized-header", []string{"POST"}, []string{strings.Repeat(" ", 1025) + "Content-Type"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := httpx.PublicIngestCORS([]string{corsSite}, corsInstance, http.MethodPost)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("invalid preflight reached handler")
			}))
			w := corsRequest(h, http.MethodOptions, http.Header{"Origin": {corsSite}, "Access-Control-Request-Method": tc.methods, "Access-Control-Request-Headers": tc.headers})
			requireCORSError(t, w, "CORS_NOT_ALLOWED")
			if w.Header().Get("Access-Control-Allow-Methods") != "" || w.Header().Get("Access-Control-Allow-Credentials") != "" {
				t.Fatalf("invalid preflight granted capabilities: %v", w.Header())
			}
		})
	}
}

func TestPublicIngestCORSReadableErrorsAndRateBudget(t *testing.T) {
	limiter := ratelimit.NewTokenBucket(0, 2)
	calls := 0
	next := httpx.RateLimit(limiter, func(*http.Request) string { return "shared-ip" })(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("X-Request-Id", "request-123")
		status := http.StatusUnauthorized
		if calls == 2 {
			status = http.StatusNotFound
		}
		w.WriteHeader(status)
	}))
	h := httpx.PublicIngestCORS([]string{corsSite}, corsInstance, http.MethodGet, http.MethodPost)(next)
	for _, requestedHeaders := range [][]string{nil, {"cOnTeNt-TyPe"}} {
		w := corsRequest(h, http.MethodOptions, http.Header{"Origin": {corsSite}, "Access-Control-Request-Method": {"POST"}, "Access-Control-Request-Headers": requestedHeaders})
		if w.Code != http.StatusNoContent || w.Body.Len() != 0 || calls != 0 {
			t.Fatalf("preflight status=%d body=%q handler calls=%d", w.Code, w.Body.String(), calls)
		}
		for key, want := range map[string]string{"Access-Control-Allow-Origin": corsSite, "Access-Control-Allow-Methods": "GET, POST", "Access-Control-Allow-Headers": "Content-Type", "Access-Control-Max-Age": "300"} {
			if got := w.Header().Get(key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
		for _, token := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
			if !hasCORSHeaderToken(w.Header(), "Vary", token) {
				t.Errorf("preflight missing Vary %s", token)
			}
		}
	}
	for _, want := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusTooManyRequests} {
		w := httptest.NewRecorder()
		w.Header().Add("Vary", "Accept-Encoding, origin")
		w.Header().Add("Vary", "Accept-Language")
		r := httptest.NewRequest(http.MethodPost, "/public/key", nil)
		r.Header.Set("Origin", corsSite)
		h.ServeHTTP(w, r)
		if w.Code != want || w.Header().Get("Access-Control-Allow-Origin") != corsSite {
			t.Fatalf("actual error = %d %v, want readable %d", w.Code, w.Header(), want)
		}
		if w.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Fatal("CORS must not grant credential access")
		}
		for _, token := range []string{"X-Request-Id", "Retry-After"} {
			if !hasCORSHeaderToken(w.Header(), "Access-Control-Expose-Headers", token) {
				t.Errorf("error does not expose %s", token)
			}
		}
		for _, token := range []string{"Accept-Encoding", "Accept-Language", "Origin"} {
			if !hasCORSHeaderToken(w.Header(), "Vary", token) {
				t.Errorf("lost Vary %s", token)
			}
		}
		if want == http.StatusTooManyRequests && w.Header().Get("Retry-After") == "" {
			t.Fatal("actual rate-limit response lacks Retry-After")
		}
	}
}

func TestPublicIngestRegisteredPreflightsBypassKeysAndLimiter(t *testing.T) {
	r := chi.NewRouter()
	limited := 0
	limit := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			limited++
			w.WriteHeader(http.StatusTooManyRequests)
		})
	}
	// Nil databases deliberately prove that OPTIONS and denied simple writes do
	// not resolve a key or execute the real handlers on any registered shape.
	(&feedback.PublicHandler{AllowedOrigins: []string{corsSite}, InstanceOrigin: corsInstance}).PublicRoutes(r, limit)
	(&telemetry.PublicHandler{AllowedOrigins: []string{corsSite}, InstanceOrigin: corsInstance}).PublicRoutes(r, limit)
	for _, path := range []string{"/feedback/public/unknown/posts", "/feedback/public/revoked/posts", "/feedback/public/unknown/posts/anything/votes", "/telemetry/ingest/unknown", "/telemetry/ingest/revoked"} {
		req := httptest.NewRequest(http.MethodOptions, path, nil)
		req.Header.Set("Origin", corsSite)
		req.Header.Set("Access-Control-Request-Method", "POST")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent || limited != 0 {
			t.Fatalf("preflight %s: status=%d limiter calls=%d", path, w.Code, limited)
		}
		req = httptest.NewRequest(http.MethodPost, path, strings.NewReader("simple write"))
		req.Header.Set("Origin", "https://denied.example")
		req.Header.Set("Content-Type", "text/plain")
		w = httptest.NewRecorder()
		r.ServeHTTP(w, req)
		requireCORSError(t, w, "ORIGIN_NOT_ALLOWED")
		if limited != 0 {
			t.Fatal("denied simple write reached limiter")
		}
	}
	for _, path := range []string{"/feedback/public/unknown/posts", "/feedback/public/unknown/posts/anything/votes", "/telemetry/ingest/unknown"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Origin", corsSite)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusTooManyRequests || w.Header().Get("Access-Control-Allow-Origin") != corsSite {
			t.Fatalf("route %s limiter response is not readable: %d %v", path, w.Code, w.Header())
		}
	}
	if limited != 3 {
		t.Fatalf("actual requests spent limiter budget %d times, want 3", limited)
	}
}
