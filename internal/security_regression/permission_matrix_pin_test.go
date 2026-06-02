// No build tag: this source-level pin runs in `make test` and `make sec-test` with
// no infrastructure (T063 / SC-009 / FR-016). The behavioral six-permission matrix
// lives in internal/ticketing/permissions_integration_test.go (Docker-gated). This
// pin guards the human-vs-agent UNIFORMITY guarantee that the behavioral test cannot
// cheaply express: authz.Resolve must decide a principal's permissions purely from
// its memberships/roles and NEVER branch on principal kind. Agent memberships are
// spec 003, but FR-016 freezes the rule now — a future "agents resolve differently"
// change must fail CI loudly even when Docker is unavailable.

package security_regression

import (
	"strings"
	"testing"
)

// TestPermissionResolverIsKindAgnosticPinned asserts authz.Resolve resolves by
// membership (HasOwnerRole / EffectivePermissions, keyed on principal + business id)
// and contains no reference to principal kind.
func TestPermissionResolverIsKindAgnosticPinned(t *testing.T) {
	src := mustRead(t, "../authz/resolver.go")

	// Resolution is membership-keyed: the locked Owner role short-circuits to the
	// whole catalog; everyone else is the union of their explicit grants.
	for _, fragment := range []string{
		"func Resolve(ctx context.Context, tx pgx.Tx, principalID, businessID uuid.UUID)",
		"q.HasOwnerRole(",
		"q.AllPermissionKeys(",
		"q.EffectivePermissions(",
	} {
		if !strings.Contains(src, fragment) {
			t.Errorf("uniformity pin: resolver.go missing %q — did permission resolution stop keying on membership?", fragment)
		}
	}

	// No kind branch: the resolver takes a principal id and never loads or switches
	// on principal.kind. A kind-gate would almost certainly surface the "Kind" token
	// (the dbgen field / a GetPrincipalKind query / a literal compare).
	if strings.Contains(src, "Kind") {
		t.Errorf("uniformity pin: resolver.go references \"Kind\" — permission resolution must NOT branch on principal kind (FR-016); humans and agents resolve identically by membership")
	}
}
