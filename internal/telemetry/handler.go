package telemetry

// handler.go — the authenticated admin surface for registering telemetry clients. Handlers stay
// thin: parse, delegate to the service, map typed sentinels. All ownership enforcement lives in
// the service's SQL predicates, never here.

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// Handler serves the authenticated telemetry-client routes.
type Handler struct{ Svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{Svc: svc} }

// ReadRoutes mounts the authenticated read endpoints (gated on telemetry.read by the caller).
func (h *Handler) ReadRoutes(r chi.Router) {
	r.Get("/businesses/{id}/telemetry/clients", h.list)
}

// WriteRoutes mounts the authenticated write endpoints (gated on telemetry.write by the caller).
func (h *Handler) WriteRoutes(r chi.Router) {
	r.Post("/businesses/{id}/telemetry/clients", h.create)
	r.Put("/businesses/{id}/telemetry/clients/{clientID}/allowed-origins", h.setAllowedOrigins)
	r.Post("/businesses/{id}/telemetry/clients/{clientID}/revoke", h.revoke)
	r.Get("/businesses/{id}/telemetry/clients/{clientID}/move-targets", h.moveTargets)
	r.Post("/businesses/{id}/telemetry/clients/{clientID}/move", h.move)
}

type createClientRequest struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// RequireSignature defaults to false — the embeddable-SDK mode. Set it only for a
	// server-to-server sender that can hold an mfs_ secret safely.
	RequireSignature bool `json:"require_signature"`
	// AllowedOrigins is required for analytics and rejected for crash clients.
	AllowedOrigins []string `json:"allowed_origins"`
}

type setAllowedOriginsRequest struct {
	AllowedOrigins []string `json:"allowed_origins"`
}

type moveClientRequest struct {
	TargetBusinessID string `json:"target_business_id"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	principalID, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	businessID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_business_id"})
		return
	}
	var req createClientRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	client, err := h.Svc.CreateClient(
		r.Context(), principalID, businessID, req.Kind, req.Name, req.RequireSignature,
		req.AllowedOrigins,
	)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, client)
}

func (h *Handler) setAllowedOrigins(w http.ResponseWriter, r *http.Request) {
	principalID, businessID, clientID, ok := moveRouteIDs(w, r)
	if !ok {
		return
	}
	var req setAllowedOriginsRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	client, err := h.Svc.SetAllowedOrigins(
		r.Context(), principalID, businessID, clientID, req.AllowedOrigins,
	)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, client)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	principalID, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	businessID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_business_id"})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	clients, err := h.Svc.ListClients(r.Context(), principalID, businessID, limit)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"clients": clients})
}

func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) {
	principalID, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	businessID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_business_id"})
		return
	}
	clientID, err := uuid.Parse(chi.URLParam(r, "clientID"))
	if err != nil {
		// Same shape as a well-formed unknown id: a parse failure must not be distinguishable
		// from "no such client".
		httpx.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	client, err := h.Svc.RevokeClient(r.Context(), principalID, businessID, clientID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, client)
}

func (h *Handler) moveTargets(w http.ResponseWriter, r *http.Request) {
	principalID, sourceBusinessID, clientID, ok := moveRouteIDs(w, r)
	if !ok {
		return
	}
	targets, err := h.Svc.MoveTargets(r.Context(), principalID, sourceBusinessID, clientID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"targets": targets})
}

func (h *Handler) move(w http.ResponseWriter, r *http.Request) {
	principalID, sourceBusinessID, clientID, ok := moveRouteIDs(w, r)
	if !ok {
		return
	}
	var req moveClientRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	targetBusinessID, err := uuid.Parse(req.TargetBusinessID)
	if err != nil {
		// Malformed, missing, invisible, and unauthorized targets share one response.
		httpx.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	client, err := h.Svc.MoveClient(
		r.Context(), principalID, sourceBusinessID, clientID, targetBusinessID,
	)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, client)
}

func moveRouteIDs(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, uuid.UUID, bool) {
	principalID, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	sourceBusinessID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_business_id"})
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	clientID, err := uuid.Parse(chi.URLParam(r, "clientID"))
	if err != nil {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	return principalID, sourceBusinessID, clientID, true
}
