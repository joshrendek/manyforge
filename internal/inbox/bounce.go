package inbox

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// BounceSuppressor is the boundary the bounce handler depends on: it suppresses a
// hard-bounced recipient (global email_suppression) and, when the bounced
// Message-ID is known, marks the correlated outbound message failed. Depending on
// the interface (not the concrete DB type) lets the handler contract be tested
// without a database. *DBBounceSuppressor satisfies it.
type BounceSuppressor interface {
	// SuppressBounce records recipient in email_suppression and, if messageID is
	// non-empty, marks the globally-unique outbound message failed. Both effects run
	// in one transaction (all-or-nothing). messageID may be empty (suppress only).
	SuppressBounce(ctx context.Context, recipient, messageID string) error
}

// BounceHandler exposes the provider hard-bounce intake webhook over HTTP. Like the
// inbound email webhook it is PUBLIC (no bearer/JWT principal): the request is
// authenticated solely by a per-deployment HMAC signature over the raw body,
// verified in constant time. It MIRRORS the inbox webhook's HMAC scheme (header,
// sha256= prefix, timestamp binding, constant-time compare) and body cap, but
// against a SEPARATE, purpose-separated secret (InboundBounceSecret).
//
// Missing/forged signature ⇒ 401, EXACTLY like the inbox webhook (auth, not a
// recipient oracle — a 401 on a bad HMAC reveals nothing about recipients: an
// attacker without the secret already knows they cannot forge). Once AUTHENTICATED,
// every outcome — hard bounce, soft bounce, parse failure, unknown recipient, missing/
// unmatched message — returns the SAME uniform 202 ack, byte-identical, so the
// response never reveals whether a recipient is a real customer or whether the
// Message-ID correlated to an outbound row. security: MF-002-BOUNCE-NO-ORACLE.
type BounceHandler struct {
	secret   []byte
	sup      BounceSuppressor
	maxBytes int64
	logger   *slog.Logger
}

// NewBounceHandler builds the hard-bounce webhook handler. secret is the
// purpose-separated InboundBounceSecret (NOT the inbound-webhook secret); maxBytes
// caps the request body in the helper itself (defense-in-depth, not only via global
// middleware).
func NewBounceHandler(sup BounceSuppressor, secret string, maxBytes int64, logger *slog.Logger) *BounceHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &BounceHandler{
		secret:   []byte(secret),
		sup:      sup,
		maxBytes: maxBytes,
		logger:   logger,
	}
}

// PublicRoutes mounts the unauthenticated bounce ingress. It is mounted in the same
// per-IP ingest-rate-limited public group as the inbound email webhook (wired in
// main.go), NOT a JWT-auth group.
func (h *BounceHandler) PublicRoutes(r chi.Router) {
	r.Post("/inbound/bounce", h.ingest)
}

// bouncePayload is the normalized hard-bounce notification a provider posts. type is
// the bounce class ("hard" triggers suppression; anything else is ignored). The
// message_id is the globally-unique outbound Message-ID we minted (rfc_message_id),
// the correlation key for marking the outbound message failed; it is optional.
type bouncePayload struct {
	Recipient string `json:"recipient"`
	Type      string `json:"type"`
	MessageID string `json:"message_id"`
}

