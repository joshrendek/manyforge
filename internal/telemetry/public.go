package telemetry

// public.go — the principal-less telemetry ingress used by app SDKs and web snippets. There is no
// principal context here (no manyforge.principal_id GUC); every DB access goes through the
// SECURITY DEFINER functions of migration 0105, which reassert tenant scope from the resolved key
// rather than trusting anything in the request body.
//
// Oracle policy: unknown, revoked, and malformed keys all produce a byte-identical 401. Nothing in
// the response, its body, or its status distinguishes "this key never existed" from "this key was
// revoked" — that difference is a client-existence oracle.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	appdb "github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/observability"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
)

// maxIngestBytes caps an ingest batch at 256 KiB. The cap is set HERE rather than relying on
// global request-size middleware — defense in depth means each ingress helper bounds its own body.
const maxIngestBytes int64 = 256 << 10

// maxBatchEvents bounds how many events one request may carry, so a small compressed body cannot
// expand into an unbounded insert.
const maxBatchEvents = 1000

// Clock skew bounds on the client-supplied occurred_at. Future events are clamped to now; events
// older than maxPastAge are dropped individually (a single stale event must not discard a batch of
// good ones). Neither bound can influence which partition a row lands in — that is keyed on
// ingested_at, a server clock.
const (
	maxFutureSkew = 5 * time.Minute
	maxPastAge    = 7 * 24 * time.Hour
)

// PublicHandler serves the principal-less ingest endpoint.
type PublicHandler struct {
	DB     *appdb.DB
	Logger *slog.Logger
	// Sealer opens the mfs_ signing secret. A client that HAS a sealed secret requires a valid
	// signature and fails closed when no sealer is configured — verification is mandatory for
	// those clients, and being unable to verify is not a reason to accept.
	Sealer *crypto.Sealer
	// PerIP and PerKey are independent limiters. Per-key alone lets one attacker with many keys
	// saturate the endpoint; per-IP alone lets one leaked key be abused from a botnet.
	PerIP  ratelimit.Limiter
	PerKey ratelimit.Limiter
	// TrustedProxies is passed to ratelimit.ClientIP for X-Forwarded-For handling.
	TrustedProxies []*net.IPNet
	Metrics        *observability.Metrics
}

// resolvedClient is the tenant scope a publishable key maps to.
type resolvedClient struct {
	id           uuid.UUID
	businessID   uuid.UUID
	tenantRootID uuid.UUID
	kind         string
	sealedSecret *string
}

// PublicRoutes mounts the ingest endpoint on the principal-less ingress group.
func (h *PublicHandler) PublicRoutes(r chi.Router) {
	r.Post("/telemetry/ingest/{key}", h.ingest)
}

type ingestRequest struct {
	Analytics []AnalyticsEvent `json:"analytics,omitempty"`
	Crash     []CrashEvent     `json:"crash,omitempty"`
}

type ingestResponse struct {
	Accepted int `json:"accepted"`
	Dropped  int `json:"dropped"`
}

// unauthorized writes the single 401 shape used for every auth failure. Callers must not vary the
// body, headers, or status by reason.
func (h *PublicHandler) unauthorized(w http.ResponseWriter) {
	h.Metrics.Inc(observability.MetricTelemetryIngestRejected)
	httpx.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
}

