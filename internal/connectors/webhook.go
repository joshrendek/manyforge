package connectors

// webhook.go — public, signed, principal-less connector webhook handler.
//
// Security model: this route has NO principal context (no manyforge.principal_id GUC).
// All DB reads + writes go through SECURITY DEFINER functions that bypass RLS:
//   - connector_webhook_context($1)  — lookup tenancy + sealed credential
//   - ingest_connector_webhook($1…$5) — dedupe delivery + enqueue outbox event
//
// Oracle policy (§5.1):
//   - Unknown or disabled connector → 202  (no existence oracle)
//   - Body over cap                  → 413
//   - Known connector, bad signature → 401  (real forgery on a real connector, reject loudly)
//   - Known connector, bad payload   → 202  (malformed payload, no oracle)
//   - Known connector, valid sig     → 202  (accepted or replay — both look identical)

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	appdb "github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// defaultMaxWebhookBytes is the cap for inbound webhook bodies (2 MiB). Connectors
// send compact JSON event payloads; 2 MiB is generous while bounding memory.
const defaultMaxWebhookBytes int64 = 2 << 20 // 2 MiB

// WebhookHandler handles public inbound webhooks for all registered connector types.
// It is principal-less: every DB operation goes through SECURITY DEFINER functions.
type WebhookHandler struct {
	DB       *appdb.DB
	Sealer   *crypto.Sealer
	Registry *Registry
	Logger   *slog.Logger
	maxBytes int64
}

// NewWebhookHandler builds a ready-to-use handler.
func NewWebhookHandler(database *appdb.DB, sealer *crypto.Sealer, reg *Registry, logger *slog.Logger) *WebhookHandler {
	return &WebhookHandler{
		DB:       database,
		Sealer:   sealer,
		Registry: reg,
		Logger:   logger,
		maxBytes: defaultMaxWebhookBytes,
	}
}

// PublicRoutes mounts the handler onto the public router. Callers should apply the
// global ingest rate-limiter middleware before calling this (mirrors inbox.Handler.PublicRoutes).
func (h *WebhookHandler) PublicRoutes(r chi.Router) {
	// {type} is present in the URL for discoverability / multi-connector routing, but
	// the actual type comes from the DB (connector_webhook_context), so it is not
	// trusted from the path. The real guard is the per-connector HMAC secret.
	r.Post("/connectors/{type}/{connectorID}/webhook", h.handle)
}

// errUnknownConnector is a sentinel returned inside WithTx when the DEFINER returns
// no row (unknown or disabled connector). Mapped to 202 by the handler.
var errUnknownConnector = errors.New("connector: not found or not enabled")

