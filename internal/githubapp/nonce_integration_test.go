//go:build integration

package githubapp_test

import (
	"context"
	"testing"

	"github.com/manyforge/manyforge/internal/githubapp"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// TestNonceConsumeIsSingleUse pins the security-relevant invariant of
// github_setup_nonce (migrations/0081): the FIRST Consume of a given nonce
// succeeds, and every subsequent Consume of the SAME nonce reports false
// (replay) rather than erroring or double-succeeding — the property that
// makes it safe to bind a nonce into signed setup/link state.
func TestNonceConsumeIsSingleUse(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb.Start: %v", err)
	}
	defer tdb.Close(ctx)

	svc := &githubapp.NonceService{DB: tdb.App}

	first, err := svc.Consume(ctx, "nonce-1")
	if err != nil {
		t.Fatalf("first Consume: %v", err)
	}
	if !first {
		t.Fatal("first Consume = false, want true")
	}

	replay, err := svc.Consume(ctx, "nonce-1")
	if err != nil {
		t.Fatalf("replay Consume: %v", err)
	}
	if replay {
		t.Fatal("replay Consume = true, want false")
	}

	// A distinct nonce is unaffected by the first nonce's consumption.
	other, err := svc.Consume(ctx, "nonce-2")
	if err != nil {
		t.Fatalf("other Consume: %v", err)
	}
	if !other {
		t.Fatal("other Consume = false, want true")
	}
}
