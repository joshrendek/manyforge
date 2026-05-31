//go:build integration

package tenancy_test

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
	"github.com/google/uuid"

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

// do issues a JSON request and decodes the (best-effort) JSON object response.
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

type fixture struct {
	c        *http.Client
	base     string
	access   string
	masterID string
}

// setup spins an ephemeral DB + full router and returns a verified, logged-in
// Owner with one master business ("Acme"). The *testdb.TestDB is returned so
// contract tests that need a second principal can seed one directly.
func setup(t *testing.T) (fixture, *testdb.TestDB) {
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
	t.Cleanup(srv.Close)
	base := srv.URL + "/api/v1"
	c := srv.Client()

	do(t, c, http.MethodPost, base+"/auth/signup", "", map[string]any{"email": "owner@x.test", "display_name": "O", "password": "supersecretpassword"})
	token := strings.TrimPrefix(cm.last, "token: ")
	do(t, c, http.MethodPost, base+"/auth/verify-email", "", map[string]any{"token": token})
	_, lb := do(t, c, http.MethodPost, base+"/auth/login", "", map[string]any{"email": "owner@x.test", "password": "supersecretpassword"})
	access, _ := lb["access_token"].(string)
	_, mb := do(t, c, http.MethodPost, base+"/businesses", access, map[string]any{"name": "Acme"})
	masterID, _ := mb["id"].(string)
	if access == "" || masterID == "" {
		t.Fatalf("setup failed: access=%q master=%v", access, mb)
	}
	return fixture{c: c, base: base, access: access, masterID: masterID}, tdb
}