func (h *WebhookHandler) handle(w http.ResponseWriter, r *http.Request) {
	// 1. Body cap (defense-in-depth; the rate-limiter middleware is above this).
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httpx.WriteJSON(w, http.StatusRequestEntityTooLarge,
				httpx.ErrorBody{Code: "PAYLOAD_TOO_LARGE", Message: "payload too large"})
			return
		}
		// Read error that is NOT the cap: malformed/aborted request. Log quietly.
		h.Logger.WarnContext(r.Context(), "connectors/webhook: body read error", "err", err)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// 2. Parse connectorID from the URL. Bad UUID → 202 (no oracle).
	rawID := chi.URLParam(r, "connectorID")
	connectorID, err := uuid.Parse(rawID)
	if err != nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// 3–9 run inside a single principal-less transaction so the DEFINER lookup +
	// DEFINER dedupe/enqueue are atomic.
	var sigBad bool // set inside the tx if sig verification fails on a known connector
	txErr := h.DB.WithTx(r.Context(), func(tx pgx.Tx) error {
		// 3. DEFINER lookup: connector_webhook_context returns tenancy + sealed cred.
		//    pgx.ErrNoRows = unknown/disabled connector → sentinel mapped to 202.
		var (
			businessID          uuid.UUID
			tenantRootID        uuid.UUID
			ctype               string
			baseURL             string
			allowPrivateBaseURL bool
			sealedSecret        string
		)
		err := tx.QueryRow(r.Context(),
			`SELECT business_id, tenant_root_id, ctype, base_url, allow_private_base_url, sealed_secret
			   FROM connector_webhook_context($1)`,
			connectorID,
		).Scan(&businessID, &tenantRootID, &ctype, &baseURL, &allowPrivateBaseURL, &sealedSecret)
		if errors.Is(err, pgx.ErrNoRows) {
			return errUnknownConnector
		}
		if err != nil {
			return err
		}

		// 4. Unseal the credential blob (Go-side AES-256-GCM decrypt).
		plain, err := h.Sealer.Open(sealedSecret)
		if err != nil {
			// Unsealing failed (tampered ciphertext / wrong key). Log server-side only;
			// never surface the sealed blob or error detail to the caller.
			h.Logger.ErrorContext(r.Context(), "connectors/webhook: unseal failed",
				"connector_id", connectorID)
			return errUnknownConnector // treat as unknown — caller gets 202
		}
		var cred Credential
		if err := json.Unmarshal(plain, &cred); err != nil {
			h.Logger.ErrorContext(r.Context(), "connectors/webhook: credential unmarshal failed",
				"connector_id", connectorID)
			return errUnknownConnector
		}

		// 5. Build the typed connector via the Registry (principal-less; factory only).
		conn, err := h.Registry.BuildSystem(ResolvedConnector{
			ID:                  connectorID.String(),
			Type:                ctype,
			BaseURL:             baseURL,
			AllowPrivateBaseURL: allowPrivateBaseURL,
			Credential:          cred,
		})
		if err != nil {
			h.Logger.ErrorContext(r.Context(), "connectors/webhook: BuildSystem failed",
				"connector_id", connectorID, "type", ctype, "err", err)
			return errUnknownConnector
		}

		// 6. Verify signature. On a KNOWN connector a bad sig → 401 (deliberate; the
		//    secret has been set and the payload is forged). Unknown connector already
		//    returned errUnknownConnector above so we never reach here for those.
		if err := conn.VerifyWebhook(r.Header, body); err != nil {
			// Signal to the outer handler to send 401. We return nil to avoid rolling
			// back the tx on a non-DB error (no writes have happened yet; rollback is
			// harmless either way, but returning nil is cleaner).
			sigBad = true
			return nil
		}

		// 7. Decode the webhook payload. Malformed payload after a valid sig → 202.
		ev, err := conn.DecodeWebhook(body)
		if err != nil {
			h.Logger.WarnContext(r.Context(), "connectors/webhook: decode failed",
				"connector_id", connectorID, "err", err)
			return nil // 202 — not an oracle
		}

		// 8. Dedupe + enqueue via DEFINER (atomically in this tx).
		//    Returns true (new), false (replay) — both map to 202.
		var accepted bool
		if err := tx.QueryRow(r.Context(),
			`SELECT ingest_connector_webhook($1, $2, $3, $4, $5)`,
			connectorID, businessID, tenantRootID, ev.DeliveryID, ev.ExternalID,
		).Scan(&accepted); err != nil {
			return err
		}

		if accepted {
			h.Logger.InfoContext(r.Context(), "connectors/webhook: accepted",
				"connector_id", connectorID, "delivery_id", ev.DeliveryID,
				"external_id", ev.ExternalID)
		}
		// replay: accepted==false → also 202, nothing to log at info level
		return nil
	})

	if txErr != nil && !errors.Is(txErr, errUnknownConnector) {
		// Unexpected DB / tx error. Log server-side; uniform 202 to caller (no oracle).
		h.Logger.ErrorContext(r.Context(), "connectors/webhook: tx error",
			"connector_id", connectorID, "err", txErr)
	}

	// sigBad is set only after a successful DEFINER lookup (known connector), so
	// returning 401 here does NOT leak connector existence for unknown connectors.
	if sigBad {
		httpx.WriteJSON(w, http.StatusUnauthorized,
			httpx.ErrorBody{Code: "UNAUTHORIZED", Message: "unauthorized"})
		return
	}

	// 9. Uniform 202: covers accepted, replay, unknown connector, decode failure,
	//    and all other non-sig, non-cap error paths.
	w.WriteHeader(http.StatusAccepted)
}
