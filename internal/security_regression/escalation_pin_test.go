// No build tag: this source-level guard runs in both `make test` and
// `make sec-test`, with no infrastructure. It pins the escalation guards in
// place so a refactor that silently drops one fails CI even if a behavioral
// test is also weakened or removed (CLAUDE.md: source-level pins for security).

package security_regression

import (
	"os"
	"strings"
	"testing"
)

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestEscalationGuardsPinned asserts the FR-023 guard at each authority-conferring
// site still exists in source. Paths are relative to this package directory.
func TestEscalationGuardsPinned(t *testing.T) {
	cases := []struct {
		name, path, fragment string
	}{
		// assign (T063): the locked Owner role is reserved to Owners...
		{"assign: owner reserved", "../tenancy/members.go", "assigning the Owner role is reserved to Owners"},
		// ...and any other role requires the actor to already hold every grant.
		{"assign: no-escalation subset", "../tenancy/members.go", "a permission you do not hold"},
		// edit (T059): a custom role cannot gain a permission the editor lacks.
		{"edit: grantable check", "../authz/role.go", "cannot grant a permission you do not hold"},
		// invite (T060): the inviter cannot confer a role above its own.
		{"invite: above own", "../invitations/service.go", "cannot invite with a role above your own"},
		// accept (T062): acceptance materialises the role STORED ON THE INVITATION
		// (bounded at create time) — there is no caller-supplied role at accept,
		// so acceptance cannot escalate.
		{"accept: role bound to invitation", "../../migrations/0010_accept_invitation.up.sql", "inv.role_id"},
		{"accept: no caller-supplied role", "../invitations/service.go", "accept_invitation($1, $2, $3::citext)"},
	}
	for _, c := range cases {
		if !strings.Contains(mustRead(t, c.path), c.fragment) {
			t.Errorf("%s: escalation guard fragment %q missing from %s — was the guard removed or refactored?", c.name, c.fragment, c.path)
		}
	}
}
