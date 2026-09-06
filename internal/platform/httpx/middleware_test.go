package httpx

import (
	"bytes"
	"crypto/ed25519"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

func captureDefaultLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

// MF-MAIL-LOG-001: matched sensitive routes must be logged as Chi patterns,
// never as concrete capability or subscriber-email paths.
func TestRequestLoggerUsesMatchedRoutePattern(t *testing.T) {
	const sentinel = "MF_CAPABILITY_SENTINEL"

	tests := []struct {
		name    string
		pattern string
		path    string
	}{
		{"confirm", "/m/confirm/{token}", "/m/confirm/" + sentinel},
		{"unsubscribe", "/m/u/{token}", "/m/u/" + sentinel},
		{"open", "/m/o/{token}", "/m/o/" + sentinel},
		{"click", "/m/c/{token}", "/m/c/" + sentinel},
		{
			"S2S subscriber email",
			"/api/v1/businesses/{id}/mailing/s2s/{key}/subscribers/{email}",
			"/api/v1/businesses/business-id/mailing/s2s/" + sentinel +
				"/subscribers/ada+" + sentinel + "@example.test",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logs := captureDefaultLogs(t)
			router := NewRouter(nil)
			router.Get(test.pattern, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, test.path, nil))

			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
			}
			if strings.Contains(logs.String(), sentinel) {
				t.Fatalf("request log disclosed sentinel %q: %s", sentinel, logs.String())
			}
			if !strings.Contains(logs.String(), test.pattern) {
				t.Fatalf("request log missing matched route pattern %q: %s", test.pattern, logs.String())
			}
		})
	}
}

func TestSafePathRedactsSensitiveUnmatchedPaths(t *testing.T) {
	const sentinel = "MF_CAPABILITY_SENTINEL"

	tests := []struct {
		name string
		path string
		want string
	}{
		{"confirm", "/m/confirm/" + sentinel + "/decoded-tail", "/m/confirm/{token}"},
		{"unsubscribe", "/m/u/" + sentinel + "/decoded-tail", "/m/u/{token}"},
		{"open", "/m/o/" + sentinel + "/decoded-tail", "/m/o/{token}"},
		{"click", "/m/c/" + sentinel + "/decoded-tail", "/m/c/{token}"},
		{
			"S2S key and subscriber email",
			"/api/v1/businesses/business-id/mailing/s2s/" + sentinel +
				"/subscribers/ada+" + sentinel + "@example.test/decoded-tail",
			"/api/v1/businesses/business-id/mailing/s2s/{redacted}",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if got := SafePath(request); got != test.want {
				t.Fatalf("SafePath() = %q, want %q", got, test.want)
			}
			if strings.Contains(SafePath(request), sentinel) {
				t.Fatalf("SafePath() disclosed sentinel %q", sentinel)
			}
		})
	}
}

func TestRecoverUsesSafePathForUnmatchedSensitivePath(t *testing.T) {
	const sentinel = "MF_CAPABILITY_SENTINEL"

	logs := captureDefaultLogs(t)
	handler := Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/m/u/"+sentinel+"/decoded-tail", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if strings.Contains(logs.String(), sentinel) {
		t.Fatalf("recovery log disclosed sentinel %q: %s", sentinel, logs.String())
	}
	if !strings.Contains(logs.String(), "/m/u/{token}") {
		t.Fatalf("recovery log missing redacted path: %s", logs.String())
	}
}
