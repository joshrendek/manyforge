package mailing

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) ReadRoutes(r chi.Router) {
	r.Get("/businesses/{id}/mailing/lists", h.listLists)
	r.Get("/businesses/{id}/mailing/lists/{lid}", h.getList)
	r.Get("/businesses/{id}/mailing/lists/{lid}/subscribers", h.listSubscribers)
	r.Get("/businesses/{id}/mailing/lists/{lid}/subscribers/export", h.exportSubscribers)
	r.Get("/businesses/{id}/mailing/lists/{lid}/subscribers/{sid}", h.getSubscriber)
	r.Get("/businesses/{id}/mailing/lists/{lid}/keys", h.listKeys)
	r.Get("/businesses/{id}/mailing/sending-profile", h.getProfile)
	r.Get("/businesses/{id}/mailing/templates", h.listTemplates)
	r.Get("/businesses/{id}/mailing/templates/{tid}", h.getTemplate)
	r.Get("/businesses/{id}/mailing/suppressions", h.listSuppressions)
}
func (h *Handler) WriteRoutes(r chi.Router) {
	r.Post("/businesses/{id}/mailing/lists", h.createList)
	r.Patch("/businesses/{id}/mailing/lists/{lid}", h.updateList)
	r.Delete("/businesses/{id}/mailing/lists/{lid}", h.archiveList)
	r.Post("/businesses/{id}/mailing/lists/{lid}/subscribers", h.createSubscriber)
	r.Post("/businesses/{id}/mailing/lists/{lid}/subscribers/from-contacts", h.fromContacts)
	r.Post("/businesses/{id}/mailing/lists/{lid}/subscribers/import", h.importSubscribers)
	r.Patch("/businesses/{id}/mailing/lists/{lid}/subscribers/{sid}", h.updateSubscriber)
	r.Delete("/businesses/{id}/mailing/lists/{lid}/subscribers/{sid}", h.deleteSubscriber)
	r.Post("/businesses/{id}/mailing/lists/{lid}/keys", h.createKey)
	r.Delete("/businesses/{id}/mailing/lists/{lid}/keys/{kid}", h.revokeKey)
	r.Put("/businesses/{id}/mailing/sending-profile", h.putProfile)
	r.Delete("/businesses/{id}/mailing/sending-profile", h.deleteProfile)
	r.Post("/businesses/{id}/mailing/sending-profile/verify", h.verifyProfile)
	r.Post("/businesses/{id}/mailing/templates", h.createTemplate)
	r.Post("/businesses/{id}/mailing/templates/preview", h.preview)
	r.Patch("/businesses/{id}/mailing/templates/{tid}", h.updateTemplate)
	r.Delete("/businesses/{id}/mailing/templates/{tid}", h.deleteTemplate)
	r.Post("/businesses/{id}/mailing/campaigns/preview", h.preview)
	r.Post("/businesses/{id}/mailing/suppressions", h.createSuppression)
	r.Delete("/businesses/{id}/mailing/suppressions/{sid}", h.deleteSuppression)
}

// SendRoutes registers operations that require the mailing send permission.
func (h *Handler) SendRoutes(r chi.Router) {
	r.Post("/businesses/{id}/mailing/sending-profile/test-send", h.testProfile)
}

type nullableString struct {
	Set   bool
	Value *string
}

