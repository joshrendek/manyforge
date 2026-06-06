package agents

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// summaryOps is the narrow read surface the accounting handler needs (fakeable in tests).
type summaryOps interface {
	SummaryForWindow(ctx context.Context, principalID, businessID uuid.UUID, w Window) (Summary, error)
}

type AccountingHandler struct{ svc summaryOps }

func NewAccountingHandler(svc summaryOps) *AccountingHandler { return &AccountingHandler{svc: svc} }

func (h *AccountingHandler) ProtectedRoutes(r chi.Router) {
	r.Get("/businesses/{id}/accounting", h.getSummary)
}

type agentUsageResp struct {
	AgentID            uuid.UUID `json:"agent_id"`
	Name               string    `json:"name"`
	MonthlyBudgetCents int       `json:"monthly_budget_cents"`
	RunCount           int64     `json:"run_count"`
	TokensIn           int64     `json:"tokens_in"`
	TokensOut          int64     `json:"tokens_out"`
	CostCents          int64     `json:"cost_cents"`
	BudgetPct          *int      `json:"budget_pct,omitempty"`
}

type summaryResp struct {
	Window struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	} `json:"window"`
	Totals struct {
		CostCents int64 `json:"cost_cents"`
		TokensIn  int64 `json:"tokens_in"`
		TokensOut int64 `json:"tokens_out"`
		RunCount  int64 `json:"run_count"`
	} `json:"totals"`
	Agents []agentUsageResp `json:"agents"`
}

func (h *AccountingHandler) getSummary(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	q := r.URL.Query()
	winName := q.Get("window")
	win, err := ResolveWindow(winName, q.Get("from"), q.Get("to"), time.Now().UTC())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	sum, err := h.svc.SummaryForWindow(r.Context(), pid, bid, win)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toSummaryResp(sum, winName))
}

// budget_pct is only meaningful for the monthly budget, so it is populated only when
// the requested window is the current month and the agent has a budget set.
func toSummaryResp(s Summary, winName string) summaryResp {
	var out summaryResp
	out.Window.From, out.Window.To = s.Window.From, s.Window.To
	out.Totals.CostCents, out.Totals.TokensIn = s.TotalCost, s.TotalTokensIn
	out.Totals.TokensOut, out.Totals.RunCount = s.TotalTokensOut, s.TotalRuns
	thisMonth := winName == "" || winName == "this_month"
	for _, a := range s.Agents {
		item := agentUsageResp{
			AgentID: a.AgentID, Name: a.Name, MonthlyBudgetCents: a.MonthlyBudgetCents,
			RunCount: a.RunCount, TokensIn: a.TokensIn, TokensOut: a.TokensOut, CostCents: a.CostCents,
		}
		if thisMonth && a.MonthlyBudgetCents > 0 {
			pct := int(a.CostCents * 100 / int64(a.MonthlyBudgetCents))
			item.BudgetPct = &pct
		}
		out.Agents = append(out.Agents, item)
	}
	return out
}
