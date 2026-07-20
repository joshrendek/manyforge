// Finding: manyforge-deo.11 (sibling) — the scoped config-edit query UpdateAICredentialConfig
// must touch ONLY config columns (default_model, max_concurrent_lanes) and must NEVER reference
// the SSRF trust flag or the sealed key, so a config edit can't reopen the trust surface the
// deo.11 note protects. Source-level pin (no build tag → runs under make test / make sec-test).
package security_regression

import (
	"strings"
	"testing"
)

// aiQuerySlice returns the text of the named query (from its `-- name: X` marker to the next
// `-- name:` marker or EOF), so per-query assertions don't false-match strings elsewhere in the file.
func aiQuerySlice(t *testing.T, sql, name string) string {
	t.Helper()
	marker := "-- name: " + name
	i := strings.Index(sql, marker)
	if i < 0 {
		return ""
	}
	rest := sql[i+len(marker):]
	if j := strings.Index(rest, "-- name:"); j >= 0 {
		return rest[:j]
	}
	return rest
}

func TestPin_UpdateAICredentialConfigIsConfigScoped(t *testing.T) {
	sql := mustReadAIQueries(t) // shared helper in ai_credential_update_pin_test.go
	q := aiQuerySlice(t, sql, "UpdateAICredentialConfig")
	if q == "" {
		t.Skip("no UpdateAICredentialConfig query yet — tripwire armed")
	}
	for _, forbidden := range []string{"allow_private_base_url", "base_url", "sealed_key_ref", "oauth_refresh_token"} {
		if strings.Contains(q, forbidden) {
			t.Errorf("UpdateAICredentialConfig must NOT touch %q — config-only, no trust/secret surface (deo.11)", forbidden)
		}
	}
	// It must be scoped to (id, business_id) — the ownership predicate.
	if !strings.Contains(q, "business_id") {
		t.Error("UpdateAICredentialConfig must be scoped by business_id (ownership predicate)")
	}
}
