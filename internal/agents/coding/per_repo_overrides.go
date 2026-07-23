package coding

import (
	"context"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// repoOverride is one repo's override of a business dimension (manyforge-e54.2): whether the
// dimension runs for this repo, and an optional per-repo severity floor (nil ⇒ inherit).
type repoOverride struct {
	Enabled     bool
	MinSeverity *string
}

// applyOverridesToPanel layers a repo's overrides onto the business panel: a matching dimension's
// Enabled is replaced, and its MinSeverity is replaced only when the override sets a non-empty
// floor (nil/blank ⇒ inherit the business floor). Everything else — model, prompt, scope — is left
// inherited. Pure; the unit test drives it directly.
func applyOverridesToPanel(panel []Dimension, overrides map[string]repoOverride) []Dimension {
	for i := range panel {
		o, ok := overrides[panel[i].Key]
		if !ok {
			continue
		}
		panel[i].Enabled = o.Enabled
		if o.MinSeverity != nil && strings.TrimSpace(*o.MinSeverity) != "" {
			panel[i].MinSeverity = *o.MinSeverity
		}
	}
	return panel
}

// applyRepoOverrides loads the repo connector's dimension overrides and layers them onto the
// business panel. Degrades to the un-overridden panel on any load error — an override hiccup must
// never fail a review (mirrors resolvePanel's degrade contract). A repo with no overrides returns
// the panel unchanged.
func (s *CodeReviewService) applyRepoOverrides(ctx context.Context, principalID, repoConnectorID uuid.UUID, panel []Dimension) []Dimension {
	var rows []dbgen.ListRepoDimensionOverridesRow
	if err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).ListRepoDimensionOverrides(ctx, repoConnectorID)
		if qerr != nil {
			return qerr
		}
		rows = r
		return nil
	}); err != nil {
		slog.Default().WarnContext(ctx, "coding: repo dimension-override load failed, using business panel",
			"err", err, "repo_connector_id", repoConnectorID)
		return panel
	}
	if len(rows) == 0 {
		return panel
	}
	overrides := make(map[string]repoOverride, len(rows))
	for _, r := range rows {
		overrides[r.DimensionKey] = repoOverride{Enabled: r.Enabled, MinSeverity: r.MinSeverity}
	}
	return applyOverridesToPanel(panel, overrides)
}
