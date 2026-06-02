package ticketing

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// Handler exposes the ticketing read slice over HTTP. Routes are thin: parse +
// validate input, call the service, project the OpenAPI schema, write JSON. Most
// authorization (tickets.read/reply/write) is enforced by RequirePermission middleware
// mounted in cmd/manyforge, plus the service-layer ownership predicate. The ONE
// handler-side authz decision is the CONDITIONAL tickets.assign gate on triage: the
// route is gated tickets.write, but an assignee CHANGE additionally requires
// tickets.assign (OpenAPI), so the handler resolves it via the injected resolver and
// renders a no-oracle 404 when absent (mirroring RequirePermission).
type Handler struct {
	svc     *Service
	db      *db.DB                   // for the conditional tickets.assign resolution tx
	resolve httpx.PermissionResolver // resolves the caller's perms at a business (RLS-scoped)
}

// NewHandler builds a ticketing HTTP handler. database+resolve back the conditional
// tickets.assign gate on triage; both may be nil in contexts that never mount triage.
func NewHandler(svc *Service, database *db.DB, resolve httpx.PermissionResolver) *Handler {
	return &Handler{svc: svc, db: database, resolve: resolve}
}

// ProtectedRoutes mounts the authenticated read endpoints. The caller wraps these
// with httpx.RequirePermission("tickets.read", …) so each is gated identically.
func (h *Handler) ProtectedRoutes(r chi.Router) {
	r.Get("/businesses/{id}/tickets", h.listTickets)
	r.Get("/businesses/{id}/tickets/{tid}", h.getTicket)
	r.Get("/businesses/{id}/tickets/{tid}/messages", h.listMessages)
	r.Get("/businesses/{id}/requesters", h.listRequesters)
	r.Get("/businesses/{id}/requesters/{rid}", h.getRequester)
}

// WriteRoutes mounts the authenticated write endpoints (US2: reply + note). The
// caller wraps these with httpx.RequirePermission("tickets.reply", …): BOTH reply
// and note are gated on tickets.reply per the migration-0015 catalog ("Send replies
// AND internal notes on a ticket").
func (h *Handler) WriteRoutes(r chi.Router) {
	r.Post("/businesses/{id}/tickets/{tid}/reply", h.reply)
	r.Post("/businesses/{id}/tickets/{tid}/note", h.addNote)
}

// TriageRoutes mounts the authenticated triage endpoint (US3: PATCH status/priority/
// tags/assignee). The caller wraps it with httpx.RequirePermission("tickets.write", …),
// same RLS-bound 404-on-lacking-perm semantics as the read/write groups.
func (h *Handler) TriageRoutes(r chi.Router) {
	r.Patch("/businesses/{id}/tickets/{tid}", h.triage)
}

// AssignableRoutes mounts the assignee-picker endpoint (FR-011). The caller wraps it
// with httpx.RequirePermission("tickets.assign", …): only a caller who can assign
// sees the candidate list; a lacking-perm or invisible business is a no-oracle 404.
func (h *Handler) AssignableRoutes(r chi.Router) {
	r.Get("/businesses/{id}/assignable-members", h.listAssignableMembers)
}

// DeleteRoutes mounts the authenticated delete/redact endpoint (US5: tickets.delete).
// The caller wraps it with httpx.RequirePermission("tickets.delete", …): a lacking-perm
// or invisible business is a no-oracle 404, identical to every other group.
func (h *Handler) DeleteRoutes(r chi.Router) {
	r.Delete("/businesses/{id}/tickets/{tid}", h.deleteTicket)
}

// --- request DTOs: exact OpenAPI component schemas ---

type replyBody struct {
	BodyText string  `json:"body_text"`
	BodyHTML *string `json:"body_html"`
}

type noteBody struct {
	BodyText string `json:"body_text"`
}

// patchBody is the triage (PATCH ticket) DTO. Status/Priority/Tags are tri-state via
// pointers (nil = absent → preserve). Assignee is decoded as a raw message so the
// handler can distinguish three cases the pointer types cannot: absent (nil → no
// change), explicit JSON null ("null" → unassign), and a quoted uuid ("\"…\"" → assign).
type patchBody struct {
	Status   *string         `json:"status"`
	Priority *string         `json:"priority"`
	Tags     *[]string       `json:"tags"`
	Assignee json.RawMessage `json:"assignee_principal_id"`
}

// --- response DTOs: exact OpenAPI component schemas ---

