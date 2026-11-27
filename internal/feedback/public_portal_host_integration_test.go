//go:build integration

package feedback_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/feedback"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// TestPublicKeyBoundToPortalHostBusiness: on a custom hostname (portal business in
// the request context) a key of another business behaves exactly like an unknown key.
func TestPublicKeyBoundToPortalHostBusiness(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	a := seedFeedbackTenant(ctx, t, tdb)
	b := seedFeedbackTenant(ctx, t, tdb)
	svc := &feedback.Service{DB: tdb.App}
	board, err := svc.CreateBoard(ctx, a.principalID, a.businessID, feedback.BoardInput{Name: "Public", IsPublic: true})
	if err != nil {
		t.Fatal(err)
	}
	key, err := svc.CreateIngestKey(ctx, a.principalID, a.businessID, board.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	h := feedback.NewPublicHandler(tdb.App, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if v := req.Header.Get("X-Test-Portal"); v != "" {
				req = req.WithContext(httpx.WithPortalBusiness(req.Context(), uuid.MustParse(v)))
			}
			next.ServeHTTP(w, req)
		})
	})
	h.PublicRoutes(r, func(next http.Handler) http.Handler { return next })

	do := func(method, path, portal, body string) (int, string) {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		if portal != "" {
			req.Header.Set("X-Test-Portal", portal)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	pa, pb := a.businessID.String(), b.businessID.String()
	base := "/feedback/public/" + key.PublishableKey + "/posts"
	unknown := "/feedback/public/fbk_unknownkey/posts"

	// Seed one post with no portal context so vote has a target.
	code, body := do(http.MethodPost, base, "", `{"title":"hello","author_identity":"d1"}`)
	if code != http.StatusCreated {
		t.Fatalf("seed submit = %d %s", code, body)
	}
	postID := jsonField(t, body, "id")

	type op struct {
		name, method, path, unknownPath, body string
		okCode                                int
	}
	ops := []op{
		{"list", http.MethodGet, base, unknown, "", http.StatusOK},
		{"submit", http.MethodPost, base, unknown, `{"title":"again","author_identity":"d2"}`, http.StatusCreated},
		{"vote", http.MethodPost, base + "/" + postID + "/votes", unknown + "/" + postID + "/votes", `{"voter_identity":"v1"}`, http.StatusOK},
	}
	for _, o := range ops {
		t.Run(o.name, func(t *testing.T) {
			wantCode, wantBody := do(o.method, o.unknownPath, pb, o.body)
			if wantCode != http.StatusUnauthorized {
				t.Fatalf("unknown key = %d, want 401", wantCode)
			}
			if code, body := do(o.method, o.path, pb, o.body); code != wantCode || body != wantBody {
				t.Fatalf("foreign-business key = %d %q, want identical to unknown %d %q", code, body, wantCode, wantBody)
			}
			if code, body := do(o.method, o.path, pa, o.body); code != o.okCode {
				t.Fatalf("own-business key = %d %s, want %d", code, body, o.okCode)
			}
			if code, body := do(o.method, o.path, "", o.body); code != o.okCode {
				t.Fatalf("no portal context = %d %s, want %d", code, body, o.okCode)
			}
		})
	}
}

func jsonField(t *testing.T, body, field string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	s, _ := m[field].(string)
	if s == "" {
		t.Fatalf("no %q in %s", field, body)
	}
	return s
}
