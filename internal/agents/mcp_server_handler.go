package agents

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// mcpServerCRUD is the subset of MCPServerService the handler needs (an interface
// so handler tests can inject a fake). *MCPServerService satisfies it.
type mcpServerCRUD interface {
	Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateMCPServerInput) (uuid.UUID, error)
	Get(ctx context.Context, principalID, businessID, id uuid.UUID) (dbgen.McpServer, error)
	List(ctx context.Context, principalID, businessID uuid.UUID) ([]dbgen.McpServer, error)
	Update(ctx context.Context, principalID, businessID, id uuid.UUID, in UpdateMCPServerInput) (dbgen.McpServer, error)
	Delete(ctx context.Context, principalID, businessID, id uuid.UUID) error
}

// Compile-time check: *MCPServerService must satisfy mcpServerCRUD.
var _ mcpServerCRUD = (*MCPServerService)(nil)

// MCPServerHandler exposes MCP server CRUD over HTTP. Mounted behind the
// agents.configure RequirePermission gate (so a lacking perm / invisible business
// is a no-oracle 404).
type MCPServerHandler struct{ svc mcpServerCRUD }

// NewMCPServerHandler builds an MCP server HTTP handler.
func NewMCPServerHandler(svc mcpServerCRUD) *MCPServerHandler {
	return &MCPServerHandler{svc: svc}
}

// ProtectedRoutes mounts authenticated MCP server endpoints under a business.
func (h *MCPServerHandler) ProtectedRoutes(r chi.Router) {
	r.Route("/businesses/{id}/mcp_servers", func(r chi.Router) {
		r.Get("/", h.listMCPServers)
		r.Post("/", h.createMCPServer)
		r.Get("/{serverID}", h.getMCPServer)
		r.Patch("/{serverID}", h.updateMCPServer)
		r.Delete("/{serverID}", h.deleteMCPServer)
	})
}

func mcpBusinessID(r *http.Request) (uuid.UUID, error) { return uuid.Parse(chi.URLParam(r, "id")) }
func mcpPathID(r *http.Request) (uuid.UUID, error)     { return uuid.Parse(chi.URLParam(r, "serverID")) }

// mcpServerResp is the OpenAPI MCPServer response shape.
// CRITICAL: sealed_auth_ref and auth_token are deliberately omitted — auth is write-only.
type mcpServerResp struct {
	ID         string    `json:"id"`
	BusinessID string    `json:"business_id"`
	Name       string    `json:"name"`
	URL        string    `json:"url"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func toMCPServerResp(m dbgen.McpServer) mcpServerResp {
	return mcpServerResp{
		ID:         m.ID.String(),
		BusinessID: m.BusinessID.String(),
		Name:       m.Name,
		URL:        m.Url,
		Enabled:    m.Enabled,
		CreatedAt:  m.CreatedAt,
		UpdatedAt:  m.UpdatedAt,
	}
}

func (h *MCPServerHandler) listMCPServers(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := mcpBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	servers, err := h.svc.List(r.Context(), pid, bid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]mcpServerResp, 0, len(servers))
	for _, m := range servers {
		out = append(out, toMCPServerResp(m))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *MCPServerHandler) createMCPServer(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := mcpBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	// enabled defaults to true when omitted; pointer field distinguishes "omitted"
	// from an explicit false.
	var in struct {
		Name      string `json:"name"`
		URL       string `json:"url"`
		AuthToken string `json:"auth_token"`
		Enabled   *bool  `json:"enabled"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	id, err := h.svc.Create(r.Context(), pid, bid, CreateMCPServerInput{
		Name:      in.Name,
		URL:       in.URL,
		AuthToken: in.AuthToken,
		Enabled:   enabled,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	// Fetch the full row to return the canonical response shape.
	created, err := h.svc.Get(r.Context(), pid, bid, id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toMCPServerResp(created))
}

func (h *MCPServerHandler) getMCPServer(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := mcpBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	sid, err := mcpPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	m, err := h.svc.Get(r.Context(), pid, bid, sid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toMCPServerResp(m))
}

func (h *MCPServerHandler) updateMCPServer(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := mcpBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	sid, err := mcpPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	// Pointer fields distinguish "absent" from "set" for PATCH semantics.
	var in struct {
		Name      *string `json:"name"`
		URL       *string `json:"url"`
		AuthToken *string `json:"auth_token"`
		Enabled   *bool   `json:"enabled"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	m, err := h.svc.Update(r.Context(), pid, bid, sid, UpdateMCPServerInput{
		Name:      in.Name,
		URL:       in.URL,
		AuthToken: in.AuthToken,
		Enabled:   in.Enabled,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toMCPServerResp(m))
}

func (h *MCPServerHandler) deleteMCPServer(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := mcpBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	sid, err := mcpPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if err := h.svc.Delete(r.Context(), pid, bid, sid); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
