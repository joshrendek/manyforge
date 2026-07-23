package feedback

// public.go — the public, principal-less feedback ingress used by Apple/Android SDKs and a
// future web portal. It has NO principal context (no manyforge.principal_id GUC); every DB
// access goes through the SECURITY DEFINER functions of migration 0102, which bypass RLS.
//
// Auth is a per-board PUBLISHABLE key (Sentry-DSN style) carried in the URL path. It is not a
// secret — the security model is: unguessable random keys + IP rate-limiting (applied by the
// ingress group middleware) + content caps + one-vote-per-identity.
//
// Oracle policy (Spec 006 public-portal boundary):
//   - Unknown / revoked key, or a key on a NON-public board → uniform 401. Never reveals which
//     businesses/boards exist (feedback_public_board returns 0 rows for all three cases).
//   - Body over cap → 413. Malformed body / missing required field → 400.
//   - Valid key, unknown post on that board (vote) → 404 (the caller already holds the board's
//     key, so this is not a business-existence oracle).

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	appdb "github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// maxPublicBytes caps public ingress bodies (64 KiB). Feedback text is small; this bounds
// memory as defense-in-depth beneath the ingress rate-limiter.
const maxPublicBytes int64 = 64 << 10

// PublicHandler serves the principal-less SDK/portal ingress.
type PublicHandler struct {
	DB       *appdb.DB
	Logger   *slog.Logger
	maxBytes int64
}

// NewPublicHandler builds a ready-to-use public ingress handler.
func NewPublicHandler(database *appdb.DB, logger *slog.Logger) *PublicHandler {
	return &PublicHandler{DB: database, Logger: logger, maxBytes: maxPublicBytes}
}

// PublicRoutes mounts the SDK/portal endpoints. The caller applies the global ingest
// rate-limiter before calling this (mirrors connectors.WebhookHandler.PublicRoutes).
func (h *PublicHandler) PublicRoutes(r chi.Router) {
	r.Post("/feedback/public/{key}/posts", h.submit)
	r.Get("/feedback/public/{key}/posts", h.list)
	r.Post("/feedback/public/{key}/posts/{postID}/votes", h.vote)
}

// publicBoard is the tenancy resolved from a publishable key (only for an enabled key on a
// public board).
type publicBoard struct {
	boardID, businessID, tenantRoot uuid.UUID
}

// resolveBoard authenticates a publishable key. found=false means unknown/revoked key or a
// non-public board — the handler answers a uniform 401 (no oracle).
func (h *PublicHandler) resolveBoard(r *http.Request, tx pgx.Tx, key string) (publicBoard, bool, error) {
	var b publicBoard
	var isPublic bool
	err := tx.QueryRow(r.Context(),
		`SELECT board_id, business_id, tenant_root_id, is_public FROM feedback_public_board($1)`,
		key,
	).Scan(&b.boardID, &b.businessID, &b.tenantRoot, &isPublic)
	if errors.Is(err, pgx.ErrNoRows) {
		return publicBoard{}, false, nil
	}
	if err != nil {
		return publicBoard{}, false, err
	}
	return b, true, nil
}

func (h *PublicHandler) submit(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	var body struct {
		Title          string `json:"title"`
		Body           string `json:"body"`
		AuthorIdentity string `json:"author_identity"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	title := trimTo(body.Title)
	if title == "" || len(title) > maxTitleLen {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "title required (1.."+strconv.Itoa(maxTitleLen)+" chars)")
		return
	}
	if len(body.Body) > maxBodyLen {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "body too long")
		return
	}

	var postID uuid.UUID
	var known bool
	txErr := h.DB.WithTx(r.Context(), func(tx pgx.Tx) error {
		b, ok, err := h.resolveBoard(r, tx, key)
		if err != nil {
			return err
		}
		if !ok {
			return nil // known stays false → 401
		}
		known = true
		return tx.QueryRow(r.Context(),
			`SELECT feedback_public_submit($1, $2, $3, $4, $5, $6)`,
			b.boardID, b.businessID, b.tenantRoot, title, body.Body, body.AuthorIdentity,
		).Scan(&postID)
	})
	if txErr != nil {
		h.Logger.ErrorContext(r.Context(), "feedback/public: submit tx error", "err", txErr)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	if !known {
		writeUnauthorized(w)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"id": postID.String(), "title": title, "status": "open", "vote_count": 0,
	})
}

func (h *PublicHandler) vote(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	postID, perr := uuid.Parse(chi.URLParam(r, "postID"))
	if perr != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	var body struct {
		VoterIdentity string `json:"voter_identity"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	vid := trimTo(body.VoterIdentity)
	if vid == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "voter_identity required")
		return
	}

	var known, accepted bool
	var count *int32
	txErr := h.DB.WithTx(r.Context(), func(tx pgx.Tx) error {
		b, ok, err := h.resolveBoard(r, tx, key)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		known = true
		return tx.QueryRow(r.Context(),
			`SELECT accepted, out_votes FROM feedback_public_vote($1, $2, $3, $4, $5)`,
			b.boardID, b.businessID, b.tenantRoot, postID, vid,
		).Scan(&accepted, &count)
	})
	if txErr != nil {
		h.Logger.ErrorContext(r.Context(), "feedback/public: vote tx error", "err", txErr)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	if !known {
		writeUnauthorized(w)
		return
	}
	if count == nil {
		// Valid key, but the post is not on this board (or is deleted). Not a business oracle.
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"voted": accepted, "vote_count": *count})
}

type publicPost struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Body      *string `json:"body,omitempty"`
	Status    string  `json:"status"`
	VoteCount int     `json:"vote_count"`
	CreatedAt string  `json:"created_at"`
}

func (h *PublicHandler) list(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	limit := 20
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n // the DEFINER clamps to [1,100]
		}
	}

	var known bool
	var items []publicPost
	txErr := h.DB.WithTx(r.Context(), func(tx pgx.Tx) error {
		b, ok, err := h.resolveBoard(r, tx, key)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		known = true
		rows, qerr := tx.Query(r.Context(),
			`SELECT id, title, body, status, vote_count, created_at FROM feedback_public_list_posts($1, $2)`,
			b.boardID, limit)
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id        uuid.UUID
				title     string
				bodyText  *string
				status    string
				voteCount int32
				createdAt time.Time
			)
			if err := rows.Scan(&id, &title, &bodyText, &status, &voteCount, &createdAt); err != nil {
				return err
			}
			items = append(items, publicPost{
				ID:        id.String(),
				Title:     title,
				Body:      bodyText,
				Status:    status,
				VoteCount: int(voteCount),
				CreatedAt: createdAt.UTC().Format(rfc3339),
			})
		}
		return rows.Err()
	})
	if txErr != nil {
		h.Logger.ErrorContext(r.Context(), "feedback/public: list tx error", "err", txErr)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	if !known {
		writeUnauthorized(w)
		return
	}
	if items == nil {
		items = []publicPost{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// decode reads and JSON-decodes a capped request body. Returns false (and writes 413/400)
// when the body is over cap or malformed.
func (h *PublicHandler) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "payload too large")
			return false
		}
		writeErr(w, http.StatusBadRequest, "VALIDATION", "invalid request")
		return false
	}
	if len(raw) == 0 {
		return true // empty body → zero-value struct (fields then validated by caller)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "invalid JSON body")
		return false
	}
	return true
}
