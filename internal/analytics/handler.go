package analytics

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// defaultRangeDays is the window a dashboard opens on when none is requested.
const defaultRangeDays = 30

// Handler serves the authenticated analytics read surface, gated on telemetry.read by the caller.
type Handler struct{ Svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{Svc: svc} }

// ReadRoutes mounts the dashboard's read endpoints.
func (h *Handler) ReadRoutes(r chi.Router) {
	r.Get("/businesses/{id}/analytics/summary", h.summary)
	r.Get("/businesses/{id}/analytics/sites/{clientID}/property-rules", h.listPropertyRules)
}

// WriteRoutes mounts analytics configuration endpoints under the caller's telemetry.write gate.
func (h *Handler) WriteRoutes(r chi.Router) {
	r.Put("/businesses/{id}/analytics/sites/{clientID}/property-rules", h.replacePropertyRules)
}

// OverviewRoutes mounts the cross-business overview. It is deliberately NOT mounted with the other
// read routes, because those sit behind RequirePermission(..., businessIDFromPath) and this path
// carries no business id — that middleware would find nothing to resolve and 404 every request.
//
// The permission check is not skipped, it MOVES: Service.Overview filters to businesses where the
// caller holds telemetry.read, in SQL, via businesses_with_permission(). Expressing it there rather
// than in middleware is what makes a multi-business read possible at all, since the middleware form
// can only answer for one business at a time.
func (h *Handler) OverviewRoutes(r chi.Router) {
	r.Get("/analytics/overview", h.overview)
}

// rangeDays parses the shared ?days= parameter into an inclusive UTC day window ending today.
func rangeDays(r *http.Request) (from, to time.Time, err error) {
	days := defaultRangeDays
	if d := r.URL.Query().Get("days"); d != "" {
		n, cerr := strconv.Atoi(d)
		if cerr != nil || n < 1 || n > maxRangeDays {
			return time.Time{}, time.Time{}, errs.ErrValidation
		}
		days = n
	}
	to = time.Now().UTC().Truncate(24 * time.Hour)
	return to.AddDate(0, 0, -(days - 1)), to, nil
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	principalID, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	from, to, err := rangeDays(r)
	if err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_days"})
		return
	}
	result, err := h.Svc.OverviewWithFreshness(r.Context(), principalID, from, to)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (h *Handler) summary(w http.ResponseWriter, r *http.Request) {
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
	clientID, err := uuid.Parse(r.URL.Query().Get("client_id"))
	if err != nil {
		// A malformed site id gets the same 404 as an unknown one — the distinction would be an
		// existence oracle.
		httpx.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}

	days := defaultRangeDays
	if d := r.URL.Query().Get("days"); d != "" {
		n, cerr := strconv.Atoi(d)
		if cerr != nil || n < 1 {
			httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_days"})
			return
		}
		if n > maxRangeDays {
			httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_days"})
			return
		}
		days = n
	}
	// Inclusive UTC day window ending today.
	to := time.Now().UTC().Truncate(24 * time.Hour)
	from := to.AddDate(0, 0, -(days - 1))

	sum, err := h.Svc.Summary(r.Context(), principalID, businessID, clientID, from, to)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sum)
}

type replacePropertyRulesRequest struct {
	Rules *[]PropertyRuleInput `json:"rules"`
}

func propertyRuleRouteIDs(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, uuid.UUID, bool) {
	principalID, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	businessID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_business_id"})
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	clientID, err := uuid.Parse(chi.URLParam(r, "clientID"))
	if err != nil {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	return principalID, businessID, clientID, true
}

func (h *Handler) listPropertyRules(w http.ResponseWriter, r *http.Request) {
	principalID, businessID, clientID, ok := propertyRuleRouteIDs(w, r)
	if !ok {
		return
	}
	rules, err := h.Svc.ListPropertyRules(r.Context(), principalID, businessID, clientID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

func (h *Handler) replacePropertyRules(w http.ResponseWriter, r *http.Request) {
	principalID, businessID, clientID, ok := propertyRuleRouteIDs(w, r)
	if !ok {
		return
	}
	var req replacePropertyRulesRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.Rules == nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_rules"})
		return
	}
	rules, err := h.Svc.ReplacePropertyRules(
		r.Context(), principalID, businessID, clientID, *req.Rules,
	)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

// mapErr converts driver errors into the typed sentinels handlers branch on. Raw pg errors never
// reach a client: their messages carry constraint names, which are column names.
func mapErr(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("analytics: not found: %w", errs.ErrNotFound)
	case errors.Is(err, errs.ErrNotFound), errors.Is(err, errs.ErrValidation),
		errors.Is(err, errs.ErrForbidden):
		return err
	case errors.As(err, &pgErr):
		return fmt.Errorf("analytics: query failed: %w", err)
	default:
		return fmt.Errorf("analytics: %w", err)
	}
}
