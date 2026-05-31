//go:build integration

package tenancy_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestUS5_TransferOwnershipContract pins the HTTP wire contract of
// POST /businesses/{id}/transfer-ownership: the status codes and error envelopes
// the handler is responsible for. Service-level behaviour (atomic swap, audit) is
// covered by TestTransferOwnership; this asserts the handler/router mapping. The
// non-mutating error cases run first (the actor stays Owner throughout); the real
// 204 transfer runs last because it demotes the caller.
func TestUS5_TransferOwnershipContract(t *testing.T) {
	f, tdb := setup(t)
	ctx := context.Background()
	master := uuid.MustParse(f.masterID)
	adminRole := presetRole(ctx, t, tdb, "admin")
	bob := seedMemberAt(ctx, t, tdb, master, master, adminRole, "co-bob@x.test")

	var ownerP uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT p.id FROM principal p JOIN account a ON a.id=p.account_id WHERE a.email='owner@x.test'").Scan(&ownerP); err != nil {
		t.Fatalf("resolve owner principal: %v", err)
	}
	xfer := f.base + "/businesses/" + f.masterID + "/transfer-ownership"

	t.Run("anonymous caller is 401", func(t *testing.T) {
		resp, _ := do(t, f.c, http.MethodPost, xfer, "", map[string]any{"to_principal_id": bob.String()})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", resp.StatusCode)
		}
	})

	t.Run("malformed to_principal_id is 400 VALIDATION", func(t *testing.T) {
		resp, b := do(t, f.c, http.MethodPost, xfer, f.access, map[string]any{"to_principal_id": "not-a-uuid"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("want 400, got %d", resp.StatusCode)
		}
		if b["code"] != "VALIDATION" {
			t.Errorf("code: want VALIDATION, got %v", b["code"])
		}
	})

	t.Run("missing to_principal_id is 400 VALIDATION", func(t *testing.T) {
		resp, b := do(t, f.c, http.MethodPost, xfer, f.access, map[string]any{})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("want 400, got %d", resp.StatusCode)
		}
		if b["code"] != "VALIDATION" {
			t.Errorf("code: want VALIDATION, got %v", b["code"])
		}
	})

	t.Run("unknown business is 404 (no oracle)", func(t *testing.T) {
		resp, _ := do(t, f.c, http.MethodPost, f.base+"/businesses/"+uuid.NewString()+"/transfer-ownership", f.access, map[string]any{"to_principal_id": bob.String()})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("want 404, got %d", resp.StatusCode)
		}
	})

	t.Run("malformed business id in path is 404", func(t *testing.T) {
		resp, _ := do(t, f.c, http.MethodPost, f.base+"/businesses/not-a-uuid/transfer-ownership", f.access, map[string]any{"to_principal_id": bob.String()})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("want 404, got %d", resp.StatusCode)
		}
	})

	t.Run("transfer to a non-member is 409", func(t *testing.T) {
		resp, _ := do(t, f.c, http.MethodPost, xfer, f.access, map[string]any{"to_principal_id": uuid.NewString()})
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("want 409, got %d", resp.StatusCode)
		}
	})

	t.Run("transfer to self is 409", func(t *testing.T) {
		resp, _ := do(t, f.c, http.MethodPost, xfer, f.access, map[string]any{"to_principal_id": ownerP.String()})
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("want 409, got %d", resp.StatusCode)
		}
	})

	t.Run("transfer at a sub-business (not the tenant root) is 409", func(t *testing.T) {
		_, sb := do(t, f.c, http.MethodPost, f.base+"/businesses", f.access, map[string]any{"name": "Sub", "parent_id": f.masterID})
		subID, _ := sb["id"].(string)
		if subID == "" {
			t.Fatalf("create sub failed: %v", sb)
		}
		resp, _ := do(t, f.c, http.MethodPost, f.base+"/businesses/"+subID+"/transfer-ownership", f.access, map[string]any{"to_principal_id": bob.String()})
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("want 409, got %d", resp.StatusCode)
		}
	})

	// Mutating happy path LAST: the transfer succeeds and demotes the caller to
	// Admin, so a repeat attempt (to a different member, to avoid the self-transfer
	// guard) is then refused with the no-oracle 404 — the caller is no longer an
	// Owner — proving the swap took effect over the wire.
	t.Run("owner transfers to a member is 204, then the demoted caller gets 404", func(t *testing.T) {
		resp, _ := do(t, f.c, http.MethodPost, xfer, f.access, map[string]any{"to_principal_id": bob.String()})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("transfer: want 204, got %d", resp.StatusCode)
		}
		resp2, _ := do(t, f.c, http.MethodPost, xfer, f.access, map[string]any{"to_principal_id": bob.String()})
		if resp2.StatusCode != http.StatusNotFound {
			t.Fatalf("demoted caller repeat transfer: want 404, got %d", resp2.StatusCode)
		}
	})
}