func (h *PublicHandler) ingest(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")

	// Rate limit BEFORE touching the database, so an invalid-key flood cannot be used to drive
	// query load. Per-IP first: it applies even when the key is garbage.
	ip := ratelimit.ClientIP(r, h.TrustedProxies)
	if h.PerIP != nil && !h.PerIP.Allow(ip) {
		httpx.WriteJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
		return
	}
	if h.PerKey != nil && !h.PerKey.Allow(key) {
		httpx.WriteJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
		return
	}

	// Reject implausible key shapes without a DB round-trip — but with the SAME 401 as a
	// well-formed unknown key, so shape is not an oracle either.
	if len(key) != len(keyPrefix)+keyBodyLen || key[:len(keyPrefix)] != keyPrefix {
		h.unauthorized(w)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxIngestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		httpx.WriteJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload_too_large"})
		return
	}

	client, err := h.resolve(r.Context(), key)
	if err != nil {
		h.unauthorized(w)
		return
	}

	// A client that carries a signing secret must present a valid signature. Fail closed: if the
	// secret exists but cannot be opened, reject rather than silently downgrading to anonymous.
	if client.sealedSecret != nil {
		if h.Sealer == nil {
			h.Logger.ErrorContext(r.Context(), "signed telemetry client but no sealer configured",
				"client_id", client.id)
			h.unauthorized(w)
			return
		}
		secret, oerr := h.Sealer.Open(*client.sealedSecret)
		if oerr != nil {
			h.Logger.ErrorContext(r.Context(), "open telemetry signing secret", "err", oerr,
				"client_id", client.id)
			h.unauthorized(w)
			return
		}
		if verr := verifyTelemetrySignature(r.Header.Get("X-Telemetry-Signature"), string(secret),
			r.Method, r.URL.RequestURI(), body, time.Now()); verr != nil {
			h.unauthorized(w)
			return
		}
	}

	var req ingestRequest
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}

	now := time.Now().UTC()
	var accepted, dropped int
	switch client.kind {
	case KindAnalytics:
		events, d := sanitizeAnalytics(req.Analytics, now)
		dropped = d
		if len(events) > 0 {
			accepted, err = h.insertAnalytics(r.Context(), client, events)
		}
	case KindCrash:
		events, d := sanitizeCrash(req.Crash, now)
		dropped = d
		if len(events) > 0 {
			accepted, err = h.insertCrash(r.Context(), client, events)
		}
	default:
		h.unauthorized(w)
		return
	}
	if err != nil {
		// Never echo the driver error: pg messages carry constraint and column names.
		h.Logger.ErrorContext(r.Context(), "telemetry ingest", "err", err, "client_id", client.id)
		httpx.WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "ingest_failed"})
		return
	}

	h.Metrics.Add(observability.MetricTelemetryIngestAccepted, int64(accepted))
	httpx.WriteJSON(w, http.StatusAccepted, ingestResponse{Accepted: accepted, Dropped: dropped})
}

func (h *PublicHandler) resolve(ctx context.Context, key string) (resolvedClient, error) {
	var c resolvedClient
	err := h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, business_id, tenant_root_id, kind, sealed_secret
			   FROM telemetry_resolve_client($1)`, key,
		).Scan(&c.id, &c.businessID, &c.tenantRootID, &c.kind, &c.sealedSecret)
	})
	if err != nil {
		return resolvedClient{}, err
	}
	return c, nil
}

func (h *PublicHandler) insertAnalytics(ctx context.Context, c resolvedClient, events []AnalyticsEvent) (int, error) {
	payload, err := json.Marshal(events)
	if err != nil {
		return 0, err
	}
	var n int
	err = h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			"SELECT telemetry_ingest_analytics($1,$2,$3,$4)",
			c.id, c.businessID, c.tenantRootID, payload).Scan(&n)
	})
	return n, err
}

func (h *PublicHandler) insertCrash(ctx context.Context, c resolvedClient, events []CrashEvent) (int, error) {
	payload, err := json.Marshal(events)
	if err != nil {
		return 0, err
	}
	var n int
	err = h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			"SELECT telemetry_ingest_crash($1,$2,$3,$4)",
			c.id, c.businessID, c.tenantRootID, payload).Scan(&n)
	})
	return n, err
}

// clampOccurredAt pulls a future timestamp back to now. A client clock running fast is ordinary;
// a client clock running fast enough to matter is hostile. Either way the row must not claim to
// have happened after it was received.
func clampOccurredAt(occurred, now time.Time) time.Time {
	if occurred.After(now.Add(maxFutureSkew)) {
		return now
	}
	return occurred
}

// tooOld reports whether an event predates the accepted window. A zero timestamp counts as too
// old: it is almost always an unset field rather than 1970.
func tooOld(occurred, now time.Time) bool {
	return occurred.IsZero() || occurred.Before(now.Add(-maxPastAge))
}

func sanitizeAnalytics(in []AnalyticsEvent, now time.Time) ([]AnalyticsEvent, int) {
	if len(in) > maxBatchEvents {
		in = in[:maxBatchEvents]
	}
	out := make([]AnalyticsEvent, 0, len(in))
	dropped := 0
	for _, e := range in {
		if e.Name == "" || tooOld(e.OccurredAt, now) {
			dropped++
			continue
		}
		e.OccurredAt = clampOccurredAt(e.OccurredAt, now)
		out = append(out, e)
	}
	return out, dropped
}

func sanitizeCrash(in []CrashEvent, now time.Time) ([]CrashEvent, int) {
	if len(in) > maxBatchEvents {
		in = in[:maxBatchEvents]
	}
	out := make([]CrashEvent, 0, len(in))
	dropped := 0
	for _, e := range in {
		if e.Platform == "" || e.Signature == "" || tooOld(e.OccurredAt, now) {
			dropped++
			continue
		}
		e.OccurredAt = clampOccurredAt(e.OccurredAt, now)
		out = append(out, e)
	}
	return out, dropped
}
