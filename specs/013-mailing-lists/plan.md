# Implementation Plan: Mailing Lists and Broadcast Campaigns

**Spec:** 013

**Design source:**
[`docs/superpowers/specs/2026-08-26-mailing-lists-automations-design.md`](../../docs/superpowers/specs/2026-08-26-mailing-lists-automations-design.md)

## Technical Context

- Backend: Go modular monolith, thin chi handlers, services, sqlc, PostgreSQL.
- Frontend: Angular standalone components and signals under `web/`.
- Shared seams: `internal/platform/notify`, `internal/platform/events`,
  `internal/platform/secrets`, `internal/platform/netsafe`, CRM activity, and
  tenant-merge fencing.
- New module: `internal/mailing`, with provider and render subpackages.
- Contract: `contracts/openapi.yaml`, expanded by each backend slice and pinned
  bidirectionally by `drift_013_test.go`.

## Constitution Check

- Tenant isolation: every new tenant table has `(business_id, tenant_root_id)`,
  scoped queries, RLS `USING` and `WITH CHECK`, immutable roots, and merge
  inventory/fence coverage.
- Security: public and worker writes cross RLS only through least-privilege
  definers; all tokens use purpose-separated HMAC/HKDF and constant-time checks;
  all external HTTP is SSRF guarded.
- Test first: each slice adds its unit, integration, contract, security pin, or
  browser coverage before implementation.
- Modular monolith: mailing owns its tables and exposes ports/events to Spec 014
  rather than allowing direct cross-module table access.
- Auditability: administrative mutations are audited in transaction; delivery
  and webhook transitions retain correlation identifiers without secrets.

## Architecture

`internal/mailing` owns lists, subscriber consent and tags, templates, sending
profiles, campaigns, delivery state, tracking, and provider webhooks. Campaign
fan-out inserts durable `mailing_delivery` rows. A multi-replica-safe worker
claims deliveries with leases, renders content, sends without holding a
transaction open, and completes or retries in a short follow-up transaction.
Public and webhook paths use uniform-response boundaries and security-definer
database functions.

Provider integrations implement one `Deliverer` interface. Relay wraps the
existing SMTP sender; Resend uses the HTTP API; SES uses raw MIME built by the
shared notification package. Spec 014 later enqueues automation deliveries
through a narrow mailing port and reuses this worker.

## Delivery Slices

1. Backend core: migration 0124, lists/subscribers/tags/keys/suppressions,
   templates, sending-profile CRUD, permissions, wiring, drift and security
   pins.
2. Frontend A: list and subscriber management, imports, CRM picker, templates,
   routes, navigation, unit and Playwright coverage.
3. Public and tokens: migration 0125, token codec, public/S2S subscription,
   confirmation/unsubscribe roots, and subscriber outbox topics.
4. Frontend B: sending profile and public subscription/confirmation/
   unsubscribe pages.
5. Rendering and providers: Markdown layout, preview, relay/Resend/SES,
   verification/test-send, config, Helm, and operator runbook.
6. Campaigns and worker: migration 0126, campaign API, resumable fan-out,
   delivery dispatcher, tracking, rollups, and relay-bounce linkage.
7. Frontend C: campaign/template preview, editor, scheduling, unsaved guard.
8. Provider webhooks: SNS verification, Resend signatures, event mapping,
   suppression, CRM activity, and remaining security pins.
9. Frontend D: campaign statistics, shared stat tiles, suppression management,
   and final browser coverage.

Each slice branches directly from current `master`, merges before the next
starts, and owns the gates listed in section 8 of the canonical design.

## Verification

Run the relevant focused tests while developing, then the complete repository
gate required by the constitution and the slice: `make test`, `make int-test`,
`make sec-test`, `make contract-test`, `make lint`, frontend unit/token/build
checks, and Playwright e2e. After slices 8 and 9, execute the documented local
end-to-end campaign flow and retain automated coverage as the evidence of
record.