func (f fixture) items(t *testing.T) []map[string]any {
	t.Helper()
	_, b := do(t, f.c, http.MethodGet, f.base+"/businesses", f.access, nil)
	raw, _ := b["items"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (f fixture) find(t *testing.T, id string) map[string]any {
	t.Helper()
	for _, m := range f.items(t) {
		if m["id"] == id {
			return m
		}
	}
	return nil
}

// TestUS2_HierarchyContract pins the HTTP wire contract (status codes + error
// shapes) of the US2 mutation routes. Service-level behaviour is covered by
// hierarchy_test.go; this asserts the handler/router layer specifically.
func TestUS2_HierarchyContract(t *testing.T) {
	f, _ := setup(t)

	mkSub := func(parent, name string) string {
		t.Helper()
		resp, b := do(t, f.c, http.MethodPost, f.base+"/businesses", f.access, map[string]any{"name": name, "parent_id": parent})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("mkSub %q: want 201, got %d (%v)", name, resp.StatusCode, b)
		}
		id, _ := b["id"].(string)
		return id
	}

	t.Run("create sub-business is 201 with parent + tenant linkage", func(t *testing.T) {
		resp, b := do(t, f.c, http.MethodPost, f.base+"/businesses", f.access, map[string]any{"name": "Engineering", "parent_id": f.masterID})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("want 201, got %d (%v)", resp.StatusCode, b)
		}
		if b["parent_id"] != f.masterID {
			t.Errorf("parent_id: want %s, got %v", f.masterID, b["parent_id"])
		}
		if b["is_tenant_root"] != false {
			t.Errorf("is_tenant_root: want false, got %v", b["is_tenant_root"])
		}
		if b["tenant_root_id"] != f.masterID {
			t.Errorf("tenant_root_id: want master %s, got %v", f.masterID, b["tenant_root_id"])
		}
	})

	t.Run("create sub rejects empty name with 400 VALIDATION", func(t *testing.T) {
		resp, b := do(t, f.c, http.MethodPost, f.base+"/businesses", f.access, map[string]any{"parent_id": f.masterID})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("want 400, got %d", resp.StatusCode)
		}
		if b["code"] != "VALIDATION" {
			t.Errorf("code: want VALIDATION, got %v", b["code"])
		}
	})

	t.Run("create sub under unknown parent is 404", func(t *testing.T) {
		resp, _ := do(t, f.c, http.MethodPost, f.base+"/businesses", f.access, map[string]any{"name": "Orphan", "parent_id": uuid.NewString()})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("want 404, got %d", resp.StatusCode)
		}
	})

	t.Run("rename is 204 and reflected in the list", func(t *testing.T) {
		id := mkSub(f.masterID, "BeforeRename")
		resp, _ := do(t, f.c, http.MethodPatch, f.base+"/businesses/"+id, f.access, map[string]any{"name": "AfterRename"})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("PATCH want 204, got %d", resp.StatusCode)
		}
		if m := f.find(t, id); m == nil || m["name"] != "AfterRename" {
			t.Errorf("rename not reflected: %v", m)
		}
	})

	t.Run("rename rejects empty name with 400 VALIDATION", func(t *testing.T) {
		id := mkSub(f.masterID, "RenameEmpty")
		resp, b := do(t, f.c, http.MethodPatch, f.base+"/businesses/"+id, f.access, map[string]any{"name": ""})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("want 400, got %d", resp.StatusCode)
		}
		if b["code"] != "VALIDATION" {
			t.Errorf("code: want VALIDATION, got %v", b["code"])
		}
	})

	t.Run("move is 204 and reparents the node", func(t *testing.T) {
		a := mkSub(f.masterID, "MoveTargetA")
		node := mkSub(f.masterID, "MoveNodeB")
		resp, _ := do(t, f.c, http.MethodPost, f.base+"/businesses/"+node+"/move", f.access, map[string]any{"new_parent_id": a})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("move want 204, got %d", resp.StatusCode)
		}
		if m := f.find(t, node); m == nil || m["parent_id"] != a {
			t.Errorf("after move parent_id: want %s, got %v", a, m["parent_id"])
		}
	})

	t.Run("move with malformed new_parent_id is 400 VALIDATION", func(t *testing.T) {
		id := mkSub(f.masterID, "MoveMalformed")
		resp, b := do(t, f.c, http.MethodPost, f.base+"/businesses/"+id+"/move", f.access, map[string]any{"new_parent_id": "not-a-uuid"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("want 400, got %d", resp.StatusCode)
		}
		if b["code"] != "VALIDATION" {
			t.Errorf("code: want VALIDATION, got %v", b["code"])
		}
	})

	t.Run("move under an unknown parent is 404", func(t *testing.T) {
		id := mkSub(f.masterID, "MoveUnknownParent")
		resp, _ := do(t, f.c, http.MethodPost, f.base+"/businesses/"+id+"/move", f.access, map[string]any{"new_parent_id": uuid.NewString()})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("want 404, got %d", resp.StatusCode)
		}
	})

	t.Run("moving the master business is refused with 409", func(t *testing.T) {
		child := mkSub(f.masterID, "MasterMoveChild")
		resp, _ := do(t, f.c, http.MethodPost, f.base+"/businesses/"+f.masterID+"/move", f.access, map[string]any{"new_parent_id": child})
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("want 409, got %d", resp.StatusCode)
		}
	})

	t.Run("archive then restore toggles status", func(t *testing.T) {
		id := mkSub(f.masterID, "ArchMe")
		if resp, _ := do(t, f.c, http.MethodPost, f.base+"/businesses/"+id+"/archive", f.access, nil); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("archive want 204, got %d", resp.StatusCode)
		}
		if m := f.find(t, id); m == nil || m["status"] != "archived" {
			t.Errorf("archive not reflected: %v", m)
		}
		if resp, _ := do(t, f.c, http.MethodPost, f.base+"/businesses/"+id+"/restore", f.access, nil); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("restore want 204, got %d", resp.StatusCode)
		}
		if m := f.find(t, id); m == nil || m["status"] != "active" {
			t.Errorf("restore not reflected: %v", m)
		}
	})

	t.Run("delete requires confirmation (400 VALIDATION)", func(t *testing.T) {
		id := mkSub(f.masterID, "DelNoConfirm")
		resp, b := do(t, f.c, http.MethodDelete, f.base+"/businesses/"+id, f.access, map[string]any{"confirm": false})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("want 400, got %d", resp.StatusCode)
		}
		if b["code"] != "VALIDATION" {
			t.Errorf("code: want VALIDATION, got %v", b["code"])
		}
	})

	t.Run("delete refuses a node with active children (409)", func(t *testing.T) {
		parent := mkSub(f.masterID, "DelParent")
		_ = mkSub(parent, "DelChild")
		resp, _ := do(t, f.c, http.MethodDelete, f.base+"/businesses/"+parent, f.access, map[string]any{"confirm": true})
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("want 409, got %d", resp.StatusCode)
		}
	})

	t.Run("delete a leaf with confirmation is 204 and removes it from the list", func(t *testing.T) {
		id := mkSub(f.masterID, "DelLeaf")
		resp, _ := do(t, f.c, http.MethodDelete, f.base+"/businesses/"+id, f.access, map[string]any{"confirm": true})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("want 204, got %d", resp.StatusCode)
		}
		if m := f.find(t, id); m != nil {
			t.Errorf("deleted leaf still listed: %v", m)
		}
	})

	t.Run("unknown id is 404 across mutation routes (no existence oracle)", func(t *testing.T) {
		ghost := uuid.NewString()
		cases := []struct {
			method, path string
			body         any
		}{
			{http.MethodPatch, "/businesses/" + ghost, map[string]any{"name": "x"}},
			{http.MethodPost, "/businesses/" + ghost + "/move", map[string]any{"new_parent_id": f.masterID}},
			{http.MethodPost, "/businesses/" + ghost + "/archive", nil},
			{http.MethodPost, "/businesses/" + ghost + "/restore", nil},
			{http.MethodDelete, "/businesses/" + ghost, map[string]any{"confirm": true}},
		}
		for _, tc := range cases {
			resp, _ := do(t, f.c, tc.method, f.base+tc.path, f.access, tc.body)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s: want 404, got %d", tc.method, tc.path, resp.StatusCode)
			}
		}
	})

	t.Run("malformed id in the path is 404", func(t *testing.T) {
		resp, _ := do(t, f.c, http.MethodPost, f.base+"/businesses/not-a-uuid/archive", f.access, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("want 404, got %d", resp.StatusCode)
		}
	})

	t.Run("mutations reject anonymous callers with 401", func(t *testing.T) {
		id := mkSub(f.masterID, "AnonTarget")
		resp, _ := do(t, f.c, http.MethodPost, f.base+"/businesses/"+id+"/archive", "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", resp.StatusCode)
		}
	})
}
