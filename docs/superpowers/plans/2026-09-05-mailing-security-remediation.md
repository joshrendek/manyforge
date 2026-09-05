# Mailing and Automation Security Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Eliminate all 23 confirmed findings in the private mailing and automation audit without retaining vulnerable compatibility paths.

**Architecture:** Add forward-only schema and function replacements, then remediate five ownership-isolated domains. Shared invariants are explicit database state: operational businesses/lists, provider feedback readiness, scoped event fingerprints, immutable activated content, and generation-fenced side effects. Existing vulnerable-state characterizations become safe regression tests.

**Tech Stack:** Go, PostgreSQL, sqlc, pgx, Chi, Angular/TypeScript, Vitest, Testcontainers, Semgrep.

**Spec:** `specs/015-mailing-security-remediation/spec.md`

## Global Constraints

- Keep the branch and every artifact local; no push, PR, publication, staging, or production request.
- Never edit migrations `0124`–`0131`; add paired forward/down migrations `0132` onward.
- Test first and record the expected RED failure before production edits.
- Preserve uniform not-found behavior and existing tenant-root/business/list containment.
- Unsubscribe remains available after business/list archive even when subscribe, confirm, enrollment, and send are blocked.
- Every provider side effect must pass current lifecycle, suppression, feedback-readiness, lease, and generation fences immediately before the call.
- Use typed service errors and generic client messages; never return or log secrets, tokens, emails in capability paths, provider bodies, or raw message content.
- Run `make generate` after SQL changes; never hand-edit generated sqlc files.
- Remove vulnerable aliases and paths rather than adding compatibility shims.

## Execution Order

1. Task 1 runs alone and establishes the generated SQL/schema contract.
2. Tasks 2, 3, 5, and 6 run in parallel from Task 1's commit with disjoint production-file ownership.
3. Task 4 starts after Task 2 so it can migrate the S2S event portion of `public.go` onto the final automation event contract without a concurrent edit.
4. Task 7 integrates all local commits and is the sole owner of shared audit-pin cleanup and final generation.

---

### Task 1: Shared Security State and SQL Contracts

**Files:**
- Create: `migrations/0132_mailing_security_state.up.sql`
- Create: `migrations/0132_mailing_security_state.down.sql`
- Modify: `db/query/mailing.sql`
- Modify: `db/query/automations.sql`
- Generated: `internal/platform/db/dbgen/*`
- Test: `internal/security_regression/mailing_security_state_integration_test.go`

**Interfaces:**
- Produces `mailing_business_operational(business_id uuid, tenant_root_id uuid) RETURNS boolean`.
- Produces `mailing_list_operational(list_id uuid, business_id uuid, tenant_root_id uuid) RETURNS boolean`.
- Adds `mailing_sending_profile.feedback_status` constrained to `pending|ready|error`, `feedback_error`, and `feedback_confirmed_at`.
- Adds immutable `automation_event.ingress_list_id`, `ingress_key_id`, and `request_fingerprint bytea`; uniqueness includes ingress scope.
- Adds `automation_version.content_snapshot jsonb` for activated send-email content.
- Adds bounded keyset queries for automation versions and changed campaign rollups.

- [ ] **Step 1: Add failing integration tests** proving archived/deleted businesses and archived lists fail the operational functions, new profiles default to `pending`, event scope/fingerprint is stored, and version queries require a bounded limit.
- [ ] **Step 2: Run the focused integration package** and record RED failures caused by missing migration objects.
- [ ] **Step 3: Implement migration 0132** with columns, constraints, indexes, helper functions, grants, fixed `search_path`, and safe backfill: existing non-relay profiles become `pending`; relay profiles become `ready` only when their existing verified-domain transport remains valid.
- [ ] **Step 4: Add keyset query contracts** using `(created_at,id)` or `(version,id)` cursors with a hard service maximum of 100 rows.
- [ ] **Step 5: Run `make generate` and the focused integration package**; record GREEN output.
- [ ] **Step 6: Commit locally** with message `fix(security): add mailing remediation state`.

### Task 2: Public Consent and Resource Lifecycle

**Files:**
- Create: `migrations/0133_mailing_consent_lifecycle.up.sql`
- Create: `migrations/0133_mailing_consent_lifecycle.down.sql`
- Modify: `internal/mailing/public.go`
- Modify: `internal/mailing/track.go`
- Modify: `internal/mailing/lists.go`
- Modify: `internal/mailing/public_integration_test.go`
- Test: `internal/mailing/public_security_integration_test.go`

