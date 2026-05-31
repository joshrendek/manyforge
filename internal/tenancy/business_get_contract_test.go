//go:build integration

package tenancy_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestGetBusinessContract pins the wire contract of GET /businesses/{id}: a 200
// for a visible business and a uniform no-oracle 404 for unknown/malformed ids.
func TestGetBusinessContract(t *testing.T) {
	f, _ := setup(t)

	t.Run("get a visible business is 200 with its fields", func(t *testing.T) {
		resp, b := do(t, f.c, http.MethodGet, f.base+"/businesses/"+f.masterID, f.access, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d (%v)", resp.StatusCode, b)
		}
		if b["id"] != f.masterID || b["is_tenant_root"] != true || b["tenant_root_id"] != f.masterID {
			t.Errorf("business body unexpected: %v", b)
		}
	})

	t.Run("unknown id is 404 (no oracle)", func(t *testing.T) {
		resp, _ := do(t, f.c, http.MethodGet, f.base+"/businesses/"+uuid.NewString(), f.access, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("want 404, got %d", resp.StatusCode)
		}
	})

	t.Run("malformed id is 404", func(t *testing.T) {
		resp, _ := do(t, f.c, http.MethodGet, f.base+"/businesses/not-a-uuid", f.access, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("want 404, got %d", resp.StatusCode)
		}
	})

	t.Run("anonymous is 401", func(t *testing.T) {
		resp, _ := do(t, f.c, http.MethodGet, f.base+"/businesses/"+f.masterID, "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", resp.StatusCode)
		}
	})
}
