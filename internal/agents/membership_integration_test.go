//go:build integration

package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// TestCreateAgentBindsMembership proves Task 6 (US3): AgentService.Create now binds
// the new agent principal's acting membership (home business + agent_runtime preset
// role) atomically, so authorized_businesses(agentPrincipalID) is non-empty and the
// agent can pass RLS/RBAC to act on its home business.
func TestCreateAgentBindsMembership(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	seed := seedAgentTenant(ctx, t, tdb)
	svc := &AgentService{DB: tdb.App}

	// (2) Create an agent → capture its acting principal.
	created, err := svc.Create(ctx, seed.principalID, seed.businessID, CreateAgentInput{
		Name: "Runtime Bot", Provider: "anthropic", Model: "claude-sonnet-4-5",
		SystemPrompt: "Act.", AllowedTools: []string{"get_ticket"},
		AutonomyMode: 1, Enabled: true, MonthlyBudgetCents: 1000,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.PrincipalID == uuid.Nil {
		t.Fatalf("Create returned nil principal id: %+v", created)
	}

	// (3) Assert a membership row exists for the agent principal carrying the
	// agent_runtime preset role, on the agent's home business.
	var roleKey string
	var membBusiness uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		`SELECT r.key, m.business_id
		   FROM membership m JOIN role r ON r.id = m.role_id
		  WHERE m.principal_id = $1`, created.PrincipalID,
	).Scan(&roleKey, &membBusiness); err != nil {
		t.Fatalf("read agent membership: %v", err)
	}
	if roleKey != "agent_runtime" {
		t.Fatalf("agent membership role = %q, want agent_runtime", roleKey)
	}
	if membBusiness != seed.businessID {
		t.Fatalf("agent membership business = %v, want home business %v", membBusiness, seed.businessID)
	}

	// The agent must hold exactly ONE membership (membership_agent_guard invariant).
	var membCount int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM membership WHERE principal_id = $1`, created.PrincipalID,
	).Scan(&membCount); err != nil {
		t.Fatalf("count agent memberships: %v", err)
	}
	if membCount != 1 {
		t.Fatalf("agent holds %d memberships, want exactly 1", membCount)
	}

	// The agent_runtime role must carry NONE of the 5 forbidden admin perms.
	var adminPerms int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM role_permission rp
		   JOIN role r ON r.id = rp.role_id
		  WHERE r.tenant_root_id IS NULL AND r.key = 'agent_runtime'
		    AND rp.permission_key IN ('members.manage','roles.manage','hierarchy.manage','business.delete','ownership.transfer')`,
	).Scan(&adminPerms); err != nil {
		t.Fatalf("count agent_runtime admin perms: %v", err)
	}
	if adminPerms != 0 {
		t.Fatalf("agent_runtime role carries %d forbidden admin perms, want 0", adminPerms)
	}

	// (4) The real proof: under the agent's own principal context,
	// authorized_businesses(agentPrincipalID) returns its home business — i.e. the
	// agent can now pass RLS for its home business.
	var authorized []uuid.UUID
	if err := tdb.App.WithPrincipal(ctx, created.PrincipalID, func(tx pgx.Tx) error {
		rows, qerr := tx.Query(ctx,
			`SELECT business_id FROM authorized_businesses($1)`, created.PrincipalID)
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var b uuid.UUID
			if serr := rows.Scan(&b); serr != nil {
				return serr
			}
			authorized = append(authorized, b)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("authorized_businesses under agent principal: %v", err)
	}
	if len(authorized) != 1 || authorized[0] != seed.businessID {
		t.Fatalf("authorized_businesses(agent) = %v, want exactly [%v]", authorized, seed.businessID)
	}
}

// TestMembershipAgentGuardForbidsApprove proves the US4 defense-in-depth hardening
// (migration 0033): the membership_agent_guard DB trigger — not merely preset-role
// omission — rejects binding an agent principal to ANY role granting agents.approve.
// An admin minting a CUSTOM tenant role with agents.approve and binding an agent to it
// would otherwise let the agent self-approve its own gated actions, collapsing the
// autonomy gate (separation of duties). The trigger fires regardless of pool, so we
// drive the raw INSERT through the RLS-exempt Super pool.
//
// Positive control: binding the agent_runtime preset role (which lacks agents.approve)
// still SUCCEEDS, confirming the hardening did not break normal agent creation.
func TestMembershipAgentGuardForbidsApprove(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	// Seed a tenant with an Owner. We do NOT reuse seed.principalID (the seed agent
	// already holds a membership; we want a fresh, unbound agent to isolate the guard's
	// perm-denylist branch from its one-membership branch).
	seed := seedAgentTenant(ctx, t, tdb)

	// A fresh agent principal, homed at the seeded business, holding NO membership yet.
	freshAgent := uuid.New()
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO principal (id,kind,home_business_id,tenant_root_id,created_at)
		 VALUES ($1,'agent',$2,$2,now())`, freshAgent, seed.businessID); err != nil {
		t.Fatalf("insert fresh agent principal: %v", err)
	}

	// A custom tenant role that grants agents.approve (the human-only approval perm).
	approveRole := uuid.New()
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at)
		 VALUES ($1,$2,'agent-approver','AgentApprover',false,now())`, approveRole, seed.businessID); err != nil {
		t.Fatalf("insert custom approve role: %v", err)
	}
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO role_permission (role_id,permission_key) VALUES ($1,'agents.approve')`, approveRole); err != nil {
		t.Fatalf("grant agents.approve to custom role: %v", err)
	}

	// Attempt to bind the agent to the agents.approve-bearing role. The
	// membership_agent_guard trigger must reject this with a raised exception
	// (RAISE EXCEPTION → SQLSTATE P0001).
	_, err = tdb.Super.Exec(ctx,
		`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at)
		 VALUES ($1,$2,$2,$3,now())`, freshAgent, seed.businessID, approveRole)
	if err == nil {
		t.Fatal("guard did NOT reject binding agents.approve to an agent principal — no-self-approval invariant not DB-enforced")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError from the guard, got %T: %v", err, err)
	}
	if pgErr.Code != "P0001" {
		t.Fatalf("expected SQLSTATE P0001 (raised exception), got %q: %s", pgErr.Code, pgErr.Message)
	}
	if !strings.Contains(pgErr.Message, "agent principal may not hold") {
		t.Fatalf("guard rejected with unexpected message %q — expected the agent-perm-denylist exception", pgErr.Message)
	}

	// Confirm the rejection left no membership row for the fresh agent.
	var bound int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM membership WHERE principal_id = $1`, freshAgent).Scan(&bound); err != nil {
		t.Fatalf("count fresh-agent memberships: %v", err)
	}
	if bound != 0 {
		t.Fatalf("fresh agent holds %d memberships after rejected insert, want 0", bound)
	}

	// Positive control: binding the agent_runtime preset role (no agents.approve) SUCCEEDS,
	// so the hardening did not break agent creation.
	var runtimeRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		`SELECT id FROM role WHERE tenant_root_id IS NULL AND key='agent_runtime'`).Scan(&runtimeRole); err != nil {
		t.Fatalf("lookup agent_runtime preset role: %v", err)
	}
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at)
		 VALUES ($1,$2,$2,$3,now())`, freshAgent, seed.businessID, runtimeRole); err != nil {
		t.Fatalf("positive control: binding agent_runtime (no agents.approve) must succeed, got: %v", err)
	}
}
