package feedback

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// Handler exposes the authenticated feedback read + write slices over HTTP. Routes are thin:
// parse + validate input, call the service, project the OpenAPI schema, write JSON.
// Authorization (feedback.read / feedback.write) is enforced by RequirePermission middleware in
// cmd/manyforge, plus the service-layer tenant_root predicate under RLS.
type Handler struct {
	svc *Service
}

// NewHandler builds an authenticated feedback HTTP handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// ReadRoutes mounts the authenticated read endpoints (gated on feedback.read by the caller).
func (h *Handler) ReadRoutes(r chi.Router) {
	r.Get("/businesses/{id}/feedback/boards", h.listBoards)
	r.Get("/businesses/{id}/feedback/boards/{bid}", h.getBoard)
	r.Get("/businesses/{id}/feedback/boards/{bid}/posts", h.listPosts)
	r.Get("/businesses/{id}/feedback/boards/{bid}/keys", h.listKeys)
	r.Get("/businesses/{id}/feedback/posts/{pid}", h.getPost)
}

// WriteRoutes mounts the authenticated write endpoints (gated on feedback.write by the caller).
func (h *Handler) WriteRoutes(r chi.Router) {
	r.Post("/businesses/{id}/feedback/boards", h.createBoard)
	r.Patch("/businesses/{id}/feedback/boards/{bid}", h.updateBoard)
	r.Post("/businesses/{id}/feedback/boards/{bid}/posts", h.createPost)
	r.Post("/businesses/{id}/feedback/boards/{bid}/keys", h.createKey)
	r.Patch("/businesses/{id}/feedback/posts/{pid}", h.setPostStatus)
	r.Delete("/businesses/{id}/feedback/posts/{pid}", h.deletePost)
	r.Post("/businesses/{id}/feedback/posts/{pid}/vote", h.votePost)
	r.Post("/businesses/{id}/feedback/posts/{pid}/convert", h.convertPost)
	r.Post("/businesses/{id}/feedback/keys/{kid}/revoke", h.revokeKey)
}

// --- request DTOs ---

