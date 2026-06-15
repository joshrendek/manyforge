//go:build integration

package agents_test

import (
	"context"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/agents"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func TestModelCatalog_ListModels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	// model_pricing is a system catalog (no RLS), seeded by migration 0038.
	// tdb.App (*db.DB) satisfies the ModelCatalog WithTx seam.
	cat := &agents.ModelCatalog{DB: tdb.App}
	models, err := cat.ListModels(ctx)
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("expected at least one seeded model in model_pricing")
	}
	for _, m := range models {
		if m.Provider == "" || m.ModelID == "" {
			t.Fatalf("model row missing provider/model_id: %+v", m)
		}
	}
}
