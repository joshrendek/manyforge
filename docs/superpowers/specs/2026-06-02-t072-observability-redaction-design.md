# T072 — Observability (ingest / outbound / outbox) + credential redaction

**Spec:** 002-support-desk · **Task:** T072 · **bd:** manyforge-n0q.1
**Date:** 2026-06-02 · **Status:** design approved, pending spec review

## Goal

Structured logging + metrics for the three spec-002 pipelines (inbound
ingestion, outbound send, outbox drain), and a structural guarantee that
credential-bearing values never reach log output. Satisfies plan.md Principle VI
(Observability & Auditability — "Structured slog with request/correlation IDs;
existing health/readiness/metrics extended") and Principle II ("credential-bearing
values redacted in logs").

## Context (current state)

- `/metrics` already serves stdlib **`expvar`** (`expvar.Handler()`), wired in
  `cmd/manyforge/main.go` and `observability.RegisterHealth`. No Prometheus.
- Logging is **slog** throughout; every worker/handler receives a `*slog.Logger`.
  `cmd/manyforge/main.go` builds it via `observability.NewLogger`, sets it as the
  slog default, and passes it to all workers.
- **No correlation/request-id infrastructure** exists in the 001 foundation.
- The codebase is already disciplined about secrets — the only key-adjacent log
  lines (`sender_subscriber.go:204/209`) log `dkim_domain`/`business_id`, never
  key bytes. Redaction here is **defense-in-depth**, not fixing an active leak.

## Decisions (resolved during brainstorming)

1. **Metrics backend: extend `expvar`** (zero new deps, already at `/metrics`,
   self-host-friendly). Not Prometheus.
2. **Redaction: a slog `ReplaceAttr` hook** in `NewLogger` (structural, fires for
   every attr incl. nested groups). Not just discipline + helpers.
3. **Correlation: domain keys, no new middleware** — ingest by
   `ticket_id`+`message_id` (the handler has no business id; routing is internal to
   Ingest), outbound by `message_id`, outbox by event `id`+`topic`. No HTTP
   request-id middleware (platform-wide; belongs to the 001 foundation, out of
   scope for this polish task).
4. **Wiring: nil-safe injected `*Metrics`** as an exported field on the pipeline
   structs/handlers, set by `main`. `nil` ⇒ no-op. Mirrors the project's existing
   "optional injected dependency" convention (e.g. `SendSubscriber.Suppression`),
   so **no constructor signatures change** and existing tests are untouched.

## Components

### (A) Metrics — `internal/platform/observability/metrics.go` (new)

```go
type Metrics struct { m *expvar.Map } // nil receiver ⇒ all methods no-op

func NewMetrics() *Metrics            // reuses the published "support" map if present (test-safe)
func (m *Metrics) Inc(key string)
func (m *Metrics) Add(key string, n int64)
func (m *Metrics) Get(key string) int64 // for tests
```

- One published `expvar.Map` named `"support"` (so values appear under
  `/metrics` → `{"support": {...}}`). `NewMetrics` checks `expvar.Get("support")`
  and reuses it, else `expvar.NewMap` — so calling it more than once (tests)
  never panics.
- Nil-safe: a `nil *Metrics` makes `Inc`/`Add` no-ops, so a worker with no metrics
  wired behaves exactly as today.
- Exported counter-key constants (producers + tests share them):

| Pipeline | Constant → key |
|---|---|
| Ingest | `MetricIngestReceived` = `ingest.received`, `MetricIngestAccepted` = `ingest.accepted`, `MetricIngestRejected` = `ingest.rejected`, `MetricIngestDuplicate` = `ingest.duplicate` |
| Outbound | `MetricOutboundSent` = `outbound.sent`, `MetricOutboundFailed` = `outbound.failed`, `MetricOutboundSuppressed` = `outbound.suppressed` |
| Outbox | `MetricOutboxDrained` = `outbox.drained`, `MetricOutboxRetried` = `outbox.retried`, `MetricOutboxDropped` = `outbox.dropped` |

**Branch-point mapping:**
- `inbox/handler.go`: `received` at entry; `rejected` on 401/413/400; on the 202
  path, capture the currently-discarded `inbox.IngestResult` (`Ingest` returns
  `(IngestResult{Created, Duplicate bool}, error)` — handler.go:125 today drops it
  as `_`) and increment `ingest.duplicate` when `result.Duplicate`, else
  `ingest.accepted`. Both still return the identical uniform 202 — no behaviour
  change, no oracle. `bounce.go`: `received`/`rejected`/`accepted` analogously.
- `notify/sender_subscriber.go`: `suppressed` on the terminal ErrSuppressed path;
  `failed` on transient dispatch error; `sent` on success.
- `events/outbox.go`: `drained` per successfully-handled event; `retried` on
  handler error (will reschedule); `dropped` on the poison/max-attempts path.

### (B) Logging consistency

- Add the two missing lifecycle `Info` events:
  - ingest success → `inbox: message ingested` with `ticket_id`, `message_id`, `created`.
  - outbound success → `notify: reply sent` with `message_id`.
- Ensure existing log calls in the three paths carry the consistent domain keys
  above. Never log request/message bodies; never `err.Error()` to clients (already
  the case — server-side logs keep the wrapped error).
- The outbox worker already logs `started` / `outbox drain` error / `dropping
  poison event`; keep as-is (just confirm topic + id keys present).

### (C) Redaction — `internal/platform/observability/redact.go` (new) + `NewLogger` edit

```go
func redactSensitive(groups []string, a slog.Attr) slog.Attr // ReplaceAttr hook
func isSensitiveKey(key string) bool                          // lowercased substring match
```

- `NewLogger` sets `&slog.HandlerOptions{Level: ..., ReplaceAttr: redactSensitive}`.
- `isSensitiveKey` lowercases the key and matches a curated **substring denylist**:
  `secret`, `password`, `passwd`, `token`, `private_key` (catches
  `dkim_private_key_ref`), `authorization`, `api_key`, `apikey`, `hmac`,
  `credential`, `session_id`.
- Deliberately **excludes** bare `key` and bare `code` — those over-match benign
  attrs (`blob_key`, `idempotency_key`, `status_code`, `error_code`). A match
  replaces the value with `slog.StringValue("[REDACTED]")`; non-matches pass
  through unchanged. Built-in attrs (`time`/`level`/`msg`) are unaffected.

### Wiring — `cmd/manyforge/main.go`

- Construct one `metrics := observability.NewMetrics()`.
- Set it on the inbox webhook handler, bounce handler, `SendSubscriber`, and
  outbox `Worker` (exported nil-safe fields). `NewLogger` already carries the
  redaction hook, so no extra logging wiring is needed.

## Error handling

- Metrics: best-effort and nil-safe; incrementing never returns an error and
  never blocks a request, send, or drain.
- Redaction: fail-safe — unrecognized attrs pass through; only denylist matches
  are scrubbed. No panic path (pure string inspection).

## Test plan

All fast (no Docker) except the two wiring assertions.

1. **`internal/platform/observability/redact_test.go`**
   - `TestRedactScrubsSensitiveAttrs`: build a logger over a `bytes.Buffer` with
     the production options; log with `secret`, `dkim_private_key_ref`, `token`
     plus safe keys (`business_id`, `blob_key`, `dkim_domain`). Assert the output
     contains `[REDACTED]` for each sensitive attr, the raw secret strings are
     **absent**, and the safe values survive verbatim. **Bite:** force
     `isSensitiveKey`→`false`; secrets appear in output → RED.
   - `TestIsSensitiveKey`: table-driven (positive + the deliberately-excluded
     `blob_key`/`status_code` negatives).
2. **`internal/platform/observability/metrics_test.go`**
   - `TestMetricsCounters`: `Inc`/`Add` then `Get` returns the value; nil receiver
     is a no-op; `NewMetrics()` twice does not panic and shares state.
3. **Wiring**
   - Extend one inbox integration test to assert `ingest.received` and
     `ingest.accepted` advance on a successful ingest (and `rejected` on a 401).
   - Extend one outbox integration test to assert `outbox.drained` advances.
   - A source-pin (`internal/security_regression` or alongside) asserting
     `handler.go`, `sender_subscriber.go`, `outbox.go` each reference the metric
     constants — so a future edit that drops instrumentation fails CI.

## Files

**New:** `internal/platform/observability/metrics.go`,
`internal/platform/observability/redact.go`, `…/redact_test.go`,
`…/metrics_test.go`.
**Modified:** `…/observability.go` (NewLogger ReplaceAttr), `inbox/handler.go`,
`inbox/bounce.go`, `notify/sender_subscriber.go`, `events/outbox.go`,
`cmd/manyforge/main.go`, + integration-test extensions.

## Out of scope (YAGNI)

Prometheus/OTel; HTTP request-id middleware; histograms/latency metrics; new
`/metrics` formats; per-tenant metric labels; log sampling.