**Interfaces:**
- Replaces public subscribe/confirm/unsubscribe definers while consuming Task 1 operational predicates.
- Adds a mailbox-confirmed reactivation state; anonymous single-opt-in never clears unsubscribe or suppression.
- Returns a generic retryable HTTP error only for non-nil internal mutation failures.
- Deduplicates open/click tracking by `(delivery_id,event_kind,destination_fingerprint)` and maintains monotonic first-event timestamps.

- [ ] **Step 1: Invert MF-MAIL-PUB-001, MF-MAIL-DB-001, MF-MAIL-ERRACK-001, MF-MAIL-LIFECYCLE-002, and MF-MAIL-TRACK-001 characterizations** into desired safe assertions.
- [ ] **Step 2: Run only the public/track focused tests** and record RED failures for reactivation, lifecycle, retry acknowledgement, and replay storage.
- [ ] **Step 3: Implement migration 0133** using single-statement ownership/lifecycle predicates and atomic suppression preservation; archive cancels pending confirmations and terminalizes list campaign work while leaving unsubscribe callable.
- [ ] **Step 4: Update handlers/services** so malformed/unknown/replay remain uniform no-op responses while durable failures return generic 503 with no token detail.
- [ ] **Step 5: Implement bounded tracking aggregation** without exposing whether a token or delivery exists.
- [ ] **Step 6: Run focused unit and integration tests**; record GREEN output.
- [ ] **Step 7: Commit locally** with message `fix(security): enforce mailing consent lifecycle`.

### Task 3: Provider Feedback and Webhook Integrity

**Files:**
- Create: `migrations/0134_mailing_provider_feedback.up.sql`
- Create: `migrations/0134_mailing_provider_feedback.down.sql`
- Modify: `internal/mailing/profile.go`
- Modify: `internal/mailing/profile_delivery.go`
- Modify: `internal/mailing/webhook.go`
- Modify: `internal/mailing/webhook_resend.go`
- Modify: `internal/mailing/webhook_ses.go`
- Modify: `internal/mailing/snsverify/verify.go`
- Modify: `internal/mailing/provider/cache.go`
- Modify: `internal/mailing/provider/resend.go`
- Modify: `internal/mailing/provider/ses.go`
- Modify: `internal/inbox/bounce.go`
- Test: `internal/mailing/webhook_*_test.go`
- Test: `internal/mailing/provider/cache_security_test.go`
- Test: `internal/inbox/bounce_test.go`
- Test: `internal/mailing/profile_delivery_security_integration_test.go`

**Interfaces:**
- Consumes Task 1 feedback state.
- Adds `Cache.Invalidate(profileID uuid.UUID)` and a fixed-capacity/TTL eviction policy.
- Persists one provider envelope and bounded deduplicated derived recipients.
- Records unmatched authentic events as pending; correlation atomically applies and marks them consumed later.
- SES feedback becomes `ready` only after a successful confirmed subscription tied to exact account/region/topic/configuration set.

- [ ] **Step 1: Invert MF-MAIL-WEBHOOK-001, MF-MAIL-WEBHOOK-002, MF-MAIL-WEBHOOK-003, MF-MAIL-FEEDBACK-001, provider-cache MF-MAIL-DELIVERY-001, the profile-test branch of MF-MAIL-DELIVERY-002, and provider branches of MF-MAIL-ERRACK-001** into safe tests.
- [ ] **Step 2: Run focused provider/webhook tests** and record RED failures for cardinality, pre-auth fetch, early correlation, missing readiness, cache retention, and 2xx-on-error.
- [ ] **Step 3: Enforce provider setup**: Resend requires a valid webhook secret; SES requires region/account-bound topic and configuration set; relevant edits reset feedback to `pending`.
- [ ] **Step 4: Replace SNS confirmation handling** with durable pending/ready/error transitions and generic retryable failure when the guarded confirmation call fails.
- [ ] **Step 5: Implement bounded envelope normalization and pending correlation** with body/cardinality caps before per-recipient database work.
- [ ] **Step 6: Add bounded cache eviction, explicit invalidation, and profile-test suppression** on profile update/delete and test send; cached clients cannot outlive deleted credential ownership indefinitely.
- [ ] **Step 7: Run focused unit/integration tests**; record GREEN output.
- [ ] **Step 8: Commit locally** with message `fix(security): require durable provider feedback`.

