//go:build integration

package agents

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// TestCredentialService_ListDelete exercises the List/Delete surface of
// CredentialService end-to-end against the RLS-scoped testdb:
//   - List returns a business's credentials ordered by provider, carrying NO key.
//   - Listing another business as the same principal returns 0 rows (RLS isolation).
//   - Delete removes a row (List drops); deleting an unknown id => ErrNotFound.
func TestCredentialService_ListDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	a := seedAgentTenant(ctx, t, tdb)
	b := seedAgentTenant(ctx, t, tdb)

	svc := &CredentialService{DB: tdb.App, Sealer: newTestSealer(t)}

	// Two credentials in business A. anthropic needs no base_url; openai (compat) does.
	anthroView, err := svc.Create(ctx, a.principalID, a.businessID, CreateCredentialInput{
		Provider: "anthropic", APIKey: "sk-anthropic", DefaultModel: "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("create anthropic: %v", err)
	}
	if _, err := svc.Create(ctx, a.principalID, a.businessID, CreateCredentialInput{
		Provider: "openai", APIKey: "sk-openai", DefaultModel: "gpt-4o",
		BaseURL: "https://api.example.com/v1",
	}); err != nil {
		t.Fatalf("create openai: %v", err)
	}

	// Created view carries the right identity but never a key.
	if anthroView.Provider != "anthropic" {
		t.Fatalf("created view provider = %q, want anthropic", anthroView.Provider)
	}
	if anthroView.ID == uuid.Nil {
		t.Fatal("created view has nil ID")
	}

	// List returns both, ordered by provider (anthropic precedes openai in the enum).
	views, err := svc.List(ctx, a.principalID, a.businessID)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("list A: got %d credentials, want 2", len(views))
	}
	if views[0].Provider != "anthropic" || views[1].Provider != "openai" {
		t.Fatalf("list A not ordered by provider: got %q, %q", views[0].Provider, views[1].Provider)
	}

	// CredentialView must NEVER expose a key. Pin structurally: the view's fields are
	// exactly the non-secret set, so a future field that could carry key material
	// (anything not in this allowlist) fails the test.
	assertNoKeyField(t, views[0])

	// Seed a credential into business B (via B's own principal) so B is NON-empty.
	// Without this, the cross-business assertion below would be satisfied trivially by
	// an empty table; with it, 0 rows is attributable to RLS, not emptiness.
	if _, err := svc.Create(ctx, b.principalID, b.businessID, CreateCredentialInput{
		Provider: "anthropic", APIKey: "sk-b", DefaultModel: "claude-sonnet-4-6",
	}); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	// RLS isolation: A's principal (no membership on B) lists 0 rows for B even though
	// B now holds a credential — the empty result is enforced by RLS, not emptiness.
	bViews, err := svc.List(ctx, a.principalID, b.businessID)
	if err != nil {
		t.Fatalf("list B as A: %v", err)
	}
	if len(bViews) != 0 {
		t.Fatalf("cross-business list returned %d rows, want 0 (RLS)", len(bViews))
	}

	// Delete one credential in A; List drops to 1.
	if err := svc.Delete(ctx, a.principalID, a.businessID, views[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	after, err := svc.List(ctx, a.principalID, a.businessID)
	if err != nil {
		t.Fatalf("list A after delete: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("list A after delete: got %d, want 1", len(after))
	}
	if after[0].Provider != "openai" {
		t.Fatalf("wrong credential survived delete: %q", after[0].Provider)
	}

	// Deleting an unknown id => ErrNotFound (rows-affected 0).
	if err := svc.Delete(ctx, a.principalID, a.businessID, uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("delete unknown id: want ErrNotFound, got %v", err)
	}
}

// assertNoKeyField fails if CredentialView's fields stray from the non-secret
// allowlist — a structural guard that the view never carries credential material.
func assertNoKeyField(t *testing.T, v CredentialView) {
	t.Helper()
	allowed := map[string]bool{
		"ID": true, "BusinessID": true, "Provider": true, "BaseURL": true,
		"DefaultModel": true, "AllowPrivateBaseURL": true, "CreatedAt": true, "UpdatedAt": true,
		// ChatGPTAccountID is the openai_codex account id — a non-secret identifier, NOT
		// the OAuth access token (that stays sealed in sealed_key_ref and is never on this
		// view). Safe to allowlist here; see CreateCredentialInput.ChatGPTAccountID.
		"ChatGPTAccountID": true,
		// Codex Increment 2 read-side connection-health fields — all non-secret: the derived
		// connection status (connected/disconnected), the ChatGPT plan name, and the access-token
		// expiry timestamp. None carry key/token material (tokens stay sealed, off this view).
		"ConnectionStatus": true, "Plan": true, "AccessExpiry": true,
	}
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		if name := rt.Field(i).Name; !allowed[name] {
			t.Fatalf("CredentialView grew an unexpected field %q — verify it is not key/secret material", name)
		}
	}
}
