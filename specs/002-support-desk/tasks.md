---
description: "Task list for Native Support Desk"
---

# Tasks: Native Support Desk

**Input**: Design documents from `specs/002-support-desk/`
**Prerequisites**: plan.md, spec.md, plan-inputs.md, research.md, data-model.md, contracts/openapi.yaml, quickstart.md

**Tests**: MANDATORY per Constitution Principle III (Test-First, NON-NEGOTIABLE) and the project's
global engineering rules. Every user story's test tasks are written FIRST and MUST FAIL before its
implementation tasks (Red→Green→Refactor). Security-critical invariants are pinned in
`internal/security_regression/` (source-level pins survive refactors). UI-bearing work adds a
real-browser Playwright spec under `web/e2e/`.

**Organization**: by user story (US1–US5) for independent implementation/testing. This slice builds the
first product vertical on the spec-001 foundation and introduces the thin first cut of three shared
platform layers (SL-C events/outbox, SL-D notify, SL-E blob), so Phase 2 (Foundational) is sizeable.

## Format: `[ID] [P?] [Story] Description`
- **[P]**: parallelizable (different files, no incomplete dependency)
- **[Story]**: US1–US5 (user-story phases only; Setup/Foundational/Polish carry no story label)
- Paths follow the modular-monolith layout in plan.md (§Project Structure)

## Build environment (from plan.md / quickstart.md)
- Go 1.25 (module is `go 1.25.5`); Angular 21 + TS 5.9 for `web/`
- New deps: `emersion/go-smtp`, `jhillyerd/enmime`, `emersion/go-msgauth`, `gocloud.dev/blob`
- Reuses spec-001 platform: `internal/platform/{config,db,errs,auth,audit,mailer,ratelimit,netsafe,httpx,observability}`
- Merge gate: `make test` + `make int-test` (⊇ `sec-test`) + `make contract-test` + `make lint` + Playwright e2e

---

## Phase 1: Setup (Shared Infrastructure)