### Task 4: Automation Authorization, Identity, and Fencing

**Files:**
- Create: `migrations/0135_automation_security.up.sql`
- Create: `migrations/0135_automation_security.down.sql`
- Modify: `internal/automations/handler.go`
- Modify: `internal/mailing/public.go` only for the S2S event ingress section, after Task 2 is committed
- Modify: `internal/automations/service.go`
- Modify: `internal/automations/events.go`
- Modify: `internal/automations/enrollments.go`
- Modify: `internal/automations/engine.go`
- Modify: `internal/automations/stepper.go`
- Modify: `internal/automations/ports.go`
- Modify: `internal/automations/types.go`
- Modify: `internal/automations/*_test.go`
- Test: `internal/automations/security_integration_test.go`

**Interfaces:**
- Consumes Task 1 event scope/fingerprint, content snapshot, and bounded version query.
- Service methods for manual enrollment and send-producing event injection require an explicit send-authorized principal context rather than relying only on route grouping.
- Side-effect ports receive enrollment generation and return a typed lost-fence result before mail/tag mutation.
- S2S event input requires `idempotency_key`; collisions compare canonical SHA-256 fingerprints in constant time.

- [ ] **Step 1: Invert AUTOMATION-EVENT-SCOPE-001, AUTOMATION-S2S-REPLAY-003, AUTOMATION-EVENT-TIME-004, AUTOMATION-FENCE-002, AUTOMATION-SEND-AUTHZ-005, MF-AUTO-001, and MF-AUTO-002** into safe assertions.
- [ ] **Step 2: Add a custom-role integration test** with `mailing.read|write` but no `mailing.send`; record RED when enrollment/event injection reaches queued delivery.
- [ ] **Step 3: Add deterministic concurrency tests** for graph replacement during activation and pause/archive/exit between claim and side effect; record RED.
- [ ] **Step 4: Implement migration 0135** replacing event ingest, claim, step-record, and enqueue/tag ports with operational scope and generation predicates.
- [ ] **Step 5: Freeze send-email content at activation** in the exact version validated under row lock or compare-and-swap; live mutable template rows are never resolved for active versions.
- [ ] **Step 6: Require send permission at handler and service boundaries** and keep foreign/unknown identifiers uniform not-found.
- [ ] **Step 7: Enforce event identity/time rules**: required key, scoped fingerprint equality, bounded future skew, and upper-bound condition evaluation.
- [ ] **Step 8: Paginate version history** with a maximum of 100 and retain versions referenced by enrollments.
- [ ] **Step 9: Run focused automation unit/integration tests**; record GREEN output.
- [ ] **Step 10: Commit locally** with message `fix(security): fence automation execution`.

### Task 5: Delivery Suppression and Resource Bounds

**Files:**
- Create: `migrations/0136_mailing_delivery_bounds.up.sql`
- Create: `migrations/0136_mailing_delivery_bounds.down.sql`
- Modify: `internal/mailing/campaigns.go`
- Modify: `internal/mailing/sendworker.go`
- Modify: `internal/platform/notify/notify.go`
- Modify: `internal/platform/notify/smtp.go`
- Modify: `internal/mailing/provider/relay.go`
- Modify: `internal/platform/config/config.go`
- Modify: `cmd/manyforge/main.go`
- Test: `internal/mailing/campaigns_integration_test.go`
- Test: `internal/mailing/sendworker_security_test.go`
- Test: `internal/platform/notify/*_test.go`

**Interfaces:**
- Consumes Task 1 operational/feedback state and Task 3 cache invalidation.
- One tick has explicit global and per-campaign fan-out budgets and never loops one campaign to completion.
- Rollup consumes a changed-delivery cursor/watermark and updates only affected campaigns.
- Production transport absence fails startup; development LogSender returns a non-accepted result and logs metadata only.

