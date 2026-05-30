//go:build integration

package authz_test

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
	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/mailer"
)

type capturingMailer struct{ last string }

func (m *capturingMailer) Send(_ context.Context, msg mailer.Message) error {
	m.last = msg.Body
	return nil
}

func do(t *testing.T, c *http.Client, method, url, bearer string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, url, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	_ = resp.Body.Close()
	return resp, out
}

type harness struct {
	c      *http.Client
	base   string
	access string
}

// setup spins an ephemeral DB + router (account + authz) and returns a verified,
// logged-in caller.
func setup(t *testing.T) harness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	pub, priv, _ := ed25519.GenerateKey(nil)
	ring, _ := auth.NewKeyRing("manyforge", "manyforge-api", "k1", priv, map[string]ed25519.PublicKey{"k1": pub})
	cm := &capturingMailer{}
	acctSvc := &account.Service{DB: tdb.App, Ring: ring, Mailer: cm, AccessTTL: 15 * time.Minute, RefreshTTL: 24 * time.Hour, TokenTTL: time.Hour}
	authzH := authz.NewHandler(&authz.Service{DB: tdb.App})

	mux := httpx.NewRouter(ring)
	mux.Route("/api/v1", func(r chi.Router) {
		account.NewHandler(acctSvc).PublicRoutes(r)
		r.Group(func(pr chi.Router) {
			pr.Use(httpx.RequireAuth)
			account.NewHandler(acctSvc).ProtectedRoutes(pr)
			authzH.ProtectedRoutes(pr)
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := srv.URL + "/api/v1"
	c := srv.Client()

	do(t, c, http.MethodPost, base+"/auth/signup", "", map[string]any{"email": "owner@x.test", "display_name": "O", "password": "supersecretpassword"})
	token := strings.TrimPrefix(cm.last, "token: ")
	do(t, c, http.MethodPost, base+"/auth/verify-email", "", map[string]any{"token": token})
	_, lb := do(t, c, http.MethodPost, base+"/auth/login", "", map[string]any{"email": "owner@x.test", "password": "supersecretpassword"})
	access, _ := lb["access_token"].(string)
	if access == "" {
		t.Fatalf("setup failed: no access token (%v)", lb)
	}
	return harness{c: c, base: base, access: access}
}

func keysOf(body map[string]any) []string {
	raw, _ := body["items"].([]any)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			if k, ok := m["key"].(string); ok {
				out = append(out, k)
			}
		}
	}
	return out
}

// TestPermissions_Contract pins the GET /permissions wire contract.
func TestPermissions_Contract(t *testing.T) {
	h := setup(t)

	t.Run("anonymous is rejected with 401", func(t *testing.T) {
		resp, _ := do(t, h.c, http.MethodGet, h.base+"/permissions", "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", resp.StatusCode)
		}
	})

	t.Run("returns the seeded catalog with key/module/description", func(t *testing.T) {
		resp, body := do(t, h.c, http.MethodGet, h.base+"/permissions", h.access, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d", resp.StatusCode)
		}
		items, _ := body["items"].([]any)
		want := []string{
			"audit.read", "business.delete", "business.read", "hierarchy.manage",
			"members.manage", "members.read", "ownership.transfer", "roles.manage", "roles.read",
		}
		got := keysOf(body)
		have := map[string]bool{}
		for _, k := range got {
			have[k] = true
		}
		for _, k := range want {
			if !have[k] {
				t.Errorf("missing permission key %q (got %v)", k, got)
			}
		}
		// fields populated on the first item
		if len(items) > 0 {
			m, _ := items[0].(map[string]any)
			if m["module"] == "" || m["description"] == "" {
				t.Errorf("permission missing module/description: %v", m)
			}
		}
		// keyset order: ascending by key
		for i := 1; i < len(got); i++ {
			if got[i-1] >= got[i] {
				t.Errorf("not sorted ascending by key at %d: %v", i, got)
				break
			}
		}
	})

	t.Run("limit is honored and pages via next_cursor", func(t *testing.T) {
		resp, body := do(t, h.c, http.MethodGet, h.base+"/permissions?limit=3", h.access, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d", resp.StatusCode)
		}
		page1 := keysOf(body)
		if len(page1) != 3 {
			t.Fatalf("limit=3: want 3 items, got %d (%v)", len(page1), page1)
		}
		cursor, _ := body["next_cursor"].(string)
		if cursor == "" {
			t.Fatal("expected a next_cursor for a full page")
		}
		_, body2 := do(t, h.c, http.MethodGet, h.base+"/permissions?limit=3&cursor="+cursor, h.access, nil)
		page2 := keysOf(body2)
		if len(page2) == 0 {
			t.Fatal("second page empty")
		}
		// disjoint + continues past the cursor
		for _, k := range page2 {
			if k <= cursor {
				t.Errorf("page2 key %q not after cursor %q", k, cursor)
			}
		}
	})
}
