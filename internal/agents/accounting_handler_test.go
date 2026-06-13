package agents

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// fakeSummaryOps implements summaryOps for accounting handler tests (no DB).
type fakeSummaryOps struct {
	sum    Summary
	err    error
	called bool
	gotWin Window
}

func (f *fakeSummaryOps) SummaryForWindow(_ context.Context, _, _ uuid.UUID, w Window) (Summary, error) {
	f.called = true
	f.gotWin = w
	return f.sum, f.err
}

// serveAccounting mounts AccountingHandler behind the real auth chain and serves one request.
// Mirrors serveRun.
func serveAccounting(h *AccountingHandler, ring *auth.KeyRing, target, bearer string, body io.Reader) *httptest.ResponseRecorder {
	mux := httpx.NewRouter(ring)
	mux.Group(func(pr chi.Router) {
		pr.Use(httpx.RequireAuth)
		h.ProtectedRoutes(pr)
	})
	req := httptest.NewRequest(http.MethodGet, target, body)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// summaryWithOneAgent builds a Summary whose single agent has the given budget + cost cents.
func summaryWithOneAgent(budgetCents int, costCents int64) Summary {
	return Summary{
		Agents: []AgentUsage{{
			AgentID: uuid.New(), Name: "Support Bot",
			MonthlyBudgetCents: budgetCents, RunCount: 3,
			TokensIn: 100, TokensOut: 50, CostCents: costCents,
		}},
	}
}

// TestToSummaryResp_BudgetPctOnThisMonth pins manyforge-deo.4: budget_pct is populated for the
// current-month window (both the explicit name and the empty default) when a budget is set.
func TestToSummaryResp_BudgetPctOnThisMonth(t *testing.T) {
	for _, win := range []string{"", "this_month"} {
		resp := toSummaryResp(summaryWithOneAgent(1000, 250), win)
		if len(resp.Agents) != 1 {
			t.Fatalf("win=%q: agents len = %d, want 1", win, len(resp.Agents))
		}
		got := resp.Agents[0].BudgetPct
		if got == nil {
			t.Fatalf("win=%q: budget_pct = nil, want non-nil on the monthly window", win)
		}
		if *got != 25 { // 250 / 1000 = 25%
			t.Errorf("win=%q: budget_pct = %d, want 25", win, *got)
		}
	}
}

// TestToSummaryResp_BudgetPctOmittedOnOtherWindow: budget_pct is meaningful only for the monthly
// budget, so a non-month window omits it even when a budget is set.
func TestToSummaryResp_BudgetPctOmittedOnOtherWindow(t *testing.T) {
	for _, win := range []string{"last_month", "last_30_days", "custom"} {
		resp := toSummaryResp(summaryWithOneAgent(1000, 250), win)
		if resp.Agents[0].BudgetPct != nil {
			t.Errorf("win=%q: budget_pct = %d, want nil (only the monthly window carries it)", win, *resp.Agents[0].BudgetPct)
		}
	}
}

// TestToSummaryResp_BudgetPctOmittedWhenNoBudget: with no budget set, budget_pct is omitted even
// on the monthly window (no divide-by-zero, no meaningless 0%).
func TestToSummaryResp_BudgetPctOmittedWhenNoBudget(t *testing.T) {
	resp := toSummaryResp(summaryWithOneAgent(0, 250), "this_month")
	if resp.Agents[0].BudgetPct != nil {
		t.Errorf("budget_pct = %d, want nil when no budget is set", *resp.Agents[0].BudgetPct)
	}
}

// TestGetSummaryHandler_OmitsBudgetPctInJSON confirms the omitempty wiring end-to-end: on a
// non-month window the budget_pct key is absent from the serialized JSON.
func TestGetSummaryHandler_OmitsBudgetPctInJSON(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	svc := &fakeSummaryOps{sum: summaryWithOneAgent(1000, 250)}
	h := NewAccountingHandler(svc)
	rec := serveAccounting(h, ring, "/businesses/"+bid.String()+"/accounting?window=last_month", mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("budget_pct")) {
		t.Errorf("response includes budget_pct on a non-month window: %s", rec.Body.String())
	}
}

// TestGetSummaryHandler_WindowErrorIs400: an unknown window is a 400, and the service is not hit.
func TestGetSummaryHandler_WindowErrorIs400(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	svc := &fakeSummaryOps{}
	h := NewAccountingHandler(svc)
	rec := serveAccounting(h, ring, "/businesses/"+bid.String()+"/accounting?window=bogus", mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown window → ErrValidation)", rec.Code)
	}
	if svc.called {
		t.Error("service must not be called when the window fails to resolve")
	}
}
