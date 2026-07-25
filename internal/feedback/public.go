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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/crypto"
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
	// Sealer nil-behavior is bifurcated by the key's own state: a signed request against a key
	// that HAS a sealed_secret gets 401 (fail closed — verification is required but impossible
	// without a sealer). A key with no sealed_secret (verified tier never enabled for it)
	// degrades to anonymous ingress and is unaffected by a nil Sealer.
	Sealer   *crypto.Sealer
	maxBytes int64
}

// NewPublicHandler builds a ready-to-use public ingress handler.
func NewPublicHandler(database *appdb.DB, logger *slog.Logger, sealer *crypto.Sealer) *PublicHandler {
	return &PublicHandler{DB: database, Logger: logger, Sealer: sealer, maxBytes: maxPublicBytes}
}

// PublicRoutes mounts the SDK/portal endpoints. The caller applies the global ingest
// rate-limiter before calling this (mirrors connectors.WebhookHandler.PublicRoutes).
func (h *PublicHandler) PublicRoutes(r chi.Router) {
	r.Post("/feedback/public/{key}/posts", h.submit)
	r.Get("/feedback/public/{key}/posts", h.list)
	r.Post("/feedback/public/{key}/posts/{postID}/votes", h.vote)
}

// publicBoard is the tenancy resolved from a publishable key (only for an enabled key on a
// public board), plus the key's own id and sealed secret (needed to verify a signed request).
type publicBoard struct {
	boardID, businessID, tenantRoot, keyID uuid.UUID
	sealedSecret                           *string
}

// resolveBoard authenticates a publishable key. found=false means unknown/revoked key or a
// non-public board — the handler answers a uniform 401 (no oracle).
func (h *PublicHandler) resolveBoard(r *http.Request, tx pgx.Tx, key string) (publicBoard, bool, error) {
	var b publicBoard
	var isPublic bool
	err := tx.QueryRow(r.Context(),
		`SELECT board_id, business_id, tenant_root_id, is_public, key_id, sealed_secret
		   FROM feedback_public_board($1)`, key,
	).Scan(&b.boardID, &b.businessID, &b.tenantRoot, &isPublic, &b.keyID, &b.sealedSecret)
	if errors.Is(err, pgx.ErrNoRows) {
		return publicBoard{}, false, nil
	}
	if err != nil {
		return publicBoard{}, false, err
	}
	return b, true, nil
}

// readBody reads the capped raw request body (needed verbatim for the signature base + body
// hash). Writes 413/400 and returns false on cap/read failure.
func (h *PublicHandler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "payload too large")
			return nil, false
		}
		writeErr(w, http.StatusBadRequest, "VALIDATION", "invalid request")
		return nil, false
	}
	return raw, true
}

// resolveVerified applies the §3 fail-closed matrix. verified=true ⇒ v: namespace; sigBad=true
// ⇒ caller must answer 401. raw is the exact request body (nil for GET). Call only after a known
// key (resolveBoard ok=true).
func (h *PublicHandler) resolveVerified(r *http.Request, raw []byte, sealedSecret *string) (verified, sigBad bool) {
	header := r.Header.Get("X-Feedback-Signature")
	if strings.TrimSpace(header) == "" {
		return false, false // no signature → anon
	}
	if sealedSecret == nil {
		h.Logger.WarnContext(r.Context(), "feedback/public: signature on key without secret")
		return false, false // nothing to verify → anon
	}
	if h.Sealer == nil {
		h.Logger.ErrorContext(r.Context(), "feedback/public: signature but sealer disabled")
		return false, true // secret exists but can't unseal → fail closed
	}
	secret, err := h.Sealer.Open(*sealedSecret)
	if err != nil {
		h.Logger.ErrorContext(r.Context(), "feedback/public: unseal failed")
		return false, true
	}
	if verr := verifyFeedbackSignature(header, string(secret), r.Method, r.URL.RequestURI(), raw, time.Now()); verr != nil {
		return false, true // present-but-bad → 401
	}
	return true, false
}

func (h *PublicHandler) submit(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	raw, ok := h.readBody(w, r)
	if !ok {
		return
	}
	var body struct {
		Title          string `json:"title"`
		Body           string `json:"body"`
		AuthorIdentity string `json:"author_identity"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "VALIDATION", "invalid JSON body")
			return
		}
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
	if len(body.IdempotencyKey) > 255 {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "idempotency_key too long")
		return
	}
	if len(body.AuthorIdentity) > 200 {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "author_identity too long")
		return
	}
	bodyHash := sha256.Sum256(raw)

	var postID uuid.UUID
	var known, verified, sigBad, deduped bool
	txErr := h.DB.WithTx(r.Context(), func(tx pgx.Tx) error {
		b, ok, err := h.resolveBoard(r, tx, key)
		if err != nil {
			return err
		}
		if !ok {
			return nil // known stays false → 401
		}
		known = true
		v, bad := h.resolveVerified(r, raw, b.sealedSecret)
		if bad {
			sigBad = true
			return nil // do not submit
		}
		verified = v
		return tx.QueryRow(r.Context(),
			`SELECT post_id, deduped FROM feedback_public_submit($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			b.boardID, b.businessID, b.tenantRoot, title, body.Body, body.AuthorIdentity,
			verified, b.keyID, body.IdempotencyKey, bodyHash[:],
		).Scan(&postID, &deduped)
	})
	if txErr != nil {
		var pgErr *pgconn.PgError
		if errors.As(txErr, &pgErr) && pgErr.Code == "FB409" {
			writeErr(w, http.StatusConflict, "CONFLICT", "idempotency key reused with a different request")
			return
		}
		h.Logger.ErrorContext(r.Context(), "feedback/public: submit tx error", "err", txErr)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	if !known {
		writeUnauthorized(w)
		return
	}
	if sigBad {
		writeUnauthorized(w)
		return
	}
	status := http.StatusCreated
	if deduped {
		status = http.StatusOK
	}
	httpx.WriteJSON(w, status, map[string]any{
		"id": postID.String(), "title": title, "status": "open", "vote_count": 0,
		"identity_verified": verified, "deduped": deduped,
	})
}

