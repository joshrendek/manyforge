//go:build integration

package agents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestAgentCRUDRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	seed := seedAgentTenant(ctx, t, tdb)
	svc := &AgentService{DB: tdb.App}

	// Create
	created, err := svc.Create(ctx, seed.principalID, seed.businessID, CreateAgentInput{
		Name: "Triage Bot", Provider: "anthropic", Model: "claude-sonnet-4-5",
		SystemPrompt: "Be helpful.", AllowedTools: []string{"get_ticket", "set_priority"},
		AutonomyMode: 1, Enabled: true, MonthlyBudgetCents: 5000,
		WebAllowedDomains: []string{"docs.sysward.com"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == uuid.Nil || created.PrincipalID == uuid.Nil {
		t.Fatalf("Create returned empty ids: %+v", created)
	}
	if created.Provider != "anthropic" || created.AutonomyMode != 1 || len(created.AllowedTools) != 2 {
		t.Fatalf("Create round-trip mismatch: %+v", created)
	}
	if len(created.WebAllowedDomains) != 1 || created.WebAllowedDomains[0] != "docs.sysward.com" {
		t.Fatalf("Create web_allowed_domains mismatch: %+v", created.WebAllowedDomains)
	}

	// The created principal is a kind='agent' principal homed at the business.
	var kind string
	var home uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		`SELECT kind, home_business_id FROM principal WHERE id = $1`, created.PrincipalID,
	).Scan(&kind, &home); err != nil {
		t.Fatalf("read agent principal: %v", err)
	}
	if kind != "agent" || home != seed.businessID {
		t.Fatalf("agent principal kind=%q home=%v, want agent/%v", kind, home, seed.businessID)
	}

	// Get
	got, err := svc.Get(ctx, seed.principalID, seed.businessID, created.ID)
	if err != nil || got.Name != "Triage Bot" {
		t.Fatalf("Get: %+v err=%v", got, err)
	}
	if len(got.WebAllowedDomains) != 1 || got.WebAllowedDomains[0] != "docs.sysward.com" {
		t.Fatalf("Get web_allowed_domains did not round-trip: %+v", got.WebAllowedDomains)
	}

	// List
	list, err := svc.List(ctx, seed.principalID, seed.businessID)
	if err != nil || len(list) != 1 {
		t.Fatalf("List: %d items err=%v", len(list), err)
	}

	// Update (PATCH semantics: only model + enabled; name/tools/web_domains preserved)
	model := "claude-opus-4-1"
	enabled := false
	upd, err := svc.Update(ctx, seed.principalID, seed.businessID, created.ID, UpdateAgentInput{
		Model: &model, Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd.Model != "claude-opus-4-1" || upd.Enabled || upd.Name != "Triage Bot" || len(upd.AllowedTools) != 2 {
		t.Fatalf("Update did not apply PATCH semantics: %+v", upd)
	}
	if len(upd.WebAllowedDomains) != 1 || upd.WebAllowedDomains[0] != "docs.sysward.com" {
		t.Fatalf("Update did not preserve web_allowed_domains: %+v", upd.WebAllowedDomains)
	}

	// Update web_allowed_domains (replace semantics: non-nil pointer = replace).
	newDomains := []string{"api.sysward.com", "status.sysward.com"}
	upd2, err := svc.Update(ctx, seed.principalID, seed.businessID, created.ID, UpdateAgentInput{
		WebAllowedDomains: &newDomains,
	})
	if err != nil {
		t.Fatalf("Update domains: %v", err)
	}
	if len(upd2.WebAllowedDomains) != 2 || upd2.WebAllowedDomains[0] != "api.sysward.com" || upd2.WebAllowedDomains[1] != "status.sysward.com" {
		t.Fatalf("Update did not replace web_allowed_domains: %+v", upd2.WebAllowedDomains)
	}

	// Duplicate name → conflict
	if _, err := svc.Create(ctx, seed.principalID, seed.businessID, CreateAgentInput{
		Name: "Triage Bot", Provider: "openai", Model: "gpt-4o", AutonomyMode: 1,
	}); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("duplicate name: want ErrConflict, got %v", err)
	}

	// Delete (removes agent + its principal)
	if err := svc.Delete(ctx, seed.principalID, seed.businessID, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(ctx, seed.principalID, seed.businessID, created.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("Get after delete: want ErrNotFound, got %v", err)
	}
	var principalCount int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM principal WHERE id = $1`, created.PrincipalID,
	).Scan(&principalCount); err != nil {
		t.Fatalf("count principal: %v", err)
	}
	if principalCount != 0 {
		t.Fatalf("agent principal not deleted with agent (count=%d)", principalCount)
	}
}

func TestAgentCrossTenantNoOracle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	a := seedAgentTenant(ctx, t, tdb)
	b := seedAgentTenant(ctx, t, tdb)
	svc := &AgentService{DB: tdb.App}

	created, err := svc.Create(ctx, a.principalID, a.businessID, CreateAgentInput{
		Name: "A Bot", Provider: "anthropic", Model: "claude-sonnet-4-5", AutonomyMode: 1,
	})
	if err != nil {
		t.Fatalf("seed tenant A agent: %v", err)
	}

	// Tenant B resolving tenant A's real agent id + A's real business id → not-found
	// (RLS excludes A's rows from B's principal). Same shape as an unknown id.
	if _, err := svc.Get(ctx, b.principalID, a.businessID, created.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Get: want ErrNotFound (no oracle), got %v", err)
	}
	if err := svc.Delete(ctx, b.principalID, a.businessID, created.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Delete: want ErrNotFound (no oracle), got %v", err)
	}
	// Tenant B cannot CREATE an agent in tenant A's business (business invisible) →
	// the principal-insert business gate yields ErrNoRows → ErrNotFound.
	if _, err := svc.Create(ctx, b.principalID, a.businessID, CreateAgentInput{
		Name: "Intruder", Provider: "anthropic", Model: "claude-sonnet-4-5", AutonomyMode: 1,
	}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Create: want ErrNotFound (no oracle), got %v", err)
	}
	// Tenant A's agent is untouched.
	list, err := svc.List(ctx, a.principalID, a.businessID)
	if err != nil || len(list) != 1 {
		t.Fatalf("tenant A list after intrusion attempts: %d err=%v", len(list), err)
	}
}
