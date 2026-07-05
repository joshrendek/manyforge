package githubapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// Shared test doubles (used across Tasks 3/5/6, all package githubapp).
type fakeAPI struct {
	convertCreds AppCreds
	userInstalls []Installation
}

func (f *fakeAPI) ConvertManifest(ctx context.Context, code string) (AppCreds, error) {
	return f.convertCreds, nil
}
func (f *fakeAPI) ExchangeOAuthCode(ctx context.Context, a, b, c string) (string, error) {
	return "utoken", nil
}
func (f *fakeAPI) ListUserInstallations(ctx context.Context, t string) ([]Installation, error) {
	return f.userInstalls, nil
}

type stubStore struct {
	cfg             AppConfig
	getErr, saveErr error
	saved           *AppCreds
}

func (s *stubStore) Get(ctx context.Context) (AppConfig, error) { return s.cfg, s.getErr }
func (s *stubStore) Save(ctx context.Context, c AppCreds) error { s.saved = &c; return s.saveErr }

type stubNonce struct{ first bool }

func (n *stubNonce) Consume(ctx context.Context, nonce string) (bool, error) { return n.first, nil }

func TestManifestRouteRejectsNonOperator(t *testing.T) {
	op := uuid.New()
	h := &Handler{OperatorPrincipal: op, StateKey: []byte("0123456789abcdef0123456789abcdef"),
		PublicBaseURL: "https://hub.example.com", Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(withPrincipalMW(uuid.New())) // not the operator
		h.OperatorRoutes(g)
	})
	req := httptest.NewRequest(http.MethodGet, "/github/app/manifest", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestManifestRouteRejectsWhenOperatorUnset(t *testing.T) {
	h := &Handler{OperatorPrincipal: uuid.Nil, StateKey: []byte("0123456789abcdef0123456789abcdef"),
		PublicBaseURL: "https://hub.example.com", Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	r := chi.NewRouter()
	r.Group(func(g chi.Router) { g.Use(withPrincipalMW(uuid.New())); h.OperatorRoutes(g) })
	req := httptest.NewRequest(http.MethodGet, "/github/app/manifest", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when operator unset", w.Code)
	}
}

func TestManifestRouteReturnsJSONForOperator(t *testing.T) {
	op := uuid.New()
	h := &Handler{OperatorPrincipal: op, StateKey: []byte("0123456789abcdef0123456789abcdef"),
		PublicBaseURL: "https://hub.example.com", Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	r := chi.NewRouter()
	r.Group(func(g chi.Router) { g.Use(withPrincipalMW(op)); h.OperatorRoutes(g) })
	req := httptest.NewRequest(http.MethodGet, "/github/app/manifest", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// NOTE: explicit tags are required — the handler's JSON keys are snake_case
	// ("action_url") and encoding/json's untagged case-insensitive field match
	// does not fold away underscores, so an untagged ActionURL field silently
	// decodes to "" instead of erroring.
	var body struct {
		ActionURL string `json:"action_url"`
		Manifest  string `json:"manifest"`
		State     string `json:"state"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body.ActionURL, "settings/apps/new") {
		t.Errorf("action_url = %q", body.ActionURL)
	}
	if !strings.Contains(body.Manifest, "hub.example.com/api/v1/github/webhook") {
		t.Error("manifest missing webhook url")
	}
	if !strings.Contains(body.Manifest, "hub.example.com/settings/github/installed") {
		t.Error("manifest missing callback_urls")
	}
	if body.State == "" {
		t.Error("missing state")
	}
}

// test helper: inject a principal into the request context via the exported setter.
func withPrincipalMW(pid uuid.UUID) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(httpx.WithPrincipal(req.Context(), pid)))
		})
	}
}
