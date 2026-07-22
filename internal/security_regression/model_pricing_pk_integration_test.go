//go:build integration

// Finding: manyforge-6fx.2 — real-PostgreSQL exercise of the provider-aware pricing key.
// The source pins in model_pricing_provider_key_pin_test.go assert the migration/schema TEXT;
// this integration test applies the full migration chain (incl. 0099) to a live database and
// proves the resulting constraint actually behaves: the PK is (provider, model_id) and two
// providers can hold the same model_id (which the original sole-model_id PK rejected), so a $0
// openai_codex slug can neither shadow nor block a metered same-named model of another provider.
package security_regression

import (
	"context"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func TestPin_ModelPricingCompositePK_DB(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("6fx.2: start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	// After all migrations, model_pricing's primary key must be composite.
	var pkdef string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conrelid = 'model_pricing'::regclass AND contype = 'p'`).Scan(&pkdef); err != nil {
		t.Fatalf("6fx.2: read model_pricing PK: %v", err)
	}
	if !strings.Contains(pkdef, "PRIMARY KEY (provider, model_id)") {
		t.Fatalf("6fx.2: model_pricing PK = %q, want composite (provider, model_id)", pkdef)
	}

	// Two providers sharing one model_id must coexist — the sole-model_id PK rejected the second.
	insertProbe := func(provider string) error {
		_, e := tdb.Super.Exec(ctx,
			`INSERT INTO model_pricing
			   (model_id, provider, display_name, context_window,
			    input_cents_per_mtok, output_cents_per_mtok, supports_tools, enabled, created_at, updated_at)
			 VALUES ('mf-6fx2-probe', $1, 'probe', 0, 0, 0, true, true, now(), now())`, provider)
		return e
	}
	if err := insertProbe("openai_codex"); err != nil {
		t.Fatalf("6fx.2: insert first (codex) probe row: %v", err)
	}
	if err := insertProbe("openai"); err != nil {
		t.Fatalf("6fx.2: same model_id under a second provider rejected — composite PK not in effect: %v", err)
	}
	// Leave the system catalog pristine for any later test sharing the container.
	if _, err := tdb.Super.Exec(ctx, `DELETE FROM model_pricing WHERE model_id = 'mf-6fx2-probe'`); err != nil {
		t.Fatalf("6fx.2: cleanup probe rows: %v", err)
	}
}
