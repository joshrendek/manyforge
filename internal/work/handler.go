package work

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// Handler exposes the management Work Board API. Routes are thin: parse + validate input, call
// the service, write JSON. Authorization (work.read / work.write) is enforced by
// RequirePermission middleware in cmd/manyforge, plus the service-layer tenant_root predicate
// under RLS.
type Handler struct {
	svc *Service
}

// NewHandler builds the management Work Board handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// ReadRoutes mounts the endpoints gated on work.read.
func (h *Handler) ReadRoutes(r chi.Router) {
	r.Get("/businesses/{id}/work/summary", h.summary)
	r.Get("/businesses/{id}/work/projects", h.listProjects)
	r.Get("/businesses/{id}/work/projects/{pid}", h.getProject)
	r.Get("/businesses/{id}/work/projects/{pid}/tasks", h.listTasks)
}

// WriteRoutes mounts the endpoints gated on work.write: mutations and token management.
func (h *Handler) WriteRoutes(r chi.Router) {
	r.Post("/businesses/{id}/work/projects", h.createProject)
	r.Post("/businesses/{id}/work/projects/{pid}/tasks", h.createTask)
	r.Patch("/businesses/{id}/work/projects/{pid}/tasks/{key}", h.updateTask)
	r.Get("/businesses/{id}/work/projects/{pid}/tokens", h.listTokens)
	r.Post("/businesses/{id}/work/projects/{pid}/tokens", h.createToken)
	r.Delete("/businesses/{id}/work/projects/{pid}/tokens/{tid}", h.revokeToken)
}

type createProjectBody struct {
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	KeyPrefix string `json:"key_prefix"`
}

type updateTaskBody struct {
	TaskFields
	Note *string `json:"note"`
}

type createTokenBody struct {
	Name string `json:"name"`
}

// caller returns the authenticated principal and the {id} business; ok=false has already
// written the uniform 404.
func caller(w http.ResponseWriter, r *http.Request) (principalID, businessID uuid.UUID, ok bool) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errNotFound)
		return uuid.Nil, uuid.Nil, false
	}
	bid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, errNotFound)
		return uuid.Nil, uuid.Nil, false
	}
	return pid, bid, true
}

func (h *Handler) summary(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := caller(w, r)
	if !ok {
		return
	}
	s, err := h.svc.Summary(r.Context(), pid, bid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s)
}

func (h *Handler) listProjects(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := caller(w, r)
	if !ok {
		return
	}
	items, err := h.svc.ListProjects(r.Context(), pid, bid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) createProject(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := caller(w, r)
	if !ok {
		return
	}
	var body createProjectBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	p, err := h.svc.CreateProject(r.Context(), pid, bid, ProjectInput(body))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, p)
}

func (h *Handler) getProject(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := caller(w, r)
	if !ok {
		return
	}
	p, err := h.svc.GetProject(r.Context(), pid, bid, chi.URLParam(r, "pid"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

func (h *Handler) listTasks(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := caller(w, r)
	if !ok {
		return
	}
	includeClosed := r.URL.Query().Get("include_closed") == "true"
	items, err := h.svc.ListTasks(r.Context(), pid, bid, chi.URLParam(r, "pid"), includeClosed)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) createTask(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := caller(w, r)
	if !ok {
		return
	}
	var body TaskFields
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	t, err := h.svc.CreateTask(r.Context(), pid, bid, chi.URLParam(r, "pid"), body)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, t)
}

func (h *Handler) updateTask(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := caller(w, r)
	if !ok {
		return
	}
	key := chi.URLParam(r, "key")
	if !keyRe.MatchString(key) {
		httpx.WriteError(w, r, errNotFound)
		return
	}
	var body updateTaskBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	t, err := h.svc.UpdateTask(r.Context(), pid, bid, chi.URLParam(r, "pid"), key, body.TaskFields, body.Note)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, t)
}

func (h *Handler) listTokens(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := caller(w, r)
	if !ok {
		return
	}
	items, err := h.svc.ListTokens(r.Context(), pid, bid, chi.URLParam(r, "pid"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) createToken(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := caller(w, r)
	if !ok {
		return
	}
	var body createTokenBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	t, err := h.svc.CreateToken(r.Context(), pid, bid, chi.URLParam(r, "pid"), body.Name)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, t)
}

func (h *Handler) revokeToken(w http.ResponseWriter, r *http.Request) {
	pid, bid, ok := caller(w, r)
	if !ok {
		return
	}
	tid, err := uuid.Parse(chi.URLParam(r, "tid"))
	if err != nil {
		httpx.WriteError(w, r, errNotFound)
		return
	}
	if err := h.svc.RevokeToken(r.Context(), pid, bid, chi.URLParam(r, "pid"), tid); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
