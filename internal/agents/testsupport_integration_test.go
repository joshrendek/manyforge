//go:build integration

package agents

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// agentSeed holds the IDs needed by integration tests: a business (tenant root)
// and an agent principal that holds a benign membership on that business, making
// authorized_businesses(principalID) return businessID so RLS passes.
type agentSeed struct {
	businessID  uuid.UUID
	principalID uuid.UUID
}

// seedAgentTenant inserts, via the RLS-exempt Super pool, a minimal tenant with:
//   - a master business (also its own tenant root),
//   - a human account+principal that holds the system Owner role (satisfying the
//     deferred tenant_owner_guard trigger),
//   - an agent principal homed at the master business,
//   - a non-admin role (business.read only), and
//   - a membership row binding the agent to its home business with that role.
//
// After commit, authorized_businesses(principalID) returns {businessID}, so
// db.WithPrincipal(ctx, principalID, …) passes the RLS policy on
// ai_provider_credential. Pattern mirrors internal/security_regression/
// agent_containment_test.go::seedAgentTenant.
func seedAgentTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) agentSeed {
	t.Helper()

	// Look up the system Owner role (preset, tenant_root_id IS NULL).
	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("preset owner role: %v", err)
	}

	masterID := uuid.New()
	agentID := uuid.New()
	benignRoleID := uuid.New()
	ownerAcctID := uuid.New()
	ownerHumanID := uuid.New()
	// Unique email per seed call so parallel seeds don't collide.
	ownerEmail := "cred-owner-" + masterID.String() + "@x.test"

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		// Human account + principal — required to satisfy tenant_owner_guard.
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Owner','active',now(),now(),now())`,
			[]any{ownerAcctID, ownerEmail}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`,
			[]any{ownerHumanID, ownerAcctID}},

		// Master business (tenant root = self).
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'AgentCredCo','active',now(),now())`,
			[]any{masterID}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`,
			[]any{masterID}},

		// Agent principal: kind='agent', homed at the master business.
		{`INSERT INTO principal (id,kind,home_business_id,tenant_root_id,created_at) VALUES ($1,'agent',$2,$2,now())`,
			[]any{agentID, masterID}},

		// The tenant must retain an Owner (deferred tenant_owner_guard fires at commit).
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{ownerHumanID, masterID, ownerRole}},

		// A non-admin custom role (business.read only) so the agent can hold a
		// membership without triggering membership_agent_guard's admin denylist.
		{`INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at) VALUES ($1,$2,'agent-cred-read','AgentCredRead',false,now())`,
			[]any{benignRoleID, masterID}},
		{`INSERT INTO role_permission (role_id,permission_key) VALUES ($1,'business.read')`,
			[]any{benignRoleID}},

		// Membership: agent on its home business with the benign role. This makes
		// authorized_businesses(agentID) return {masterID}, satisfying RLS.
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{agentID, masterID, benignRoleID}},
	}

	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return agentSeed{businessID: masterID, principalID: agentID}
}
