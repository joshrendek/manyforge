package feedback

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// rfc3339 renders timestamps in the same shape as the rest of the API (crm.rfc3339).
const rfc3339 = "2006-01-02T15:04:05.999999999Z07:00"

// trimTo trims surrounding whitespace (named to avoid colliding with strings.TrimSpace at call
// sites and to keep the public/authed handlers terse).
func trimTo(s string) string { return strings.TrimSpace(s) }

// writeErr writes a stable {code,message} error envelope at the given status.
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	httpx.WriteJSON(w, status, httpx.ErrorBody{Code: code, Message: msg})
}

// writeUnauthorized is the uniform 401 for the public ingress oracle boundary — identical for
// an unknown key, a revoked key, and a key on a non-public board.
func writeUnauthorized(w http.ResponseWriter) {
	writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
}

// pathUUID parses a chi URL param as a UUID.
func pathUUID(r *http.Request, key string) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, key))
}

// parseLimit reads the limit query param; absent/malformed → 0 (the service applies the default
// and enforces the cap).
func parseLimit(r *http.Request) int {
	s := r.URL.Query().Get("limit")
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// errValidation builds a safe-to-surface 400 (mirrors crm.errValidation).
func errValidation(msg string) error { return &validationError{msg: msg} }

type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }
func (e *validationError) Is(target error) bool {
	return target == errs.ErrValidation
}

// uuidStrPtr renders an optional uuid.UUID as an optional string for the JSON view.
func uuidStrPtr(u *uuid.UUID) *string {
	if u == nil {
		return nil
	}
	s := u.String()
	return &s
}
