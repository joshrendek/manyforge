package netsafe

import "testing"

// HostAllowlist is the single source of truth shared by the CONNECT egress proxy
// (enforcement, cmd/mf-egress-proxy) and the code-review service (pre-flight
// validation, internal/agents/coding). These tests pin its matching semantics so
// the validator and the enforcer cannot drift (manyforge-0qj).

func TestParseHostAllowlistTrimsAndDropsEmpties(t *testing.T) {
	a := ParseHostAllowlist(" api.anthropic.com , ,openrouter.ai,")
	if !a.Allows("api.anthropic.com") {
		t.Error("whitespace-padded host should be allowed after trim")
	}
	if !a.Allows("openrouter.ai") {
		t.Error("openrouter.ai should be allowed")
	}
	if a.Allows("") {
		t.Error("empty host must never be allowed (empties dropped from CSV)")
	}
	if len(a) != 2 {
		t.Errorf("want 2 entries after dropping empties, got %d (%v)", len(a), a)
	}
}

func TestHostAllowlistMatchesBareAndHostPort(t *testing.T) {
	a := ParseHostAllowlist("api.anthropic.com")
	// CONNECT requests arrive as host:port; the matcher must compare the bare host.
	if !a.Allows("api.anthropic.com:443") {
		t.Error("host:port form should match an allowlisted bare host")
	}
	if !a.Allows("api.anthropic.com") {
		t.Error("bare host should match")
	}
}

func TestHostAllowlistDeniesUnlistedAndEmpty(t *testing.T) {
	a := ParseHostAllowlist("api.anthropic.com")
	if a.Allows("evil.example.com") {
		t.Error("unlisted host must be denied")
	}
	if a.Allows("evil.example.com:443") {
		t.Error("unlisted host:port must be denied")
	}

	// A nil/empty allowlist denies everything (fail-closed).
	var empty HostAllowlist
	if empty.Allows("api.anthropic.com") {
		t.Error("nil allowlist must deny all (fail-closed)")
	}
	if ParseHostAllowlist("").Allows("api.anthropic.com") {
		t.Error("empty-CSV allowlist must deny all (fail-closed)")
	}
}
