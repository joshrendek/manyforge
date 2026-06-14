package agents

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// mcpToolPolicyCRUD is the handler's view of the policy service (fakeable).
type mcpToolPolicyCRUD interface {
	List(ctx context.Context, principalID, businessID, serverID uuid.UUID) ([]MCPToolPolicy, error)
	Upsert(ctx context.Context, principalID, businessID, serverID uuid.UUID, toolName, effect string) (MCPToolPolicy, error)
	Delete(ctx context.Context, principalID, businessID, serverID uuid.UUID, toolName string) error
}

// toolDiscoverer is the handler's view of the MCP host (fakeable).
type toolDiscoverer interface {
	DiscoverServerToolDefs(ctx context.Context, principalID, businessID, serverID uuid.UUID) ([]DiscoveredToolDef, bool, error)
}

// MCPToolPolicyHandler serves the per-tool policy + discovery endpoints. Mounted nested under
// /businesses/{id}/mcp_servers/{serverID} (so it shares the agents.configure gate).
type MCPToolPolicyHandler struct {
	policies   mcpToolPolicyCRUD
	discoverer toolDiscoverer
}

func NewMCPToolPolicyHandler(p mcpToolPolicyCRUD, d toolDiscoverer) *MCPToolPolicyHandler {
	return &MCPToolPolicyHandler{policies: p, discoverer: d}
}

// Mount registers the nested routes on a router already scoped to /{serverID}.
func (h *MCPToolPolicyHandler) Mount(r chi.Router) {
	r.Get("/tools", h.listTools)
	r.Get("/tool_policies", h.listPolicies)
	r.Put("/tool_policies/{toolName}", h.putPolicy)
	r.Delete("/tool_policies/{toolName}", h.deletePolicy)
}

type toolDefResp struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Effect      string `json:"effect"` // read | reversible | external
}
type discoverResp struct {
	Reachable bool          `json:"reachable"`
	Tools     []toolDefResp `json:"tools"`
}
type policyResp struct {
	ToolName string `json:"tool_name"`
	Effect   string `json:"effect"`
}

func (h *MCPToolPolicyHandler) listTools(w http.ResponseWriter, r *http.Request) {
	pid, bid, sid, ok := h.ids(w, r)
	if !ok {
		return
	}
	policies, err := h.policies.List(r.Context(), pid, bid, sid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	byName := map[string]string{}
	for _, p := range policies {
		byName[p.ToolName] = p.Effect
	}
	defs, reachable, err := h.discoverer.DiscoverServerToolDefs(r.Context(), pid, bid, sid)
	if err != nil {
		httpx.WriteError(w, r, err) // ErrNotFound → 404 for a foreign/unknown server
		return
	}
	resp := discoverResp{Reachable: reachable, Tools: []toolDefResp{}}
	for _, d := range defs {
		eff := byName[d.Name]
		if eff == "" {
			eff = "external"
		}
		resp.Tools = append(resp.Tools, toolDefResp{Name: d.Name, Description: d.Description, Effect: eff})
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (h *MCPToolPolicyHandler) listPolicies(w http.ResponseWriter, r *http.Request) {
	pid, bid, sid, ok := h.ids(w, r)
	if !ok {
		return
	}
	policies, err := h.policies.List(r.Context(), pid, bid, sid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := []policyResp{}
	for _, p := range policies {
		out = append(out, policyResp{ToolName: p.ToolName, Effect: p.Effect})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *MCPToolPolicyHandler) putPolicy(w http.ResponseWriter, r *http.Request) {
	pid, bid, sid, ok := h.ids(w, r)
	if !ok {
		return
	}
	toolName := chi.URLParam(r, "toolName")
	var in struct {
		Effect string `json:"effect"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	p, err := h.policies.Upsert(r.Context(), pid, bid, sid, toolName, in.Effect)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, policyResp{ToolName: p.ToolName, Effect: p.Effect})
}

func (h *MCPToolPolicyHandler) deletePolicy(w http.ResponseWriter, r *http.Request) {
	pid, bid, sid, ok := h.ids(w, r)
	if !ok {
		return
	}
	if err := h.policies.Delete(r.Context(), pid, bid, sid, chi.URLParam(r, "toolName")); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ids extracts the principal + business + server ids; any failure is a no-oracle 404.
func (h *MCPToolPolicyHandler) ids(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, uuid.UUID, bool) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	bid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	sid, err := uuid.Parse(chi.URLParam(r, "serverID"))
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	return pid, bid, sid, true
}
