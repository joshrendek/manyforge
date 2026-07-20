//go:build integration

package agents

import (
	"context"
	"errors"
	"strings"
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

	view, err := svc.Create(ctx, ten.principalID, ten.businessID, CreateCredentialInput{
		Provider: "anthropic", APIKey: "sk-live", DefaultModel: "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if view.ID == uuid.Nil {
		t.Fatal("create returned nil id")
	}
	if view.Provider != "anthropic" {
		t.Fatalf("create view provider = %q, want anthropic", view.Provider)
	}
	if view.MaxConcurrentLanes != 4 {
		t.Fatalf("MaxConcurrentLanes: got %d, want 4 (DB default)", view.MaxConcurrentLanes)
	}
	id := view.ID

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

func TestCredentialTrustGrantAudited(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	ten := seedAgentTenant(ctx, t, tdb)
	svc := &CredentialService{DB: tdb.App, Sealer: newTestSealer(t)}

	// A trusted self-host credential writes exactly one trust-grant audit row, atomically.
	view, err := svc.Create(ctx, ten.principalID, ten.businessID, CreateCredentialInput{
		Provider: "ollama", DefaultModel: "llama3",
		BaseURL: "http://127.0.0.1:11434/v1", AllowPrivateBaseURL: true,
	})
	if err != nil {
		t.Fatalf("create trusted: %v", err)
	}
	id := view.ID
	var n int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND action='ai_credential.created' AND decision='trust_private_base_url'`,
		id).Scan(&n); err != nil {
		t.Fatalf("count trust audit: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 trust-grant audit row, got %d", n)
	}
	var actor uuid.UUID
	var inputs []byte
	if err := tdb.Super.QueryRow(ctx,
		`SELECT actor_principal_id, inputs FROM audit_entry WHERE target_id=$1 AND action='ai_credential.created' AND decision='trust_private_base_url'`,
		id).Scan(&actor, &inputs); err != nil {
		t.Fatalf("read trust audit row: %v", err)
	}
	if actor != ten.principalID {
		t.Fatalf("audit actor = %s, want %s", actor, ten.principalID)
	}
	// jsonb is re-serialized by Postgres with a space after each colon; match that form.
	if !strings.Contains(string(inputs), `"provider": "ollama"`) || !strings.Contains(string(inputs), `"base_url": "http://127.0.0.1:11434/v1"`) {
		t.Fatalf("audit inputs missing provider/base_url: %s", inputs)
	}

	// A non-trusted credential writes NO trust-grant row.
	view2, err := svc.Create(ctx, ten.principalID, ten.businessID, CreateCredentialInput{
		Provider: "openai", DefaultModel: "gpt-4o", BaseURL: "https://api.example.com/v1",
	})
	if err != nil {
		t.Fatalf("create untrusted: %v", err)
	}
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND decision='trust_private_base_url'`,
		view2.ID).Scan(&n); err != nil {
		t.Fatalf("count untrusted audit: %v", err)
	}
	if n != 0 {
		t.Fatalf("untrusted credential must write no trust-grant row, got %d", n)
	}
}

// TestOpenAICodexAccountIDRoundTrips pins the openai_codex-specific column end to end:
// Create persists chatgpt_account_id (a non-secret account identifier, NOT the sealed
// OAuth access token) and Resolve returns it alongside the unsealed key.
func TestOpenAICodexAccountIDRoundTrips(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	ten := seedAgentTenant(ctx, t, tdb)
	svc := &CredentialService{DB: tdb.App, Sealer: newTestSealer(t)}

	view, err := svc.Create(ctx, ten.principalID, ten.businessID, CreateCredentialInput{
		Provider:         "openai_codex",
		APIKey:           "codex-test-token", // stands in for the OAuth access token
		DefaultModel:     "gpt-5",
		ChatGPTAccountID: "acct-abc-123",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The returned view exercises credViewFromRow's deref of a populated (non-nil)
	// chatgpt_account_id — the account id is non-secret and IS surfaced on CredentialView.
	if view.ChatGPTAccountID != "acct-abc-123" {
		t.Fatalf("create view acct = %q; want acct-abc-123", view.ChatGPTAccountID)
	}

	got, err := svc.Resolve(ctx, ten.principalID, ten.businessID, "openai_codex")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.APIKey != "codex-test-token" || got.ChatGPTAccountID != "acct-abc-123" {
		t.Fatalf("got key=%q acct=%q; want token + acct-abc-123", got.APIKey, got.ChatGPTAccountID)
	}
}
