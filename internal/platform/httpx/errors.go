package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// ErrorBody is the stable error envelope returned to clients.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError maps a service-layer error to a stable HTTP response. Unauthorized
// (ErrForbidden) collapses to 404 so it is indistinguishable from "doesn't
// exist" — no existence oracle (FR-011/FR-026). Wrapped errors are logged
// server-side and never echoed, except validation messages which are safe.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errs.ErrValidation):
		WriteJSON(w, http.StatusBadRequest, ErrorBody{Code: "VALIDATION", Message: err.Error()})
	case errors.Is(err, errs.ErrConflict):
		WriteJSON(w, http.StatusConflict, ErrorBody{Code: "CONFLICT", Message: "conflict"})
	case errors.Is(err, errs.ErrNotFound), errors.Is(err, errs.ErrForbidden):
		WriteJSON(w, http.StatusNotFound, ErrorBody{Code: "NOT_FOUND", Message: "not found"})
	default:
		slog.ErrorContext(r.Context(), "unhandled error", "err", err, "path", r.URL.Path)
		WriteJSON(w, http.StatusInternalServerError, ErrorBody{Code: "INTERNAL", Message: "internal error"})
	}
}
