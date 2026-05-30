//go:build integration

package invitations_test

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
	"github.com/manyforge/manyforge/internal/invitations"
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

type stack struct {
	c    *http.Client
	base string
	cm   *capturingMailer
}

func newStack(t *testing.T) stack {
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

	mux := httpx.NewRouter(ring)
	mux.Route("/api/v1", func(r chi.Router) {
		account.NewHandler(acctSvc).PublicRoutes(r)
		r.Group(func(pr chi.Router) {
			pr.Use(httpx.RequireAuth)
			account.NewHandler(acctSvc).ProtectedRoutes(pr)
			tenancy.NewHandler(&tenancy.Service{DB: tdb.App}).ProtectedRoutes(pr)
			authz.NewHandler(&authz.Service{DB: tdb.App}).ProtectedRoutes(pr)
			invitations.NewHandler(&invitations.Service{DB: tdb.App, Mailer: cm}).ProtectedRoutes(pr)
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return stack{c: srv.Client(), base: srv.URL + "/api/v1", cm: cm}
}

const pw = "supersecretpassword"

// register signs up, verifies, and logs in; returns the access token.
func (s stack) register(t *testing.T, email string) string {
	t.Helper()
	do(t, s.c, http.MethodPost, s.base+"/auth/signup", "", map[string]any{"email": email, "display_name": "U", "password": pw})
	token := strings.TrimPrefix(s.cm.last, "token: ")
	do(t, s.c, http.MethodPost, s.base+"/auth/verify-email", "", map[string]any{"token": token})
	return s.login(t, email)
}

// registerUnverified signs up and logs in WITHOUT verifying.
func (s stack) registerUnverified(t *testing.T, email string) string {
	t.Helper()
	do(t, s.c, http.MethodPost, s.base+"/auth/signup", "", map[string]any{"email": email, "display_name": "U", "password": pw})
	return s.login(t, email)
}

func (s stack) login(t *testing.T, email string) string {
	t.Helper()
	_, lb := do(t, s.c, http.MethodPost, s.base+"/auth/login", "", map[string]any{"email": email, "password": pw})
	access, _ := lb["access_token"].(string)
	if access == "" {
		t.Fatalf("login %s: no access token (%v)", email, lb)
	}
	return access
}

func (s stack) createMaster(t *testing.T, access, name string) string {
	t.Helper()
	_, b := do(t, s.c, http.MethodPost, s.base+"/businesses", access, map[string]any{"name": name})
	id, _ := b["id"].(string)
	if id == "" {
		t.Fatalf("create master: %v", b)
	}
	return id
}

func (s stack) roleID(t *testing.T, access, master, key string) string {
	t.Helper()
	_, b := do(t, s.c, http.MethodGet, s.base+"/businesses/"+master+"/roles", access, nil)
	raw, _ := b["items"].([]any)
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok && m["key"] == key {
			return m["id"].(string)
		}
	}
	t.Fatalf("role %q not found", key)
	return ""
}

// invite creates an invitation and returns the raw token captured from the mailer.
func (s stack) invite(t *testing.T, access, master, email, roleID string, wantStatus int) string {
	t.Helper()
	resp, b := do(t, s.c, http.MethodPost, s.base+"/businesses/"+master+"/invitations", access, map[string]any{"email": email, "role_id": roleID})
	if resp.StatusCode != wantStatus {
		t.Fatalf("invite %s: want %d, got %d (%v)", email, wantStatus, resp.StatusCode, b)
	}
	return strings.TrimPrefix(s.cm.last, "token: ")
}

func (s stack) businessIDs(t *testing.T, access string) map[string]bool {
	t.Helper()
	_, b := do(t, s.c, http.MethodGet, s.base+"/businesses", access, nil)
	raw, _ := b["items"].([]any)
	out := map[string]bool{}
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out[m["id"].(string)] = true
		}
	}
	return out
}

func TestInvitations(t *testing.T) {
	s := newStack(t)
	owner := s.register(t, "owner@x.test")
	master := s.createMaster(t, owner, "Acme")
	memberRole := s.roleID(t, owner, master, "member")
	adminRole := s.roleID(t, owner, master, "admin")
	ownerRole := s.roleID(t, owner, master, "owner")

	t.Run("invite -> accept -> scoped access -> single-use", func(t *testing.T) {
		token := s.invite(t, owner, master, "alice@x.test", memberRole, http.StatusAccepted)
		alice := s.register(t, "alice@x.test")

		if s.businessIDs(t, alice)[master] {
			t.Fatal("alice should not see the business before accepting")
		}
		resp, b := do(t, s.c, http.MethodPost, s.base+"/invitations/accept", alice, map[string]any{"token": token})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("accept: want 200, got %d (%v)", resp.StatusCode, b)
		}
		if b["business_id"] != master {
			t.Errorf("accept business_id: want %s, got %v", master, b["business_id"])
		}
		if !s.businessIDs(t, alice)[master] {
			t.Error("alice should see the business after accepting (scoped access)")
		}
		// single-use: replay is gone
		if resp, _ := do(t, s.c, http.MethodPost, s.base+"/invitations/accept", alice, map[string]any{"token": token}); resp.StatusCode != http.StatusGone {
			t.Errorf("replay: want 410, got %d", resp.StatusCode)
		}
	})

	t.Run("unknown token is 410", func(t *testing.T) {
		bob := s.register(t, "bob@x.test")
		if resp, _ := do(t, s.c, http.MethodPost, s.base+"/invitations/accept", bob, map[string]any{"token": "not-a-real-token"}); resp.StatusCode != http.StatusGone {
			t.Errorf("unknown token: want 410, got %d", resp.StatusCode)
		}
	})

	t.Run("empty token is 400", func(t *testing.T) {
		bob := s.login(t, "bob@x.test")
		if resp, _ := do(t, s.c, http.MethodPost, s.base+"/invitations/accept", bob, map[string]any{"token": ""}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("empty token: want 400, got %d", resp.StatusCode)
		}
	})

	t.Run("email mismatch is 403", func(t *testing.T) {
		token := s.invite(t, owner, master, "carol@x.test", memberRole, http.StatusAccepted)
		dave := s.register(t, "dave@x.test") // different email accepts carol's token
		resp, b := do(t, s.c, http.MethodPost, s.base+"/invitations/accept", dave, map[string]any{"token": token})
		if resp.StatusCode != http.StatusForbidden || b["code"] != "EMAIL_MISMATCH" {
			t.Errorf("mismatch: want 403 EMAIL_MISMATCH, got %d (%v)", resp.StatusCode, b)
		}
	})

	t.Run("unverified accepter is 403", func(t *testing.T) {
		token := s.invite(t, owner, master, "erin@x.test", memberRole, http.StatusAccepted)
		erin := s.registerUnverified(t, "erin@x.test")
		resp, b := do(t, s.c, http.MethodPost, s.base+"/invitations/accept", erin, map[string]any{"token": token})
		if resp.StatusCode != http.StatusForbidden || b["code"] != "EMAIL_NOT_VERIFIED" {
			t.Errorf("unverified: want 403 EMAIL_NOT_VERIFIED, got %d (%v)", resp.StatusCode, b)
		}
	})

	t.Run("list shows invitations; revoke makes the token gone", func(t *testing.T) {
		token := s.invite(t, owner, master, "frank@x.test", memberRole, http.StatusAccepted)
		_, list := do(t, s.c, http.MethodGet, s.base+"/businesses/"+master+"/invitations", owner, nil)
		items, _ := list["items"].([]any)
		var invID string
		for _, r := range items {
			m := r.(map[string]any)
			if m["email"] == "frank@x.test" && m["status"] == "pending" {
				invID = m["id"].(string)
				role := m["role"].(map[string]any)
				if role["key"] != "member" {
					t.Errorf("listed role: want member, got %v", role["key"])
				}
			}
		}
		if invID == "" {
			t.Fatal("frank's pending invitation not listed")
		}
		if resp, _ := do(t, s.c, http.MethodDelete, s.base+"/businesses/"+master+"/invitations/"+invID, owner, nil); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("revoke: want 204, got %d", resp.StatusCode)
		}
		frank := s.register(t, "frank@x.test")
		if resp, _ := do(t, s.c, http.MethodPost, s.base+"/invitations/accept", frank, map[string]any{"token": token}); resp.StatusCode != http.StatusGone {
			t.Errorf("accept revoked: want 410, got %d", resp.StatusCode)
		}
	})

	t.Run("resend rotates the token: old is gone, new works", func(t *testing.T) {
		oldToken := s.invite(t, owner, master, "grace@x.test", memberRole, http.StatusAccepted)
		// find the invitation id
		_, list := do(t, s.c, http.MethodGet, s.base+"/businesses/"+master+"/invitations", owner, nil)
		var invID string
		for _, r := range list["items"].([]any) {
			m := r.(map[string]any)
			if m["email"] == "grace@x.test" && m["status"] == "pending" {
				invID = m["id"].(string)
			}
		}
		resp, _ := do(t, s.c, http.MethodPost, s.base+"/businesses/"+master+"/invitations/"+invID+"/resend", owner, nil)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("resend: want 202, got %d", resp.StatusCode)
		}
		newToken := strings.TrimPrefix(s.cm.last, "token: ")
		grace := s.register(t, "grace@x.test")
		if resp, _ := do(t, s.c, http.MethodPost, s.base+"/invitations/accept", grace, map[string]any{"token": oldToken}); resp.StatusCode != http.StatusGone {
			t.Errorf("old token after resend: want 410, got %d", resp.StatusCode)
		}
		if resp, _ := do(t, s.c, http.MethodPost, s.base+"/invitations/accept", grace, map[string]any{"token": newToken}); resp.StatusCode != http.StatusOK {
			t.Errorf("new token: want 200, got %d", resp.StatusCode)
		}
	})

	t.Run("create/list by a non-member is 404", func(t *testing.T) {
		stranger := s.register(t, "stranger@x.test")
		if resp, _ := do(t, s.c, http.MethodPost, s.base+"/businesses/"+master+"/invitations", stranger, map[string]any{"email": "x@x.test", "role_id": memberRole}); resp.StatusCode != http.StatusNotFound {
			t.Errorf("stranger create: want 404, got %d", resp.StatusCode)
		}
		if resp, _ := do(t, s.c, http.MethodGet, s.base+"/businesses/"+master+"/invitations", stranger, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("stranger list: want 404, got %d", resp.StatusCode)
		}
	})

	t.Run("create with malformed role_id is 400", func(t *testing.T) {
		if resp, _ := do(t, s.c, http.MethodPost, s.base+"/businesses/"+master+"/invitations", owner, map[string]any{"email": "x@x.test", "role_id": "not-a-uuid"}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("malformed role_id: want 400, got %d", resp.StatusCode)
		}
	})

	// FR-023 escalation, exercised end-to-end: an admin (who lacks business.delete /
	// ownership.transfer) cannot invite with a role above its own.
	t.Run("escalation: admin cannot invite owner-role (409) but can invite member", func(t *testing.T) {
		adminToken := s.invite(t, owner, master, "ivan@x.test", adminRole, http.StatusAccepted)
		ivan := s.register(t, "ivan@x.test")
		if resp, _ := do(t, s.c, http.MethodPost, s.base+"/invitations/accept", ivan, map[string]any{"token": adminToken}); resp.StatusCode != http.StatusOK {
			t.Fatalf("ivan accept admin: want 200, got %d", resp.StatusCode)
		}
		// ivan (admin) invites someone as Owner -> escalation refused
		if resp, _ := do(t, s.c, http.MethodPost, s.base+"/businesses/"+master+"/invitations", ivan, map[string]any{"email": "judy@x.test", "role_id": ownerRole}); resp.StatusCode != http.StatusConflict {
			t.Errorf("admin invites owner: want 409, got %d", resp.StatusCode)
		}
		// but inviting as member (subset of admin) is fine
		if resp, _ := do(t, s.c, http.MethodPost, s.base+"/businesses/"+master+"/invitations", ivan, map[string]any{"email": "judy@x.test", "role_id": memberRole}); resp.StatusCode != http.StatusAccepted {
			t.Errorf("admin invites member: want 202, got %d", resp.StatusCode)
		}
	})
}
