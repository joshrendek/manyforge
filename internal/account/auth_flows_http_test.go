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

// TestAuthFlowsHTTP pins the wire contract of the T078 flows end-to-end through
// the router: password reset, magic link, and email change. Service behaviour is
// covered by the auth_flows_test.go integration tests.
func TestAuthFlowsHTTP(t *testing.T) {
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

	mailedToken := func() string { return strings.TrimPrefix(cm.last, "token: ") }
	signIn := func(email string) string {
		t.Helper()
		post(t, c, base+"/auth/signup", "", map[string]any{"email": email, "display_name": "H", "password": "supersecretpassword"})
		post(t, c, base+"/auth/verify-email", "", map[string]any{"token": mailedToken()})
		_, body := post(t, c, base+"/auth/login", "", map[string]any{"email": email, "password": "supersecretpassword"})
		access, _ := body["access_token"].(string)
		if access == "" {
			t.Fatalf("sign-in %s: no access token", email)
		}
		return access
	}

	t.Run("password reset: request 202, confirm 204, new password logs in", func(t *testing.T) {
		signIn("flow-pr@x.test")
		if resp, _ := post(t, c, base+"/auth/password-reset", "", map[string]any{"email": "flow-pr@x.test"}); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("reset request: want 202, got %d", resp.StatusCode)
		}
		tok := mailedToken()
		if resp, _ := post(t, c, base+"/auth/password-reset/confirm", "", map[string]any{"token": tok, "password": "newsecurepass12"}); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("reset confirm: want 204, got %d", resp.StatusCode)
		}
		if resp, _ := post(t, c, base+"/auth/login", "", map[string]any{"email": "flow-pr@x.test", "password": "newsecurepass12"}); resp.StatusCode != http.StatusOK {
			t.Errorf("login with new password: want 200, got %d", resp.StatusCode)
		}
	})

	t.Run("password reset: unknown email is still 202 (no oracle); bad token confirm is 400", func(t *testing.T) {
		if resp, _ := post(t, c, base+"/auth/password-reset", "", map[string]any{"email": "nobody@x.test"}); resp.StatusCode != http.StatusAccepted {
			t.Errorf("reset request (unknown): want 202, got %d", resp.StatusCode)
		}
		if resp, b := post(t, c, base+"/auth/password-reset/confirm", "", map[string]any{"token": "bogus", "password": "newsecurepass12"}); resp.StatusCode != http.StatusBadRequest || b["code"] != "VALIDATION" {
			t.Errorf("bad-token confirm: want 400 VALIDATION, got %d (%v)", resp.StatusCode, b)
		}
	})

	t.Run("magic link: request 202, consume returns a 200 token pair", func(t *testing.T) {
		signIn("flow-ml@x.test")
		if resp, _ := post(t, c, base+"/auth/magic-link", "", map[string]any{"email": "flow-ml@x.test"}); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("magic-link request: want 202, got %d", resp.StatusCode)
		}
		resp, body := post(t, c, base+"/auth/magic-link/consume", "", map[string]any{"token": mailedToken()})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("magic-link consume: want 200, got %d", resp.StatusCode)
		}
		if access, _ := body["access_token"].(string); access == "" {
			t.Errorf("magic-link consume should return an access_token, got %v", body)
		}
	})

	t.Run("email change: 401 anon, 202 authed, 204 confirm, new email logs in", func(t *testing.T) {
		access := signIn("flow-ec@x.test")
		if resp, _ := post(t, c, base+"/me/email-change", "", map[string]any{"new_email": "x@x.test"}); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("email-change anon: want 401, got %d", resp.StatusCode)
		}
		if resp, _ := post(t, c, base+"/me/email-change", access, map[string]any{"new_email": "flow-ec-new@x.test"}); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("email-change request: want 202, got %d", resp.StatusCode)
		}
		if resp, _ := post(t, c, base+"/me/email-change/confirm", access, map[string]any{"token": mailedToken()}); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("email-change confirm: want 204, got %d", resp.StatusCode)
		}
		if resp, _ := post(t, c, base+"/auth/login", "", map[string]any{"email": "flow-ec-new@x.test", "password": "supersecretpassword"}); resp.StatusCode != http.StatusOK {
			t.Errorf("login with new email: want 200, got %d", resp.StatusCode)
		}
	})
}