// ingest handles POST /inbound/bounce. Order (intentional):
//  1. Body cap (http.MaxBytesReader) — over the cap is refused before any work.
//  2. HMAC verify (constant-time, mirrors the inbox webhook scheme). Missing/forged ⇒ 401.
//  3. Parse the bounce payload.
//  4. On a VALID hard bounce with a non-empty recipient: suppress + (if message_id
//     present) mark the outbound message failed, in one transaction.
//
// Missing/forged signature ⇒ 401 (auth, not a recipient oracle); once authenticated,
// ALL outcomes return a uniform 202 (no recipient/message existence oracle). So a
// soft bounce, a parse error, an unknown recipient, and a missing/unmatched message
// are byte-identical to a matching hard bounce — an authenticated caller cannot probe
// which recipients are real customers or which Message-IDs exist. An unexpected
// suppression error is logged server-side and still answered 202 (a provider retry is
// harmless: SuppressBounce is idempotent). security: MF-002-BOUNCE-NO-ORACLE.
func (h *BounceHandler) ingest(w http.ResponseWriter, r *http.Request) {
	// 1. Body cap. A body over the cap reads as an error; we DON'T leak that as 413
	//    (which would be an oracle on the cap) — we fall through to the uniform 202
	//    without doing any work.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// Operability only: a Warn (no body, no recipient — leaks nothing) so an
		// oversize/aborted bounce body is observable. Still the uniform 202.
		h.logger.WarnContext(r.Context(), "inbox: bounce body read failed (capped or aborted)", "err", err)
		h.writeAccepted(w)
		return
	}

	// 2. HMAC verify (constant-time). Missing/forged ⇒ 401, EXACTLY like the inbox
	//    webhook (handler.go): authentication is separate from the no-oracle property —
	//    a 401 on a bad HMAC leaks nothing about recipients (an attacker without the
	//    secret already knows they cannot forge), and returning 202 to an unauthenticated
	//    request is wrong semantics. The no-oracle ack applies to AUTHENTICATED outcomes.
	if !h.verify(r.Header.Get("X-MF-Signature"), r.Header.Get("X-MF-Timestamp"), body) {
		httpx.WriteJSON(w, http.StatusUnauthorized, httpx.ErrorBody{Code: "UNAUTHORIZED", Message: "unauthorized"})
		return
	}

	// 3. Parse. A malformed body is not a client-error 400 here (that would distinguish
	//    a parse outcome); fall through to 202.
	var p bouncePayload
	if jerr := json.Unmarshal(body, &p); jerr != nil {
		h.writeAccepted(w)
		return
	}

	// 4. Only a hard bounce with a recipient suppresses (FR-013). A soft bounce, or a
	//    hard bounce with no recipient, is a no-op — still 202.
	if strings.EqualFold(p.Type, "hard") && p.Recipient != "" {
		if serr := h.sup.SuppressBounce(r.Context(), p.Recipient, p.MessageID); serr != nil {
			// Log server-side (wrapped) and still ack 202: never echo the error, and a
			// provider retry against the idempotent SuppressBounce is harmless.
			h.logger.ErrorContext(r.Context(), "inbox: bounce suppression failed", "err", serr)
		}
	}

	h.writeAccepted(w)
}

// writeAccepted writes the single uniform 202 ack. EVERY AUTHENTICATED outcome flows
// through here so the response is byte-identical across them (no recipient/message
// existence oracle); only the pre-auth 401 (bad/forged signature) is distinct.
func (h *BounceHandler) writeAccepted(w http.ResponseWriter) {
	w.WriteHeader(http.StatusAccepted)
}

// verify checks the X-MF-Signature against the SEPARATE bounce secret. It delegates
// to the package-shared verifyHMAC so the bounce and inbound-email webhooks can never
// drift apart in auth strength; the thin method survives to carry the bounce secret
// field and this finding's tag. security: MF-002-BOUNCE-SIG.
func (h *BounceHandler) verify(sig, timestamp string, body []byte) bool {
	return verifyHMAC(h.secret, sig, timestamp, body)
}

// DBBounceSuppressor is the production BounceSuppressor backed by the real database.
// It is principal-less (the bounce webhook carries no end-user principal): the global
// email_suppression table is NOT RLS-protected, so InsertSuppression runs in a plain
// WithTx; the RLS-protected ticket_message is marked failed via the SECURITY DEFINER
// mark_bounced_message (correlated by the globally-unique outbound Message-ID),
// called raw because it has no sqlc query. Both run in ONE transaction.
type DBBounceSuppressor struct{ db *db.DB }

// NewDBBounceSuppressor wires the DB-backed suppressor.
func NewDBBounceSuppressor(database *db.DB) *DBBounceSuppressor {
	return &DBBounceSuppressor{db: database}
}

// SuppressBounce inserts the recipient into email_suppression and, when messageID is
// non-empty, marks the correlated outbound message failed via mark_bounced_message —
// in one transaction. InsertSuppression is ON CONFLICT DO NOTHING and the DEFINER's
// UPDATE is keyed on a globally-unique Message-ID, so the whole operation is
// idempotent under provider at-least-once redelivery.
func (s *DBBounceSuppressor) SuppressBounce(ctx context.Context, recipient, messageID string) error {
	return s.db.WithTx(ctx, func(tx pgx.Tx) error {
		// email_suppression is global (no tenant scope, no RLS) ⇒ principal-less dbgen
		// in a plain WithTx is correct.
		if err := dbgen.New(tx).InsertSuppression(ctx, dbgen.InsertSuppressionParams{
			Email:  recipient,
			Reason: "hard_bounce",
		}); err != nil {
			return err
		}
		if messageID == "" {
			return nil
		}
		// ticket_message IS RLS-protected and we hold no principal ⇒ the mark MUST go
		// through the SECURITY DEFINER (migration 0020), called raw (no sqlc query).
		_, err := tx.Exec(ctx, "SELECT mark_bounced_message($1)", messageID)
		return err
	})
}

// compile-time assertion that the DB suppressor satisfies the handler boundary.
var _ BounceSuppressor = (*DBBounceSuppressor)(nil)
