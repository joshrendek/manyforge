package agents

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// ModelInfo is the non-pricing projection of a catalog model, for the agent
// form's model picker.
type ModelInfo struct {
	Provider string
	ModelID  string
}

// modelLister is the metadata seam the agent handler reads models through.
type modelLister interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// toolLister is the metadata seam the agent handler reads tools through.
// Satisfied by *ToolRegistry.
type toolLister interface {
	All() []Tool
}

var _ toolLister = (*ToolRegistry)(nil)
var _ modelLister = (*ModelCatalog)(nil)

// ModelCatalog reads the model_pricing system catalog. It is NOT RLS-scoped
// (model_pricing has no tenant), so it uses WithTx (no principal) — the same
// path LoadModelRegistry uses.
type ModelCatalog struct{ DB modelPricingDB }

// ListModels returns the enabled catalog models (provider + id).
func (c *ModelCatalog) ListModels(ctx context.Context) ([]ModelInfo, error) {
	var rows []dbgen.ListModelPricingRow
	err := c.DB.WithTx(ctx, func(tx pgx.Tx) error {
		r, e := dbgen.New(tx).ListModelPricing(ctx)
		rows = r
		return e
	})
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, ModelInfo{Provider: r.Provider, ModelID: r.ModelID})
	}
	return out, nil
}