// TestUS5_AuditReadContract pins the HTTP wire contract of
// GET /businesses/{id}/audit: a 200 metadata-only page, keyset pagination, and
// the no-oracle 404. Service-level gating/ordering is covered by TestAuditRead.
func TestUS5_AuditReadContract(t *testing.T) {
	f, tdb := setup(t)
	ctx := context.Background()
	master := uuid.MustParse(f.masterID)
	memberRoleID := presetRole(ctx, t, tdb, "member")
	adminRole := presetRole(ctx, t, tdb, "admin")
	bob := seedMemberAt(ctx, t, tdb, master, master, memberRoleID, "au-co-bob@x.test")

	// A second master-scoped, audited mutation over the wire (business.created at
	// setup is the first), so pagination has at least two entries to walk.
	if resp, _ := do(t, f.c, http.MethodPatch, f.base+"/businesses/"+f.masterID+"/members/"+bob.String(), f.access, map[string]any{"role_id": adminRole.String()}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("seed mutation (role change): want 204, got %d", resp.StatusCode)
	}
	auditURL := f.base + "/businesses/" + f.masterID + "/audit"

	t.Run("200 returns a metadata-only page", func(t *testing.T) {
		resp, b := do(t, f.c, http.MethodGet, auditURL, f.access, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d", resp.StatusCode)
		}
		items, _ := b["items"].([]any)
		if len(items) < 1 {
			t.Fatalf("want >=1 audit entry, got %v", b)
		}
		first, _ := items[0].(map[string]any)
		if first["action"] == nil || first["id"] == nil || first["created_at"] == nil {
			t.Errorf("entry missing required metadata fields: %v", first)
		}
		// Audit read is metadata-only: the value payloads must not be exposed.
		if _, ok := first["new_value"]; ok {
			t.Errorf("audit entry must not expose new_value: %v", first)
		}
		if _, ok := first["old_value"]; ok {
			t.Errorf("audit entry must not expose old_value: %v", first)
		}
	})

	t.Run("pagination: limit=1 yields a next_cursor and walks without overlap", func(t *testing.T) {
		resp, b := do(t, f.c, http.MethodGet, auditURL+"?limit=1", f.access, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("page 1: want 200, got %d", resp.StatusCode)
		}
		items, _ := b["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("page 1: want exactly 1 entry, got %d", len(items))
		}
		cursor, _ := b["next_cursor"].(string)
		if cursor == "" {
			t.Fatalf("page 1: want a next_cursor, got %v", b["next_cursor"])
		}
		firstID, _ := items[0].(map[string]any)["id"].(string)

		resp2, b2 := do(t, f.c, http.MethodGet, auditURL+"?limit=1&cursor="+cursor, f.access, nil)
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("page 2: want 200, got %d", resp2.StatusCode)
		}
		items2, _ := b2["items"].([]any)
		if len(items2) != 1 {
			t.Fatalf("page 2: want exactly 1 entry, got %d", len(items2))
		}
		if secondID, _ := items2[0].(map[string]any)["id"].(string); secondID == firstID {
			t.Errorf("pagination overlap: page 2 repeated page 1's entry %s", firstID)
		}
	})

	t.Run("unknown business is 404", func(t *testing.T) {
		resp, _ := do(t, f.c, http.MethodGet, f.base+"/businesses/"+uuid.NewString()+"/audit", f.access, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("want 404, got %d", resp.StatusCode)
		}
	})
}
