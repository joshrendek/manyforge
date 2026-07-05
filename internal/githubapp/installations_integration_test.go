//go:build integration

package githubapp_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/githubapp"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestInstallationLifecycleLinkAndQuarantine(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb.Start: %v", err)
	}
	defer tdb.Close(ctx)

	// Seed business A (+member principal +agent), business B (+member principal), via tdb.Super
	// (RLS-exempt). Mirror the seed INSERTs an existing coding/connectors //go:build integration
	// test uses (business, principal, membership, agent). Capture: bizA, agentA, memberA,
	// bizB, memberB.
	bizA, agentA, memberA, bizB, memberB, foreignAgentB := seedTwoBusinesses(t, ctx, tdb)

	svc := &githubapp.InstallationService{DB: tdb.App}
	if err := svc.UpsertFromEvent(ctx, 7788, "bluescripts-net", "Organization"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// RLS quarantine: unlinked row invisible to any business principal.
	if c := countInstalls(t, ctx, tdb, memberA); c != 0 {
		t.Fatalf("bizA sees %d unlinked, want 0", c)
	}

	// C-2: linking with a foreign business's agent must fail (agent not in bizA).
	if err := svc.Link(ctx, 7788, bizA, foreignAgentB); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("link with foreign agent = %v, want ErrNotFound", err)
	}
	// Valid link.
	if err := svc.Link(ctx, 7788, bizA, agentA); err != nil {
		t.Fatalf("link: %v", err)
	}
	// Re-link is a no-op (already linked).
	if err := svc.Link(ctx, 7788, bizA, agentA); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("relink = %v, want ErrNotFound", err)
	}
	// Now visible to bizA, invisible to bizB.
	if c := countInstalls(t, ctx, tdb, memberA); c != 1 {
		t.Fatalf("bizA sees %d after link, want 1", c)
	}
	if c := countInstalls(t, ctx, tdb, memberB); c != 0 {
		t.Fatalf("bizB sees %d, want 0", c)
	}
	_ = bizB
}

// countInstalls runs a raw count under memberPrincipal's RLS context via the App pool.
func countInstalls(t *testing.T, ctx context.Context, tdb *testdb.TestDB, principal uuid.UUID) int {
	var n int
	err := tdb.App.WithPrincipal(ctx, principal, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM github_app_installation").Scan(&n)
	})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// seedTwoBusinesses seeds two independent tenants (mirroring
// connectors.seedConnectorTenant / coding.seedCodingTenant: an owner human
// principal + master business + benign-role agent principal + a real agent
// row), returning:
//   - bizA / bizB: the two businesses' ids,
//   - agentA: the agent-table row id of an agent belonging to bizA,
//   - memberA / memberB: a principal holding an authorized membership in
//     bizA / bizB respectively (used to prove RLS visibility),
//   - foreignAgentB: the agent-table row id of an agent belonging to bizB —
//     used to prove Link rejects an agent that isn't in the target business.
func seedTwoBusinesses(t *testing.T, ctx context.Context, tdb *testdb.TestDB) (bizA, agentA, memberA, bizB, memberB, foreignAgentB uuid.UUID) {
	t.Helper()

	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("preset owner role: %v", err)
	}

	// seedBiz seeds one tenant and returns (businessID, ownerHumanPrincipalID,
	// agentRowID). It mirrors seedCodingTenant/seedConnectorTenant exactly:
	// an owner human account+principal (satisfies tenant_owner_guard), a
	// master business (tenant root = self), a benign business.read-only role,
	// an agent principal holding that role, and a real agent row.
	seedBiz := func(name string) (businessID, ownerHumanID, agentRowID uuid.UUID) {
		businessID = uuid.New()
		agentPrincipalID := uuid.New()
		agentRowID = uuid.New()
		benignRoleID := uuid.New()
		ownerAcctID := uuid.New()
		ownerHumanID = uuid.New()
		ownerEmail := "ghapp-owner-" + businessID.String() + "@x.test"

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
			{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,$2,'active',now(),now())`,
				[]any{businessID, name}},
			{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`,
				[]any{businessID}},
			{`INSERT INTO principal (id,kind,home_business_id,tenant_root_id,created_at) VALUES ($1,'agent',$2,$2,now())`,
				[]any{agentPrincipalID, businessID}},
			{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
				[]any{ownerHumanID, businessID, ownerRole}},
			{`INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at) VALUES ($1,$2,'ghapp-read','GHAppRead',false,now())`,
				[]any{benignRoleID, businessID}},
			{`INSERT INTO role_permission (role_id,permission_key) VALUES ($1,'business.read')`,
				[]any{benignRoleID}},
			{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
				[]any{agentPrincipalID, businessID, benignRoleID}},
			// A real agent row so github_link_installation's agent-in-business guard
			// (EXISTS (SELECT 1 FROM agent WHERE id=... AND business_id=...)) resolves.
			{`INSERT INTO agent (id,business_id,tenant_root_id,principal_id,name,provider,model,system_prompt,allowed_tools,autonomy_mode,enabled,monthly_budget_cents,created_at,updated_at,allowed_mcp_servers,retriage_on_reply,web_allowed_domains)
			  VALUES ($1,$2,$2,$3,'GHAgent','anthropic','m','',ARRAY[]::text[],3,true,0,now(),now(),ARRAY[]::uuid[],false,ARRAY[]::text[])`,
				[]any{agentRowID, businessID, agentPrincipalID}},
		}
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
				t.Fatalf("seed exec: %v\nSQL: %s", err, s.sql)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit seed: %v", err)
		}
		return businessID, ownerHumanID, agentRowID
	}

	bizA, memberA, agentA = seedBiz("GHAppCoA")
	bizB, memberB, foreignAgentB = seedBiz("GHAppCoB")
	return bizA, agentA, memberA, bizB, memberB, foreignAgentB
}
