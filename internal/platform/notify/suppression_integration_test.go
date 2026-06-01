//go:build integration

package notify

import (
	"context"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// TestDBSuppressionChecker (T039-suppression) — DBSuppression correctly reports
// suppressed and non-suppressed addresses against the real email_suppression table.
func TestDBSuppressionChecker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	// Seed a suppression row via the RLS-exempt Super pool.
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO email_suppression (email, reason) VALUES ($1, $2)`,
		"bounced@example.com", "hard_bounce",
	); err != nil {
		t.Fatalf("seed suppression: %v", err)
	}

	checker := DBSuppression{DB: tdb.App}

	// The seeded address must be suppressed.
	suppressed, err := checker.IsSuppressed(ctx, "bounced@example.com")
	if err != nil {
		t.Fatalf("IsSuppressed(bounced): %v", err)
	}
	if !suppressed {
		t.Errorf("IsSuppressed(bounced@example.com) = false, want true")
	}

	// A different address must not be suppressed.
	clean, err := checker.IsSuppressed(ctx, "clean@example.com")
	if err != nil {
		t.Fatalf("IsSuppressed(clean): %v", err)
	}
	if clean {
		t.Errorf("IsSuppressed(clean@example.com) = true, want false")
	}
}
