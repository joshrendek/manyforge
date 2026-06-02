# T072 Observability + Credential Redaction — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add expvar metrics + consistent structured logging for the inbound-ingest, outbound-send, and outbox-drain pipelines, and a slog-level redaction guard so credential-bearing values can never reach log output.

**Architecture:** Two new files in `internal/platform/observability` — a nil-safe `*Metrics` wrapping one published `expvar.Map`, and a `redactSensitive` `ReplaceAttr` hook wired into `NewLogger`. The three pipelines receive the `*Metrics` as an exported, nil-safe field (no constructor-signature changes) and increment counters at their existing branch points.

**Tech Stack:** Go, stdlib `expvar`, stdlib `log/slog`. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-06-02-t072-observability-redaction-design.md`

---

## File Structure

- `internal/platform/observability/metrics.go` (new) — `Metrics` type, counter-key constants.
- `internal/platform/observability/metrics_test.go` (new) — counter unit tests.
- `internal/platform/observability/redact.go` (new) — `redactSensitive` + `isSensitiveKey`.
- `internal/platform/observability/redact_test.go` (new) — redaction unit tests.
- `internal/platform/observability/observability.go` (modify) — `NewLogger` gets the `ReplaceAttr` hook.
- `internal/inbox/handler.go` (modify) — `Metrics` field + ingest counters + capture `IngestResult`.
- `internal/inbox/bounce.go` (modify) — `Metrics` field + counters.
- `internal/inbox/handler_test.go` (modify) — ingest-counter assertions (no Docker).
- `internal/platform/notify/sender_subscriber.go` (modify) — `Metrics` field + send counters + sent log.
- `internal/platform/notify/send_integration_test.go` (modify) — `outbound.sent` assertion.
- `internal/platform/events/outbox.go` (modify) — `Metrics` field + drain counters.
- `internal/platform/events/outbox_integration_test.go` (modify) — `outbox.drained` assertion.
- `cmd/manyforge/main.go` (modify) — construct + wire `*Metrics`.
- `cmd/manyforge/observability_wiring_test.go` (new, `contract` tag) — source-pin that all three pipelines reference the metric constants.

---

### Task 1: Metrics type + counter constants

**Files:**
- Create: `internal/platform/observability/metrics.go`
- Test: `internal/platform/observability/metrics_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/platform/observability/metrics_test.go`:

```go
package observability

import "testing"

func TestMetricsCounters(t *testing.T) {
	m := NewMetrics()

	m.Inc(MetricIngestReceived)
	m.Inc(MetricIngestReceived)
	m.Add(MetricOutboxDrained, 5)

	if got := m.Get(MetricIngestReceived); got != 2 {
		t.Errorf("%s = %d, want 2", MetricIngestReceived, got)
	}
	if got := m.Get(MetricOutboxDrained); got != 5 {
		t.Errorf("%s = %d, want 5", MetricOutboxDrained, got)
	}
	if got := m.Get("never.touched"); got != 0 {
		t.Errorf("unset counter = %d, want 0", got)
	}
}

func TestMetricsNilSafe(t *testing.T) {
	var m *Metrics // nil
	m.Inc(MetricOutboundSent)        // must not panic
	m.Add(MetricOutboundSent, 3)     // must not panic
	if got := m.Get(MetricOutboundSent); got != 0 {
		t.Errorf("nil Get = %d, want 0", got)
	}
}

