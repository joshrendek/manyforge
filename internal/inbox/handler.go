package inbox

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// Handler exposes the inbound provider webhook over HTTP. It is PUBLIC: there is no
// bearer/JWT principal on this route — the request is authenticated solely by the
// per-provider HMAC signature (constant-time verified in the WebhookAdapter).
type Handler struct {
	adapter  *WebhookAdapter
	ingester Ingester
	maxBytes int64
	logger   *slog.Logger
}

// NewWebhookHandler builds the inbound webhook HTTP handler. It depends on the
// Ingester interface (not the concrete *Service) so the handler contract can be
// tested without a database. secret is the per-provider HMAC-SHA256 shared secret;
// maxBytes caps the request body (defense-in-depth: enforced here, not only via
// global middleware). cfg is accepted for forward-compatibility (system domain,
// etc.); the webhook path itself needs no reply-token verification.
func NewWebhookHandler(ing Ingester, secret string, maxBytes int64, _ Config, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		adapter:  NewWebhookAdapter(secret),
		ingester: ing,
		maxBytes: maxBytes,
		logger:   logger,
	}
}

// PublicRoutes mounts the unauthenticated inbound ingress. It is mounted in the
// public route group; a public-group rate limiter (T032) can later wrap this route
// without changing the handler.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Post("/inbound/email/{provider}", h.ingest)
}

// ingest handles POST /inbound/email/{provider}. Order (intentional):
//  1. Body cap → 413 (http.MaxBytesReader; over the cap is refused before any work).
//  2. HMAC verify → 401 on missing/forged signature (constant-time).
//  3. Decode the verified body → RawMessage; a malformed body is a 400.
//  4. Ingest. Routed (Created), duplicate (Duplicate), and unknown-recipient
//     (IsNoRoute) ALL return an IDENTICAL 202 — no existence oracle (FR-003/005).
//  5. Any other error → 500 with a stable generic body; the wrapped error is logged
//     server-side and NEVER echoed to the caller.
func (h *Handler) ingest(w http.ResponseWriter, r *http.Request) {
	// 1. Body cap. MaxBytesReader makes Read return an error once the cap is
	//    exceeded; we surface that as 413 (FR-020). Enforced here so the signature
	//    is never computed over an unbounded body.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httpx.WriteJSON(w, http.StatusRequestEntityTooLarge,
				httpx.ErrorBody{Code: "PAYLOAD_TOO_LARGE", Message: "payload too large"})
			return
		}
		// A read error that is not the cap is a malformed/aborted request body.
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{Code: "VALIDATION", Message: "invalid request body"})
		return
	}

	// 2. HMAC verify (constant-time). Missing/forged ⇒ 401, ingestion never runs.
	if !h.adapter.verify(r.Header.Get("X-MF-Signature"), r.Header.Get("X-MF-Timestamp"), body) {
		httpx.WriteJSON(w, http.StatusUnauthorized, httpx.ErrorBody{Code: "UNAUTHORIZED", Message: "unauthorized"})
		return
	}

	// 3. Decode the verified body into a RawMessage. A body that passed the cap and
	//    signature but is structurally malformed is a client error — but it does NOT
	//    leak recipient existence (routing happens in Ingest, below), so a 400 here
	//    is safe and not an oracle.
	msg, err := h.adapter.decode(r, body)
	if err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{Code: "VALIDATION", Message: "invalid inbound payload"})
		return
	}

	// 4. Ingest. Map routed / duplicate / unknown-recipient ALL to an identical 202.
	if _, err := h.ingester.Ingest(r.Context(), msg); err != nil {
		if IsNoRoute(err) {
			// Unknown recipient: dropped, zero rows written, byte-identical to a
			// routed message (no existence oracle, FR-003/SC-006).
			h.writeAccepted(w)
			return
		}
		// 5. Any other error: log server-side (wrapped), return a stable generic
		//    body. NEVER echo err.Error() — it can leak DB/schema/library internals.
		h.logger.ErrorContext(r.Context(), "inbox: webhook ingest failed",
			"err", err, "provider", chiProvider(r))
		httpx.WriteJSON(w, http.StatusInternalServerError, httpx.ErrorBody{Code: "INTERNAL", Message: "internal error"})
		return
	}

	// Routed or duplicate: the same uniform 202.
	h.writeAccepted(w)
}

// writeAccepted writes the single uniform 202 ack. Routed, duplicate, and
// unknown-recipient outcomes MUST all flow through here so the response is
// byte-identical across them (no existence oracle).
func (h *Handler) writeAccepted(w http.ResponseWriter) {
	w.WriteHeader(http.StatusAccepted)
}

// chiProvider returns the {provider} path param.
func chiProvider(r *http.Request) string {
	return chi.URLParam(r, "provider")
}
