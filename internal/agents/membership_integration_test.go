//go:build integration

package agents

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
