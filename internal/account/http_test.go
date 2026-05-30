//go:build integration

package account_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
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
	"github.com/manyforge/manyforge/internal/platform/mailer"
	"github.com/manyforge/manyforge/internal/tenancy"
)

type capturingMailer struct{ last string }

func (m *capturingMailer) Send(_ context.Context, msg mailer.Message) error {
	m.last = msg.Body
	return nil
}

func post(t *testing.T, c *http.Client, url, bearer string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(http.MethodPost, url, &buf)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	_ = resp.Body.Close()
	return resp, out
}

func get(t *testing.T, c *http.Client, url, bearer string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	_ = resp.Body.Close()
	return resp, out
}

func TestUS1_HTTPFlow(t *testing.T) {
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

	// signup -> 202; token captured from the mailer
	if resp, _ := post(t, c, base+"/auth/signup", "", map[string]any{
		"email": "h@x.test", "display_name": "H", "password": "supersecretpassword",
	}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("signup: want 202, got %d", resp.StatusCode)
	}
	token := strings.TrimPrefix(cm.last, "token: ")
	if token == "" {
		t.Fatal("no verification token captured")
	}

	// verify -> 204
	if resp, _ := post(t, c, base+"/auth/verify-email", "", map[string]any{"token": token}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("verify: want 204, got %d", resp.StatusCode)
	}

	// login -> 200 + tokens
	resp, body := post(t, c, base+"/auth/login", "", map[string]any{"email": "h@x.test", "password": "supersecretpassword"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: want 200, got %d", resp.StatusCode)
	}
	access, _ := body["access_token"].(string)
	if access == "" {
		t.Fatal("no access token")
	}

	// protected endpoints reject anonymous
	if resp, _ := get(t, c, base+"/me", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /me anon: want 401, got %d", resp.StatusCode)
	}
	if resp, _ := post(t, c, base+"/businesses", "", map[string]any{"name": "Acme"}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /businesses anon: want 401, got %d", resp.StatusCode)
	}

	// GET /me with bearer
	resp, body = get(t, c, base+"/me", access)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /me: want 200, got %d", resp.StatusCode)
	}
	if body["email"] != "h@x.test" || body["email_verified"] != true {
		t.Errorf("GET /me body unexpected: %v", body)
	}

	// create master business
	resp, body = post(t, c, base+"/businesses", access, map[string]any{"name": "Acme"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create business: want 201, got %d (%v)", resp.StatusCode, body)
	}
	if body["name"] != "Acme" || body["is_tenant_root"] != true {
		t.Errorf("business body unexpected: %v", body)
	}
	if body["tenant_root_id"] != body["id"] {
		t.Errorf("master tenant_root_id should equal id: %v", body)
	}
}
