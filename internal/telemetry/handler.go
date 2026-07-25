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

// Routes mounts the admin surface. The caller supplies the permission middleware.
func (h *Handler) Routes(r chi.Router) {
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Post("/{clientID}/revoke", h.revoke)
}

type createClientRequest struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	principalID, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	businessID, err := uuid.Parse(chi.URLParam(r, "businessID"))
	if err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_business_id"})
		return
	}
	var req createClientRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	client, err := h.Svc.CreateClient(r.Context(), principalID, businessID, req.Kind, req.Name)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, client)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	principalID, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	businessID, err := uuid.Parse(chi.URLParam(r, "businessID"))
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
	businessID, err := uuid.Parse(chi.URLParam(r, "businessID"))
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
