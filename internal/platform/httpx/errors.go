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

// MaxJSONBodyBytes caps the request body the authenticated JSON API will read
// (manyforge-0yv, defense-in-depth). The control-plane payloads here (replies,
// notes, settings) are small; 1 MiB is generous for any of them while refusing an
// unbounded stream. Distinct from config.InboundMaxBytes (30 MiB), which sizes raw
// inbound *mail* on the public ingress — a different, larger workload.
const MaxJSONBodyBytes int64 = 1 << 20 // 1 MiB

// DecodeJSON decodes the request body into v, writing a 400 and returning false on
// malformed input. The body is capped at MaxJSONBodyBytes via http.MaxBytesReader;
// an over-cap body is refused with 413 before it can be fully buffered (so the cap
// holds even when no global request-size middleware is mounted — defense-in-depth,
// manyforge-0yv). The pre-existing 400-on-malformed contract is unchanged.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, MaxJSONBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			WriteJSON(w, http.StatusRequestEntityTooLarge, ErrorBody{Code: "PAYLOAD_TOO_LARGE", Message: "payload too large"})
			return false
		}
		WriteJSON(w, http.StatusBadRequest, ErrorBody{Code: "VALIDATION", Message: "invalid JSON body"})
		return false
	}
	return true
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
	case errors.Is(err, errs.ErrRateLimited):
		WriteJSON(w, http.StatusTooManyRequests, ErrorBody{Code: "RATE_LIMITED", Message: "rate limited"})
	case errors.Is(err, errs.ErrCodexDisconnected):
		WriteJSON(w, http.StatusConflict, ErrorBody{Code: "CODEX_DISCONNECTED", Message: "codex credential disconnected; reconnect required"})
	case errors.Is(err, errs.ErrUpstream):
		WriteJSON(w, http.StatusBadGateway, ErrorBody{Code: "UPSTREAM", Message: "upstream provider error"})
	default:
		slog.ErrorContext(r.Context(), "unhandled error", "err", err, "path", r.URL.Path)
		WriteJSON(w, http.StatusInternalServerError, ErrorBody{Code: "INTERNAL", Message: "internal error"})
	}
}
