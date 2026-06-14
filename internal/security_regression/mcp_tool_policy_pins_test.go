// Source-level pins for manyforge-k0d (MCP per-tool reclassification). No build tag → run in
// `make test` + `make sec-test`. They make a refactor that drops a k0d guardrail fail loudly.
package security_regression

import (
	"strings"
	"testing"
)

// TestPin_ToolPolicyPromotionsOnly pins the schema guardrail: effect IN (0,1) means a policy can
// ONLY promote to Read/Reversible — External(2)/Irreversible(3) are structurally unstorable, so
// an admin can never fabricate a more-permissive-than-intended class. External = absence of a row.
func TestPin_ToolPolicyPromotionsOnly(t *testing.T) {
	mig := mustRead(t, "../../migrations/0053_mcp_tool_policy.up.sql")
	if !strings.Contains(mig, "CHECK (effect IN (0, 1))") {
		t.Error("0053: mcp_tool_policy.effect must be CHECK (effect IN (0, 1)) — promotions only, no External/Irreversible")
	}
}

// TestPin_ToolPolicyTenantScopedAndCascade pins RLS isolation + the FK cascade lifecycle.
func TestPin_ToolPolicyTenantScopedAndCascade(t *testing.T) {
	mig := mustRead(t, "../../migrations/0053_mcp_tool_policy.up.sql")
	for _, frag := range []string{
		"ENABLE ROW LEVEL SECURITY",
		"authorized_businesses(current_principal())",
		"REFERENCES mcp_server (id, tenant_root_id) ON DELETE CASCADE",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0053: missing fragment %q (tenant isolation / cascade lifecycle)", frag)
		}
	}
}

// TestPin_ToolPolicyDiscoveryDefaultsClosed pins that discovery still defaults to External and
// only promotes from the policy map (the override is layered, not a replacement of the default).
func TestPin_ToolPolicyDiscoveryDefaultsClosed(t *testing.T) {
	host := mustRead(t, "../agents/mcp_host.go")
	for _, frag := range []string{
		"effect := EffectExternal",
		"policyMap[capturedDef.Name]",
	} {
		if !strings.Contains(host, frag) {
			t.Errorf("mcp_host.go: missing fragment %q — discovery must default External and override from the policy map", frag)
		}
	}
}
