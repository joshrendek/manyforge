//go:build integration

// manyforge-1oq (Spec 003 US3): an agent that has acted must NOT 500 on delete.
// agent_run.agent_id and ticket_message.author_principal_id are FK RESTRICT; deleting
// such an agent raises Postgres 23503, which must map to 409 Conflict (disable instead).
package security_regression

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/agents"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// agentDeleteSeed holds the IDs a delete-FK test needs: a business (tenant root)
// and an agent principal that holds a benign (business.read) membership on that
// business, so authorized_businesses(principalID) returns {businessID} and the
// principal passes RLS when calling AgentService / AgentRunStore.
type agentDeleteSeed struct {
	businessID  uuid.UUID
	principalID uuid.UUID
}

// seedAgentDeleteTenant inserts, via the RLS-exempt Super pool, a minimal tenant:
// a master business (its own tenant root), a human owner principal (to satisfy the
// deferred tenant_owner_guard), an agent principal homed at the master, a non-admin
// role (business.read only), and the agent's membership on its home business. After
// commit, authorized_businesses(principalID) returns {businessID}. Mirrors
// internal/agents/testsupport_integration_test.go::seedAgentTenant.
func seedAgentDeleteTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) agentDeleteSeed {
	t.Helper()

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
	ownerEmail := "agent-del-owner-" + masterID.String() + "@x.test"

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Owner','active',now(),now(),now())`,
			[]any{ownerAcctID, ownerEmail}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`,
			[]any{ownerHumanID, ownerAcctID}},

		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'AgentDelCo','active',now(),now())`,
			[]any{masterID}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`,
			[]any{masterID}},

		{`INSERT INTO principal (id,kind,home_business_id,tenant_root_id,created_at) VALUES ($1,'agent',$2,$2,now())`,
			[]any{agentID, masterID}},

		// The tenant must retain an Owner (deferred tenant_owner_guard fires at commit).
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{ownerHumanID, masterID, ownerRole}},

		// Non-admin role (business.read) so the agent membership clears membership_agent_guard.
		{`INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at) VALUES ($1,$2,'agent-del-read','AgentDelRead',false,now())`,
			[]any{benignRoleID, masterID}},
		{`INSERT INTO role_permission (role_id,permission_key) VALUES ($1,'business.read')`,
			[]any{benignRoleID}},

		// Agent membership on its home business: makes authorized_businesses(agentID) = {masterID}.
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
	return agentDeleteSeed{businessID: masterID, principalID: agentID}
}

// TestAgentDeleteAfterActing_Returns409 pins manyforge-1oq: once an agent owns an
// agent_run row (agent_run.agent_id → agent FK RESTRICT), a hard Delete raises
// Postgres 23503, which must surface as ErrConflict ("disable it instead") — never
// a generic 500 and never ErrValidation. A never-acted agent still deletes cleanly.
func TestAgentDeleteAfterActing_Returns409(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	seed := seedAgentDeleteTenant(ctx, t, tdb)
	svc := &agents.AgentService{DB: tdb.App}
	runs := &agents.AgentRunStore{DB: tdb.App}

	// An agent that acts (owns an agent_run) cannot be hard-deleted.
	acted, err := svc.Create(ctx, seed.principalID, seed.businessID, agents.CreateAgentInput{
		Name: "Acted Bot", Provider: "anthropic", Model: "claude-sonnet-4-5", AutonomyMode: 1,
	})
	if err != nil {
		t.Fatalf("Create acted agent: %v", err)
	}
	if _, err := runs.CreateRun(ctx, seed.principalID, seed.businessID, acted.ID, "manual", uuid.NewString(), nil, nil); err != nil {
		t.Fatalf("CreateRun (make agent act): %v", err)
	}

	delErr := svc.Delete(ctx, seed.principalID, seed.businessID, acted.ID)
	if !errors.Is(delErr, errs.ErrConflict) {
		t.Fatalf("Delete of acted agent: want ErrConflict (FK 23503 → 409), got %v", delErr)
	}
	if errors.Is(delErr, errs.ErrValidation) || errors.Is(delErr, errs.ErrNotFound) {
		t.Fatalf("Delete of acted agent must be ErrConflict only, got %v", delErr)
	}

	// Sanity: a never-acted agent still deletes cleanly.
	clean, err := svc.Create(ctx, seed.principalID, seed.businessID, agents.CreateAgentInput{
		Name: "Clean Bot", Provider: "anthropic", Model: "claude-sonnet-4-5", AutonomyMode: 1,
	})
	if err != nil {
		t.Fatalf("Create clean agent: %v", err)
	}
	if err := svc.Delete(ctx, seed.principalID, seed.businessID, clean.ID); err != nil {
		t.Fatalf("Delete of never-acted agent must succeed, got %v", err)
	}
}
