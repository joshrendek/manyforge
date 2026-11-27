package portaldomain

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// domainService is the slice of *Service the handler needs; tests substitute a fake.
type domainService interface {
	Create(ctx context.Context, principalID, businessID uuid.UUID, hostname string) (Domain, error)
	List(ctx context.Context, principalID, businessID uuid.UUID, cursor string, limit int) ([]Domain, string, error)
	Verify(ctx context.Context, principalID, businessID, domainID uuid.UUID) (Domain, error)
	Delete(ctx context.Context, principalID, businessID, domainID uuid.UUID) error
}

// Handler exposes portal-domain management over HTTP. The caller mounts Routes
// behind the inbox.manage permission gate; the service enforces ownership in SQL.
type Handler struct {
	svc          domainService
	instanceHost string // CNAME target shown to the business; "" when unset
}

// NewHandler builds the HTTP handler for svc.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc, instanceHost: svc.InstanceHost}
}

// Routes mounts the four portal-domain endpoints.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/businesses/{id}/portal-domains", h.list)
	r.Post("/businesses/{id}/portal-domains", h.create)
	r.Post("/businesses/{id}/portal-domains/{did}/verify", h.verify)
	r.Delete("/businesses/{id}/portal-domains/{did}", h.delete)
}

type createBody struct {
	Hostname string `json:"hostname"`
}

type txtRecordResp struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type cnameTargetResp struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

type dnsResp struct {
	VerificationTXT txtRecordResp   `json:"verification_txt"`
	Target          cnameTargetResp `json:"target"`
}

type domainResp struct {
	ID         string  `json:"id"`
	Hostname   string  `json:"hostname"`
	Verified   bool    `json:"verified"`
	VerifiedAt *string `json:"verified_at"`
	DNS        dnsResp `json:"dns"`
	CreatedAt  string  `json:"created_at"`
}

func (h *Handler) toResp(d Domain) domainResp {
	var verifiedAt *string
	if d.VerifiedAt != nil {
		s := d.VerifiedAt.UTC().Format(time.RFC3339Nano)
		verifiedAt = &s
	}
	return domainResp{
		ID:         d.ID.String(),
		Hostname:   d.Hostname,
		Verified:   d.VerifiedAt != nil,
		VerifiedAt: verifiedAt,
		DNS: dnsResp{
			VerificationTXT: txtRecordResp{Name: d.Verification.Name, Value: d.Verification.Value},
			Target:          cnameTargetResp{Type: "CNAME", Name: d.Hostname, Value: h.instanceHost},
		},
		CreatedAt: d.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// ids parses the principal and the business (and optionally domain) path ids. Any
// failure is a 404, never a distinguishable validation error.
func ids(w http.ResponseWriter, r *http.Request, withDomain bool) (pid, bid, did uuid.UUID, ok bool) {
	pid, found := httpx.PrincipalFromContext(r.Context())
	if !found {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var err error
	if bid, err = uuid.Parse(chi.URLParam(r, "id")); err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if withDomain {
		if did, err = uuid.Parse(chi.URLParam(r, "did")); err != nil {
			httpx.WriteError(w, r, errs.ErrNotFound)
			return
		}
	}
	return pid, bid, did, true
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	pid, bid, _, ok := ids(w, r, false)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit")) // malformed → 0 → service default
	items, next, err := h.svc.List(r.Context(), pid, bid, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	resp := make([]domainResp, 0, len(items))
	for _, d := range items {
		resp = append(resp, h.toResp(d))
	}
	var nextCursor any // JSON null on the last page
	if next != "" {
		nextCursor = next
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": resp, "next_cursor": nextCursor})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	pid, bid, _, ok := ids(w, r, false)
	if !ok {
		return
	}
	var body createBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Hostname) == "" {
		httpx.WriteError(w, r, errValidation("hostname required"))
		return
	}
	d, err := h.svc.Create(r.Context(), pid, bid, body.Hostname)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, h.toResp(d))
}

func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
	pid, bid, did, ok := ids(w, r, true)
	if !ok {
		return
	}
	d, err := h.svc.Verify(r.Context(), pid, bid, did)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.toResp(d))
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	pid, bid, did, ok := ids(w, r, true)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), pid, bid, did); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