- [ ] **Step 1: Invert the content-cache branch of MF-MAIL-DELIVERY-001, campaign-test branch of MF-MAIL-DELIVERY-002, MF-MAIL-DELIVERY-003, MF-MAIL-DELIVERY-004, and MF-MAIL-ROLLUP-001** into safe tests.
- [ ] **Step 2: Run focused delivery tests** and record RED for suppressed test recipients, fallback acceptance/log content, unbounded tick work, stale cache entries, and historical rollup rewrites.
- [ ] **Step 3: Centralize suppression decisions** for campaign test send and regular worker send; explicit override requires `mailing.send`, a reason, and an audit row. Consume Task 3's profile-test suppression helper without editing `profile_delivery.go`.
- [ ] **Step 4: Replace unbounded compiled-template map** with a fixed byte/entry bound plus TTL and invalidation keyed by immutable content version.
- [ ] **Step 5: Implement bounded fan-out and incremental rollup** in migration 0136; include fairness across campaigns and a durable watermark or changed-campaign queue.
- [ ] **Step 6: Make transport startup fail closed outside development** and make development logging metadata-only with no provider-accepted result.
- [ ] **Step 7: Add final operational/list/feedback/generation renewal** immediately before provider send.
- [ ] **Step 8: Run focused unit/integration tests**; record GREEN output.
- [ ] **Step 9: Commit locally** with message `fix(security): bound mailing delivery work`.

### Task 6: Capability-Safe HTTP and Frontend Routes

**Files:**
- Modify: `internal/platform/httpx/middleware.go`
- Modify: `internal/platform/httpx/errors.go`
- Modify: `internal/platform/httpx/*_test.go`
- Modify: `web/src/app/core/mailing.service.ts`
- Modify: `web/src/app/core/automations.service.ts`
- Modify: `web/src/app/core/mailing.service.spec.ts`
- Modify: `web/src/app/core/automations.service.spec.ts`
- Modify only if route validation is shared: `web/src/app/core/route-id.ts`

**Interfaces:**
- Produces `httpx.SafePath(*http.Request) string`, preferring Chi route patterns and redacting capability/email segments for unmatched/recovery paths.
- Produces one frontend `routeSegmentUUID(value: string): string` helper that validates canonical UUID text and returns `encodeURIComponent(value)`.

- [ ] **Step 1: Invert MF-MAIL-LOG-001 and MF-WEB-013-001** into tests that assert sentinels never enter logs or normalized alternate API targets.
- [ ] **Step 2: Add table tests** for confirm/unsubscribe/open/click/S2S paths and slash, backslash, dot, query, fragment, encoded-separator, uppercase, and malformed UUID route inputs; record RED.
- [ ] **Step 3: Implement centralized safe path logging** and use it in request, panic recovery, and shared HTTP error logging.
- [ ] **Step 4: Implement canonical UUID segment validation/encoding** and migrate every mailing/automation service method in one clean cutover.
- [ ] **Step 5: Run focused Go and Angular tests**; record GREEN output.
- [ ] **Step 6: Commit locally** with message `fix(security): protect mailing capability paths`.

### Task 7: Integrate, Regenerate, and Retire Audit Pins

**Files:**
- Modify: `internal/security_regression/mailing_automation_audit_pins_test.go`
- Modify: every audit characterization file named in `SCAN.md`
- Modify: `SCAN.md`
- Modify: `specs/015-mailing-security-remediation/spec.md` status only after verification
- Modify: generated sqlc files from `make generate`

**Interfaces:**
- Consumes all prior task commits.
- Produces one coherent migration chain, generated query API, green safe regressions, and updated private remediation ledger.

- [ ] **Step 1: Cherry-pick domain commits in task order** and resolve generated-code conflicts by discarding generated hunks, then run `make generate` once.
- [ ] **Step 2: Replace source-level vulnerable-state pins** with observable safe regressions or delete them when a stronger behavioral test covers the same ID.
- [ ] **Step 3: Run each finding-specific command from `SCAN.md`** and require GREEN safe assertions.
- [ ] **Step 4: Run `make test`, `make sec-test`, `make int-test`, and `make lint`**; fix every failure before continuing.
- [ ] **Step 5: Run `cd web && npm test -- --watch=false`** and require all files/tests green.
- [ ] **Step 6: Run the approved Semgrep OSS rulesets** and triage every new alert.
- [ ] **Step 7: Dispatch a whole-branch security/code review** covering cross-task migration order, final provider fences, tenant/list predicates, error shapes, and test fidelity; address every load-bearing finding.
- [ ] **Step 8: Update `SCAN.md`** with fixed status and exact verification output; mark Spec 015 implemented locally.
- [ ] **Step 9: Commit locally** with message `fix(security): complete mailing remediation` and do not push.
