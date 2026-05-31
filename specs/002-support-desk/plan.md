# Implementation Plan: Native Support Desk

**Branch**: `002-support-desk` | **Date**: 2026-05-31 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/002-support-desk/spec.md`; HOW decisions from [plan-inputs.md](./plan-inputs.md); program context in [`docs/ROADMAP.md`](../../docs/ROADMAP.md).

## Summary

Deliver the first product vertical on the tenant foundation (spec 001): a human-operated support desk. Inbound customer email — received via a pluggable `InboundSource` (a provider **webhook** adapter and a **built-in SMTP receiver**, both components of the single binary) — is resolved by recipient address to exactly one business and turned into a threaded **ticket** with an identified **requester**. Authorized members triage (status / priority / tags / assignee) and reply, with replies threaded back via standard headers plus an unforgeable reply token. Businesses can bring their own address/domain (forward-in / subdomain-MX / provider-route) without rerouting their primary mail. The slice introduces the thin first cut of three shared layers — **SL-C** eventing + transactional outbox + activity stream, **SL-D** templated/in-app notifications, **SL-E** attachment storage — and inherits the foundation's dual-enforced isolation, typed errors, no-existence-oracle boundary, and in-transaction audit. No AI (spec 003) and no external-system sync (spec 004); the requester carries a CRM seam for spec 005.

## Technical Context

**Language/Version**: Go 1.25 (matches the existing module); Angular 21 + TypeScript 5.9 for the agent UI.

**Primary Dependencies**: existing — `chi/v5`, `pgx/v5`, `sqlc`, `golang-migrate`, `golang-jwt/jwt v5` (EdDSA), `x/crypto`. New (to be confirmed in research.md): `emersion/go-smtp` (SMTP receiver), `jhillyerd/enmime` (MIME parsing of inbound mail), `emersion/go-msgauth` (DKIM signing/verification + SPF/DMARC result parsing), `gocloud.dev/blob` (object storage abstraction: local FS for self-host + S3-compatible).

**Storage**: PostgreSQL 16 (system of record); object storage via the blob abstraction (local filesystem default, S3-compatible optional) for attachments.

**Testing**: `go test` + `testcontainers-go` (ephemeral Postgres) for unit/integration; `internal/security_regression/` source-level pins; Playwright for the Angular agent flow; a new `make contract-test` target for shared-layer interface contracts; automated performance test for SC-010.

**Target Platform**: single self-hostable Linux binary serving `/api/v1` (HTTP) and an optional SMTP listener on a configurable address; PostgreSQL 16; optional S3-compatible object store.

**Project Type**: web service (Go modular-monolith backend) + Angular SPA frontend.

**Performance Goals**: SC-010 — ticket-list and ticket-load p95 < 200 ms at 10,000 tickets/business; SC-001 — inbound email visible as a ticket within 30 s of receipt; bounded ingestion/outbound throughput under rate limits.

**Constraints**: dual-enforced tenant isolation (RLS + app predicate) on all new tables; one deployable binary (SMTP receiver is an in-process component, not a second service); no-existence-oracle on all reads and on unknown-recipient ingestion; append-only audit in the same transaction as every mutation; attachments MIME-sniffed; webhook HMAC verified in constant time.

**Scale/Scope**: this slice — `internal/inbox`, `internal/ticketing`, thin `internal/platform/{events,notify,blob}`, six new tenant tables, six new permissions, ~15 endpoints, one inbound ingestion path. Multi-channel, queues/teams, AI, and external sync are explicitly out (deferred to 003/004/005/006).

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | How this feature satisfies it | Gate |
|-----------|-------------------------------|------|
| **I. Tenant Isolation & Hierarchy Integrity** | All six new tables carry `business_id` + immutable `tenant_root_id` with composite FKs; each gets an RLS policy mirroring 001's self-deriving model; reads scope by SQL predicate AND RLS; unknown/unauthorized → identical 404; unknown inbound recipient → silent drop, no oracle. Principal-less ingestion runs through a business-scoped `SECURITY DEFINER` function (like `accept_invitation`) that cannot widen beyond the resolved business. | PASS |
| **II. Security & Data Privacy by Default** | Caller-supplied UUIDs (assignee, ticket, address) ownership-checked in SQL before persistence; typed `errs` sentinels, no raw error echo; webhook caller authenticated by provider HMAC (constant-time); inbound attachments MIME-sniffed against an allowlist; outbound mail built without `fmt.Sprintf` of user input; DKIM private keys never logged/committed; credential-bearing values redacted in logs. | PASS |
| **III. Test-First, Automated Verification** | TDD red→green→refactor; new `security_regression` pins (isolation, ingestion-scope, idempotency, MIME-sniff, webhook-signature, no-oracle); contract + integration tests on testcontainers; Playwright e2e (inbound→ticket→reply); SC-010 perf test. Merge gate: `make test` + `make int-test` + `make sec-test` + `make contract-test` + e2e. | PASS |
| **IV. Bounded, Auditable AI Agents** | No agents introduced in this slice; the ticket/requester model is the data agents will act on in 003. Nothing here weakens the agent seam or autonomy gate. | PASS (N/A) |
| **V. Modular Monolith & Service-Layer** | New code under `internal/inbox`, `internal/ticketing`, `internal/platform/{events,notify,blob}`; thin handlers → services → sqlc; modules communicate via interfaces (e.g., `InboundSource`, `Blob`, `Notifier`, event bus), not cross-table access; the SMTP receiver is an in-process component of `cmd/manyforge` (one deployable). | PASS |
| **VI. Observability & Auditability** | Every support mutation (ingestion, reply, note, triage, address/domain config) writes an `audit_entry` in the same transaction; ingestion records the source as actor. Structured slog with request/correlation IDs; outbox side-effects committed in the same transaction as the source write (no fire-and-forget); existing health/readiness/metrics extended. | PASS |
| **VII. Open Source, Open-Core, Community Trust** | All 002 code ships in `internal/` (MIT), nothing under `ee/`; self-hosters are first-class (built-in SMTP receiver + local-FS blob mean no third-party email provider or cloud storage is required); telemetry stays opt-in; DKIM/secret material is generated/stored at runtime, never committed. | PASS |

No violations → **Complexity Tracking is empty**. Two design choices are documented because they look unusual but are compliant: (a) the SMTP receiver is an in-process listener within the single binary (Principle V — one deployable); (b) inbound ingestion uses a `SECURITY DEFINER` function because external mail has no principal (Principle I — the controlled, audited exception, scoped to one business, exactly as 001 does for invitation acceptance).

## Project Structure

### Documentation (this feature)

```text
specs/002-support-desk/
├── spec.md              # WHAT/WHY (approved)
├── plan-inputs.md       # HOW decisions feeding this plan
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/           # Phase 1 output (OpenAPI for the new endpoints)
└── tasks.md             # Phase 2 output (/speckit-tasks — not created here)
```

### Source Code (repository root)

```text
cmd/manyforge/
└── main.go                      # wire the SMTP listener (configurable addr) + new routes + outbox worker

