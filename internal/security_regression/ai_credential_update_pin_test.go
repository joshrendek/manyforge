// Finding: manyforge-deo.11 / Spec 003 §3.5 — the per-credential allow_private_base_url
// SSRF trust flag is set at create time (InsertAIProviderCredential). db/query/ai.sql has
// NO update query today; the day one is added it MUST carry allow_private_base_url (and the
// service must re-validate via validateBaseURL + re-audit the trust grant) or an update
// built from a partial body silently zeros the flag — demoting a trusted self-host
// credential to the locked-down dialer, or leaving a stale trust the operator believes was
// revoked. These source-level pins (no build tag → run under `make test`/`make sec-test`)
// lock the create invariant today and arm a tripwire for the future update query.
package security_regression

import (
	"os"
	"strings"
	"testing"
)

func mustReadAIQueries(t *testing.T) string {
	t.Helper()
	const path = "../../db/query/ai.sql"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// The create query persists the trust flag — the invariant any future update must mirror.
func TestPin_InsertAICredentialCarriesTrustFlag(t *testing.T) {
	sql := mustReadAIQueries(t)
	if !strings.Contains(sql, "name: InsertAIProviderCredential") {
		t.Fatal("ai.sql: InsertAIProviderCredential query is missing")
	}
	if !strings.Contains(sql, "allow_private_base_url") {
		t.Error("ai.sql: InsertAIProviderCredential must persist allow_private_base_url (the SSRF trust flag)")
	}
}

// Tripwire: when an UpdateAIProviderCredential query is added it MUST reference
// allow_private_base_url so the trust flag is carried, not silently zeroed. Skips today
// (no update query) by design — it fires the moment the query lands without the flag.
func TestPin_UpdateAICredentialCarriesTrustFlag(t *testing.T) {
	sql := mustReadAIQueries(t)
	if !strings.Contains(sql, "name: UpdateAIProviderCredential") {
		t.Skip("no UpdateAIProviderCredential query yet (manyforge-deo.11) — tripwire armed")
	}
	if !strings.Contains(sql, "allow_private_base_url") {
		t.Error("ai.sql: UpdateAIProviderCredential MUST carry allow_private_base_url, else the update zeros the SSRF trust flag (manyforge-deo.11)")
	}
}

// The requirement stays documented inline so a future query author sees it in context.
func TestPin_AICredentialUpdateNoteDocumented(t *testing.T) {
	sql := mustReadAIQueries(t)
	if !strings.Contains(sql, "deo.11") || !strings.Contains(sql, "allow_private_base_url") {
		t.Error("ai.sql: the manyforge-deo.11 allow_private_base_url update requirement note must stay inline")
	}
}
