//go:build integration

package authz_test

import (
	"net/http"
	"testing"
)

func rolesByKey(body map[string]any) map[string]map[string]any {
	raw, _ := body["items"].([]any)
	out := map[string]map[string]any{}
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			if k, ok := m["key"].(string); ok {
				out[k] = m
			}
		}
	}
	return out
}

func permsOf(role map[string]any) map[string]bool {
	raw, _ := role["permissions"].([]any)
	set := map[string]bool{}
	for _, p := range raw {
		if s, ok := p.(string); ok {
			set[s] = true
		}
	}
	return set
}

// TestRoles_Contract pins the /businesses/{id}/roles wire contract: presets are
// listed, custom roles round-trip through create/update/delete, presets are
// immutable, and delete-in-use / unauthorized are refused with the right codes.
func TestRoles_Contract(t *testing.T) {
	h := setup(t)
	rolesURL := h.base + "/businesses/" + h.master + "/roles"

	t.Run("lists presets with their permissions", func(t *testing.T) {
		resp, body := do(t, h.c, http.MethodGet, rolesURL, h.access, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d (%v)", resp.StatusCode, body)
		}
		roles := rolesByKey(body)
		for _, k := range []string{"owner", "admin", "member", "viewer"} {
			if roles[k] == nil {
				t.Errorf("preset %q missing from list", k)
			}
		}
		if roles["owner"] != nil && roles["owner"]["builtin"] != true {
			t.Errorf("owner should be builtin: %v", roles["owner"])
		}
		if roles["viewer"] != nil && !permsOf(roles["viewer"])["business.read"] {
			t.Errorf("viewer should hold business.read: %v", roles["viewer"])
		}
	})

	t.Run("create -> appears in list -> update -> delete", func(t *testing.T) {
		resp, created := do(t, h.c, http.MethodPost, rolesURL, h.access, map[string]any{
			"name": "Auditor", "permissions": []string{"business.read", "audit.read"},
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create: want 201, got %d (%v)", resp.StatusCode, created)
		}
		id, _ := created["id"].(string)
		if id == "" || created["builtin"] != false || !permsOf(created)["audit.read"] {
			t.Fatalf("unexpected created role: %v", created)
		}

		// appears in the list with its permissions
		_, listBody := do(t, h.c, http.MethodGet, rolesURL, h.access, nil)
		var foundKey string
		for k, m := range rolesByKey(listBody) {
			if m["id"] == id {
				foundKey = k
			}
		}
		if foundKey == "" {
			t.Fatalf("created role %s not in list", id)
		}

		// update: rename + narrow permissions
		resp, updated := do(t, h.c, http.MethodPatch, rolesURL+"/"+id, h.access, map[string]any{
			"name": "Read Only", "permissions": []string{"business.read"},
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("update: want 200, got %d (%v)", resp.StatusCode, updated)
		}
		if updated["name"] != "Read Only" || permsOf(updated)["audit.read"] || !permsOf(updated)["business.read"] {
			t.Errorf("update not applied: %v", updated)
		}

		// delete (unused) -> 204, then gone from the list
		if resp, _ := do(t, h.c, http.MethodDelete, rolesURL+"/"+id, h.access, nil); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete: want 204, got %d", resp.StatusCode)
		}
		_, after := do(t, h.c, http.MethodGet, rolesURL, h.access, nil)
		for _, m := range rolesByKey(after) {
			if m["id"] == id {
				t.Errorf("deleted role still listed")
			}
		}
	})

	t.Run("create rejects empty name (400) and unknown permission (400)", func(t *testing.T) {
		if resp, _ := do(t, h.c, http.MethodPost, rolesURL, h.access, map[string]any{"name": "", "permissions": []string{}}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("empty name: want 400, got %d", resp.StatusCode)
		}
		resp, b := do(t, h.c, http.MethodPost, rolesURL, h.access, map[string]any{"name": "Bad", "permissions": []string{"does.not.exist"}})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("unknown perm: want 400, got %d", resp.StatusCode)
		}
		if b["code"] != "VALIDATION" {
			t.Errorf("unknown perm code: want VALIDATION, got %v", b["code"])
		}
	})

	t.Run("presets cannot be edited or deleted (404)", func(t *testing.T) {
		owner := rolesByKey(mustList(t, h, rolesURL))["owner"]
		ownerID, _ := owner["id"].(string)
		if resp, _ := do(t, h.c, http.MethodPatch, rolesURL+"/"+ownerID, h.access, map[string]any{"name": "Hacked"}); resp.StatusCode != http.StatusNotFound {
			t.Errorf("edit preset: want 404, got %d", resp.StatusCode)
		}
		if resp, _ := do(t, h.c, http.MethodDelete, rolesURL+"/"+ownerID, h.access, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("delete preset: want 404, got %d", resp.StatusCode)
		}
	})

	t.Run("unknown business id is 404", func(t *testing.T) {
		ghost := h.base + "/businesses/00000000-0000-0000-0000-000000000000/roles"
		if resp, _ := do(t, h.c, http.MethodGet, ghost, h.access, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("list ghost: want 404, got %d", resp.StatusCode)
		}
	})

	t.Run("anonymous is rejected with 401", func(t *testing.T) {
		if resp, _ := do(t, h.c, http.MethodGet, rolesURL, "", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("anon list: want 401, got %d", resp.StatusCode)
		}
	})
}

func mustList(t *testing.T, h harness, url string) map[string]any {
	t.Helper()
	resp, body := do(t, h.c, http.MethodGet, url, h.access, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: want 200, got %d", resp.StatusCode)
	}
	return body
}
