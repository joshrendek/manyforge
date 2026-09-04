package automations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

type Handler struct{ service *Service }

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) ReadRoutes(router chi.Router) {
	router.Get("/businesses/{id}/mailing/automations", h.list)
	router.Get("/businesses/{id}/mailing/automations/{aid}", h.get)
	router.Get("/businesses/{id}/mailing/automations/{aid}/versions", h.versions)
	router.Get("/businesses/{id}/mailing/automations/{aid}/versions/{vid}", h.version)
	router.Get("/businesses/{id}/mailing/automations/{aid}/enrollments", h.enrollments)
	router.Get("/businesses/{id}/mailing/automations/{aid}/enrollments/{eid}", h.enrollment)
	router.Get("/businesses/{id}/mailing/automations/{aid}/stats", h.stats)
}

func (h *Handler) WriteRoutes(router chi.Router) {
	router.Post("/businesses/{id}/mailing/automations", h.create)
	router.Patch("/businesses/{id}/mailing/automations/{aid}", h.update)
	router.Post("/businesses/{id}/mailing/automations/{aid}/versions", h.cloneVersion)
	router.Put("/businesses/{id}/mailing/automations/{aid}/versions/{vid}/graph", h.putGraph)
	router.Post("/businesses/{id}/mailing/automations/{aid}/versions/{vid}/validate", h.validate)
	router.Post("/businesses/{id}/mailing/automations/{aid}/pause", h.pause)
	router.Post("/businesses/{id}/mailing/automations/{aid}/archive", h.archive)
	router.Post("/businesses/{id}/mailing/automations/{aid}/enrollments", h.enroll)
	router.Post("/businesses/{id}/mailing/automations/{aid}/enrollments/{eid}/exit", h.exitEnrollment)
	router.Post("/businesses/{id}/mailing/events", h.createEvent)
}

func (h *Handler) SendRoutes(router chi.Router) {
	router.Post("/businesses/{id}/mailing/automations/{aid}/versions/{vid}/activate", h.activate)
	router.Post("/businesses/{id}/mailing/automations/{aid}/resume", h.resume)
}

type nullableString struct {
	Set   bool
	Value *string
}

