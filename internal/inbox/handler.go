package inbox

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/observability"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
)

// Handler exposes the inbound provider webhook over HTTP. It is PUBLIC: there is no
// bearer/JWT principal on this route — the request is authenticated solely by the
// per-provider HMAC signature (constant-time verified in the WebhookAdapter).
type Handler struct {
	adapter  *WebhookAdapter
	ingester Ingester
	maxBytes int64
	logger   *slog.Logger

	// recipientLimiter throttles ingestion per DECODED recipient address (FR-020).
	// It is applied AFTER decode but BEFORE recipient resolution, uniformly on the
	// normalized recipient string, so a 429 cannot reveal whether the recipient
	// routes (no existence oracle). nil disables the per-recipient cap (the per-IP
	// cap is a separate middleware on the route group, wired in main.go).
	recipientLimiter ratelimit.Limiter

	// Metrics counts ingest outcomes (received/accepted/rejected/duplicate). Set by
	// main after construction; nil ⇒ no-op (existing callers/tests unaffected).
	Metrics *observability.Metrics
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

// SetRecipientLimiter installs the per-recipient ingest limiter (FR-020). It is set
// after construction so the limiter (shared with the per-IP middleware's knobs) is
// built once in main.go. A nil limiter disables the per-recipient cap.
func (h *Handler) SetRecipientLimiter(l ratelimit.Limiter) { h.recipientLimiter = l }

// PublicRoutes mounts the unauthenticated inbound ingress. It is mounted in a
// dedicated public group whose per-IP rate limiter (T032) wraps this route in
// main.go; the per-recipient cap is enforced inside ingest.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Post("/inbound/email/{provider}", h.ingest)
}

// ingest handles POST /inbound/email/{provider}. Order (intentional):
//  1. Body cap → 413 (http.MaxBytesReader; over the cap is refused before any work).
//  2. HMAC verify → 401 on missing/forged signature (constant-time).
//  3. Decode the verified body → RawMessage; a malformed body is a 400.
//  4. Per-recipient rate limit (FR-020) → 429. Applied on the NORMALIZED recipient
//     string BEFORE resolution, so a KNOWN and an UNKNOWN recipient are throttled
//     IDENTICALLY — the 429 never reveals whether the recipient routes (no oracle).
//  5. Ingest. Routed (Created), duplicate (Duplicate), and unknown-recipient
//     (IsNoRoute) ALL return an IDENTICAL 202 — no existence oracle (FR-003/005).
//  6. Any other error → 500 with a stable generic body; the wrapped error is logged
//     server-side and NEVER echoed to the caller.
//
// The per-IP cap is a separate middleware (httpx.RateLimit + ratelimit.ClientIP) on
// the route group in main.go; this method enforces only the per-recipient layer.
func (h *Handler) ingest(w http.ResponseWriter, r *http.Request) {
	// Count every call (received), regardless of outcome.
	h.Metrics.Inc(observability.MetricIngestReceived)

	// 1. Body cap. MaxBytesReader makes Read return an error once the cap is
	//    exceeded; we surface that as 413 (FR-020). Enforced here so the signature
	//    is never computed over an unbounded body.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.Metrics.Inc(observability.MetricIngestRejected)
			httpx.WriteJSON(w, http.StatusRequestEntityTooLarge,
				httpx.ErrorBody{Code: "PAYLOAD_TOO_LARGE", Message: "payload too large"})
			return
		}
		// A read error that is not the cap is a malformed/aborted request body.
		h.Metrics.Inc(observability.MetricIngestRejected)
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{Code: "VALIDATION", Message: "invalid request body"})
		return
	}

	// 2. HMAC verify (constant-time). Missing/forged ⇒ 401, ingestion never runs.
	if !h.adapter.verify(r.Header.Get("X-MF-Signature"), r.Header.Get("X-MF-Timestamp"), body) {
		h.Metrics.Inc(observability.MetricIngestRejected)
		httpx.WriteJSON(w, http.StatusUnauthorized, httpx.ErrorBody{Code: "UNAUTHORIZED", Message: "unauthorized"})
		return
	}

	// 3. Decode the verified body into a RawMessage. A body that passed the cap and
	//    signature but is structurally malformed is a client error — but it does NOT
	//    leak recipient existence (routing happens in Ingest, below), so a 400 here
	//    is safe and not an oracle.
	msg, err := h.adapter.decode(r, body)
	if err != nil {
		h.Metrics.Inc(observability.MetricIngestRejected)
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{Code: "VALIDATION", Message: "invalid inbound payload"})
		return
	}

	// 4. Per-recipient rate limit (FR-020). Keyed on the NORMALIZED recipient (plus/
	//    VERP segment stripped, lowercased) so plus-addressing variants share one
	//    budget. Enforced BEFORE Ingest (which is what resolves the recipient), so an
	//    unknown recipient is throttled identically to a routing one — the 429 reveals
	//    nothing about existence. security: MF-002-INGEST-SCOPE (no-oracle).
	if h.recipientLimiter != nil {
		key, _ := normalizeRecipient(msg.EnvelopeRecipient)
		if !h.recipientLimiter.Allow("rcpt:" + key) {
			h.Metrics.Inc(observability.MetricIngestRejected)
			w.Header().Set("Retry-After", "1")
			httpx.WriteJSON(w, http.StatusTooManyRequests, httpx.ErrorBody{Code: "RATE_LIMITED", Message: "too many requests"})
			return
		}
	}

	// 5. Ingest. Map routed / duplicate / unknown-recipient ALL to an identical 202.
	res, err := h.ingester.Ingest(r.Context(), msg)
	if err != nil {
		if IsNoRoute(err) {
			// Unknown recipient: dropped, zero rows, byte-identical to a routed 202.
			h.Metrics.Inc(observability.MetricIngestAccepted)
			h.writeAccepted(w)
			return
		}
		h.Metrics.Inc(observability.MetricIngestRejected)
		h.logger.ErrorContext(r.Context(), "inbox: webhook ingest failed",
			"err", err, "provider", chiProvider(r))
		httpx.WriteJSON(w, http.StatusInternalServerError, httpx.ErrorBody{Code: "INTERNAL", Message: "internal error"})
		return
	}

	// Routed or duplicate: the same uniform 202 (no oracle). Count separately so a
	// replay storm is visible without changing the response.
	if res.Duplicate {
		h.Metrics.Inc(observability.MetricIngestDuplicate)
	} else {
		h.Metrics.Inc(observability.MetricIngestAccepted)
		h.logger.InfoContext(r.Context(), "inbox: message ingested",
			"ticket_id", res.TicketID, "message_id", res.MessageID, "created", res.Created)
	}
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