func (h *PublicHandler) vote(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	postID, perr := uuid.Parse(chi.URLParam(r, "postID"))
	if perr != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	raw, ok := h.readBody(w, r)
	if !ok {
		return
	}
	var body struct {
		VoterIdentity string `json:"voter_identity"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "VALIDATION", "invalid JSON body")
			return
		}
	}
	vid := trimTo(body.VoterIdentity)
	if vid == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "voter_identity required")
		return
	}
	if len(vid) > 200 {
		writeErr(w, http.StatusBadRequest, "VALIDATION", "voter_identity too long")
		return
	}

	var known, verified, sigBad, accepted bool
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
		v, bad := h.resolveVerified(r, raw, b.sealedSecret)
		if bad {
			sigBad = true
			return nil
		}
		verified = v
		return tx.QueryRow(r.Context(),
			`SELECT accepted, out_votes FROM feedback_public_vote($1,$2,$3,$4,$5,$6)`,
			b.boardID, b.businessID, b.tenantRoot, postID, vid, verified,
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
	if sigBad {
		writeUnauthorized(w)
		return
	}
	if count == nil {
		// Valid key, but the post is not on this board (or is deleted). Not a business oracle.
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"voted": accepted, "vote_count": *count, "identity_verified": verified,
	})
}

type publicPost struct {
	ID               string  `json:"id"`
	Title            string  `json:"title"`
	Body             *string `json:"body,omitempty"`
	Status           string  `json:"status"`
	VoteCount        int     `json:"vote_count"`
	CreatedAt        string  `json:"created_at"`
	ViewerVoted      bool    `json:"viewer_voted"`
	IdentityVerified bool    `json:"identity_verified"`
}

// namespacedParam prefixes a read identity with the caller's authoritative tier (a:/v:) and
// caps to 200 — matching the write-side transform so it can match a stored identity. Returns
// nil for an empty value (→ SQL NULL → no filter / viewer_voted=false).
func namespacedParam(v string, verified bool) *string {
	v = trimTo(v)
	if v == "" {
		return nil
	}
	// Bound the byte length before the []rune conversion below: an attacker-controlled query
	// value could otherwise force allocation of an unbounded rune slice. 800 bytes is always
	// >= any 200-rune prefix (a rune is at most 4 bytes), and a mid-rune byte cut here is
	// harmless since the []rune(...)[:200] re-yields a valid UTF-8 prefix regardless.
	if len(v) > 800 {
		v = v[:800]
	}
	if r := []rune(v); len(r) > 200 {
		v = string(r[:200])
	}
	prefix := "a:"
	if verified {
		prefix = "v:"
	}
	s := prefix + v
	return &s
}

func (h *PublicHandler) list(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	limit := 20
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n // the DEFINER clamps to [1,100]
		}
	}

	var known, sigBad bool
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
		verified, bad := h.resolveVerified(r, nil, b.sealedSecret) // GET: empty body
		if bad {
			sigBad = true
			return nil
		}
		pViewer := namespacedParam(r.URL.Query().Get("voter_identity"), verified)
		pAuthor := namespacedParam(r.URL.Query().Get("author"), verified)

		rows, qerr := tx.Query(r.Context(),
			`SELECT id, title, body, status, vote_count, created_at, viewer_voted, identity_verified
			   FROM feedback_public_list_posts($1, $2, $3, $4)`,
			b.boardID, limit, pViewer, pAuthor)
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id         uuid.UUID
				title      string
				bodyText   *string
				status     string
				voteCount  int32
				createdAt  time.Time
				viewerVote bool
				idVerified bool
			)
			if err := rows.Scan(&id, &title, &bodyText, &status, &voteCount, &createdAt, &viewerVote, &idVerified); err != nil {
				return err
			}
			items = append(items, publicPost{
				ID: id.String(), Title: title, Body: bodyText, Status: status,
				VoteCount: int(voteCount), CreatedAt: createdAt.UTC().Format(rfc3339),
				ViewerVoted: viewerVote, IdentityVerified: idVerified,
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
	if sigBad {
		writeUnauthorized(w)
		return
	}
	if items == nil {
		items = []publicPost{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}
