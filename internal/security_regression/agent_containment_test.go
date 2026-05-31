//go:build integration

package security_regression

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// agentTenant is a seeded tenant whose master "AgentCo" has a human Owner and a
// child business, plus a single AI-agent principal homed at the master. benign
// is a custom role granting only business.read; admin is a custom role granting
// members.manage (an admin-class permission). The agent holds no membership yet
// — each subtest exercises one admission attempt against membership_agent_guard.
type agentTenant struct {
	master, child uuid.UUID
	agent         uuid.UUID
	benignRole    uuid.UUID
	adminRole     uuid.UUID
}

func seedAgentTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) agentTenant {
	t.Helper()
	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("preset owner role: %v", err)
	}
	a := agentTenant{
		master: uuid.New(), child: uuid.New(), agent: uuid.New(),
		benignRole: uuid.New(), adminRole: uuid.New(),
	}
	ownerHuman, ownerAcct := uuid.New(), uuid.New()
	// Unique email per seed call so the suite can seed several tenants.
	ownerEmail := "agent-owner-" + a.master.String() + "@x.test"

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'O','active',now(),now(),now())`,
			[]any{ownerAcct, ownerEmail}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`,
			[]any{ownerHuman, ownerAcct}},
		// Master (tenant root) and a child business in the same tenant.
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'AgentCo','active',now(),now())`, []any{a.master}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`, []any{a.master}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,$2,$2,'AgentChild','active',now(),now())`, []any{a.child, a.master}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$2),($2,$1,1,$2)`, []any{a.child, a.master}},
		// The agent principal: tenant-bound to the master (FR-027).
		{`INSERT INTO principal (id,kind,home_business_id,tenant_root_id,created_at) VALUES ($1,'agent',$2,$2,now())`, []any{a.agent, a.master}},
		// The tenant must retain an Owner (deferred tenant_owner_guard fires at commit).
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`, []any{ownerHuman, a.master, ownerRole}},
		{`INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at) VALUES ($1,$2,'agent-benign','AgBenign',false,now()),($3,$2,'agent-admin','AgAdmin',false,now())`,
			[]any{a.benignRole, a.master, a.adminRole}},
		{`INSERT INTO role_permission (role_id,permission_key) VALUES ($1,'business.read'),($2,'members.manage')`, []any{a.benignRole, a.adminRole}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return a
}

func agentMembershipCount(ctx context.Context, t *testing.T, tdb *testdb.TestDB, agent uuid.UUID) int {
	t.Helper()
	var n int
	if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM membership WHERE principal_id=$1", agent).Scan(&n); err != nil {
		t.Fatalf("count agent memberships: %v", err)
	}
	return n
}

// grantAgentMembership attempts to admit the agent into business with role,
// using the RLS-exempt superuser so RLS cannot mask the membership_agent_guard
// rejection — the trigger is the foundation's admission control for agents and
// must reject regardless of who performs the write.
func grantAgentMembership(ctx context.Context, tdb *testdb.TestDB, agent, business, tenantRoot, role uuid.UUID) error {
	_, err := tdb.Super.Exec(ctx,
		`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$3,$4,now())`,
		agent, business, tenantRoot, role)
	return err
}

// TestAgentContainmentRefused proves FR-027/SC-011 at the foundation's agent
// admission boundary: membership_agent_guard (migration 0004). An AI-agent
// principal can never gain a membership outside its single home business (so it
// can never read/list/act on another business via a grant) and can never hold an
// administrative permission. A benign membership on the home business is the
// control that proves the guard does not over-reject — and that it then bounds
// the agent to exactly one membership.
func TestAgentContainmentRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	t.Run("agent cannot be a member of a non-home business", func(t *testing.T) {
		a := seedAgentTenant(ctx, t, tdb)

		// Same tenant, different business: the role's tenant matches, so only the
		// agent guard can reject — isolating the cross-business containment rule.
		err := grantAgentMembership(ctx, tdb, a.agent, a.child, a.master, a.benignRole)
		if err == nil || !strings.Contains(err.Error(), "home business") {
			t.Errorf("agent membership on a non-home business: want a 'home business' rejection, got %v", err)
		}
		if n := agentMembershipCount(ctx, t, tdb, a.agent); n != 0 {
			t.Errorf("agent must hold no membership after a refused cross-business grant, got %d", n)
		}
	})

	t.Run("agent cannot hold any administrative permission", func(t *testing.T) {
		a := seedAgentTenant(ctx, t, tdb)

		// The full admin-class denylist enforced by the trigger (FR-027). Each is
		// tried on the home business so only the admin-permission rule can reject.
		adminPerms := []string{"members.manage", "roles.manage", "hierarchy.manage", "business.delete", "ownership.transfer"}
		for _, perm := range adminPerms {
			role := uuid.New()
			if _, err := tdb.Super.Exec(ctx,
				`INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at) VALUES ($1,$2,$3,$3,false,now())`,
				role, a.master, "agent-perm-"+perm); err != nil {
				t.Fatalf("seed admin role for %q: %v", perm, err)
			}
			if _, err := tdb.Super.Exec(ctx, `INSERT INTO role_permission (role_id,permission_key) VALUES ($1,$2)`, role, perm); err != nil {
				t.Fatalf("seed perm %q: %v", perm, err)
			}
			err := grantAgentMembership(ctx, tdb, a.agent, a.master, a.master, role)
			if err == nil || !strings.Contains(err.Error(), "administrative permissions") {
				t.Errorf("agent membership with %q: want an 'administrative permissions' rejection, got %v", perm, err)
			}
		}
		if n := agentMembershipCount(ctx, t, tdb, a.agent); n != 0 {
			t.Errorf("agent must hold no membership after refused admin grants, got %d", n)
		}
	})

	t.Run("agent holds exactly one benign membership on its home business", func(t *testing.T) {
		a := seedAgentTenant(ctx, t, tdb)

		// Control: a benign role on the home business is admitted.
		if err := grantAgentMembership(ctx, tdb, a.agent, a.master, a.master, a.benignRole); err != nil {
			t.Fatalf("control: benign membership on home business must be admitted, got %v", err)
		}
		// A second membership (even within the tenant) is refused — the agent is
		// bounded to one, on the home business only.
		err := grantAgentMembership(ctx, tdb, a.agent, a.child, a.master, a.benignRole)
		if err == nil || !strings.Contains(err.Error(), "home business") {
			t.Errorf("agent second membership: want a 'home business' rejection, got %v", err)
		}
		if n := agentMembershipCount(ctx, t, tdb, a.agent); n != 1 {
			t.Errorf("agent must hold exactly one membership, got %d", n)
		}
	})
}
