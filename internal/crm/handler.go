package crm

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// Handler exposes the CRM read + write slices (contacts + companies) over HTTP. Routes
// are thin: parse + validate input, call the service, project the OpenAPI schema, write
// JSON. Authorization (crm.read / crm.write) is enforced entirely by RequirePermission
// middleware mounted in cmd/manyforge, plus the service-layer ownership predicate
// (tenant_root_id) under RLS — there is NO conditional handler-side authz gate here (unlike
// ticketing's tickets.assign), so every endpoint in a group is gated identically. db +
// resolve are carried for symmetry with the ticketing handler and future conditional gates;
// no current handler uses them.
type Handler struct {
	contacts  *ContactService
	companies *CompanyService
	activity  *ActivityService
	db        *db.DB
	resolve   httpx.PermissionResolver
}

// NewHandler builds a CRM HTTP handler over the contact + company + activity services.
func NewHandler(c *ContactService, co *CompanyService, a *ActivityService, database *db.DB, resolve httpx.PermissionResolver) *Handler {
	return &Handler{contacts: c, companies: co, activity: a, db: database, resolve: resolve}
}

// ReadRoutes mounts the authenticated read endpoints. The caller wraps these with
// httpx.RequirePermission(crm.read, …) so each is gated identically (RLS-bound 404 on a
// lacking perm / invisible business — no existence oracle).
func (h *Handler) ReadRoutes(r chi.Router) {
	r.Get("/businesses/{id}/contacts", h.listContacts)
	r.Get("/businesses/{id}/contacts/{cid}", h.getContact)
	r.Get("/businesses/{id}/contacts/{cid}/activity", h.listActivity)
	r.Get("/businesses/{id}/companies", h.listCompanies)
	r.Get("/businesses/{id}/companies/{coid}", h.getCompany)
}

// WriteRoutes mounts the authenticated write endpoints. The caller wraps these with
// httpx.RequirePermission(crm.write, …), same RLS-bound 404-on-lacking-perm semantics.
func (h *Handler) WriteRoutes(r chi.Router) {
	r.Post("/businesses/{id}/contacts", h.createContact)
	r.Patch("/businesses/{id}/contacts/{cid}", h.updateContact)
	r.Delete("/businesses/{id}/contacts/{cid}", h.deleteContact)
	r.Post("/businesses/{id}/contacts/{cid}/merge", h.mergeContact)
	r.Post("/businesses/{id}/companies", h.createCompany)
	r.Patch("/businesses/{id}/companies/{coid}", h.updateCompany)
	r.Delete("/businesses/{id}/companies/{coid}", h.deleteCompany)
}

// --- request DTOs: exact OpenAPI component schemas ---

// createContactBody / updateContactBody carry company_id as a string so the handler can
// reject a malformed UUID as a 400 validation error (rather than the service receiving a
// zero UUID). On update both fields are tri-state via pointers (nil = absent → preserved
// by the service COALESCE narg); a present-but-empty/null company_id is NOT supported by
// the Phase-A service (it only preserves or sets), so the handler does not model "unset".
type createContactBody struct {
	PrimaryEmail string  `json:"primary_email"`
	DisplayName  *string `json:"display_name"`
	CompanyID    *string `json:"company_id"`
}

type updateContactBody struct {
	DisplayName *string `json:"display_name"`
	CompanyID   *string `json:"company_id"`
}

type createCompanyBody struct {
	Name   string  `json:"name"`
	Domain *string `json:"domain"`
}

type updateCompanyBody struct {
	Name   *string `json:"name"`
	Domain *string `json:"domain"`
}

// mergeBody is the contact-merge payload: the loser folds into the winner ({cid}).
type mergeBody struct {
	LoserID string `json:"loser_id"`
}

// --- response DTOs: exact OpenAPI component schemas ---

