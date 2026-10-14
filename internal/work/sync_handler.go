package work

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
)

// maxSyncBytes caps a sync request body (1 MiB). A large import (up to 1000 ops
// with long descriptions) may exceed it and must be split across syncs.
const maxSyncBytes int64 = 1 << 20

// Per-token sync budget. Agents sync after every mutation, so this is generous; it bounds one
// leaked or runaway token, not normal use.
const (
	SyncTokenRateRPS   = 10
	SyncTokenRateBurst = 30
)

// SyncHandler serves POST /api/v1/work/sync for `mf`. It is NOT behind RequireAuth: it
// authenticates a project-scoped mfw_ bearer token, then runs as the token's issuer.
type SyncHandler struct {
	svc *Service
	// PerToken limits each token (keyed by token id) after it resolves. nil = unlimited.
	PerToken ratelimit.Limiter
}

// NewSyncHandler builds the token-authenticated sync handler.
func NewSyncHandler(svc *Service, perToken ratelimit.Limiter) *SyncHandler {
	return &SyncHandler{svc: svc, PerToken: perToken}
}

// Routes mounts the sync endpoint. The caller supplies the group's pre-auth per-IP limiter.
func (h *SyncHandler) Routes(r chi.Router) {
	r.Post("/work/sync", h.sync)
}

// writeUnauthorized is the uniform 401 for every token problem — missing, malformed, unknown,
// revoked, project gone, issuer lost access — so the response is never an existence oracle.
func writeUnauthorized(w http.ResponseWriter) {
	httpx.WriteJSON(w, http.StatusUnauthorized, httpx.ErrorBody{Code: "UNAUTHORIZED", Message: "unauthorized"})
}

// bearerToken extracts the Authorization bearer credential (scheme case-insensitive).
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func (h *SyncHandler) sync(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r)
	if !ok {
		writeUnauthorized(w)
		return
	}
	a, err := h.svc.ResolveToken(r.Context(), token)
	if errors.Is(err, errTokenUnauthorized) {
		writeUnauthorized(w)
		return
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if h.PerToken != nil && !h.PerToken.Allow(a.TokenID.String()) {
		w.Header().Set("Retry-After", "1")
		httpx.WriteJSON(w, http.StatusTooManyRequests, httpx.ErrorBody{Code: "RATE_LIMITED", Message: "too many requests"})
		return
	}

	var req SyncRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxSyncBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httpx.WriteJSON(w, http.StatusRequestEntityTooLarge, httpx.ErrorBody{Code: "PAYLOAD_TOO_LARGE", Message: "payload too large"})
			return
		}
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{Code: "VALIDATION", Message: "invalid JSON body"})
		return
	}

	resp, err := h.svc.Sync(r.Context(), a, req)
	if errors.Is(err, errTokenUnauthorized) {
		writeUnauthorized(w)
		return
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}