type requesterResp struct {
	ID           string  `json:"id"`
	TenantRootID string  `json:"tenant_root_id"`
	Email        string  `json:"email"`
	DisplayName  *string `json:"display_name"`
	ContactID    *string `json:"contact_id"`
	FirstSeenAt  string  `json:"first_seen_at"`
	LastSeenAt   string  `json:"last_seen_at"`
}

type assignableMemberResp struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

func toAssignableMemberResp(m AssignableMember) assignableMemberResp {
	return assignableMemberResp{
		ID:          m.ID.String(),
		Email:       m.Email,
		DisplayName: m.DisplayName,
	}
}

type ticketResp struct {
	ID                  string        `json:"id"`
	BusinessID          string        `json:"business_id"`
	TenantRootID        string        `json:"tenant_root_id"`
	Subject             string        `json:"subject"`
	Status              string        `json:"status"`
	Priority            string        `json:"priority"`
	AssigneePrincipalID *string       `json:"assignee_principal_id"`
	Requester           requesterResp `json:"requester"`
	Tags                []string      `json:"tags"`
	MessageCount        int           `json:"message_count"`
	LastMessageAt       *string       `json:"last_message_at"`
	CreatedAt           string        `json:"created_at"`
	UpdatedAt           string        `json:"updated_at"`
}

type attachmentResp struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	BlobKey     string `json:"blob_key"`
}

type messageResp struct {
	ID                string           `json:"id"`
	TicketID          string           `json:"ticket_id"`
	Direction         string           `json:"direction"`
	MessageID         *string          `json:"message_id"`
	InReplyTo         *string          `json:"in_reply_to"`
	References        []string         `json:"references"`
	AuthorPrincipalID *string          `json:"author_principal_id"`
	BodyText          *string          `json:"body_text"`
	BodyHTML          *string          `json:"body_html"`
	Attachments       []attachmentResp `json:"attachments"`
	SPFResult         string           `json:"spf_result"`
	DKIMResult        string           `json:"dkim_result"`
	DMARCResult       string           `json:"dmarc_result"`
	DeliveryState     *string          `json:"delivery_state"`
	CreatedAt         string           `json:"created_at"`
}

const rfc3339 = "2006-01-02T15:04:05.999999999Z07:00"

func toRequesterResp(r Requester) requesterResp {
	return requesterResp{
		ID:           r.ID.String(),
		TenantRootID: r.TenantRootID.String(),
		Email:        r.Email,
		DisplayName:  r.DisplayName,
		ContactID:    uuidStrPtr(r.ContactID),
		FirstSeenAt:  r.FirstSeenAt.UTC().Format(rfc3339),
		LastSeenAt:   r.LastSeenAt.UTC().Format(rfc3339),
	}
}

func toTicketResp(t Ticket) ticketResp {
	var lastMsg *string
	if t.LastMessageAt != nil {
		s := t.LastMessageAt.UTC().Format(rfc3339)
		lastMsg = &s
	}
	tags := t.Tags
	if tags == nil {
		tags = []string{}
	}
	return ticketResp{
		ID:                  t.ID.String(),
		BusinessID:          t.BusinessID.String(),
		TenantRootID:        t.TenantRootID.String(),
		Subject:             t.Subject,
		Status:              t.Status,
		Priority:            t.Priority,
		AssigneePrincipalID: uuidStrPtr(t.AssigneePrincipalID),
		Requester:           toRequesterResp(t.Requester),
		Tags:                tags,
		MessageCount:        t.MessageCount,
		LastMessageAt:       lastMsg,
		CreatedAt:           t.CreatedAt.UTC().Format(rfc3339),
		UpdatedAt:           t.UpdatedAt.UTC().Format(rfc3339),
	}
}

func toMessageResp(m Message) messageResp {
	atts := make([]attachmentResp, 0, len(m.Attachments))
	for _, a := range m.Attachments {
		atts = append(atts, attachmentResp{
			ID: a.ID.String(), Filename: a.Filename, ContentType: a.ContentType,
			Size: a.Size, BlobKey: a.BlobKey,
		})
	}
	refs := m.References
	if refs == nil {
		refs = []string{}
	}
	return messageResp{
		ID:                m.ID.String(),
		TicketID:          m.TicketID.String(),
		Direction:         m.Direction,
		MessageID:         m.MessageID,
		InReplyTo:         m.InReplyTo,
		References:        refs,
		AuthorPrincipalID: uuidStrPtr(m.AuthorPrincipalID),
		BodyText:          m.BodyText,
		BodyHTML:          m.BodyHTML,
		Attachments:       atts,
		SPFResult:         m.SPFResult,
		DKIMResult:        m.DKIMResult,
		DMARCResult:       m.DMARCResult,
		DeliveryState:     m.DeliveryState,
		CreatedAt:         m.CreatedAt.UTC().Format(rfc3339),
	}
}

