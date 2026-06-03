//go:build integration

package agents

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
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

	// A different tenant cannot resolve it (RLS / no-oracle not-found).
	other := seedAgentTenant(ctx, t, tdb)
	if _, err := svc.Resolve(ctx, other.principalID, other.businessID, "anthropic"); err == nil {
		t.Fatal("cross-tenant Resolve must fail (not found)")
	}
}
