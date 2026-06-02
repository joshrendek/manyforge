package ticketing

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// IdentityHandler exposes the US4 inbox-management surface (custom email domains +
// their verification + custom inbound addresses) over HTTP. Routes are thin: parse +
// validate input, call the service, project the OpenAPI schema, write JSON. All five
// endpoints are gated on inbox.manage by RequirePermission middleware mounted in
// cmd/manyforge (RLS-bound 404-on-lacking-perm, no oracle); the service additionally
// enforces the ownership predicate in SQL.
type IdentityHandler struct {
	svc *IdentityService
}

// NewIdentityHandler builds the US4 identity HTTP handler.
func NewIdentityHandler(svc *IdentityService) *IdentityHandler {
	return &IdentityHandler{svc: svc}
}

// Routes mounts the five inbox-management endpoints. The caller wraps these with
// httpx.RequirePermission("inbox.manage", …) so each is gated identically.
func (h *IdentityHandler) Routes(r chi.Router) {
	r.Get("/businesses/{id}/email-domains", h.listEmailDomains)
	r.Post("/businesses/{id}/email-domains", h.createEmailDomain)
	r.Post("/businesses/{id}/email-domains/{did}/verify", h.verifyEmailDomain)
	r.Get("/businesses/{id}/inbound-addresses", h.listInboundAddresses)
	r.Post("/businesses/{id}/inbound-addresses", h.createInboundAddress)
}

// --- request DTOs: exact OpenAPI component schemas ---

type createEmailDomainBody struct {
	Domain string `json:"domain"`
	Mode   string `json:"mode"`
}

type createInboundAddressBody struct {
	Address       string `json:"address"`
	EmailDomainID string `json:"email_domain_id"`
}

// --- response DTOs: exact OpenAPI component schemas ---

type txtRecordResp struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type dnsChallengeResp struct {
	VerificationTXT txtRecordResp `json:"verification_txt"`
	DKIMRecord      txtRecordResp `json:"dkim_record"`
	SPFHint         string        `json:"spf_hint"`
	MXHint          *string       `json:"mx_hint"`
}

type emailDomainResp struct {
	ID           string           `json:"id"`
	BusinessID   string           `json:"business_id"`
	TenantRootID string           `json:"tenant_root_id"`
	Domain       string           `json:"domain"`
	Mode         string           `json:"mode"`
	Verification string           `json:"verification"`
	VerifiedAt   *string          `json:"verified_at"`
	DKIMState    string           `json:"dkim_state"`
	SPFState     string           `json:"spf_state"`
	DNSChallenge dnsChallengeResp `json:"dns_challenge"`
	CreatedAt    string           `json:"created_at"`
}

type inboundAddressResp struct {
	ID            string  `json:"id"`
	BusinessID    string  `json:"business_id"`
	TenantRootID  string  `json:"tenant_root_id"`
	Address       string  `json:"address"`
	Kind          string  `json:"kind"`
	EmailDomainID *string `json:"email_domain_id"`
	Active        bool    `json:"active"`
	CreatedAt     string  `json:"created_at"`
}

func toEmailDomainResp(d EmailDomain) emailDomainResp {
	var verifiedAt *string
	if d.VerifiedAt != nil {
		s := d.VerifiedAt.UTC().Format(rfc3339)
		verifiedAt = &s
	}
	return emailDomainResp{
		ID:           d.ID.String(),
		BusinessID:   d.BusinessID.String(),
		TenantRootID: d.TenantRootID.String(),
		Domain:       d.Domain,
		Mode:         d.Mode,
		Verification: d.Verification,
		VerifiedAt:   verifiedAt,
		DKIMState:    d.DKIMState,
		SPFState:     d.SPFState,
		DNSChallenge: dnsChallengeResp{
			VerificationTXT: txtRecordResp{Name: d.DNSChallenge.VerificationTXT.Name, Value: d.DNSChallenge.VerificationTXT.Value},
			DKIMRecord:      txtRecordResp{Name: d.DNSChallenge.DKIMRecord.Name, Value: d.DNSChallenge.DKIMRecord.Value},
			SPFHint:         d.DNSChallenge.SPFHint,
			MXHint:          d.DNSChallenge.MXHint,
		},
		CreatedAt: d.CreatedAt.UTC().Format(rfc3339),
	}
}

func toInboundAddressResp(a InboundAddress) inboundAddressResp {
	return inboundAddressResp{
		ID:            a.ID.String(),
		BusinessID:    a.BusinessID.String(),
		TenantRootID:  a.TenantRootID.String(),
		Address:       a.Address,
		Kind:          a.Kind,
		EmailDomainID: uuidStrPtr(a.EmailDomainID),
		Active:        a.Active,
		CreatedAt:     a.CreatedAt.UTC().Format(rfc3339),
	}
}

// --- handlers ---

func (h *IdentityHandler) listEmailDomains(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	items, next, err := h.svc.ListEmailDomains(r.Context(), pid, bid, Pagination{
		Cursor: r.URL.Query().Get("cursor"), Limit: parseLimit(r),
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	resp := make([]emailDomainResp, 0, len(items))
	for _, d := range items {
		resp = append(resp, toEmailDomainResp(d))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": resp, "next_cursor": nextCursorJSON(next)})
}

func (h *IdentityHandler) createEmailDomain(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var body createEmailDomainBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Domain) == "" {
		httpx.WriteError(w, r, errValidation("domain required"))
		return
	}
	if strings.TrimSpace(body.Mode) == "" {
		httpx.WriteError(w, r, errValidation("mode required"))
		return
	}
	d, err := h.svc.CreateEmailDomain(r.Context(), pid, bid, CreateEmailDomainInput(body))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toEmailDomainResp(d))
}

func (h *IdentityHandler) verifyEmailDomain(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	did, err := pathUUID(r, "did")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	d, err := h.svc.VerifyEmailDomain(r.Context(), pid, bid, did)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toEmailDomainResp(d))
}

func (h *IdentityHandler) listInboundAddresses(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	items, next, err := h.svc.ListInboundAddresses(r.Context(), pid, bid, Pagination{
		Cursor: r.URL.Query().Get("cursor"), Limit: parseLimit(r),
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	resp := make([]inboundAddressResp, 0, len(items))
	for _, a := range items {
		resp = append(resp, toInboundAddressResp(a))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": resp, "next_cursor": nextCursorJSON(next)})
}

func (h *IdentityHandler) createInboundAddress(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := pathUUID(r, "id")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var body createInboundAddressBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Address) == "" {
		httpx.WriteError(w, r, errValidation("address required"))
		return
	}
	domainID, perr := uuid.Parse(strings.TrimSpace(body.EmailDomainID))
	if perr != nil {
		httpx.WriteError(w, r, errValidation("invalid email_domain_id"))
		return
	}
	a, err := h.svc.CreateInboundAddress(r.Context(), pid, bid, CreateInboundAddressInput{Address: body.Address, EmailDomainID: domainID})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toInboundAddressResp(a))
}

// nextCursorJSON renders an empty cursor as a literal JSON null (matching the
// Page.next_cursor "last page" shape) and a non-empty cursor as the token string.
func nextCursorJSON(cursor string) any {
	if cursor == "" {
		return nil
	}
	return cursor
}
