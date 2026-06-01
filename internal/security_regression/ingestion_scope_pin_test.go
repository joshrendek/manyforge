// Finding MF-002-INGEST-SCOPE (doc_support.go) — source-level pin. No build tag:
// runs under both `make test` and `make sec-test` with no infrastructure. Pins the
// single-business re-verification inside ingest_inbound_message: the function
// re-asserts the recipient resolves to the asserted (business, tenant_root) and
// RAISEs 'ingest scope violation' otherwise, so a resolution bug upstream can
// never widen a write beyond the one resolved business (FR-017).

package security_regression

import (
	"strings"
	"testing"
)

func TestIngestScopeReassertionPinned(t *testing.T) {
	src := mustRead(t, "../../migrations/0014_support_rls.up.sql")
	for _, fragment := range []string{
		// the literal exception the DEFINER function raises on a scope mismatch
		"ingest scope violation",
		// the re-verification lookup against inbound_address inside the function
		"PERFORM 1 FROM inbound_address",
	} {
		if !strings.Contains(src, fragment) {
			t.Errorf("MF-002-INGEST-SCOPE: pin %q missing from 0014_support_rls.up.sql — was the single-business re-assertion dropped or widened?", fragment)
		}
	}
}