func TestNewMetricsTwiceShares(t *testing.T) {
	a := NewMetrics()
	b := NewMetrics() // must not panic (expvar.NewMap would); shares the map
	a.Add("shared.key", 7)
	if got := b.Get("shared.key"); got != 7 {
		t.Errorf("second handle sees %d, want 7 (shared map)", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run '^TestMetrics|^TestNewMetrics' ./internal/platform/observability/`
Expected: FAIL — `undefined: NewMetrics` (and the constants).

- [ ] **Step 3: Write minimal implementation**

Create `internal/platform/observability/metrics.go`:

```go
package observability

import "expvar"

// Counter keys for the spec-002 pipelines, published under the "support" expvar
// map (so /metrics shows {"support": {...}}). Producers and tests share these.
const (
	MetricIngestReceived  = "ingest.received"
	MetricIngestAccepted  = "ingest.accepted"
	MetricIngestRejected  = "ingest.rejected"
	MetricIngestDuplicate = "ingest.duplicate"

	MetricOutboundSent       = "outbound.sent"
	MetricOutboundFailed     = "outbound.failed"
	MetricOutboundSuppressed = "outbound.suppressed"

	MetricOutboxDrained = "outbox.drained"
	MetricOutboxRetried = "outbox.retried"
	MetricOutboxDropped = "outbox.dropped"
)

// Metrics is a thin, nil-safe wrapper over a published expvar.Map. A nil *Metrics
// makes every method a no-op, so a pipeline with no metrics wired behaves exactly
// as before. expvar serves the underlying map at /metrics with zero new deps.
type Metrics struct{ m *expvar.Map }

const metricsMapName = "support"

// NewMetrics returns a handle to the published "support" map, creating it on first
// call and reusing it thereafter (so repeated calls — e.g. in tests — never trip
// expvar.NewMap's duplicate-registration panic).
func NewMetrics() *Metrics {
	if v := expvar.Get(metricsMapName); v != nil {
		if mp, ok := v.(*expvar.Map); ok {
			return &Metrics{m: mp}
		}
	}
	return &Metrics{m: expvar.NewMap(metricsMapName)}
}

// Inc adds 1 to the named counter. No-op on a nil receiver.
func (m *Metrics) Inc(key string) { m.Add(key, 1) }

// Add adds n to the named counter. No-op on a nil receiver.
func (m *Metrics) Add(key string, n int64) {
	if m == nil || m.m == nil {
		return
	}
	m.m.Add(key, n)
}

// Get reads the named counter (0 if unset). For tests/inspection.
func (m *Metrics) Get(key string) int64 {
	if m == nil || m.m == nil {
		return 0
	}
	if v, ok := m.m.Get(key).(*expvar.Int); ok && v != nil {
		return v.Value()
	}
	return 0
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run '^TestMetrics|^TestNewMetrics' ./internal/platform/observability/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/observability/metrics.go internal/platform/observability/metrics_test.go
git commit -m "feat(002): T072 — expvar Metrics type + pipeline counter constants"
```

---

### Task 2: Credential-redaction hook in NewLogger

**Files:**
- Create: `internal/platform/observability/redact.go`
- Modify: `internal/platform/observability/observability.go:25`
- Test: `internal/platform/observability/redact_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/platform/observability/redact_test.go`:

```go
package observability

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestIsSensitiveKey(t *testing.T) {
	sensitive := []string{
		"secret", "InboundWebhookSecret", "password", "passwd",
		"token", "access_token", "refresh_token", "dkim_private_key_ref",
		"private_key", "authorization", "api_key", "apiKey", "hmac",
		"credential", "session_id",
	}
	for _, k := range sensitive {
		if !isSensitiveKey(k) {
			t.Errorf("isSensitiveKey(%q) = false, want true", k)
		}
	}
	safe := []string{"business_id", "blob_key", "dkim_domain", "dkim_selector",
		"status_code", "error_code", "message_id", "topic", "provider"}
	for _, k := range safe {
		if isSensitiveKey(k) {
			t.Errorf("isSensitiveKey(%q) = true, want false (over-redaction)", k)
		}
	}
}

func TestRedactScrubsSensitiveAttrs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf,
		&slog.HandlerOptions{ReplaceAttr: redactSensitive}))

	logger.Info("send attempt",
		"webhook_secret", "s3cr3t-value",
		"dkim_private_key_ref", "vault://abc123",
		"token", "tok_live_xyz",
		"business_id", "biz-42",
		"blob_key", "tenant/obj-1",
	)
	out := buf.String()

	// Secrets must be gone, replaced by the marker.
	for _, leak := range []string{"s3cr3t-value", "vault://abc123", "tok_live_xyz"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaked secret %q:\n%s", leak, out)
		}
	}
	if n := strings.Count(out, "[REDACTED]"); n != 3 {
		t.Errorf("want 3 [REDACTED] markers, got %d:\n%s", n, out)
	}
	// Safe values must survive.
	for _, keep := range []string{"biz-42", "tenant/obj-1"} {
		if !strings.Contains(out, keep) {
			t.Errorf("log dropped safe value %q:\n%s", keep, out)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run '^TestIsSensitiveKey$|^TestRedactScrubsSensitiveAttrs$' ./internal/platform/observability/`
Expected: FAIL — `undefined: isSensitiveKey` / `undefined: redactSensitive`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/platform/observability/redact.go`:

```go
package observability

import (
	"log/slog"
	"strings"
)

// redactedMarker replaces the value of any credential-bearing attribute.
const redactedMarker = "[REDACTED]"

// sensitiveSubstrings is the credential denylist matched (case-insensitively) as a
// substring of an attribute KEY. Deliberately curated to avoid over-matching benign
// keys: bare "key" and "code" are EXCLUDED (they would scrub blob_key / status_code).
// "private_key" catches dkim_private_key_ref. Extend this list, never loosen it.
var sensitiveSubstrings = []string{
	"secret", "password", "passwd", "token", "private_key",
	"authorization", "api_key", "apikey", "hmac", "credential", "session_id",
}

// isSensitiveKey reports whether an attribute key names a credential-bearing value.
func isSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	for _, s := range sensitiveSubstrings {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// redactSensitive is a slog ReplaceAttr hook: it replaces the value of any
// sensitive-keyed attribute with the redaction marker. slog invokes ReplaceAttr for
// every non-group attribute, including those nested inside groups, so this is a
// structural guard — a future careless log call cannot leak a secret by key. The
// built-in time/level/msg attrs (their keys are not in the denylist) pass through.
func redactSensitive(_ []string, a slog.Attr) slog.Attr {
	if isSensitiveKey(a.Key) {
		a.Value = slog.StringValue(redactedMarker)
	}
	return a
}
```

- [ ] **Step 4: Wire the hook into NewLogger**

In `internal/platform/observability/observability.go`, replace line 25:

```go
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
```

with:

```go
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:       lvl,
		ReplaceAttr: redactSensitive,
	}))
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/platform/observability/`
Expected: PASS (all tests in the package).

- [ ] **Step 6: Bite-prove the redaction**

Temporarily edit `redact.go` `isSensitiveKey` to `return false` (first line of the body). Run:
`go test -run '^TestRedactScrubsSensitiveAttrs$' ./internal/platform/observability/`
Expected: FAIL — output now contains `s3cr3t-value`/`vault://abc123`/`tok_live_xyz`. Revert the edit; re-run; PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/platform/observability/redact.go internal/platform/observability/redact_test.go internal/platform/observability/observability.go
git commit -m "feat(002): T072 — slog credential-redaction hook (denylist ReplaceAttr)"
```

---

### Task 3: Instrument the ingest pipeline (inbox webhook + bounce)

**Files:**
- Modify: `internal/inbox/handler.go` (struct + `ingest`)
- Modify: `internal/inbox/bounce.go` (struct + `ingest`)
- Test: `internal/inbox/handler_test.go` (new test)

- [ ] **Step 1: Write the failing test**

Append to `internal/inbox/handler_test.go`:

```go
func TestWebhookIncrementsMetrics(t *testing.T) {
	cfg := Config{SystemInboundDomain: testSystemDomain}
	m := observability.NewMetrics()
	base := m.Get(observability.MetricIngestReceived)
	baseAcc := m.Get(observability.MetricIngestAccepted)
	baseDup := m.Get(observability.MetricIngestDuplicate)
	baseRej := m.Get(observability.MetricIngestRejected)

	h := NewWebhookHandler(
		&fakeIngester{result: IngestResult{TicketID: uuid.New(), MessageID: uuid.New(), Created: true}},
		testWebhookSecret, 1<<20, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.Metrics = m
	r := chi.NewRouter()
	h.PublicRoutes(r)

	// Accepted (signed, routed) → 202.
	rec := postSigned(t, r, validInboundBody(), testWebhookSecret)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	// Rejected (missing signature) → 401.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/inbound/email/postmark", strings.NewReader(validInboundBody()))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	if got := m.Get(observability.MetricIngestReceived) - base; got != 2 {
		t.Errorf("received delta = %d, want 2", got)
	}
	if got := m.Get(observability.MetricIngestAccepted) - baseAcc; got != 1 {
		t.Errorf("accepted delta = %d, want 1", got)
	}
	if got := m.Get(observability.MetricIngestRejected) - baseRej; got != 1 {
		t.Errorf("rejected delta = %d, want 1", got)
	}
	if got := m.Get(observability.MetricIngestDuplicate) - baseDup; got != 0 {
		t.Errorf("duplicate delta = %d, want 0", got)
	}
}
```

> NOTE: reuse the existing test helpers in `handler_test.go` for signing/body. If `postSigned`/`validInboundBody` are named differently there, adapt the calls to the existing helpers (read the file's top first). Add imports `net/http/httptest`, `strings`, and `github.com/manyforge/manyforge/internal/platform/observability` if not already present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run '^TestWebhookIncrementsMetrics$' ./internal/inbox/`
Expected: FAIL — `h.Metrics undefined` (field doesn't exist yet).

- [ ] **Step 3: Add the Metrics field to Handler**

In `internal/inbox/handler.go`, add the import `"github.com/manyforge/manyforge/internal/platform/observability"` and add a field to the `Handler` struct (after `recipientLimiter`):

```go
	// Metrics counts ingest outcomes (received/accepted/rejected/duplicate). Set by
	// main after construction; nil ⇒ no-op (existing callers/tests unaffected).
	Metrics *observability.Metrics
```

- [ ] **Step 4: Increment counters in `ingest`**

In `internal/inbox/handler.go`, instrument `ingest`. Add at the very top of the method body (before step 1):

```go
	h.Metrics.Inc(observability.MetricIngestReceived)
```

Before EACH rejection return, add `h.Metrics.Inc(observability.MetricIngestRejected)`:
- the 413 branch (before `httpx.WriteJSON(... StatusRequestEntityTooLarge ...)`),
- the 400 invalid-request-body branch,
- the 401 unauthorized branch,
- the 400 invalid-inbound-payload branch,
- the 429 rate-limit branch,
- the 500 internal-error branch (the non-`IsNoRoute` ingest error).

Replace the step-5 ingest block:

```go
	// 5. Ingest. Map routed / duplicate / unknown-recipient ALL to an identical 202.
	if _, err := h.ingester.Ingest(r.Context(), msg); err != nil {
		if IsNoRoute(err) {
			h.writeAccepted(w)
			return
		}
		h.logger.ErrorContext(r.Context(), "inbox: webhook ingest failed",
			"err", err, "provider", chiProvider(r))
		httpx.WriteJSON(w, http.StatusInternalServerError, httpx.ErrorBody{Code: "INTERNAL", Message: "internal error"})
		return
	}

	// Routed or duplicate: the same uniform 202.
	h.writeAccepted(w)
```

with:

```go
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
```

> NOTE: `IngestResult` (verified at `internal/inbox/service.go:38`) is `{TicketID, MessageID uuid.UUID; Created, Duplicate, Suppressed bool}` — it does NOT carry a business id (routing happens inside Ingest), so the handler-layer correlation keys are `ticket_id`+`message_id` (both DB row ids, never the raw body). A `Suppressed` ingest (bounded mail-loop auto-reply) still returns 202 and is counted as accepted — no separate counter.

- [ ] **Step 5: Add the Metrics field + counters to BounceHandler**

In `internal/inbox/bounce.go`, add the `observability` import and a `Metrics *observability.Metrics` field to `BounceHandler`. In `ingest`: add `h.Metrics.Inc(observability.MetricIngestReceived)` at the top; `h.Metrics.Inc(observability.MetricIngestRejected)` before the 401 return; and `h.Metrics.Inc(observability.MetricIngestAccepted)` immediately before the FINAL `h.writeAccepted(w)` (line ~141, the authenticated-completion path).

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test -run '^TestWebhook' ./internal/inbox/ && go build ./...`
Expected: PASS; build clean.

- [ ] **Step 7: Commit**

```bash
git add internal/inbox/handler.go internal/inbox/bounce.go internal/inbox/handler_test.go
git commit -m "feat(002): T072 — instrument inbound ingest with metrics + ingested log"
```

---

### Task 4: Instrument the outbound-send pipeline

**Files:**
- Modify: `internal/platform/notify/sender_subscriber.go` (struct + `Handle`)
- Test: `internal/platform/notify/send_integration_test.go`

- [ ] **Step 1: Write the failing test**

Read `internal/platform/notify/send_integration_test.go` to find the existing happy-path send test and its tenant/seed helpers + how it builds `SendSubscriber` and invokes `Handle`. Append a test that mirrors the happy-path setup and asserts the counter:

```go
func TestSendIncrementsSentMetric(t *testing.T) {
	// ... reuse the existing happy-path setup: start DB, seed a tenant with a
	// pending outbound ticket.replied event, build the capturing sender ...
	m := observability.NewMetrics()
	before := m.Get(observability.MetricOutboundSent)

	sub := SendSubscriber{Sender: cap, Logger: testLogger(t), Metrics: m}
	// invoke Handle inside a tx exactly as the existing send test does:
	if err := runHandleInTx(ctx, tdb, sub, evt); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got := m.Get(observability.MetricOutboundSent) - before; got != 1 {
		t.Errorf("outbound.sent delta = %d, want 1", got)
	}
}
```

> NOTE: the exact helper names (`cap`, `runHandleInTx`, `evt`, `testLogger`) come from the existing test file — adapt to whatever it already uses to drive a successful send. Add the `observability` import. This is an `//go:build integration` file.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags integration -run '^TestSendIncrementsSentMetric$' -p 1 ./internal/platform/notify/`
Expected: FAIL — `unknown field Metrics in struct literal`.

- [ ] **Step 3: Add the Metrics field**

In `internal/platform/notify/sender_subscriber.go`, add import `"github.com/manyforge/manyforge/internal/platform/observability"` and a field to `SendSubscriber` (after `Sealer`):

```go
	// Metrics counts outbound outcomes (sent/failed/suppressed). nil ⇒ no-op.
	Metrics *observability.Metrics
```

- [ ] **Step 4: Increment counters in `Handle`**

In `Handle`, instrument the post-`Send` branches (currently around lines 144–158):

```go
	if serr := s.Sender.Send(ctx, mail); serr != nil {
		if errors.Is(serr, ErrSuppressed) {
			s.Metrics.Inc(observability.MetricOutboundSuppressed)
			if merr := s.mark(ctx, tx, p.MessageRowID, e.TenantRootID, "failed", "recipient suppressed"); merr != nil {
				return merr
			}
			return nil
		}
		s.Metrics.Inc(observability.MetricOutboundFailed)
		return fmt.Errorf("notify: send: dispatch: %w", serr)
	}

	s.Metrics.Inc(observability.MetricOutboundSent)
	s.logger().InfoContext(ctx, "notify: reply sent", "message_id", p.RFCMessageID)
	return s.mark(ctx, tx, p.MessageRowID, e.TenantRootID, "sent", "")
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test -tags integration -run '^TestSend' -p 1 ./internal/platform/notify/`
Expected: PASS (the new test + existing send tests).

- [ ] **Step 6: Commit**

```bash
git add internal/platform/notify/sender_subscriber.go internal/platform/notify/send_integration_test.go
git commit -m "feat(002): T072 — instrument outbound send with metrics + sent log"
```

---

### Task 5: Instrument the outbox-drain pipeline

**Files:**
- Modify: `internal/platform/events/outbox.go` (struct + `drainOnce`)
- Test: `internal/platform/events/outbox_integration_test.go`

- [ ] **Step 1: Write the failing test**

Read `internal/platform/events/outbox_integration_test.go` for the existing drain-success test and its helpers (how it enqueues an event, builds the `Worker`, and calls `drainOnce`/`Run`). Append:

```go
func TestOutboxIncrementsDrainedMetric(t *testing.T) {
	// ... reuse existing setup: start DB, subscribe a no-op handler, enqueue 1 event ...
	m := observability.NewMetrics()
	before := m.Get(observability.MetricOutboxDrained)

	w := &Worker{DB: database, Bus: bus, Logger: testLogger(t), Metrics: m}
	if _, err := w.drainOnce(ctx); err != nil {
		t.Fatalf("drainOnce: %v", err)
	}

	if got := m.Get(observability.MetricOutboxDrained) - before; got != 1 {
		t.Errorf("outbox.drained delta = %d, want 1", got)
	}
}
```

> NOTE: adapt helper names to the existing file. This is `//go:build integration`. `drainOnce` is unexported but the test is in `package events`, so it is callable.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags integration -run '^TestOutboxIncrementsDrainedMetric$' -p 1 ./internal/platform/events/`
Expected: FAIL — `unknown field Metrics`.

- [ ] **Step 3: Add the Metrics field**

In `internal/platform/events/outbox.go`, add import `"github.com/manyforge/manyforge/internal/platform/observability"` and a field to `Worker` (after `MaxAttempts`):

```go
	// Metrics counts drain outcomes (drained/retried/dropped). nil ⇒ no-op.
	Metrics *observability.Metrics
```

- [ ] **Step 4: Increment counters in `drainOnce`**

In the `for _, e := range batch` loop of `drainOnce`:
- after the successful `mark_outbox_processed` (the `derr == nil` branch), before `continue`: `w.Metrics.Inc(observability.MetricOutboxDrained)`.
- in the poison branch (`e.Attempts+1 >= w.MaxAttempts`), after the existing `ErrorContext` log: `w.Metrics.Inc(observability.MetricOutboxDropped)`.
- in the reschedule branch (after the `WarnContext` log): `w.Metrics.Inc(observability.MetricOutboxRetried)`.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test -tags integration -run '^TestOutbox' -p 1 ./internal/platform/events/ && go build ./...`
Expected: PASS; build clean.

- [ ] **Step 6: Commit**

```bash
git add internal/platform/events/outbox.go internal/platform/events/outbox_integration_test.go
git commit -m "feat(002): T072 — instrument outbox drain with metrics"
```

---

### Task 6: Wire metrics in main + source-pin + final gate

**Files:**
- Modify: `cmd/manyforge/main.go`
- Create: `cmd/manyforge/observability_wiring_test.go` (`contract` tag)
- Modify: `specs/002-support-desk/tasks.md` (tick T072)

- [ ] **Step 1: Write the failing source-pin test**

Create `cmd/manyforge/observability_wiring_test.go`:

```go
//go:build contract

package main

import (
	"os"
	"strings"
	"testing"
)

// T072 — pins that each pipeline is instrumented: its source references the
// observability metric constants. A future edit that strips instrumentation
// (re-blinding a pipeline) fails CI.
func TestPipelinesInstrumented(t *testing.T) {
	cases := map[string][]string{
		"../../internal/inbox/handler.go":                     {"MetricIngestReceived", "MetricIngestAccepted", "MetricIngestRejected"},
		"../../internal/platform/notify/sender_subscriber.go": {"MetricOutboundSent", "MetricOutboundFailed", "MetricOutboundSuppressed"},
		"../../internal/platform/events/outbox.go":            {"MetricOutboxDrained", "MetricOutboxRetried", "MetricOutboxDropped"},
	}
	for file, consts := range cases {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		src := string(b)
		for _, c := range consts {
			if !strings.Contains(src, c) {
				t.Errorf("%s does not reference observability.%s — pipeline not instrumented", file, c)
			}
		}
	}
}
```

> NOTE: the relative paths assume the test runs with CWD = `cmd/manyforge`. Verify with `go test` (CWD is the package dir). If a path is wrong, fix it to the actual location.

- [ ] **Step 2: Run test to verify it passes (instrumentation already added in Tasks 3–5)**

Run: `go test -tags contract -run '^TestPipelinesInstrumented$' ./cmd/manyforge/`
Expected: PASS. (Bite-check: temporarily rename one const reference in `handler.go` → RED → revert.)

- [ ] **Step 3: Wire the metrics handle in main**

In `cmd/manyforge/main.go`, after `logger := observability.NewLogger(...)` (line 47), add:

```go
	metrics := observability.NewMetrics()
```

Then attach it at each construction site:
- line 156: `outboxWorker := &events.Worker{DB: database, Bus: eventBus, Logger: logger, Metrics: metrics}`
- line 234: `sendSub := notify.SendSubscriber{Sender: sender, Logger: logger, Sealer: dkimSealer, Metrics: metrics}`
- after line 183–187 (`inboxWebhookH := inbox.NewWebhookHandler(...)`): `inboxWebhookH.Metrics = metrics`
- after line 190 (`bounceH := inbox.NewBounceHandler(...)`): `bounceH.Metrics = metrics`

- [ ] **Step 4: Build + full gate**

Run, expecting all PASS / clean:
```bash
go build ./...
make test
make contract-test
~/go/bin/golangci-lint run ./...
```
Then the touched integration packages:
```bash
go test -tags integration -p 1 ./internal/inbox/ ./internal/platform/notify/ ./internal/platform/events/
```
Expected: all PASS.

- [ ] **Step 5: Tick T072 and commit**

Edit `specs/002-support-desk/tasks.md`: change the `- [ ] T072` line to `- [X] T072`.

```bash
git add cmd/manyforge/main.go cmd/manyforge/observability_wiring_test.go specs/002-support-desk/tasks.md
git commit -m "feat(002): T072 — wire pipeline metrics in main; source-pin instrumentation; close T072"
```

- [ ] **Step 6: Update bd**

```bash
bd update manyforge-n0q.1 --notes "T072 DONE: expvar Metrics + slog redaction hook + instrumented ingest/outbound/outbox. Remaining Phase 8: T073, T074, T075."
```

---

## Self-Review notes

- **Spec coverage:** (A) metrics → Tasks 1,3,4,5,6; (B) logging/domain keys → Tasks 3 (ingested log + business_id/message_id), 4 (reply sent + message_id); (C) redaction → Task 2. Wiring → Task 6. Test plan items all mapped (unit redact/metrics, ingest handler test, outbound+outbox integration assertions, source-pin).
- **Type consistency:** counter constants defined in Task 1 are referenced verbatim in Tasks 3–6; `*observability.Metrics` field name `Metrics` used consistently; nil-safe methods mean unset fields are safe.
- **Assumptions flagged inline:** `IngestResult.BusinessID` existence (Task 3), and the existing-helper names in the integration tests (Tasks 4,5) — the worker must read the real files and adapt, never invent symbols.