- [X] T001 Add Go dependencies (`emersion/go-smtp`, `jhillyerd/enmime/v2`, `emersion/go-msgauth`, `gocloud.dev/blob` + `fileblob` + `s3blob`). Fetched + cached; `go mod tidy` prunes unused requires, so each is re-pinned (offline, from cache) at its first import in Phase 2/US1 — keeps a CI tidy-check clean
- [X] T002 [P] Add support-desk config to `internal/platform/config/config.go` (`SMTPAddr`, `InboundWebhookSecret`, `BlobURL`, `InboundSystemDomain`, `DKIMKeyPath`, `InboundMaxBytes`, `AttachmentMaxBytes`, ingest/outbound rate knobs) + `.env.example` block per quickstart.md
- [X] T003 [P] Added `make contract-test` (`go test -tags contract ./...`) to `Makefile` + a CI step in `.github/workflows/ci.yml`
- [X] T004 [P] Created sqlc query placeholders `db/query/{inbox,ticketing,notify}.sql` (auto-globbed by `sqlc.yaml`'s `queries: db/query`; filled in T011)
- [X] T005 [P] Scaffolded module dirs with `doc.go`: `internal/inbox/`, `internal/ticketing/`, `internal/platform/{events,notify,blob}/`
- [X] T006 [P] Added `internal/security_regression/doc_support.go` with stable finding-ID constants (`MF-002-ISOLATION`/`INGEST-SCOPE`/`THREAD-IDEMPOTENCY`/`MIME-SNIFF`/`WEBHOOK-SIG`)

---

## Phase 2: Foundational (Blocking Prerequisites)

**⚠️ CRITICAL**: No user-story work begins until this phase is complete. These tasks carry the shared
schema, RLS, permission catalog, and the three platform layers every story sits on.

### Schema & migrations (forward-only, golang-migrate — data-model.md §Migrations)
- [X] T007 Migration `migrations/0013_support_desk.up.sql` (+ `.down.sql`): six enums (`inbound_address_kind`, `email_domain_mode`, `email_domain_spf_state`, `ticket_status`, `ticket_priority`, `ticket_message_direction`); tables `email_domain`, `inbound_address`, `requester`, `ticket`, `ticket_tag`, `ticket_message`, `attachment` with all PKs, every `UNIQUE (id, tenant_root_id)` backing a child composite FK, composite FKs `(…, tenant_root_id) → parent(id, tenant_root_id)`, CHECKs (direction/author, body-present, kind/domain, size>0), and indexes incl. SC-010 list index `ticket(business_id, status, last_message_at DESC)` + thread-load `ticket_message(ticket_id, created_at)`; reuse 001's `tenant_root_id` immutability trigger on each table
- [X] T008 Migration `migrations/0014_support_rls.up.sql` (+ `.down.sql`): RLS `ENABLE` (NOT `FORCE` — owner-owned DEFINER fns must bypass RLS for principal-less ingest) + self-deriving policies on all seven tables; `resolve_inbound_address()` DEFINER routing lookup (enforces FR-013 unverified-domain no-route + no-oracle); `ingest_inbound_message()` DEFINER scoped to ONE business (greppable `ingest scope violation` re-assertion; in-function business-scoped threading; idempotent message insert; in-tx audit). Validated on a throwaway PG: idempotency, reopen, threading (header+hint), scope-violation, domain routing all pass
- [X] T009 Migration `migrations/0015_support_permissions.up.sql` (+ `.down.sql`): six `-- security: system catalog` permission rows (`tickets.read/reply/write/assign/delete`, `inbox.manage`) + preset grants per the matrix (owner/admin all six; member read/reply/write/assign; viewer read)
- [X] T010 Migration `migrations/0016_events_notify.up.sql` (+ `.down.sql`): `outbox` (drain index) + `notification` (unread feed) tenant-keyed tables, RLS by `tenant_root_id` (+ `principal_id`), and the principal-less `claim_outbox_batch`/`mark_outbox_processed`/`reschedule_outbox` DEFINER drain functions
- [X] T011 sqlc query wrappers — `db/query/notify.sql` (EnqueueOutbox + notification insert/list/unread/read). The DEFINER functions (resolve/ingest/claim/mark/reschedule) are invoked via **raw pgx** (sqlc can't resolve a function's RETURNS columns — same as the foundation's `accept_invitation`); `inbox.sql`/`ticketing.sql` table queries land with US1/US4. Schema mirror + `dbgen` models done; `make generate` green

### Platform layers (thin first cut — research R6; all [P], independent files)
- [X] T012 [P] SL-E blob: `internal/platform/blob/blob.go` — `Store` interface over `gocloud.dev/blob` (`fileblob` default, `s3blob` optional), tenant-scoped `Key()`, `Sniff()` (`http.DetectContentType` first 512B + allowlist, declared type never trusted). Unit tests: allowlist (png/text pass, exe rejected) + fileblob put/get/delete round-trip
- [X] T013 [P] SL-C events/outbox: `internal/platform/events/{bus.go,outbox.go}` — `Enqueue(tx,…)` same-tx insert (uuid v7), in-process `Bus`, at-least-once `Worker` (claim via `claim_outbox_batch`, dispatch under per-event SAVEPOINT, mark/reschedule-with-backoff/dead-letter via the DEFINER fns). **Integration-tested**: at-least-once drain + reschedule-on-failure against real RLS DB
- [X] T014 [P] SL-D notify: `internal/platform/notify/notify.go` — `InApp(tx,…)` notification writer; `Mail` (threaded headers + Reply-To token + sending identity) + `Sender` iface with dev `LogSender` honoring `mailer.SuppressionChecker` (real SMTP/DKIM in US2/US4)
- [X] T015 [P] Reply-token HMAC: `internal/ticketing/replytoken.go` — `Sign`/`Verify` `base64url(id).base64url(HMAC_SHA256(key,id))`, constant-time (`subtle.ConstantTimeCompare`); forged/tampered → no-match. Unit-tested
- [X] T016 Wired `cmd/manyforge/main.go`: event bus + outbox worker goroutine started with a shutdown-scoped context (cancelled before pool close). SMTP listener + inbox/ticketing routes attach in US1 with their adapters/handlers

**Checkpoint**: Schema, RLS, permissions, and the three platform layers exist. User stories can begin.

---

## Phase 3: User Story 1 - Receive customer email as a threaded ticket (Priority: P1) 🎯 MVP

**Goal**: Inbound email (webhook OR built-in SMTP) resolves by recipient to one business and becomes a
threaded ticket with a deduped requester; re-delivery is idempotent; unknown recipients are silently
dropped with no oracle.

**Independent Test**: Send mail to a business's auto-provisioned system address via the webhook and via
SMTP; assert one ticket + one requester (deduped by sender email). Replay the same `Message-ID`: no
duplicate. Send to an unrouted address: no data written, response identical to the routable case.

### Tests for User Story 1 (write FIRST, must FAIL) ⚠️
- [ ] T017 [P] [US1] Contract test for `POST /inbound/email/{provider}` (202 uniform for routed/unknown/duplicate; 401 bad signature; 413 over cap) wired into the OpenAPI-drift suite in `cmd/drift_test.go`
- [X] T018 [P] [US1] Integration test `internal/inbox/ingest_integration_test.go` (testcontainers): webhook ingest → ticket+requester; SMTP ingest → same ticket shape; requester dedup within tenant; auto-provisioned system address routes
- [X] T019 [P] [US1] Security pin `internal/security_regression/threading_idempotency_test.go`: replay same `message_id` → zero dup (SC-002); header threading 100% / 0% mis-thread (SC-003); forged reply token rejected (constant-time)
- [X] T020 [P] [US1] Security pin `internal/security_regression/ingestion_scope_test.go`: `ingest_inbound_message` aborts on address/business mismatch and touches only the resolved business's rows (FR-017) + source-level `strings.Contains` pin on the single-business re-verification
- [X] T021 [P] [US1] Security pin `internal/security_regression/mime_sniff_test.go`: declared `Content-Type` that lies / falls outside the allowlist is rejected before any row is written (SC-007)
- [X] T022 [P] [US1] Security pin `internal/security_regression/webhook_sig_test.go`: provider HMAC verified with `ConstantTimeCompare`; tampered body/signature rejected (incl. source-level pin on the constant-time call)

### Implementation for User Story 1
- [X] T023 [P] [US1] `internal/inbox/source.go` — `InboundSource` interface + `RawMessage`/`ParsedEmail`/`ParsedAttachment`/`AuthResults`/`AutoHeaders` types; parse via `enmime.ReadEnvelope` (degrade safely on malformed mail)
- [X] T024 [US1] `internal/inbox/resolve.go` — recipient → `(business_id, tenant_root_id, email_domain_id)` lookup against `inbound_address` (lowercase, strip plus/VERP token and hand it to threading); no-match returns "no match" only, never which (FR-003)
- [X] T025 [US1] `internal/inbox/thread.go` — threading precedence: (1) `In-Reply-To`/`References` vs `ticket_message(tenant_root_id, message_id)`; (2) HMAC reply token; (3) `[#ref]` subject match scoped to business; (4) no match → new ticket. Synthetic deterministic `message_id` for header-less mail (research R4)
- [X] T026 [US1] `internal/ticketing/requester.go` — requester upsert/dedup `ON CONFLICT (tenant_root_id, email) DO UPDATE` (bump `last_seen_at`, COALESCE display name); exposes the CRM-seam `contact_id` (nullable, no FK)
- [X] T027 [US1] `internal/inbox/service.go` — `Ingest(ctx, RawMessage)`: parse → resolve → MIME-sniff+store attachments via blob → thread → call `ingest_inbound_message` (requester upsert + ticket find/create + message insert `ON CONFLICT DO NOTHING` + attachments + audit + outbox `message.received`/`ticket.created`) in ONE tx; record `auth_results` (SPF/DKIM/DMARC, FR-019, flag not reject) and `is_auto_reply` loop-guard flag (FR-018) on the inbound message
- [X] T028 [US1] `internal/inbox/webhook.go` + `internal/inbox/handler.go` — `WebhookAdapter` + `POST /api/v1/inbound/email/{provider}`: per-provider HMAC constant-time verify, handler-level body cap (413), per-provider payload decoders → `RawMessage`; returns 202 uniformly (FR-002/FR-003/FR-005)
- [X] T029 [US1] `internal/inbox/smtp.go` — in-process `SMTPAdapter` on `emersion/go-smtp` (started by T016 when `SMTPAddr` set): `MaxMessageBytes`/`MaxRecipients` caps, opportunistic STARTTLS, RCPT-TO allowlist that returns a GENERIC `550` identical for unknown vs not-yours (no oracle), inbound-only/no-relay
- [X] T030 [US1] System-address auto-provisioning: hook business creation to insert a `system` `inbound_address` (`b-{shortid}@<InboundSystemDomain>`, per-business random localpart) so FR-001 zero-config inbound always works — `internal/inbox/provision.go` invoked from the tenancy business-create path
- [ ] T031 [US1] `internal/ticketing/service.go` (read slice) + `internal/ticketing/handler.go`: `GET /businesses/{id}/tickets` (keyset, status/priority/assignee/tag filters), `GET …/tickets/{tid}`, `GET …/tickets/{tid}/messages` (keyset), `GET …/requesters` + `…/requesters/{rid}` — all `tickets.read`-gated, dual-enforced (RLS + app predicate), cross-tenant/unknown → identical 404; issue `reply_token` at ticket creation
- [ ] T032 [US1] Per-provider/per-recipient ingest rate limit (reuse `internal/platform/ratelimit`) + webhook per-IP cap (FR-020); wire into T028/T029
- [ ] T033 [P] [US1] Frontend: `web/src/app/core/ticket.service.ts` (ticket/requester/messages API client) + `web/src/app/pages/support/` ticket-list + thread-view components; route under the dashboard; add to `web/src/app/app.routes.ts`
- [ ] T034 [US1] Playwright `web/e2e/support.spec.ts` (US1 portion): ingest a message (seeded via API/webhook) → ticket appears in the support list → open thread shows the inbound message + requester

**Checkpoint**: A business receives email (both adapters) as an idempotent, threaded, isolated ticket. MVP shippable.

---

## Phase 4: User Story 2 - Reply to a ticket and keep the conversation threaded (Priority: P1)

**Goal**: An authorized member replies; the requester gets a threaded email; the reply is recorded
outbound; the requester's response threads back. Internal notes are recorded but never delivered;
hard bounces suppress the recipient and surface on the ticket.

**Independent Test**: Reply from a ticket → outbound message recorded + email dispatched with
`In-Reply-To`/`References` + reply token. Simulate the requester replying to that `Message-ID` →
appends to the same ticket. Add a note → recorded, never mailed. Bounce a reply → recipient suppressed,
failure visible.

### Tests for User Story 2 (write FIRST, must FAIL) ⚠️
- [ ] T035 [P] [US2] Contract tests for `POST …/tickets/{tid}/reply` and `…/tickets/{tid}/note` (201 shapes, 404 no-oracle, 409 suppressed recipient) in `cmd/drift_test.go`
- [ ] T036 [P] [US2] Integration test `internal/ticketing/reply_integration_test.go`: reply → outbound message + threading headers round-trip; requester reply with `In-Reply-To` → appends to same ticket; note never enqueues outbound mail; hard bounce → `email_suppression` + ticket-visible failure

### Implementation for User Story 2
- [ ] T037 [US2] `internal/ticketing/service.go` reply path: `POST …/tickets/{tid}/reply` (`tickets.reply`) — insert outbound `ticket_message`, update `last_message_at` in-tx, enqueue threaded outbound mail + notify via outbox, audit in-tx; build outbound headers (`In-Reply-To`/`References` + `Reply-To: support+{token}@…` + `[#ref]` subject)
- [ ] T038 [US2] Internal note path: `POST …/tickets/{tid}/note` (`tickets.reply`) — insert `note`-direction message, audited; NEVER enqueues outbound mail (FR-009)
- [ ] T039 [US2] Outbound send in `internal/platform/notify` worker subscriber: dispatch the queued reply through the extended mailer (system-address from-identity for US2; custom identity added in US4), stamp `Auto-Submitted` headers (loop cooperation), record dispatch on the message
- [ ] T040 [US2] Bounce handling: process hard bounces → insert into 001's `email_suppression`, mark the outbound message failed, surface on the ticket; block sends to suppressed recipients (409)
- [ ] T041 [US2] Outbound send rate limit per business/per requester (FR-020) in the send path
- [ ] T042 [P] [US2] Frontend: reply composer + note toggle in `web/src/app/pages/support/` thread view; wire to `ticket.service.ts`
- [ ] T043 [US2] Playwright `web/e2e/support.spec.ts` (US2 portion): open a ticket → send a reply → outbound message appears in the thread; add a note → appears, distinct from a reply

**Checkpoint**: Two-way threaded conversation works end-to-end; notes stay internal; bounces are handled.

---

## Phase 5: User Story 3 - Triage a ticket (status, priority, tags, assignment) (Priority: P2)

**Goal**: Authorized members set status/priority/tags/assignee with immediate effect and in-tx audit;
ineligible assignees are refused; an inbound reply reopens a solved/closed ticket.

**Independent Test**: PATCH status/priority/tags/assignee → each persists + audits. Assign an ineligible
principal → refused (same not-found shape). Deliver an inbound reply to a `solved` ticket → reopens to
`open` and appends. A member without triage permission → refused.

### Tests for User Story 3 (write FIRST, must FAIL) ⚠️
- [ ] T044 [P] [US3] Contract test for `PATCH …/tickets/{tid}` (partial update; omitted fields preserved; `assignee_principal_id:null` unassigns; 409 ineligible/invalid transition) in `cmd/drift_test.go`
- [ ] T045 [P] [US3] Integration test `internal/ticketing/triage_integration_test.go`: each triage field persists + writes an `audit_entry`; ineligible assignee refused; lifecycle transitions per the data-model.md state table
- [ ] T046 [P] [US3] Integration test `internal/inbox/reopen_integration_test.go`: inbound reply on `pending`/`solved`/`closed` → status `open` in the SAME tx as the message insert (FR-010)

### Implementation for User Story 3
- [ ] T047 [US3] `internal/ticketing/service.go` triage path: `PATCH …/tickets/{tid}` (`tickets.write`) — partial update (pointer/COALESCE semantics, omitted fields preserved), tag full-replacement when `tags` present, lifecycle-transition validation, `last_message_at` untouched; audit old→new in-tx per changed field
- [ ] T048 [US3] Assignee eligibility (`tickets.assign`): SQL predicate verifying the principal is a member of the ticket's business or an authorized ancestor BEFORE persist (caller-supplied-UUID check); ineligible → refused with the no-oracle shape (FR-011)
- [ ] T049 [US3] Reopen-on-reply: in `internal/inbox/service.go` ingest tx, set `status='open'` when an inbound message lands on `pending`/`solved`/`closed`, audited in the same tx (FR-010)
- [ ] T050 [P] [US3] Frontend: triage controls (status/priority/tags/assignee pickers) in `web/src/app/pages/support/` thread view; `tickets.assign`-gated assignee control
- [ ] T051 [US3] Playwright `web/e2e/support.spec.ts` (US3 portion): change status + priority + assignee in the UI → persists on reload; assign + verify it shows

**Checkpoint**: Tickets are managed work — triaged, assigned, reopened on reply, fully audited.

---

## Phase 6: User Story 4 - Bring your own support address or domain (Priority: P2)

**Goal**: Configure custom receive/send identity in `forward_in` / `subdomain_mx` / `provider_route`
mode without rerouting primary mail; prove ownership via DNS TXT; once verified, inbound routes to the
business and replies send DKIM-authenticated from the custom identity. Unverified → no route, outbound
falls back to the system address.

**Independent Test**: Add a domain in each mode → `unverified` + `dns_challenge` returned. Verify the TXT
→ `verified`. Route inbound to the custom address → correct business. Reply → sent from the custom
identity (DKIM-signed). Unverified custom → inbound doesn't route, outbound falls back to system address.
Confirm the domain's primary (whole-domain) mail flow is never required to change.

### Tests for User Story 4 (write FIRST, must FAIL) ⚠️
- [ ] T052 [P] [US4] Contract tests for `POST/GET …/email-domains`, `POST …/email-domains/{did}/verify`, `POST/GET …/inbound-addresses` (201/200 shapes, `dns_challenge`, 404 no-oracle, 409 unverified-domain reference) in `cmd/drift_test.go`
- [ ] T053 [P] [US4] Integration test `internal/ticketing/identity_integration_test.go`: create domain (all three modes) → unverified + challenge; verify (stub resolver) → verified; custom `inbound_address` requires a verified domain (409 otherwise); inbound routes to the custom address; outbound selects custom identity when verified, else system fallback
- [ ] T054 [P] [US4] Integration test for DKIM signing: a reply from a verified domain is DKIM-signed with the per-domain selector/key (verify with `go-msgauth`)

### Implementation for User Story 4
- [ ] T055 [US4] `internal/ticketing/identity.go` — email-domain create/list: generate `verify_token` (`mf-verify=<base64url(32B)>`), return `dns_challenge` (TXT `_manyforge.<domain>` + DKIM record + SPF/MX hints) per mode; `inbox.manage`-gated, audited in-tx
- [ ] T056 [US4] DNS TXT verification: `POST …/email-domains/{did}/verify` — resolve the TXT via the SSRF-guarded `internal/platform/netsafe` resolver, set `verified_at` on match (idempotent re-verify); independent receive/send verification state
- [ ] T057 [US4] DKIM keygen + signing: generate per-domain Ed25519 (RSA-2048 fallback) keypair at runtime, store the private key as an encrypted `dkim_private_key_ref` (NEVER logged/committed), publish selector+public key in the challenge; sign verified-outbound with `emersion/go-msgauth/dkim` (research R3)
- [ ] T058 [US4] Custom inbound addresses: `POST/GET …/inbound-addresses` (`inbox.manage`) — create a `custom` address bound to a VERIFIED `email_domain` (ownership re-checked in SQL; unverified → 409); extend T024 resolution to route custom addresses
- [ ] T059 [US4] Outbound identity selection in the send path (T039): verified custom identity → send + DKIM-sign as that domain; unverified/absent → fall back to the always-available system address (FR-013)
- [ ] T060 [P] [US4] Frontend: `web/src/app/pages/support/` inbox-settings page — add domain (mode picker), show DNS challenge records, trigger verify, list addresses/domains with verification + DKIM/SPF state
- [ ] T061 [US4] Playwright `web/e2e/support.spec.ts` (US4 portion): add a `forward_in` domain → challenge shown → (stub-verified) → domain shows verified and its address listed

**Checkpoint**: Businesses can receive/send under their own brand in three modes; the desk still works on the system address out of the box.

---

## Phase 7: User Story 5 - Scoped, isolated, audited support (Priority: P3)

**Goal**: Prove the new entities uphold the foundation's guarantees — cross-tenant invisibility, no
allowed-vs-exists oracle, permission enforcement, and an in-tx audit entry for every mutation.

**Independent Test**: Two unrelated tenants each with a desk → neither can list/open/reference the
other's tickets/messages/requesters/addresses by any id (identical 404). A member lacking `tickets.read`
cannot view tickets. Each support mutation produced an `audit_entry`.

### Tests for User Story 5 (write FIRST, must FAIL) ⚠️
- [ ] T062 [P] [US5] Security pin `internal/security_regression/support_isolation_test.go`: RLS matrix across all seven new tables for absent/malformed/sideways/cross-root `principal_id`; app predicate AND RLS each deny independently; cross-tenant GET → identical 404 (SC-004/SC-006)
- [ ] T063 [P] [US5] Integration test `internal/ticketing/permissions_integration_test.go`: the six-permission enforcement matrix — each grants exactly its action set and denies the rest (SC-009), uniformly for human and agent principals
- [ ] T064 [P] [US5] Integration test `internal/ticketing/audit_integration_test.go`: every support mutation (ingestion, reply, note, status/priority/tag/assignee change, address/domain config, redact) writes an in-tx `audit_entry` with actor/source + before/after (SC-005)

### Implementation for User Story 5
- [ ] T065 [US5] Audit any mutation paths still missing an in-tx `audit_entry` (sweep US1–US4 service methods); ensure the ingestion source (`actor_kind='system'`, `actor_label='inbox:<source>'`) is recorded for principal-less ingest (FR-014)
- [ ] T066 [US5] `tickets.delete` soft-delete/redact: `internal/ticketing/service.go` redact-in-place (`ticket.redacted_at`, blank PII-bearing columns via 001's erasure proc, schedule attachment-blob deletion via outbox), excluded from lists/gets (`WHERE redacted_at IS NULL` → 404 to non-privileged); audited in-tx (research R7, data-model decision)
- [ ] T067 [US5] Verify every list/get query in `inbox`/`ticketing` carries both the app `business_id`/`tenant_root_id` predicate and relies on RLS; confirm no 403-vs-404 distinction anywhere (no-oracle audit of handlers)

**Checkpoint**: Isolation, permission, and audit guarantees are proven by automated tests for the new surface.

---

## Phase 8: Polish & Cross-Cutting Concerns

- [ ] T068 [P] Contract suite (`make contract-test`): assert the shared-layer interfaces (`InboundSource`, `Blob`, `Notifier`, event-bus) and the ~15 new endpoints against `contracts/openapi.yaml`; extend the OpenAPI-drift gate in `cmd/drift_test.go`
- [ ] T069 SC-010 performance test `internal/ticketing/perf_test.go` (build tag `integration`, `TestSC010`): seed 10,000 tickets/business at realistic thread depth; assert ticket-list and ticket-load p95 < 200 ms with RLS ENABLED
- [ ] T070 SC-011 loop-guard test `internal/inbox/loopguard_test.go`: a mail loop between two automated systems is bounded (per-requester rate cap + `is_auto_reply` detection) before exceeding the bound; suppression is audited
- [ ] T071 [P] Verify pagination max-page-size caps (silent cap to 100) on all five support list endpoints (FR-020)
- [ ] T072 [P] Structured logging + metrics for ingestion/outbound/outbox (extend `internal/platform/observability`); redact credential-bearing values (webhook secrets, DKIM refs) in all logs
- [ ] T073 [P] Run the quickstart.md validation walkthrough end-to-end against a fresh DB; fix any drift between docs and behavior
- [ ] T074 [P] Update `ARCHITECTURE.md` (support-desk module map, SL-C/SL-D/SL-E layers, ingestion `SECURITY DEFINER` exception) and `README.md` (run the SMTP receiver + webhook)
- [ ] T075 Final merge-gate run: `make test && make int-test && make contract-test && make lint` + `cd web && npm run e2e` all green; resolve any failures (no "pre-existing" exceptions)

---

## Dependencies & Execution Order

### Phase dependencies
- **Setup (P1)**: no dependencies — start immediately.
- **Foundational (P2)**: depends on Setup — **BLOCKS all user stories**. Within P2: migrations T007→T008→T009→T010 are ordered (RLS/permissions depend on the schema); T011 sqlc after the tables exist; platform layers T012–T015 are parallel; T016 wiring last.
- **User Stories (P3–P7)**: all depend on Foundational. US1 is the MVP and should land first. US2 and US3 build on US1's ticket/message model (sequence after US1, but are independently testable). US4 is largely independent (inbox identity) and can run parallel to US2/US3 once US1's resolution path exists. US5 verifies the surface and is best run after US1–US4 (its audit/permission tests reference all mutation paths).
- **Polish (P8)**: depends on the desired user stories being complete.

### Story-level dependencies
- **US1 (P1)** → Foundational only.
- **US2 (P1)** → US1 (replies onto US1 tickets; reuses outbox/notify).
- **US3 (P2)** → US1 (triages US1 tickets); the reopen-on-reply task touches the US1 ingest tx.
- **US4 (P2)** → US1 (extends address resolution + outbound identity); otherwise independent of US2/US3.
- **US5 (P3)** → US1–US4 (proves isolation/permission/audit across all new mutation paths).

### Within each story
- Tests FIRST and MUST FAIL before implementation (Red→Green→Refactor).
- Models/SQL → services → endpoints → frontend → e2e.
- Security-regression pins are part of the story they guard, not deferred.

### Parallel opportunities
- All `[P]` Setup tasks (T002–T006) run together.
- Platform layers T012–T015 run together once the schema migrations land.
- Each story's `[P]` test tasks run together; backend service work and the `[P]` frontend task run in parallel within a story.
- US4 can proceed in parallel with US2/US3 once US1's resolution path (T024) exists.

---

## Parallel Example: User Story 1

```bash
# Write all US1 tests first (they must fail):
Task: T017 Contract test for POST /inbound/email/{provider} in cmd/drift_test.go
Task: T018 Integration test for webhook+SMTP ingest in internal/inbox/ingest_integration_test.go
Task: T019 Pin threading_idempotency_test.go
Task: T020 Pin ingestion_scope_test.go
Task: T021 Pin mime_sniff_test.go
Task: T022 Pin webhook_sig_test.go

# Then parallelizable implementation seeds:
Task: T023 InboundSource interface + types in internal/inbox/source.go
Task: T033 Frontend ticket.service.ts + support/ list+thread components
```

---

## Implementation Strategy

### MVP first (User Story 1 only)
1. Phase 1 Setup → 2. Phase 2 Foundational (CRITICAL — blocks all stories) → 3. Phase 3 US1 →
4. **STOP and VALIDATE**: run the US1 independent test (webhook + SMTP ingest, dedup, no-oracle) →
5. Demo: a business receives email as threaded, isolated tickets.

### Incremental delivery
- Foundation ready → **US1 (MVP)** receive → **US2** reply → **US3** triage → **US4** BYO domain →
  **US5** prove isolation/audit. Each story is independently testable and adds value without breaking
  the previous ones. Polish (contract/perf/loop-guard/docs) lands last.

### Notes
- `[P]` = different files, no incomplete dependency.
- Commit after each task or logical group; keep `make sec-test` green continuously.
- Verify tests fail before implementing; never trust a declared `Content-Type`; never echo wrapped
  errors to clients; every mutation audits in the same transaction.
- Track higher-level progress on bd epic `manyforge-n0q`; this file is the canonical build list.
