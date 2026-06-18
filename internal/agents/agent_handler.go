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

// agentCRUD is the subset of AgentService the handler needs (an interface so
// handler tests can inject a fake). *AgentService satisfies it.
type agentCRUD interface {
	Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateAgentInput) (Agent, error)
	Get(ctx context.Context, principalID, businessID, agentID uuid.UUID) (Agent, error)
	List(ctx context.Context, principalID, businessID uuid.UUID) ([]Agent, error)
	Update(ctx context.Context, principalID, businessID, agentID uuid.UUID, in UpdateAgentInput) (Agent, error)
	Delete(ctx context.Context, principalID, businessID, agentID uuid.UUID) error
}

// Compile-time check: *AgentService must satisfy agentCRUD.
var _ agentCRUD = (*AgentService)(nil)

// Handler exposes agent-definition CRUD over HTTP. Mounted behind the
// agents.configure RequirePermission gate (so a lacking perm / invisible business
// is a no-oracle 404).
type Handler struct {
	svc    agentCRUD
	tools  toolLister  // optional, late-wired via SetMetadata; backs /agents/tools
	models modelLister // optional, late-wired via SetMetadata; backs /agents/models
}

// NewHandler builds an agents HTTP handler.
func NewHandler(svc agentCRUD) *Handler { return &Handler{svc: svc} }

// SetMetadata late-wires the tool registry + model catalog that back the
// /agents/tools and /agents/models read endpoints. Optional — so plain
// NewHandler(svc) keeps working.
func (h *Handler) SetMetadata(tools toolLister, models modelLister) {
	h.tools = tools
	h.models = models
}

// ProtectedRoutes mounts authenticated agent endpoints under a business.
func (h *Handler) ProtectedRoutes(r chi.Router) {
	r.Route("/businesses/{id}/agents", func(r chi.Router) {
		r.Get("/", h.listAgents)
		r.Post("/", h.createAgent)
		// Static routes must be registered before /{agentID} (chi matches static
		// segments before params, so "tools"/"models" never hit the agent lookup).
		r.Get("/tools", h.listTools)
		r.Get("/models", h.listModels)
		r.Get("/{agentID}", h.getAgent)
		r.Patch("/{agentID}", h.updateAgent)
		r.Delete("/{agentID}", h.deleteAgent)
	})
}

func agentBusinessID(r *http.Request) (uuid.UUID, error) { return uuid.Parse(chi.URLParam(r, "id")) }
func agentPathID(r *http.Request) (uuid.UUID, error)     { return uuid.Parse(chi.URLParam(r, "agentID")) }

// agentResp is the OpenAPI Agent response shape.
type agentResp struct {
	ID                 string      `json:"id"`
	BusinessID         string      `json:"business_id"`
	PrincipalID        string      `json:"principal_id"`
	Name               string      `json:"name"`
	Provider           string      `json:"provider"`
	Model              string      `json:"model"`
	SystemPrompt       string      `json:"system_prompt"`
	AllowedTools       []string    `json:"allowed_tools"`
	AutonomyMode       int         `json:"autonomy_mode"`
	Enabled            bool        `json:"enabled"`
	MonthlyBudgetCents int         `json:"monthly_budget_cents"`
	AllowedMCPServers  []uuid.UUID `json:"allowed_mcp_servers"`
	RetriageOnReply    bool        `json:"retriage_on_reply"`
	WebAllowedDomains  []string    `json:"web_allowed_domains"`
	CreatedAt          time.Time   `json:"created_at"`
	UpdatedAt          time.Time   `json:"updated_at"`
}