func (v *nullableString) UnmarshalJSON(raw []byte) error {
	v.Set = true
	if bytes.Equal(raw, []byte("null")) {
		v.Value = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	v.Value = &s
	return nil
}

type listBody struct {
	Slug        *string        `json:"slug"`
	Name        *string        `json:"name"`
	Description nullableString `json:"description"`
	DoubleOptIn *bool          `json:"double_opt_in"`
}
type subscriberBody struct {
	Email            string         `json:"email"`
	FirstName        nullableString `json:"first_name"`
	LastName         nullableString `json:"last_name"`
	Attributes       map[string]any `json:"attributes"`
	Tags             []string       `json:"tags"`
	SkipConfirmation bool           `json:"skip_confirmation"`
	Status           *string        `json:"status"`
	StatusReason     nullableString `json:"status_reason"`
}
type contactsBody struct {
	ContactIDs       []uuid.UUID `json:"contact_ids"`
	SkipConfirmation bool        `json:"skip_confirmation"`
}
type keyBody struct {
	Label *string `json:"label"`
}
type listKeyResp struct {
	ID             uuid.UUID  `json:"id"`
	BusinessID     uuid.UUID  `json:"business_id"`
	TenantRootID   uuid.UUID  `json:"tenant_root_id"`
	ListID         uuid.UUID  `json:"list_id"`
	PublishableKey string     `json:"publishable_key"`
	Label          *string    `json:"label"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	RevokedAt      *time.Time `json:"revoked_at"`
	HasSecret      bool       `json:"has_secret"`
}
type createListKeyResp struct {
	listKeyResp
	Secret string `json:"secret,omitempty"`
}
type profileBody struct {
	Mode                string             `json:"mode"`
	FromEmail           string             `json:"from_email"`
	FromName            string             `json:"from_name"`
	ReplyTo             *string            `json:"reply_to"`
	PostalAddress       *string            `json:"postal_address"`
	EmailDomainID       *uuid.UUID         `json:"email_domain_id"`
	Resend              *ResendCredentials `json:"resend"`
	SES                 *SESCredentials    `json:"ses"`
	SESRegion           *string            `json:"ses_region"`
	SESConfigurationSet *string            `json:"ses_configuration_set"`
	SNSTopicARN         *string            `json:"sns_topic_arn"`
}
type templateBody struct {
	Name         *string        `json:"name"`
	Subject      *string        `json:"subject"`
	Preheader    nullableString `json:"preheader"`
	BodyMarkdown *string        `json:"body_markdown"`
	TrackOpens   *bool          `json:"track_opens"`
	TrackClicks  *bool          `json:"track_clicks"`
}
type suppressionBody struct {
	Email  string `json:"email"`
	Reason string `json:"reason"`
}
type previewBody struct {
	BodyMarkdown  string  `json:"body_markdown"`
	Preheader     *string `json:"preheader"`
	FromName      *string `json:"from_name"`
	PostalAddress *string `json:"postal_address"`
}
type testSendBody struct {
	To string `json:"to"`
}

func requestIDs(w http.ResponseWriter, r *http.Request, names ...string) (uuid.UUID, []uuid.UUID, bool) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return uuid.Nil, nil, false
	}
	ids := make([]uuid.UUID, len(names))
	for i, name := range names {
		id, err := uuid.Parse(chi.URLParam(r, name))
		if err != nil {
			httpx.WriteError(w, r, errs.ErrNotFound)
			return uuid.Nil, nil, false
		}
		ids[i] = id
	}
	return pid, ids, true
}
func queryLimit(r *http.Request) int  { n, _ := strconv.Atoi(r.URL.Query().Get("limit")); return n }
func noContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

func (h *Handler) listLists(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id")
	if !ok {
		return
	}
	v, err := h.svc.ListLists(r.Context(), pid, ids[0], r.URL.Query().Get("cursor"), queryLimit(r))
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) getList(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "lid")
	if !ok {
		return
	}
	v, err := h.svc.GetList(r.Context(), pid, ids[0], ids[1])
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) createList(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id")
	if !ok {
		return
	}
	var b listBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	doi := true
	if b.DoubleOptIn != nil {
		doi = *b.DoubleOptIn
	}
	v, err := h.svc.CreateList(r.Context(), pid, ids[0], ListInput{
		Slug: stringValue(b.Slug), Name: stringValue(b.Name),
		Description: b.Description.Value, DoubleOptIn: doi,
	})
	write(w, r, http.StatusCreated, v, err)
}
func (h *Handler) updateList(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "lid")
	if !ok {
		return
	}
	var b listBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	v, err := h.svc.UpdateList(r.Context(), pid, ids[0], ids[1], ListUpdate{
		Name: b.Name, Description: b.Description.Value,
		SetDescription: b.Description.Set, DoubleOptIn: b.DoubleOptIn,
	})
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) archiveList(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "lid")
	if !ok {
		return
	}
	if err := h.svc.ArchiveList(r.Context(), pid, ids[0], ids[1]); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	noContent(w)
}

func (h *Handler) listSubscribers(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "lid")
	if !ok {
		return
	}
	q := r.URL.Query()
	v, err := h.svc.ListSubscribers(r.Context(), pid, ids[0], ids[1], SubscriberFilter{
		Query: q.Get("q"), Status: q.Get("status"), Tag: q.Get("tag"),
		Cursor: q.Get("cursor"), Limit: queryLimit(r),
	})
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) getSubscriber(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "lid", "sid")
	if !ok {
		return
	}
	v, err := h.svc.GetSubscriber(r.Context(), pid, ids[0], ids[1], ids[2])
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) createSubscriber(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "lid")
	if !ok {
		return
	}
	var b subscriberBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	v, err := h.svc.CreateSubscriber(r.Context(), pid, ids[0], ids[1], SubscriberInput{
		Email: b.Email, FirstName: b.FirstName.Value, LastName: b.LastName.Value,
		Attributes: b.Attributes, Tags: b.Tags, SkipConfirmation: b.SkipConfirmation,
		ConsentSource: "manual",
	})
	write(w, r, http.StatusCreated, v, err)
}
func (h *Handler) updateSubscriber(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "lid", "sid")
	if !ok {
		return
	}
	var b subscriberBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	var tags *[]string
	if b.Tags != nil {
		tags = &b.Tags
	}
	v, err := h.svc.UpdateSubscriber(r.Context(), pid, ids[0], ids[1], ids[2], SubscriberUpdate{
		FirstName: b.FirstName.Value, SetFirstName: b.FirstName.Set,
		LastName: b.LastName.Value, SetLastName: b.LastName.Set,
		Attributes: b.Attributes, SetAttributes: b.Attributes != nil,
		Status: b.Status, StatusReason: b.StatusReason.Value,
		SetStatusReason: b.StatusReason.Set, Tags: tags,
	})
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) deleteSubscriber(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "lid", "sid")
	if !ok {
		return
	}
	if err := h.svc.UnsubscribeSubscriber(r.Context(), pid, ids[0], ids[1], ids[2], "manual"); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	noContent(w)
}
func (h *Handler) fromContacts(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "lid")
	if !ok {
		return
	}
	var b contactsBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	v, err := h.svc.AddSubscribersFromContacts(r.Context(), pid, ids[0], ids[1], b.ContactIDs, b.SkipConfirmation)
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) importSubscribers(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "lid")
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCSVBytes+(64<<10))
	mr, err := r.MultipartReader()
	if err != nil {
		httpx.WriteError(w, r, validation("multipart form required"))
		return
	}
	var file []byte
	attested, skip := false, false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			httpx.WriteError(w, r, validation("invalid multipart form"))
			return
		}
		raw, err := io.ReadAll(io.LimitReader(part, maxCSVBytes+1))
		closeErr := part.Close()
		if err != nil {
			httpx.WriteError(w, r, validation("cannot read multipart field"))
			return
		}
		if closeErr != nil {
			httpx.WriteError(w, r, validation("cannot close multipart field"))
			return
		}
		switch part.FormName() {
		case "file":
			if len(raw) > maxCSVBytes {
				httpx.WriteError(w, r, validation("CSV exceeds 5 MiB"))
				return
			}
			file = raw
		case "consent_attested":
			attested = strings.EqualFold(strings.TrimSpace(string(raw)), "true")
		case "skip_confirmation":
			skip = strings.EqualFold(strings.TrimSpace(string(raw)), "true")
		}
	}
	if !attested {
		httpx.WriteError(w, r, validation("consent_attested must be true"))
		return
	}
	if len(file) == 0 {
		httpx.WriteError(w, r, validation("file is required"))
		return
	}
	v, err := h.svc.ImportCSV(r.Context(), pid, ids[0], ids[1], bytes.NewReader(file), skip)
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) exportSubscribers(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "lid")
	if !ok {
		return
	}
	if _, err := h.svc.GetList(r.Context(), pid, ids[0], ids[1]); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=subscribers.csv")
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"email", "first_name", "last_name", "status", "tags", "consent_source", "consent_at"}); err != nil {
		return
	}
	cursor := ""
	for {
		page, err := h.svc.ListSubscribers(r.Context(), pid, ids[0], ids[1], SubscriberFilter{Cursor: cursor, Limit: maxPageSize})
		if err != nil {
			return
		}
		for _, s := range page.Items {
			record := []string{
				csvSafe(s.Email), csvSafe(value(s.FirstName)), csvSafe(value(s.LastName)),
				s.Status, csvSafe(strings.Join(s.Tags, ";")), s.ConsentSource,
				s.ConsentAt.UTC().Format("2006-01-02T15:04:05Z"),
			}
			if err := cw.Write(record); err != nil {
				return
			}
		}
		cw.Flush()
		if cw.Error() != nil || page.NextCursor == nil {
			return
		}
		cursor = *page.NextCursor
	}
}

func (h *Handler) listKeys(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "lid")
	if !ok {
		return
	}
	v, err := h.svc.ListListKeys(r.Context(), pid, ids[0], ids[1], queryLimit(r))
	items := make([]listKeyResp, 0, len(v))
	for _, key := range v {
		items = append(items, toListKeyResp(key))
	}
	write(w, r, http.StatusOK, map[string]any{"items": items}, err)
}
func (h *Handler) createKey(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "lid")
	if !ok {
		return
	}
	var b keyBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	v, err := h.svc.CreateListKey(r.Context(), pid, ids[0], ids[1], b.Label)
	write(w, r, http.StatusCreated, createListKeyResp{
		listKeyResp: toListKeyResp(v), Secret: v.Secret,
	}, err)
}
func (h *Handler) revokeKey(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "lid", "kid")
	if !ok {
		return
	}
	if err := h.svc.RevokeListKey(r.Context(), pid, ids[0], ids[1], ids[2]); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	noContent(w)
}
func (h *Handler) getProfile(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id")
	if !ok {
		return
	}
	v, err := h.svc.GetSendingProfile(r.Context(), pid, ids[0])
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) putProfile(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id")
	if !ok {
		return
	}
	var b profileBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	v, err := h.svc.PutSendingProfile(r.Context(), pid, ids[0], SendingProfileInput(b))
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) deleteProfile(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id")
	if !ok {
		return
	}
	if err := h.svc.DeleteSendingProfile(r.Context(), pid, ids[0]); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	noContent(w)
}
func (h *Handler) verifyProfile(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id")
	if !ok {
		return
	}
	v, err := h.svc.VerifySendingProfile(r.Context(), pid, ids[0])
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) testProfile(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id")
	if !ok {
		return
	}
	var body testSendBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if err := h.svc.TestSendingProfile(r.Context(), pid, ids[0], body.To); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	noContent(w)
}

func (h *Handler) listTemplates(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id")
	if !ok {
		return
	}
	v, err := h.svc.ListTemplates(r.Context(), pid, ids[0], r.URL.Query().Get("cursor"), queryLimit(r))
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) getTemplate(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "tid")
	if !ok {
		return
	}
	v, err := h.svc.GetTemplate(r.Context(), pid, ids[0], ids[1])
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) createTemplate(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id")
	if !ok {
		return
	}
	var b templateBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	trackOpens, trackClicks := true, true
	if b.TrackOpens != nil {
		trackOpens = *b.TrackOpens
	}
	if b.TrackClicks != nil {
		trackClicks = *b.TrackClicks
	}
	v, err := h.svc.CreateTemplate(r.Context(), pid, ids[0], TemplateInput{
		Name: stringValue(b.Name), Subject: stringValue(b.Subject),
		Preheader: b.Preheader.Value, BodyMarkdown: stringValue(b.BodyMarkdown),
		TrackOpens: trackOpens, TrackClicks: trackClicks,
	})
	write(w, r, http.StatusCreated, v, err)
}
func (h *Handler) updateTemplate(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "tid")
	if !ok {
		return
	}
	var b templateBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	v, err := h.svc.UpdateTemplate(r.Context(), pid, ids[0], ids[1], TemplateUpdate{
		Name: b.Name, Subject: b.Subject, Preheader: b.Preheader.Value,
		SetPreheader: b.Preheader.Set, BodyMarkdown: b.BodyMarkdown,
		TrackOpens: b.TrackOpens, TrackClicks: b.TrackClicks,
	})
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "tid")
	if !ok {
		return
	}
	if err := h.svc.DeleteTemplate(r.Context(), pid, ids[0], ids[1]); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	noContent(w)
}
func (h *Handler) preview(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id")
	if !ok {
		return
	}
	var body previewBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	v, err := h.svc.Preview(r.Context(), pid, ids[0], PreviewInput(body))
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) listSuppressions(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id")
	if !ok {
		return
	}
	v, err := h.svc.ListSuppressions(r.Context(), pid, ids[0], r.URL.Query().Get("cursor"), queryLimit(r))
	write(w, r, http.StatusOK, v, err)
}
func (h *Handler) createSuppression(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id")
	if !ok {
		return
	}
	var b suppressionBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	v, err := h.svc.CreateSuppression(r.Context(), pid, ids[0], b.Email, b.Reason)
	write(w, r, http.StatusCreated, v, err)
}
func (h *Handler) deleteSuppression(w http.ResponseWriter, r *http.Request) {
	pid, ids, ok := requestIDs(w, r, "id", "sid")
	if !ok {
		return
	}
	if err := h.svc.DeleteSuppression(r.Context(), pid, ids[0], ids[1]); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	noContent(w)
}

func write(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, status, v)
}
func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
func value(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@", rune(v[0])) {
		return "'" + v
	}
	return v
}
func toListKeyResp(v ListKey) listKeyResp {
	return listKeyResp{
		ID: v.ID, BusinessID: v.BusinessID, TenantRootID: v.TenantRootID,
		ListID: v.ListID, PublishableKey: v.PublishableKey, Label: v.Label,
		Status: v.Status, CreatedAt: v.CreatedAt, RevokedAt: v.RevokedAt,
		HasSecret: v.HasSecret,
	}
}
