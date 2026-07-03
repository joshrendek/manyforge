package coding

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// resolvePanel returns the business's configured review panel (spec 008), or the zero-config
// defaultPanel() when the business has configured no dimensions. Configured rows are read under
// the caller's principal so RLS scopes them to the tenant. A resolver failure must NOT brick
// reviews — it degrades to the default single "general" lane (always valid) and logs, so a
// transient DB blip yields a legacy-shaped review rather than a failed job.
func (s *CodeReviewService) resolvePanel(ctx context.Context, principalID, businessID uuid.UUID) []Dimension {
	var dims []Dimension
	if err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		rows, err := dbgen.New(tx).ListReviewDimensions(ctx, businessID)
		if err != nil {
			return err
		}
		dims = make([]Dimension, 0, len(rows))
		for _, r := range rows {
			dims = append(dims, dimensionFromRow(r))
		}
		return nil
	}); err != nil {
		slog.Default().WarnContext(ctx, "coding: resolve review panel failed, using default panel",
			"err", err, "business_id", businessID)
		return defaultPanel()
	}
	if len(dims) == 0 {
		return defaultPanel()
	}
	return dims
}

// dimensionFromRow maps a persisted review_dimension row to an engine Dimension. A NULL
// provider (and empty model) means "use the review's resolved default credential"; the label
// is derived from the key since the row carries no label column.
func dimensionFromRow(r dbgen.ReviewDimension) Dimension {
	d := Dimension{
		Key:         r.Dimension,
		Label:       dimensionLabel(r.Dimension),
		Model:       r.Model,
		Prompt:      r.Prompt,
		ScopeGlobs:  r.ScopeGlobs,
		MinSeverity: r.MinSeverity,
		Enabled:     r.Enabled,
		Order:       int(r.SortOrder),
	}
	if r.Provider.Valid {
		d.Provider = string(r.Provider.AiProvider)
	}
	return d
}
