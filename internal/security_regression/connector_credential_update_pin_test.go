// Finding: manyforge-a7j.7 / Spec 004 §3.5 — connector base_url + allow_private_base_url are
// validated and the on-prem trust grant is audited ONLY at create time (internal/connectors/
// credential.go). UpdateConnector intentionally does NOT mutate them. The day base_url or the
// trust flag becomes mutable (a new UpdateConnectorCredential query, or adding them to an
// existing UPDATE), the service MUST re-run validateBaseURL on the new value AND re-audit the
// trust grant — else an update silently bypasses the SSRF/trust checks. These source-level
// pins (no build tag → run under `make test`/`make sec-test`) lock the safe state today and
// fire the moment a connector UPDATE starts touching those columns. Mirrors manyforge-deo.11.
package security_regression

import (
	"os"
	"strings"
	"testing"
)

func connectorQuerySQL(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	for _, p := range []string{"../../db/query/connector.sql", "../../db/query/connector_manage.sql"} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		sb.Write(b)
	}
	return sb.String()
}

// No connector UPDATE may assign base_url / allow_private_base_url without the service
// re-validating + re-auditing. Today none does; an assignment (SET col = ...) trips this.
func TestPin_ConnectorUpdateDoesNotMutateTrustColumns(t *testing.T) {
	sql := connectorQuerySQL(t)
	for _, frag := range []string{"base_url =", "allow_private_base_url ="} {
		if strings.Contains(sql, frag) {
			t.Errorf("connector query mutates %q — an UPDATE to base_url/trust MUST re-run validateBaseURL + re-audit the trust grant (manyforge-a7j.7)", strings.TrimSuffix(frag, " ="))
		}
	}
}

// The re-validation requirement stays documented inline for a future query author.
func TestPin_ConnectorCredentialUpdateNoteDocumented(t *testing.T) {
	sql := connectorQuerySQL(t)
	if !strings.Contains(sql, "a7j.7") || !strings.Contains(sql, "validateBaseURL") {
		t.Error("connector_manage.sql: the manyforge-a7j.7 re-validate/re-audit note must stay inline")
	}
}
