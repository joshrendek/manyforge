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

func TestCredentialCRUDRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	// seedAgentTenant returns a business + an agent principal authorized on it.
	ten := seedAgentTenant(ctx, t, tdb)

	svc := &CredentialService{DB: tdb.App, Sealer: newTestSealer(t)}

	id, err := svc.Create(ctx, ten.principalID, ten.businessID, CreateCredentialInput{
		Provider: "anthropic", APIKey: "sk-live", DefaultModel: "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("create returned nil id")
	}

	got, err := svc.Resolve(ctx, ten.principalID, ten.businessID, "anthropic")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.APIKey != "sk-live" || got.Model != "claude-sonnet-4-6" {
		t.Fatalf("resolved = %+v", got)
	}

	// The raw key is NEVER in the column — only the sealed ref.
	var stored *string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT sealed_key_ref FROM ai_provider_credential WHERE id=$1`, id).Scan(&stored); err != nil {
		t.Fatalf("read sealed ref: %v", err)
	}
	if stored == nil || *stored == "sk-live" {
		t.Fatalf("api key stored unsealed: %v", stored)
	}

	// A second Create for the same (business, provider) violates UNIQUE(business_id,
	// provider) → SQLSTATE 23505, which the service maps to ErrConflict (→ 409).
	if _, err := svc.Create(ctx, ten.principalID, ten.businessID, CreateCredentialInput{
		Provider: "anthropic", APIKey: "sk-dup", DefaultModel: "claude-sonnet-4-6",
	}); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("duplicate credential: want ErrConflict, got %v", err)
	}

	other := seedAgentTenant(ctx, t, tdb)
	// Adversarial: tenant B's principal asks for tenant A's business_id (the row that
	// actually exists). RLS on the business table (authorized_businesses(current_principal))
	// must exclude ten.businessID for other.principalID, so the INSERT…SELECT/GET yields
	// no row → not-found. This is the real isolation boundary.
	if _, err := svc.Resolve(ctx, other.principalID, ten.businessID, "anthropic"); err == nil {
		t.Fatal("cross-tenant Resolve of tenant A's credential by tenant B must fail (RLS)")
	}
	// Sanity: tenant B asking for its own (empty) business is also not-found.
	if _, err := svc.Resolve(ctx, other.principalID, other.businessID, "anthropic"); err == nil {
		t.Fatal("tenant B has no credential; must be not-found")
	}
}
