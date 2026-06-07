// us3_jira_factory_source_pin (Spec 004 US3 §7, manyforge-a7j.3): source-level pin
// that the Jira factory constructs its outbound HTTP client through the SSRF-guarded
// netsafe package, and never falls back to a bare http.DefaultClient.
//
// This file has NO build tag (a pure string match needs no infrastructure), so it
// runs under `make test` AND `make sec-test` for fast feedback — matching the other
// source-level pins in this suite (e.g. webhook_sig_test.go). The behavioural SSRF
// refusal pins live in the integration-tagged us3_jira_inbound_pin_test.go.
package security_regression

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUS3_JiraFactoryUsesNetsafe verifies the Jira factory builds its HTTP client via
// netsafe.NewClientWithOptions. A future refactor that swaps to a bare client would
// fail CI here even if the behavioural SSRF test somehow passed.
func TestUS3_JiraFactoryUsesNetsafe(t *testing.T) {
	// MF-004-SSRF (source pin) — Spec 004 §7
	path := filepath.Join("..", "connectors", "jira", "factory.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("MF-004-SSRF SOURCE PIN: read %s: %v", path, err)
	}
	src := string(raw)
	if !strings.Contains(src, "netsafe.NewClientWithOptions") {
		t.Fatalf("MF-004-SSRF SOURCE PIN: jira/factory.go no longer calls netsafe.NewClientWithOptions — SSRF guard dropped")
	}
	if strings.Contains(src, "http.DefaultClient") {
		t.Fatalf("MF-004-SSRF SOURCE PIN: jira/factory.go references http.DefaultClient — prod path must use netsafe only")
	}
}
