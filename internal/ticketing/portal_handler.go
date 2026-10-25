package ticketing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
)

type PortalHandler struct {
	svc                       *PortalService
	trusted                   []*net.IPNet
	global, ip, key, customer ratelimit.Limiter
	timeout                   time.Duration
	inflight                  chan struct{}
}

func NewPortalHandler(svc *PortalService, trusted []*net.IPNet) *PortalHandler {
	return &PortalHandler{svc: svc, trusted: trusted, global: ratelimit.NewTokenBucket(100, 100), ip: ratelimit.NewTokenBucket(2, 30), key: ratelimit.NewTokenBucket(20, 60), customer: ratelimit.NewTokenBucket(2, 20), timeout: 20 * time.Second, inflight: make(chan struct{}, 32)}
}

// CustomerRoutes must be mounted outside JWT middleware: its only authority is a
// server-only integration bearer plus the authenticated backend's opaque user ID.
func (h *PortalHandler) CustomerRoutes(r chi.Router) {
	r.Route("/support/portal", func(r chi.Router) {
		r.Use(h.customerGate)
		r.Get("/contact", h.contact)
		r.Post("/contact/verification", h.sendVerification)
		r.Post("/contact/verify", h.verify)
		r.Get("/tickets", h.list)
		r.Post("/tickets", h.create)
		r.Get("/tickets/{ticketID}", h.thread)
		r.Post("/tickets/{ticketID}/messages", h.message)
	})
}

// ManagementRoutes must be inside the normal interactive JWT group. The service
// additionally enforces tickets.write and inherited locked Owner membership.
func (h *PortalHandler) ManagementRoutes(r chi.Router) {
	r.With(h.bounded).Post("/businesses/{id}/support/portal-keys", h.createKey)
	r.With(h.bounded).Get("/businesses/{id}/support/portal-keys", h.listKeys)
	r.With(h.bounded).Delete("/businesses/{id}/support/portal-keys/{keyID}", h.revokeKey)
}

type portalScopeContextKey struct{}

func (h *PortalHandler) bounded(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Global admission precedes attacker-keyed limiter allocation and all DB/body work.
		if !h.global.Allow("all") || !h.ip.Allow(ratelimit.ClientIP(r, h.trusted)) {
			portalHTTPError(w, ErrPortalRate)
			return
		}
		select {
		case h.inflight <- struct{}{}:
			defer func() { <-h.inflight }()
		default:
			portalHTTPError(w, ErrPortalRate)
			return
		}
		deadline := time.Now().Add(h.timeout)
		controller := http.NewResponseController(w)
		// Context cancellation alone cannot unblock a network body read.
		if err := controller.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			portalHTTPError(w, ErrPortalUnavailable)
			return
		}
		if err := controller.SetWriteDeadline(deadline.Add(5 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			portalHTTPError(w, ErrPortalUnavailable)
			return
		}
		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (h *PortalHandler) customerGate(next http.Handler) http.Handler {
	return h.bounded(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Values("Authorization")
		external := r.Header.Values("X-MF-External-User")
		names := r.Header.Values("X-MF-External-Name")
		if len(auth) != 1 || len(external) != 1 || len(names) > 1 || !strings.HasPrefix(auth[0], "Bearer ") || len(auth[0]) != 55 {
			portalHTTPError(w, ErrPortalUnauthorized)
			return
		}
		name := ""
		if len(names) == 1 {
			name = names[0]
		}
		if !portalIdentity(external[0], name) {
			portalHTTPError(w, ErrPortalUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth[0], "Bearer ")
		hash := sha256.Sum256([]byte(token))
		rateKey := hex.EncodeToString(hash[:])
		if !h.key.Allow(rateKey) || !h.customer.Allow(external[0]) {
			portalHTTPError(w, ErrPortalRate)
			return
		}
		p, err := h.svc.Authenticate(r.Context(), token, external[0], name)
		if err != nil {
			portalHTTPError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), portalScopeContextKey{}, p)))
	}))
}

