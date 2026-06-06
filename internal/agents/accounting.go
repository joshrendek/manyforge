package agents

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// AgentUsage is one agent's rollup within a window.
type AgentUsage struct {
	AgentID            uuid.UUID
	Name               string
	MonthlyBudgetCents int
	RunCount           int64
	TokensIn           int64
	TokensOut          int64
	CostCents          int64
}

// Summary is a business-wide rollup for a window: totals (summed from the rows) plus
// the per-agent breakdown.
type Summary struct {
	Window    Window
	TotalCost int64
	TotalIn   int64
	TotalOut  int64
	TotalRuns int64
	Agents    []AgentUsage
}

type accountingDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
}

// AccountingStore reads usage aggregates. Read-only; separate from AgentRunStore.
type AccountingStore struct{ DB accountingDB }

// SummaryForWindow returns the per-agent rollup for a business over the window,
// with business totals summed from the rows (one round-trip).
func (s *AccountingStore) SummaryForWindow(ctx context.Context, principalID, businessID uuid.UUID, w Window) (Summary, error) {
	out := Summary{Window: w}
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		rows, e := dbgen.New(tx).AccountingSummaryByAgent(ctx, dbgen.AccountingSummaryByAgentParams{
			BusinessID: businessID,
			FromTs:     w.From,
			ToTs:       w.To,
		})
		if e != nil {
			return e
		}
		for _, r := range rows {
			out.Agents = append(out.Agents, AgentUsage{
				AgentID:            r.AgentID,
				Name:               r.Name,
				MonthlyBudgetCents: int(r.MonthlyBudgetCents),
				RunCount:           r.RunCount,
				TokensIn:           r.TokensIn,
				TokensOut:          r.TokensOut,
				CostCents:          r.CostCents,
			})
			out.TotalCost += r.CostCents
			out.TotalIn += r.TokensIn
			out.TotalOut += r.TokensOut
			out.TotalRuns += r.RunCount
		}
		return nil
	})
	if err != nil {
		return Summary{}, mapAgentRunErr(err)
	}
	return out, nil
}