func toAgentResp(a Agent) agentResp {
	tools := a.AllowedTools
	if tools == nil {
		tools = []string{}
	}
	mcpServers := a.AllowedMCPServers
	if mcpServers == nil {
		mcpServers = []uuid.UUID{}
	}
	webDomains := a.WebAllowedDomains
	if webDomains == nil {
		webDomains = []string{}
	}
	return agentResp{
		ID: a.ID.String(), BusinessID: a.BusinessID.String(), PrincipalID: a.PrincipalID.String(),
		Name: a.Name, Provider: a.Provider, Model: a.Model, SystemPrompt: a.SystemPrompt,
		AllowedTools: tools, AutonomyMode: a.AutonomyMode, Enabled: a.Enabled,
		MonthlyBudgetCents: a.MonthlyBudgetCents, AllowedMCPServers: mcpServers,
		RetriageOnReply:   a.RetriageOnReply,
		WebAllowedDomains: webDomains,
		CreatedAt:         a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
}

func (h *Handler) listAgents(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := agentBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	agents, err := h.svc.List(r.Context(), pid, bid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]agentResp, 0, len(agents))
	for _, a := range agents {
		out = append(out, toAgentResp(a))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *Handler) createAgent(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := agentBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	// autonomy_mode and enabled default (1 / true) when omitted, matching the DB
	// defaults — pointer fields distinguish "omitted" from an explicit value.
	var in struct {
		Name               string      `json:"name"`
		Provider           string      `json:"provider"`
		Model              string      `json:"model"`
		SystemPrompt       string      `json:"system_prompt"`
		AllowedTools       []string    `json:"allowed_tools"`
		AutonomyMode       *int        `json:"autonomy_mode"`
		Enabled            *bool       `json:"enabled"`
		MonthlyBudgetCents int         `json:"monthly_budget_cents"`
		AllowedMCPServers  []uuid.UUID `json:"allowed_mcp_servers"`
		RetriageOnReply    *bool       `json:"retriage_on_reply"`
		WebAllowedDomains  []string    `json:"web_allowed_domains"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	mode := 1
	if in.AutonomyMode != nil {
		mode = *in.AutonomyMode
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	retriageOnReply := false
	if in.RetriageOnReply != nil {
		retriageOnReply = *in.RetriageOnReply
	}
	created, err := h.svc.Create(r.Context(), pid, bid, CreateAgentInput{
		Name: in.Name, Provider: in.Provider, Model: in.Model, SystemPrompt: in.SystemPrompt,
		AllowedTools: in.AllowedTools, AutonomyMode: mode, Enabled: enabled,
		MonthlyBudgetCents: in.MonthlyBudgetCents, AllowedMCPServers: in.AllowedMCPServers,
		RetriageOnReply:   retriageOnReply,
		WebAllowedDomains: in.WebAllowedDomains,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toAgentResp(created))
}

func (h *Handler) getAgent(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := agentBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	aid, err := agentPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	a, err := h.svc.Get(r.Context(), pid, bid, aid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toAgentResp(a))
}

func (h *Handler) updateAgent(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := agentBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	aid, err := agentPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	// Pointer fields distinguish "absent" from "set" for PATCH semantics.
	var in struct {
		Name               *string      `json:"name"`
		Model              *string      `json:"model"`
		SystemPrompt       *string      `json:"system_prompt"`
		AllowedTools       *[]string    `json:"allowed_tools"`
		AutonomyMode       *int         `json:"autonomy_mode"`
		Enabled            *bool        `json:"enabled"`
		MonthlyBudgetCents *int         `json:"monthly_budget_cents"`
		AllowedMCPServers  *[]uuid.UUID `json:"allowed_mcp_servers"`
		RetriageOnReply    *bool        `json:"retriage_on_reply"`
		WebAllowedDomains  *[]string    `json:"web_allowed_domains"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	a, err := h.svc.Update(r.Context(), pid, bid, aid, UpdateAgentInput{
		Name: in.Name, Model: in.Model, SystemPrompt: in.SystemPrompt,
		AllowedTools: in.AllowedTools, AutonomyMode: in.AutonomyMode,
		Enabled: in.Enabled, MonthlyBudgetCents: in.MonthlyBudgetCents,
		AllowedMCPServers: in.AllowedMCPServers, RetriageOnReply: in.RetriageOnReply,
		WebAllowedDomains: in.WebAllowedDomains,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toAgentResp(a))
}

func (h *Handler) deleteAgent(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := agentBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	aid, err := agentPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if err := h.svc.Delete(r.Context(), pid, bid, aid); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// toolResp is the metadata projection of a tool for the agent form's tool picker.
type toolResp struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Effect       string `json:"effect"`
	RequiredPerm string `json:"required_perm,omitempty"`
}

// modelResp is the metadata projection of a catalog model for the model picker.
type modelResp struct {
	Provider string `json:"provider"`
	ModelID  string `json:"model_id"`
}

// listTools returns the catalog of tools available to agents. Nil-guarded: a
// plain NewHandler(svc) without SetMetadata returns 404 here.
func (h *Handler) listTools(w http.ResponseWriter, r *http.Request) {
	if h.tools == nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	all := h.tools.All()
	out := make([]toolResp, 0, len(all))
	for _, t := range all {
		out = append(out, toolResp{Name: t.Name, Description: t.Description, Effect: t.Effect.String(), RequiredPerm: t.RequiredPerm})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

// listModels returns the catalog of models available to agents. Nil-guarded: a
// plain NewHandler(svc) without SetMetadata returns 404 here.
func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	if h.models == nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	models, err := h.models.ListModels(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]modelResp, 0, len(models))
	for _, m := range models {
		// modelResp is the JSON-tagged view of ModelInfo (identical shape).
		out = append(out, modelResp(m))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}