internal/
├── inbox/                       # inbound ingestion
│   ├── source.go                # InboundSource interface + RawMessage/ParsedEmail types
│   ├── webhook.go               # WebhookAdapter (HMAC-verified provider POST)
│   ├── smtp.go                  # SMTPAdapter (emersion/go-smtp receiver, in-process)
│   ├── resolve.go               # recipient → business lookup (inbound_address)
│   ├── thread.go                # threading (headers + reply token + subject fallback)
│   ├── service.go               # ingestion orchestration → SECURITY DEFINER ingest
│   └── handler.go               # POST /inbound/email/{provider}
├── ticketing/
│   ├── service.go               # tickets, messages, requesters, reply, note, triage
│   ├── handler.go               # ticket/message/requester/inbound-address/email-domain routes
│   ├── requester.go             # requester upsert/dedup (CRM seam)
│   └── identity.go              # email_domain verification + DKIM/SPF sending identity
└── platform/
    ├── events/                  # SL-C: event bus + transactional outbox + activity stream
    ├── notify/                  # SL-D: templated email (extends Mailer) + in-app notifications
    └── blob/                    # SL-E: gocloud.dev/blob wrapper (fileblob + s3blob), MIME-sniff

db/query/
├── inbox.sql                    # address resolution, ingestion, threading lookups
├── ticketing.sql                # ticket/message/requester/tag CRUD + lists (keyset)
└── notify.sql                   # notifications + outbox

migrations/
├── 0013_support_desk.up.sql     # inbound_address, email_domain, requester, ticket,
│                                #   ticket_tag, ticket_message, attachment (+ composite FKs, indexes)
├── 0014_support_rls.up.sql      # RLS policies + SECURITY DEFINER ingestion function
├── 0015_support_permissions.up.sql  # seed tickets.* / inbox.manage + preset grants
└── 0016_events_notify.up.sql    # outbox + notification tables (SL-C/SL-D)

web/src/app/
├── core/ticket.service.ts       # ticket/inbox API client
└── pages/support/               # ticket list + thread view + reply composer (Angular)

internal/security_regression/
├── support_isolation_test.go    # RLS matrix + cross-tenant 404 (new tables)
├── ingestion_scope_test.go      # ingestion cannot widen beyond resolved business
├── threading_idempotency_test.go# replayed message-id → no dup; 0% mis-thread
├── mime_sniff_test.go           # spoofed Content-Type rejected
└── webhook_sig_test.go          # provider HMAC constant-time verify
```

**Structure Decision**: Web-service modular monolith (matches spec 001). Two new domain modules (`inbox`, `ticketing`) plus three thin platform layers (`events`, `notify`, `blob`) under `internal/`, surfaced through `cmd/manyforge` (HTTP routes + an in-process SMTP listener + an outbox worker). Database changes are forward-only migrations with sqlc-generated queries; the Angular agent UI gains a `support/` feature area.

## Complexity Tracking

> No Constitution Check violations. Table intentionally empty.

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| — | — | — |