func portalScope(r *http.Request) PortalScope {
	return r.Context().Value(portalScopeContextKey{}).(PortalScope)
}
func portalJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func portalHTTPError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	message := "support unavailable"
	switch {
	case errors.Is(err, ErrPortalUnauthorized):
		status = 401
		message = "invalid credentials"
	case errors.Is(err, ErrPortalInvalid):
		status = 400
		message = "invalid request"
	case errors.Is(err, errs.ErrNotFound):
		status = 404
		message = "not found"
	case errors.Is(err, ErrPortalConflict):
		status = 409
		message = "contact verification required or request conflict"
	case errors.Is(err, ErrPortalRate):
		status = 429
		message = "rate limit exceeded"
		w.Header().Set("Retry-After", "60")
	}
	portalJSON(w, status, map[string]string{"error": message})
}
func portalDecode(w http.ResponseWriter, r *http.Request, out any) bool {
	if r.URL.RawQuery != "" {
		portalHTTPError(w, ErrPortalInvalid)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		portalHTTPError(w, ErrPortalInvalid)
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		portalHTTPError(w, ErrPortalInvalid)
		return false
	}
	return true
}
func (h *PortalHandler) contact(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		portalHTTPError(w, ErrPortalInvalid)
		return
	}
	out, err := h.svc.Contact(r.Context(), portalScope(r))
	if err != nil {
		portalHTTPError(w, err)
		return
	}
	portalJSON(w, 200, out)
}
func (h *PortalHandler) sendVerification(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	if !portalDecode(w, r, &in) {
		return
	}
	if err := h.svc.SendContactVerification(r.Context(), portalScope(r), in.Email); err != nil {
		portalHTTPError(w, err)
		return
	}
	portalJSON(w, 200, map[string]bool{"sent": true})
}
func (h *PortalHandler) verify(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if !portalDecode(w, r, &in) {
		return
	}
	if err := h.svc.VerifyContact(r.Context(), portalScope(r), in.Code); err != nil {
		portalHTTPError(w, err)
		return
	}
	portalJSON(w, 200, map[string]bool{"verified": true})
}
func (h *PortalHandler) list(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		portalHTTPError(w, ErrPortalInvalid)
		return
	}
	out, err := h.svc.ListTickets(r.Context(), portalScope(r))
	if err != nil {
		portalHTTPError(w, err)
		return
	}
	portalJSON(w, 200, map[string]any{"tickets": out})
}
func (h *PortalHandler) create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Subject        string `json:"subject"`
		Body           string `json:"body"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !portalDecode(w, r, &in) {
		return
	}
	id, err := portalUUID(in.IdempotencyKey)
	if err != nil {
		portalHTTPError(w, err)
		return
	}
	out, err := h.svc.CreateTicket(r.Context(), portalScope(r), in.Subject, in.Body, id)
	if err != nil {
		portalHTTPError(w, err)
		return
	}
	portalJSON(w, 201, map[string]any{"ticket": out})
}
func (h *PortalHandler) thread(w http.ResponseWriter, r *http.Request) {
	id, err := portalUUID(chi.URLParam(r, "ticketID"))
	if err != nil {
		portalHTTPError(w, errs.ErrNotFound)
		return
	}
	q := r.URL.Query()
	if len(q) > 1 || (len(q) == 1 && len(q["before"]) != 1) {
		portalHTTPError(w, ErrPortalInvalid)
		return
	}
	out, err := h.svc.Thread(r.Context(), portalScope(r), id, q.Get("before"))
	if err != nil {
		portalHTTPError(w, err)
		return
	}
	portalJSON(w, 200, out)
}
func (h *PortalHandler) message(w http.ResponseWriter, r *http.Request) {
	id, err := portalUUID(chi.URLParam(r, "ticketID"))
	if err != nil {
		portalHTTPError(w, errs.ErrNotFound)
		return
	}
	var in struct {
		Body           string `json:"body"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !portalDecode(w, r, &in) {
		return
	}
	request, err := portalUUID(in.IdempotencyKey)
	if err != nil {
		portalHTTPError(w, err)
		return
	}
	out, err := h.svc.AddMessage(r.Context(), portalScope(r), id, in.Body, request)
	if err != nil {
		portalHTTPError(w, err)
		return
	}
	portalJSON(w, 201, map[string]any{"message": out})
}
func portalManagementIDs(r *http.Request) (uuid.UUID, uuid.UUID, error) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		return uuid.Nil, uuid.Nil, errs.ErrNotFound
	}
	bid, err := portalUUID(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, uuid.Nil, errs.ErrNotFound
	}
	return pid, bid, nil
}
func (h *PortalHandler) createKey(w http.ResponseWriter, r *http.Request) {
	pid, bid, err := portalManagementIDs(r)
	if err != nil {
		portalHTTPError(w, err)
		return
	}
	var in struct {
		Label string `json:"label"`
		Days  *int   `json:"expires_in_days"`
	}
	if !portalDecode(w, r, &in) {
		return
	}
	days := 90
	if in.Days != nil {
		days = *in.Days
		if days < 1 || days > 365 {
			portalHTTPError(w, ErrPortalInvalid)
			return
		}
	}
	out, err := h.svc.CreateKey(r.Context(), pid, bid, in.Label, days)
	if err != nil {
		portalHTTPError(w, err)
		return
	}
	portalJSON(w, 201, out)
}
func (h *PortalHandler) listKeys(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		portalHTTPError(w, ErrPortalInvalid)
		return
	}
	pid, bid, err := portalManagementIDs(r)
	if err != nil {
		portalHTTPError(w, err)
		return
	}
	out, err := h.svc.ListKeys(r.Context(), pid, bid)
	if err != nil {
		portalHTTPError(w, err)
		return
	}
	portalJSON(w, 200, map[string]any{"keys": out})
}
func (h *PortalHandler) revokeKey(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		portalHTTPError(w, ErrPortalInvalid)
		return
	}
	pid, bid, err := portalManagementIDs(r)
	if err != nil {
		portalHTTPError(w, err)
		return
	}
	id, err := portalUUID(chi.URLParam(r, "keyID"))
	if err != nil {
		portalHTTPError(w, errs.ErrNotFound)
		return
	}
	if err := h.svc.RevokeKey(r.Context(), pid, bid, id); err != nil {
		portalHTTPError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(204)
}