type contactResp struct {
	ID           string  `json:"id"`
	TenantRootID string  `json:"tenant_root_id"`
	PrimaryEmail string  `json:"primary_email"`
	DisplayName  *string `json:"display_name"`
	CompanyID    *string `json:"company_id"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

type companyResp struct {
	ID           string  `json:"id"`
	TenantRootID string  `json:"tenant_root_id"`
	Name         string  `json:"name"`
	Domain       *string `json:"domain"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

type activityResp struct {
	ID           string          `json:"id"`
	TenantRootID string          `json:"tenant_root_id"`
	BusinessID   string          `json:"business_id"`
	ContactID    string          `json:"contact_id"`
	Kind         string          `json:"kind"`
	OccurredAt   string          `json:"occurred_at"`
	Actor        *string         `json:"actor"`
	SourceType   string          `json:"source_type"`
	SourceID     *string         `json:"source_id"`
	Summary      string          `json:"summary"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	CreatedAt    string          `json:"created_at"`
}

const rfc3339 = "2006-01-02T15:04:05.999999999Z07:00"

func toContactResp(c Contact) contactResp {
	return contactResp{
		ID:           c.ID.String(),
		TenantRootID: c.TenantRootID.String(),
		PrimaryEmail: c.PrimaryEmail,
		DisplayName:  c.DisplayName,
		CompanyID:    uuidStrPtr(c.CompanyID),
		CreatedAt:    c.CreatedAt.UTC().Format(rfc3339),
		UpdatedAt:    c.UpdatedAt.UTC().Format(rfc3339),
	}
}

func toCompanyResp(c Company) companyResp {
	return companyResp{
		ID:           c.ID.String(),
		TenantRootID: c.TenantRootID.String(),
		Name:         c.Name,
		Domain:       c.Domain,
		CreatedAt:    c.CreatedAt.UTC().Format(rfc3339),
		UpdatedAt:    c.UpdatedAt.UTC().Format(rfc3339),
	}
}

func toActivityResp(a ActivityEntry) activityResp {
	return activityResp{
		ID:           a.ID.String(),
		TenantRootID: a.TenantRootID.String(),
		BusinessID:   a.BusinessID.String(),
		ContactID:    a.ContactID.String(),
		Kind:         a.Kind,
		OccurredAt:   a.OccurredAt.UTC().Format(rfc3339),
		Actor:        a.Actor,
		SourceType:   a.SourceType,
		SourceID:     uuidStrPtr(a.SourceID),
		Summary:      a.Summary,
		Metadata:     a.Metadata,
		CreatedAt:    a.CreatedAt.UTC().Format(rfc3339),
	}
}

// --- contact handlers ---

func (h *Handler) listContacts(w http.ResponseWriter, r *http.Request) {
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
	page, err := h.contacts.List(r.Context(), pid, bid, r.URL.Query().Get("cursor"), parseLimit(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items := make([]contactResp, 0, len(page.Items))
	for _, c := range page.Items {
		items = append(items, toContactResp(c))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": page.NextCursor})
}

func (h *Handler) getContact(w http.ResponseWriter, r *http.Request) {
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
	cid, err := pathUUID(r, "cid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	c, err := h.contacts.Get(r.Context(), pid, bid, cid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toContactResp(c))
}

// listActivity returns a keyset page of a contact's activity timeline, newest-first.
// Reads both the {id} business and {cid} contact path params; an unknown/foreign id
// collapses to ErrNotFound in the service (no existence oracle), mirroring getContact.
func (h *Handler) listActivity(w http.ResponseWriter, r *http.Request) {
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
	cid, err := pathUUID(r, "cid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	page, err := h.activity.ListForContact(r.Context(), pid, bid, cid, r.URL.Query().Get("cursor"), parseLimit(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items := make([]activityResp, 0, len(page.Items))
	for _, a := range page.Items {
		items = append(items, toActivityResp(a))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": page.NextCursor})
}

func (h *Handler) createContact(w http.ResponseWriter, r *http.Request) {
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
	var body createContactBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.PrimaryEmail) == "" {
		httpx.WriteError(w, r, errValidation("primary_email required"))
		return
	}
	companyID, verr := parseOptUUID(body.CompanyID, "company_id")
	if verr != nil {
		httpx.WriteError(w, r, verr)
		return
	}
	c, err := h.contacts.Create(r.Context(), pid, bid, ContactInput{
		PrimaryEmail: body.PrimaryEmail,
		DisplayName:  body.DisplayName,
		CompanyID:    companyID,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toContactResp(c))
}

func (h *Handler) updateContact(w http.ResponseWriter, r *http.Request) {
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
	cid, err := pathUUID(r, "cid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var body updateContactBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	companyID, verr := parseOptUUID(body.CompanyID, "company_id")
	if verr != nil {
		httpx.WriteError(w, r, verr)
		return
	}
	// PrimaryEmail is immutable on update (the service ignores it); send only the
	// mutable fields. A nil field is preserved by the service COALESCE narg.
	c, err := h.contacts.Update(r.Context(), pid, bid, cid, ContactInput{
		DisplayName: body.DisplayName,
		CompanyID:   companyID,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toContactResp(c))
}

func (h *Handler) deleteContact(w http.ResponseWriter, r *http.Request) {
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
	cid, err := pathUUID(r, "cid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if derr := h.contacts.SoftDelete(r.Context(), pid, bid, cid); derr != nil {
		httpx.WriteError(w, r, derr)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mergeContact folds the loser contact (body) into the winner ({cid}). A bad/empty
// loser_id is a 400; the service rejects self-merge (ErrValidation) and collapses a
// cross-tenant / unknown loser or winner to a no-oracle 404.
func (h *Handler) mergeContact(w http.ResponseWriter, r *http.Request) {
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
	cid, err := pathUUID(r, "cid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var body mergeBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	loserID, perr := uuid.Parse(strings.TrimSpace(body.LoserID))
	if perr != nil {
		httpx.WriteError(w, r, errValidation("invalid loser_id"))
		return
	}
	if merr := h.contacts.Merge(r.Context(), pid, bid, cid, loserID); merr != nil {
		httpx.WriteError(w, r, merr)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "merged"})
}

// --- company handlers ---

func (h *Handler) listCompanies(w http.ResponseWriter, r *http.Request) {
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
	page, err := h.companies.List(r.Context(), pid, bid, r.URL.Query().Get("cursor"), parseLimit(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items := make([]companyResp, 0, len(page.Items))
	for _, c := range page.Items {
		items = append(items, toCompanyResp(c))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": page.NextCursor})
}

func (h *Handler) getCompany(w http.ResponseWriter, r *http.Request) {
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
	coid, err := pathUUID(r, "coid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	c, err := h.companies.Get(r.Context(), pid, bid, coid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toCompanyResp(c))
}

func (h *Handler) createCompany(w http.ResponseWriter, r *http.Request) {
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
	var body createCompanyBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		httpx.WriteError(w, r, errValidation("name required"))
		return
	}
	c, err := h.companies.Create(r.Context(), pid, bid, CompanyInput(body))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toCompanyResp(c))
}

func (h *Handler) updateCompany(w http.ResponseWriter, r *http.Request) {
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
	coid, err := pathUUID(r, "coid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var body updateCompanyBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	// Name is sent through the service COALESCE narg: an absent (nil) or empty Name
	// preserves the current value; a nil Domain preserves the current value.
	in := CompanyInput{Domain: body.Domain}
	if body.Name != nil {
		in.Name = *body.Name
	}
	c, err := h.companies.Update(r.Context(), pid, bid, coid, in)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toCompanyResp(c))
}

func (h *Handler) deleteCompany(w http.ResponseWriter, r *http.Request) {
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
	coid, err := pathUUID(r, "coid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if derr := h.companies.Delete(r.Context(), pid, bid, coid); derr != nil {
		httpx.WriteError(w, r, derr)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- input parsing ---

func pathUUID(r *http.Request, key string) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, key))
}

// parseLimit reads the limit query param. Absent/malformed → 0 (the service applies the
// default and enforces the ≤200 cap), so this never validates the ceiling itself.
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

// parseOptUUID parses an optional caller-supplied UUID body field. nil/empty → nil (absent,
// preserved on update); a non-empty value MUST be a valid UUID or it is a 400 validation
// error (never a zero UUID silently reaching the service). field names the param for the
// error message.
func parseOptUUID(s *string, field string) (*uuid.UUID, error) {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil, nil
	}
	id, err := uuid.Parse(strings.TrimSpace(*s))
	if err != nil {
		return nil, errValidation("invalid " + field)
	}
	return &id, nil
}

// errValidation builds a safe-to-surface 400. validationError carries a message and
// satisfies errors.Is for errs.ErrValidation so httpx.WriteError renders it as a 400 with
// the message (mirrors ticketing.errValidation).
func errValidation(msg string) error {
	return &validationError{msg: msg}
}

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