// --- handlers ---

func (h *Handler) listTickets(w http.ResponseWriter, r *http.Request) {
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
	filter, ferr := parseTicketFilter(r)
	if ferr != nil {
		httpx.WriteError(w, r, ferr)
		return
	}
	page, err := h.svc.ListTickets(r.Context(), pid, bid, filter, r.URL.Query().Get("cursor"), parseLimit(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items := make([]ticketResp, 0, len(page.Items))
	for _, t := range page.Items {
		items = append(items, toTicketResp(t))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": page.NextCursor})
}

func (h *Handler) getTicket(w http.ResponseWriter, r *http.Request) {
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
	tid, err := pathUUID(r, "tid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	t, err := h.svc.GetTicket(r.Context(), pid, bid, tid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toTicketResp(t))
}

// deleteTicket redacts (soft-deletes in place) a ticket the caller holds tickets.delete
// for, returning 204. Unknown / foreign-tenant / already-redacted all surface as a
// no-oracle 404 (RedactTicket maps them to ErrNotFound). The blob bytes are purged
// out-of-band via the attachment.purge outbox.
func (h *Handler) deleteTicket(w http.ResponseWriter, r *http.Request) {
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
	tid, err := pathUUID(r, "tid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if rerr := h.svc.RedactTicket(r.Context(), pid, bid, tid); rerr != nil {
		httpx.WriteError(w, r, rerr)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listMessages(w http.ResponseWriter, r *http.Request) {
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
	tid, err := pathUUID(r, "tid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	page, err := h.svc.ListMessages(r.Context(), pid, bid, tid, r.URL.Query().Get("cursor"), parseLimit(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items := make([]messageResp, 0, len(page.Items))
	for _, m := range page.Items {
		items = append(items, toMessageResp(m))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": page.NextCursor})
}

func (h *Handler) listRequesters(w http.ResponseWriter, r *http.Request) {
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
	var email *string
	if e := r.URL.Query().Get("email"); e != "" {
		email = &e
	}
	page, err := h.svc.ListRequesters(r.Context(), pid, bid, email, r.URL.Query().Get("cursor"), parseLimit(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items := make([]requesterResp, 0, len(page.Items))
	for _, rq := range page.Items {
		items = append(items, toRequesterResp(rq))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": page.NextCursor})
}

// listAssignableMembers returns the business's candidate ticket assignees (FR-011) —
// a single server-capped page, gated on tickets.assign by the route middleware.
func (h *Handler) listAssignableMembers(w http.ResponseWriter, r *http.Request) {
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
	members, err := h.svc.ListAssignableMembers(r.Context(), pid, bid, parseLimit(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items := make([]assignableMemberResp, 0, len(members))
	for _, m := range members {
		items = append(items, toAssignableMemberResp(m))
	}
	// Single capped page: next_cursor is always null (overflow members remain
	// assignable by principal id via the eligibility-checked triage PATCH).
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nil})
}

func (h *Handler) getRequester(w http.ResponseWriter, r *http.Request) {
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
	rid, err := pathUUID(r, "rid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	rq, err := h.svc.GetRequester(r.Context(), pid, bid, rid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toRequesterResp(rq))
}

func (h *Handler) reply(w http.ResponseWriter, r *http.Request) {
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
	tid, err := pathUUID(r, "tid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var body replyBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.BodyText) == "" {
		httpx.WriteError(w, r, errValidation("body_text required"))
		return
	}
	m, err := h.svc.Reply(r.Context(), pid, bid, tid, ReplyInput(body))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toMessageResp(m))
}

func (h *Handler) addNote(w http.ResponseWriter, r *http.Request) {
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
	tid, err := pathUUID(r, "tid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var body noteBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.BodyText) == "" {
		httpx.WriteError(w, r, errValidation("body_text required"))
		return
	}
	m, err := h.svc.AddNote(r.Context(), pid, bid, tid, NoteInput(body))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toMessageResp(m))
}

func (h *Handler) triage(w http.ResponseWriter, r *http.Request) {
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
	tid, err := pathUUID(r, "tid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var body patchBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}

	// Validate enums at the boundary (house pattern: 400 with a safe message).
	if body.Status != nil && !validStatus(*body.Status) {
		httpx.WriteError(w, r, errValidation("invalid status"))
		return
	}
	if body.Priority != nil && !validPriority(*body.Priority) {
		httpx.WriteError(w, r, errValidation("invalid priority"))
		return
	}
	// Reject empty/whitespace-only tags here so junk never reaches the service.
	if body.Tags != nil {
		for _, tag := range *body.Tags {
			if strings.TrimSpace(tag) == "" {
				httpx.WriteError(w, r, errValidation("invalid tag"))
				return
			}
		}
	}

	in := TriageInput{Status: body.Status, Priority: body.Priority, Tags: body.Tags}
	// Tri-state assignee: absent (nil) → no change; explicit null → unassign; quoted
	// uuid → assign. The handler honors AssigneeSet/Assignee; the service eligibility-gates.
	if body.Assignee != nil {
		in.AssigneeSet = true
		if string(body.Assignee) != "null" {
			var s string
			if jerr := json.Unmarshal(body.Assignee, &s); jerr != nil {
				httpx.WriteError(w, r, errValidation("invalid assignee_principal_id"))
				return
			}
			aid, perr := uuid.Parse(s)
			if perr != nil {
				httpx.WriteError(w, r, errValidation("invalid assignee_principal_id"))
				return
			}
			in.Assignee = &aid
		}
	}

	// Conditional permission (OpenAPI): the route is gated tickets.write, but
	// CHANGING the assignee additionally requires tickets.assign. When the body sets
	// the assignee, resolve the caller's perms at the business (RLS-scoped) and refuse
	// with a no-oracle 404 if tickets.assign is absent — same shape RequirePermission
	// renders. We gate on the field being PRESENT (not on it being a net change): a
	// caller without tickets.assign must not even probe assignment. (A nil resolver/db
	// means triage was mounted without the gate wired — fail closed.)
	if in.AssigneeSet {
		if h.db == nil || h.resolve == nil {
			httpx.WriteError(w, r, errs.ErrNotFound)
			return
		}
		var allowed bool
		if derr := h.db.WithPrincipal(r.Context(), pid, func(tx pgx.Tx) error {
			perms, rerr := h.resolve(r.Context(), tx, pid, bid)
			if rerr != nil {
				return rerr
			}
			allowed = perms.Has("tickets.assign")
			return nil
		}); derr != nil {
			httpx.WriteError(w, r, derr)
			return
		}
		if !allowed {
			httpx.WriteError(w, r, errs.ErrNotFound)
			return
		}
	}

	t, err := h.svc.Triage(r.Context(), pid, bid, tid, in)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toTicketResp(t))
}

// --- input parsing ---

func pathUUID(r *http.Request, key string) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, key))
}

// parseLimit reads the limit query param. Absent/malformed → 0 (service applies
// the default); the service also enforces the ≤100 cap, so this never validates
// the ceiling itself.
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

// parseTicketFilter reads the status/priority/assignee/tag query facets. Enum
// values are validated against the closed sets so a bad value is a 400, not an
// empty result that looks like "no such tickets". The assignee `unassigned`
// sentinel is recognized; any other value must be a UUID.
func parseTicketFilter(r *http.Request) (TicketFilter, error) {
	q := r.URL.Query()
	var f TicketFilter

	if s := q.Get("status"); s != "" {
		if !validStatus(s) {
			return f, errValidation("invalid status")
		}
		f.Status = &s
	}
	if p := q.Get("priority"); p != "" {
		if !validPriority(p) {
			return f, errValidation("invalid priority")
		}
		f.Priority = &p
	}
	if a := q.Get("assignee"); a != "" {
		if a == "unassigned" {
			f.Unassigned = true
		} else {
			id, err := uuid.Parse(a)
			if err != nil {
				return f, errValidation("invalid assignee")
			}
			f.Assignee = &id
		}
	}
	if t := q.Get("tag"); t != "" {
		f.Tag = &t
	}
	return f, nil
}

func validStatus(s string) bool {
	switch s {
	case "new", "open", "pending", "solved", "closed":
		return true
	}
	return false
}

func validPriority(p string) bool {
	switch p {
	case "low", "normal", "high", "urgent":
		return true
	}
	return false
}

func errValidation(msg string) error {
	return &validationError{msg: msg}
}

// validationError carries a safe-to-surface message and satisfies errors.Is for
// errs.ErrValidation so httpx.WriteError renders it as a 400 with the message.
type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }
func (e *validationError) Is(target error) bool {
	return target == errs.ErrValidation
}

func uuidStrPtr(u *uuid.UUID) *string {
	if u == nil {
		return nil
	}
	s := u.String()
	return &s
}