func (value *nullableString) UnmarshalJSON(raw []byte) error {
	value.Set = true
	if bytes.Equal(raw, []byte("null")) {
		value.Value = nil
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	value.Value = &text
	return nil
}

type automationBody struct {
	Name          *string        `json:"name"`
	Description   nullableString `json:"description"`
	AllowReenroll *bool          `json:"allow_reenroll"`
}

type invalidGraphBody struct {
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Issues  []Issue `json:"issues"`
}

func requestIDs(w http.ResponseWriter, request *http.Request, names ...string) (uuid.UUID, []uuid.UUID, bool) {
	principalID, ok := httpx.PrincipalFromContext(request.Context())
	if !ok {
		httpx.WriteError(w, request, errs.ErrNotFound)
		return uuid.Nil, nil, false
	}
	ids := make([]uuid.UUID, len(names))
	for index, name := range names {
		id, err := uuid.Parse(chi.URLParam(request, name))
		if err != nil {
			httpx.WriteError(w, request, errs.ErrNotFound)
			return uuid.Nil, nil, false
		}
		ids[index] = id
	}
	return principalID, ids, true
}

func write(w http.ResponseWriter, request *http.Request, status int, value any, err error) {
	if err != nil {
		var invalid *InvalidGraphError
		if errors.As(err, &invalid) {
			httpx.WriteJSON(w, http.StatusUnprocessableEntity, invalidGraphBody{Code: "AUTOMATION_INVALID", Message: "automation graph is invalid", Issues: invalid.Issues})
			return
		}
		httpx.WriteError(w, request, err)
		return
	}
	httpx.WriteJSON(w, status, value)
}

func (h *Handler) list(w http.ResponseWriter, request *http.Request) {
	principalID, ids, ok := requestIDs(w, request, "id")
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
	value, err := h.service.List(request.Context(), principalID, ids[0], request.URL.Query().Get("cursor"), limit)
	write(w, request, http.StatusOK, value, err)
}

func (h *Handler) get(w http.ResponseWriter, request *http.Request) {
	principalID, ids, ok := requestIDs(w, request, "id", "aid")
	if !ok {
		return
	}
	value, err := h.service.Get(request.Context(), principalID, ids[0], ids[1])
	write(w, request, http.StatusOK, value, err)
}

func (h *Handler) create(w http.ResponseWriter, request *http.Request) {
	principalID, ids, ok := requestIDs(w, request, "id")
	if !ok {
		return
	}
	var body automationBody
	if !httpx.DecodeJSON(w, request, &body) {
		return
	}
	name := ""
	if body.Name != nil {
		name = *body.Name
	}
	reenroll := false
	if body.AllowReenroll != nil {
		reenroll = *body.AllowReenroll
	}
	value, err := h.service.Create(request.Context(), principalID, ids[0], CreateInput{Name: name, Description: body.Description.Value, AllowReenroll: reenroll})
	write(w, request, http.StatusCreated, value, err)
}

func (h *Handler) update(w http.ResponseWriter, request *http.Request) {
	principalID, ids, ok := requestIDs(w, request, "id", "aid")
	if !ok {
		return
	}
	var body automationBody
	if !httpx.DecodeJSON(w, request, &body) {
		return
	}
	value, err := h.service.Update(request.Context(), principalID, ids[0], ids[1], UpdateInput{Name: body.Name, SetDescription: body.Description.Set, Description: body.Description.Value, AllowReenroll: body.AllowReenroll})
	write(w, request, http.StatusOK, value, err)
}

func (h *Handler) versions(w http.ResponseWriter, request *http.Request) {
	principalID, ids, ok := requestIDs(w, request, "id", "aid")
	if !ok {
		return
	}
	value, err := h.service.Versions(request.Context(), principalID, ids[0], ids[1])
	write(w, request, http.StatusOK, map[string]any{"items": value}, err)
}

func (h *Handler) version(w http.ResponseWriter, request *http.Request) {
	principalID, ids, ok := requestIDs(w, request, "id", "aid", "vid")
	if !ok {
		return
	}
	value, err := h.service.Version(request.Context(), principalID, ids[0], ids[1], ids[2])
	write(w, request, http.StatusOK, value, err)
}

func (h *Handler) cloneVersion(w http.ResponseWriter, request *http.Request) {
	principalID, ids, ok := requestIDs(w, request, "id", "aid")
	if !ok {
		return
	}
	value, err := h.service.CloneVersion(request.Context(), principalID, ids[0], ids[1])
	write(w, request, http.StatusCreated, value, err)
}

func (h *Handler) putGraph(w http.ResponseWriter, request *http.Request) {
	principalID, ids, ok := requestIDs(w, request, "id", "aid", "vid")
	if !ok {
		return
	}
	var graph Graph
	if !httpx.DecodeJSON(w, request, &graph) {
		return
	}
	value, err := h.service.PutGraph(request.Context(), principalID, ids[0], ids[1], ids[2], graph)
	write(w, request, http.StatusOK, value, err)
}

func (h *Handler) validate(w http.ResponseWriter, request *http.Request) {
	principalID, ids, ok := requestIDs(w, request, "id", "aid", "vid")
	if !ok {
		return
	}
	value, err := h.service.ValidateVersion(request.Context(), principalID, ids[0], ids[1], ids[2])
	write(w, request, http.StatusOK, value, err)
}

func (h *Handler) activate(w http.ResponseWriter, request *http.Request) {
	principalID, ids, ok := requestIDs(w, request, "id", "aid", "vid")
	if !ok {
		return
	}
	value, err := h.service.Activate(request.Context(), principalID, ids[0], ids[1], ids[2])
	write(w, request, http.StatusOK, value, err)
}

func (h *Handler) pause(w http.ResponseWriter, request *http.Request) {
	h.transition(w, request, (*Service).Pause)
}

func (h *Handler) resume(w http.ResponseWriter, request *http.Request) {
	h.transition(w, request, (*Service).Resume)
}

func (h *Handler) archive(w http.ResponseWriter, request *http.Request) {
	h.transition(w, request, (*Service).Archive)
}

func (h *Handler) transition(w http.ResponseWriter, request *http.Request, operation func(*Service, context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (Automation, error)) {
	principalID, ids, ok := requestIDs(w, request, "id", "aid")
	if !ok {
		return
	}
	value, err := operation(h.service, request.Context(), principalID, ids[0], ids[1])
	write(w, request, http.StatusOK, value, err)
}

func (h *Handler) enrollments(w http.ResponseWriter, request *http.Request) {
	principalID, ids, ok := requestIDs(w, request, "id", "aid")
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
	value, err := h.service.ListEnrollments(request.Context(), principalID, ids[0], ids[1], EnrollmentFilter{
		Status: request.URL.Query().Get("status"), NodeID: request.URL.Query().Get("node_id"),
		Cursor: request.URL.Query().Get("cursor"), Limit: limit,
	})
	write(w, request, http.StatusOK, value, err)
}

func (h *Handler) enrollment(w http.ResponseWriter, request *http.Request) {
	principalID, ids, ok := requestIDs(w, request, "id", "aid", "eid")
	if !ok {
		return
	}
	value, err := h.service.GetEnrollment(request.Context(), principalID, ids[0], ids[1], ids[2])
	write(w, request, http.StatusOK, value, err)
}

func (h *Handler) enroll(w http.ResponseWriter, request *http.Request) {
	principalID, ids, ok := requestIDs(w, request, "id", "aid")
	if !ok {
		return
	}
	var body struct {
		SubscriberID uuid.UUID `json:"subscriber_id"`
	}
	if !httpx.DecodeJSON(w, request, &body) {
		return
	}
	if body.SubscriberID == uuid.Nil {
		write(w, request, http.StatusBadRequest, nil, validation("subscriber_id is required"))
		return
	}
	value, err := h.service.Enroll(request.Context(), principalID, ids[0], ids[1], body.SubscriberID)
	write(w, request, http.StatusCreated, value, err)
}

func (h *Handler) exitEnrollment(w http.ResponseWriter, request *http.Request) {
	principalID, ids, ok := requestIDs(w, request, "id", "aid", "eid")
	if !ok {
		return
	}
	value, err := h.service.ExitEnrollment(request.Context(), principalID, ids[0], ids[1], ids[2])
	write(w, request, http.StatusOK, value, err)
}

func (h *Handler) stats(w http.ResponseWriter, request *http.Request) {
	principalID, ids, ok := requestIDs(w, request, "id", "aid")
	if !ok {
		return
	}
	var versionID *uuid.UUID
	if raw := request.URL.Query().Get("version_id"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			write(w, request, http.StatusBadRequest, nil, validation("invalid version_id"))
			return
		}
		versionID = &parsed
	}
	value, err := h.service.Stats(request.Context(), principalID, ids[0], ids[1], versionID)
	write(w, request, http.StatusOK, value, err)
}

func (h *Handler) createEvent(w http.ResponseWriter, request *http.Request) {
	principalID, ids, ok := requestIDs(w, request, "id")
	if !ok {
		return
	}
	var body EventInput
	if !httpx.DecodeJSON(w, request, &body) {
		return
	}
	value, err := h.service.CreateEvent(request.Context(), principalID, ids[0], body)
	status := http.StatusOK
	if value.Created {
		status = http.StatusCreated
	}
	write(w, request, status, value, err)
}
