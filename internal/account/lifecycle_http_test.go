//go:build integration

package account_test

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/manyforge/manyforge/internal/account"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/tenancy"
)

// TestAccountLifecycleHTTP pins the wire contract of the T077 lifecycle routes:
// GET /me/export (200), POST /me/deactivate (204/409), POST /me/delete (202).
// Service behaviour is covered by the lifecycle_test.go integration tests.
func TestAccountLifecycleHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	pub, priv, _ := ed25519.GenerateKey(nil)
	ring, _ := auth.NewKeyRing("manyforge", "manyforge-api", "k1", priv, map[string]ed25519.PublicKey{"k1": pub})
	cm := &capturingMailer{}
	acctSvc := &account.Service{DB: tdb.App, Ring: ring, Mailer: cm, AccessTTL: 15 * time.Minute, RefreshTTL: 24 * time.Hour, TokenTTL: time.Hour}
	tenSvc := &tenancy.Service{DB: tdb.App}

	mux := httpx.NewRouter(ring)
	mux.Route("/api/v1", func(r chi.Router) {
		account.NewHandler(acctSvc).PublicRoutes(r)
		r.Group(func(pr chi.Router) {
			pr.Use(httpx.RequireAuth)
			account.NewHandler(acctSvc).ProtectedRoutes(pr)
			tenancy.NewHandler(tenSvc).ProtectedRoutes(pr)
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := srv.Client()
	base := srv.URL + "/api/v1"

	signIn := func(email string) string {
		t.Helper()
		post(t, c, base+"/auth/signup", "", map[string]any{"email": email, "display_name": "H", "password": "supersecretpassword"})
		token := strings.TrimPrefix(cm.last, "token: ")
		post(t, c, base+"/auth/verify-email", "", map[string]any{"token": token})
		_, body := post(t, c, base+"/auth/login", "", map[string]any{"email": email, "password": "supersecretpassword"})
		access, _ := body["access_token"].(string)
		if access == "" {
			t.Fatalf("sign-in %s: no access token", email)
		}
		return access
	}

	t.Run("export returns the account and its memberships (200)", func(t *testing.T) {
		access := signIn("life-exp@x.test")
		if resp, _ := post(t, c, base+"/businesses", access, map[string]any{"name": "LifeCo"}); resp.StatusCode != http.StatusCreated {
			t.Fatalf("create business: want 201, got %d", resp.StatusCode)
		}
		resp, body := get(t, c, base+"/me/export", access)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("export: want 200, got %d", resp.StatusCode)
		}
		acc, _ := body["account"].(map[string]any)
		if acc == nil || acc["email"] != "life-exp@x.test" {
			t.Errorf("export account block unexpected: %v", body["account"])
		}
		if ms, _ := body["memberships"].([]any); len(ms) < 1 {
			t.Errorf("export should include >=1 membership, got %v", body["memberships"])
		}
	})

	t.Run("deactivate is refused for the last Owner (409)", func(t *testing.T) {
		access := signIn("life-deact@x.test")
		if resp, _ := post(t, c, base+"/businesses", access, map[string]any{"name": "DeCo"}); resp.StatusCode != http.StatusCreated {
			t.Fatalf("create business: want 201, got %d", resp.StatusCode)
		}
		resp, _ := post(t, c, base+"/me/deactivate", access, nil)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("last-owner deactivate: want 409, got %d", resp.StatusCode)
		}
	})

	t.Run("delete a non-owner account is 202 with a purge_after", func(t *testing.T) {
		access := signIn("life-del@x.test")
		resp, body := post(t, c, base+"/me/delete", access, nil)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("delete: want 202, got %d", resp.StatusCode)
		}
		if pa, _ := body["purge_after"].(string); pa == "" {
			t.Errorf("delete should return a purge_after, got %v", body)
		}
	})

	t.Run("lifecycle endpoints reject anonymous (401)", func(t *testing.T) {
		if resp, _ := get(t, c, base+"/me/export", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("export anon: want 401, got %d", resp.StatusCode)
		}
		if resp, _ := post(t, c, base+"/me/deactivate", "", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("deactivate anon: want 401, got %d", resp.StatusCode)
		}
		if resp, _ := post(t, c, base+"/me/delete", "", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("delete anon: want 401, got %d", resp.StatusCode)
		}
	})
}
