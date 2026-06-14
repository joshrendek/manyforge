package agents

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// runOps is the narrow surface the run HTTP handler needs (fakeable in tests).
type runOps interface {
	Trigger(ctx context.Context, principalID, businessID, agentID uuid.UUID, trigger string, targetType *string, targetID *uuid.UUID) (AgentRun, error)
	GetRun(ctx context.Context, principalID, businessID, agentID, runID uuid.UUID) (AgentRun, error)
	ListRuns(ctx context.Context, principalID, businessID, agentID uuid.UUID, f RunListFilter, cursor string, limit int) ([]AgentRun, *string, error)
}

// RunService triggers agent runs (as the agent principal) and reads run status.
type RunService struct {
	agents *AgentService
	engine *Engine
	runs   *AgentRunStore
}

var _ runOps = (*RunService)(nil)

// NewRunService wires the agent lookup + engine + run store.
func NewRunService(a *AgentService, e *Engine, r *AgentRunStore) *RunService {
	return &RunService{agents: a, engine: e, runs: r}
}

// Trigger loads the (caller-visible) agent under the caller's RLS context, then runs
// the loop AS the agent principal (ag.PrincipalID) so all tool calls, audit rows, and
// run records happen under the agent's acting identity.
//
// NOTE: targetID is a PROMPT HINT (which ticket the agent should look at first), NOT an
// ownership gate. Authorization is enforced downstream by the tools, which run under the
// agent's own RLS principal — a targetID the agent cannot access simply yields nothing
// when a tool reads it. Do not mistake the absence of an ownership check here for a bug.
func (s *RunService) Trigger(ctx context.Context, principalID, businessID, agentID uuid.UUID, trigger string, targetType *string, targetID *uuid.UUID) (AgentRun, error) {
	ag, err := s.agents.Get(ctx, principalID, businessID, agentID)
	if err != nil {
		return AgentRun{}, err
	}
	return s.engine.Run(ctx, ag.PrincipalID, ag, trigger, targetType, targetID)
}

// GetRun reads a run within the caller's business AND under the given agent. The
// agentID is threaded into the SQL predicate so a run is only visible via its own
// agent's path (no same-business cross-agent IDOR).
func (s *RunService) GetRun(ctx context.Context, principalID, businessID, agentID, runID uuid.UUID) (AgentRun, error) {
	return s.runs.Get(ctx, principalID, businessID, agentID, runID)
}

// ListRuns delegates to the run store (keyset pagination).
func (s *RunService) ListRuns(ctx context.Context, principalID, businessID, agentID uuid.UUID, f RunListFilter, cursor string, limit int) ([]AgentRun, *string, error) {
	return s.runs.ListRuns(ctx, principalID, businessID, agentID, f, cursor, limit)
}

// RunHandler is the thin HTTP layer over runOps.
type RunHandler struct{ svc runOps }

// NewRunHandler builds the run HTTP handler.
func NewRunHandler(svc runOps) *RunHandler { return &RunHandler{svc: svc} }

// ProtectedRoutes mounts the run endpoints (caller must gate with agents.run).
func (h *RunHandler) ProtectedRoutes(r chi.Router) {
	r.Route("/businesses/{id}/agents/{agentID}/runs", func(r chi.Router) {
		r.Post("/", h.triggerRun)
		r.Get("/", h.listRuns)
		r.Get("/{runID}", h.getRun)
	})
}

type triggerRequest struct {
	Trigger    string     `json:"trigger"`
	TargetType *string    `json:"target_type"`
	TargetID   *uuid.UUID `json:"target_id"`
}

type runResp struct {
	ID            uuid.UUID `json:"id"`
	AgentID       uuid.UUID `json:"agent_id"`
	Trigger       string    `json:"trigger"`
	Status        string    `json:"status"`
	TokensIn      int       `json:"tokens_in"`
	TokensOut     int       `json:"tokens_out"`
	CostCents     int64     `json:"cost_cents"`
	CorrelationID string    `json:"correlation_id"`
	Error         *string   `json:"error,omitempty"`
}

func toRunResp(r AgentRun) runResp {
	return runResp{
		ID: r.ID, AgentID: r.AgentID, Trigger: r.Trigger, Status: r.Status,
		TokensIn: r.TokensIn, TokensOut: r.TokensOut, CostCents: r.CostCents,
		CorrelationID: r.CorrelationID, Error: r.Error,
	}
}

func runBusinessID(r *http.Request) (uuid.UUID, error) { return uuid.Parse(chi.URLParam(r, "id")) }
func runAgentID(r *http.Request) (uuid.UUID, error)    { return uuid.Parse(chi.URLParam(r, "agentID")) }
func runPathID(r *http.Request) (uuid.UUID, error)     { return uuid.Parse(chi.URLParam(r, "runID")) }

func (h *RunHandler) triggerRun(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := runBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	aid, err := runAgentID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	// Body is optional: an empty body defaults to a "manual" trigger. DecodeJSON
	// writes its own 400 on malformed JSON (and returns false), so we only decode
	// when a body is actually present.
	var in triggerRequest
	if r.Body != nil && r.ContentLength != 0 {
		if !httpx.DecodeJSON(w, r, &in) {
			return
		}
	}
	trigger := in.Trigger
	if trigger == "" {
		trigger = "manual"
	}
	run, err := h.svc.Trigger(r.Context(), pid, bid, aid, trigger, in.TargetType, in.TargetID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, toRunResp(run))
}

func (h *RunHandler) getRun(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := runBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	// Parse {agentID} too: the run read is scoped by agent_id in SQL, so a malformed
	// agentID is a not-found (no oracle), same as a malformed business/run id.
	aid, err := runAgentID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	rid, err := runPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	run, err := h.svc.GetRun(r.Context(), pid, bid, aid, rid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toRunResp(run))
}

type runListItem struct {
	ID            uuid.UUID `json:"id"`
	AgentID       uuid.UUID `json:"agent_id"`
	Trigger       string    `json:"trigger"`
	Status        string    `json:"status"`
	TokensIn      int       `json:"tokens_in"`
	TokensOut     int       `json:"tokens_out"`
	CostCents     int64     `json:"cost_cents"`
	CorrelationID string    `json:"correlation_id"`
	Error         *string   `json:"error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

func toRunListItem(r AgentRun) runListItem {
	return runListItem{
		ID: r.ID, AgentID: r.AgentID, Trigger: r.Trigger, Status: r.Status,
		TokensIn: r.TokensIn, TokensOut: r.TokensOut, CostCents: r.CostCents,
		CorrelationID: r.CorrelationID, Error: r.Error, CreatedAt: r.CreatedAt,
	}
}

func (h *RunHandler) listRuns(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := runBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	aid, err := runAgentID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	q := r.URL.Query()
	win, err := ResolveWindow(q.Get("window"), q.Get("from"), q.Get("to"), time.Now().UTC())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			limit = n
		}
	}
	runs, next, err := h.svc.ListRuns(r.Context(), pid, bid, aid, RunListFilter{Status: q.Get("status"), Window: win}, q.Get("cursor"), limit)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items := make([]runListItem, 0, len(runs))
	for _, rn := range runs {
		items = append(items, toRunListItem(rn))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}