type createBoardBody struct {
	Slug        *string `json:"slug"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	IsPublic    *bool   `json:"is_public"`
}

type updateBoardBody struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	IsPublic    *bool   `json:"is_public"`
}

type createPostBody struct {
	Title string  `json:"title"`
	Body  *string `json:"body"`
}

type setStatusBody struct {
	Status string `json:"status"`
}

type createKeyBody struct {
	Label *string `json:"label"`
}

// --- response DTOs ---

type boardResp struct {
	ID           string  `json:"id"`
	BusinessID   string  `json:"business_id"`
	TenantRootID string  `json:"tenant_root_id"`
	Slug         string  `json:"slug"`
	Name         string  `json:"name"`
	Description  *string `json:"description"`
	IsPublic     bool    `json:"is_public"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

type postResp struct {
	ID                string  `json:"id"`
	BusinessID        string  `json:"business_id"`
	TenantRootID      string  `json:"tenant_root_id"`
	BoardID           string  `json:"board_id"`
	Title             string  `json:"title"`
	Body              *string `json:"body"`
	Status            string  `json:"status"`
	VoteCount         int     `json:"vote_count"`
	AuthorKind        string  `json:"author_kind"`
	AuthorPrincipalID *string `json:"author_principal_id"`
	AuthorIdentity    *string `json:"author_identity"`
	TicketID          *string `json:"ticket_id"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
}

type ingestKeyResp struct {
	ID             string  `json:"id"`
	BusinessID     string  `json:"business_id"`
	TenantRootID   string  `json:"tenant_root_id"`
	BoardID        string  `json:"board_id"`
	PublishableKey string  `json:"publishable_key"`
	Label          *string `json:"label"`
	Status         string  `json:"status"`
	CreatedAt      string  `json:"created_at"`
	RevokedAt      *string `json:"revoked_at"`
}

func toBoardResp(b Board) boardResp {
	return boardResp{
		ID:           b.ID.String(),
		BusinessID:   b.BusinessID.String(),
		TenantRootID: b.TenantRootID.String(),
		Slug:         b.Slug,
		Name:         b.Name,
		Description:  b.Description,
		IsPublic:     b.IsPublic,
		CreatedAt:    b.CreatedAt.UTC().Format(rfc3339),
		UpdatedAt:    b.UpdatedAt.UTC().Format(rfc3339),
	}
}

func toPostResp(p Post) postResp {
	return postResp{
		ID:                p.ID.String(),
		BusinessID:        p.BusinessID.String(),
		TenantRootID:      p.TenantRootID.String(),
		BoardID:           p.BoardID.String(),
		Title:             p.Title,
		Body:              p.Body,
		Status:            p.Status,
		VoteCount:         p.VoteCount,
		AuthorKind:        p.AuthorKind,
		AuthorPrincipalID: uuidStrPtr(p.AuthorPrincipalID),
		AuthorIdentity:    p.AuthorIdentity,
		TicketID:          uuidStrPtr(p.TicketID),
		CreatedAt:         p.CreatedAt.UTC().Format(rfc3339),
		UpdatedAt:         p.UpdatedAt.UTC().Format(rfc3339),
	}
}

func toIngestKeyResp(k IngestKey) ingestKeyResp {
	var revoked *string
	if k.RevokedAt != nil {
		s := k.RevokedAt.UTC().Format(rfc3339)
		revoked = &s
	}
	return ingestKeyResp{
		ID:             k.ID.String(),
		BusinessID:     k.BusinessID.String(),
		TenantRootID:   k.TenantRootID.String(),
		BoardID:        k.BoardID.String(),
		PublishableKey: k.PublishableKey,
		Label:          k.Label,
		Status:         k.Status,
		CreatedAt:      k.CreatedAt.UTC().Format(rfc3339),
		RevokedAt:      revoked,
	}
}

// --- board handlers ---

func (h *Handler) listBoards(w http.ResponseWriter, r *http.Request) {
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
	page, err := h.svc.ListBoards(r.Context(), pid, bid, r.URL.Query().Get("cursor"), parseLimit(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items := make([]boardResp, 0, len(page.Items))
	for _, b := range page.Items {
		items = append(items, toBoardResp(b))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": page.NextCursor})
}

func (h *Handler) getBoard(w http.ResponseWriter, r *http.Request) {
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
	boardID, err := pathUUID(r, "bid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	b, err := h.svc.GetBoard(r.Context(), pid, bid, boardID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toBoardResp(b))
}

func (h *Handler) createBoard(w http.ResponseWriter, r *http.Request) {
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
	var body createBoardBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if trimTo(body.Name) == "" {
		httpx.WriteError(w, r, errValidation("name required"))
		return
	}
	in := BoardInput{Name: body.Name, Description: body.Description}
	if body.Slug != nil {
		in.Slug = *body.Slug
	}
	if body.IsPublic != nil {
		in.IsPublic = *body.IsPublic
	}
	b, err := h.svc.CreateBoard(r.Context(), pid, bid, in)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toBoardResp(b))
}

func (h *Handler) updateBoard(w http.ResponseWriter, r *http.Request) {
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
	boardID, err := pathUUID(r, "bid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var body updateBoardBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	b, err := h.svc.UpdateBoard(r.Context(), pid, bid, boardID, BoardUpdate(body))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toBoardResp(b))
}

// --- post handlers ---

func (h *Handler) listPosts(w http.ResponseWriter, r *http.Request) {
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
	boardID, err := pathUUID(r, "bid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	page, err := h.svc.ListPosts(r.Context(), pid, bid, boardID, r.URL.Query().Get("cursor"), parseLimit(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items := make([]postResp, 0, len(page.Items))
	for _, p := range page.Items {
		items = append(items, toPostResp(p))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": page.NextCursor})
}

func (h *Handler) getPost(w http.ResponseWriter, r *http.Request) {
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
	postID, err := pathUUID(r, "pid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	p, err := h.svc.GetPost(r.Context(), pid, bid, postID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toPostResp(p))
}

func (h *Handler) createPost(w http.ResponseWriter, r *http.Request) {
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
	boardID, err := pathUUID(r, "bid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var body createPostBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if trimTo(body.Title) == "" {
		httpx.WriteError(w, r, errValidation("title required"))
		return
	}
	p, err := h.svc.CreatePost(r.Context(), pid, bid, boardID, PostInput(body))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toPostResp(p))
}

func (h *Handler) setPostStatus(w http.ResponseWriter, r *http.Request) {
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
	postID, err := pathUUID(r, "pid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var body setStatusBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	p, err := h.svc.SetPostStatus(r.Context(), pid, bid, postID, trimTo(body.Status))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toPostResp(p))
}

func (h *Handler) deletePost(w http.ResponseWriter, r *http.Request) {
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
	postID, err := pathUUID(r, "pid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if derr := h.svc.DeletePost(r.Context(), pid, bid, postID); derr != nil {
		httpx.WriteError(w, r, derr)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) votePost(w http.ResponseWriter, r *http.Request) {
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
	postID, err := pathUUID(r, "pid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	voted, count, err := h.svc.Vote(r.Context(), pid, bid, postID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"voted": voted, "vote_count": count})
}

func (h *Handler) convertPost(w http.ResponseWriter, r *http.Request) {
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
	postID, err := pathUUID(r, "pid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	ticketID, err := h.svc.ConvertToTicket(r.Context(), pid, bid, postID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ticket_id": ticketID.String()})
}

// --- ingest key handlers ---

func (h *Handler) listKeys(w http.ResponseWriter, r *http.Request) {
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
	boardID, err := pathUUID(r, "bid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	keys, err := h.svc.ListIngestKeys(r.Context(), pid, bid, boardID, parseLimit(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items := make([]ingestKeyResp, 0, len(keys))
	for _, k := range keys {
		items = append(items, toIngestKeyResp(k))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) createKey(w http.ResponseWriter, r *http.Request) {
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
	boardID, err := pathUUID(r, "bid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var body createKeyBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	k, err := h.svc.CreateIngestKey(r.Context(), pid, bid, boardID, body.Label)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toIngestKeyResp(k))
}

func (h *Handler) revokeKey(w http.ResponseWriter, r *http.Request) {
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
	keyID, err := pathUUID(r, "kid")
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	k, err := h.svc.RevokeIngestKey(r.Context(), pid, bid, keyID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toIngestKeyResp(k))
}
