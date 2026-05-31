// No build tag: this source-level guard runs in both `make test` and
// `make sec-test`, with no infrastructure. It pins the agent-containment guard
// (FR-027/SC-011) in place so a refactor that silently drops or weakens it fails
// CI even if the behavioral test is also weakened or removed (CLAUDE.md:
// source-level pins for security).

package security_regression

import (
	"strings"
	"testing"
)

// TestAgentContainmentGuardPinned asserts the membership_agent_guard trigger and
// each of its three containment rules still exist in migration 0004, including
// the full admin-class permission denylist. Paths are relative to this package.
func TestAgentContainmentGuardPinned(t *testing.T) {
	src := mustRead(t, "../../migrations/0004_membership.up.sql")
	cases := []struct {
		name, fragment string
	}{
		{"guard function present", "CREATE FUNCTION membership_agent_guard()"},
		{"guard trigger wired", "CREATE TRIGGER membership_agent_trg BEFORE INSERT OR UPDATE ON membership"},
		{"only home business", "agent principal may only be a member of its home business"},
		{"only one membership", "agent principal may hold only one membership"},
		{"no admin permissions", "agent principal may not hold administrative permissions"},
		// The admin-class denylist must stay complete — dropping any key would let
		// an agent hold that administrative permission (SC-011).
		{"denylist: members.manage", "'members.manage'"},
		{"denylist: roles.manage", "'roles.manage'"},
		{"denylist: hierarchy.manage", "'hierarchy.manage'"},
		{"denylist: business.delete", "'business.delete'"},
		{"denylist: ownership.transfer", "'ownership.transfer'"},
	}
	for _, c := range cases {
		if !strings.Contains(src, c.fragment) {
			t.Errorf("%s: agent-containment fragment %q missing from migration 0004 — was the guard removed or refactored?", c.name, c.fragment)
		}
	}
}
