package agents

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/ai"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// modelPricingDB is the subset of db.DB the loader needs. model_pricing is a
// system catalog (no RLS), so a plain transaction without a principal reads it.
type modelPricingDB interface {
	WithTx(ctx context.Context, fn func(pgx.Tx) error) error
}

func modelRowToAIModel(r dbgen.ListModelPricingRow) ai.Model {
	return ai.Model{
		ID:                 r.ModelID,
		Provider:           r.Provider,
		ContextWindow:      int(r.ContextWindow),
		InputCentsPerMTok:  r.InputCentsPerMtok,
		OutputCentsPerMTok: r.OutputCentsPerMtok,
		SupportsTools:      r.SupportsTools,
	}
}

// LoadModelRegistry builds an ai.Registry from the model_pricing catalog. It is the
// prod source of truth; unit tests seed via ai.RegisterDefaults instead. An empty
// catalog is an error — a misconfigured deploy should fail loudly, not run with zero
// models.
func LoadModelRegistry(ctx context.Context, database modelPricingDB) (*ai.Registry, error) {
	reg := ai.NewRegistry()
	err := database.WithTx(ctx, func(tx pgx.Tx) error {
		rows, e := dbgen.New(tx).ListModelPricing(ctx)
		if e != nil {
			return e
		}
		for _, row := range rows {
			reg.Register(modelRowToAIModel(row))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("agents: load model registry: %w", err)
	}
	if reg.Len() == 0 {
		return nil, fmt.Errorf("agents: model_pricing catalog is empty or unseeded")
	}
	return reg, nil
}
