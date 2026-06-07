# Spec 004 — External Ticketing Connectors (Jira → Zendesk)

**Status:** Design approved 2026-06-06 · **Epic:** `manyforge-a7j` · **Shared layer:** SL-B
**Depends on:** Spec 002 (ticket model, `manyforge-n0q`), Spec 003 (agent runtime + tools, `manyforge-deo`)
**Blocks:** Spec 005 (CRM, `manyforge-nwr`), Spec 006 (Feedback, `manyforge-saz`), Spec 007 (Coding agents, `manyforge-7ml`)
**Size:** M–L · **Branch:** `004-external-connectors`

## 1. Summary

Add SL-B: a generic envelope-encrypted credential vault and a connector framework
that bidirectionally syncs native ManyForge tickets with external ticketing systems,
and exposes external tickets to Spec 003 agents as gated, audited tools.

The product remains fully standalone (a self-hoster with no Jira is unaffected);
connectors are opt-in per business. **Jira is built first, end-to-end**, to prove the
framework against a real API; **Zendesk follows as a thin second implementation**
reusing the same vault, interface, sync engine, and webhook plumbing.

### Decisions locked during brainstorming

| Fork | Decision | Rationale |
|------|----------|-----------|
| Decomposition | One connector E2E (Jira), then thin 2nd (Zendesk) | De-risk the framework against one real API before generalizing; vertical-slice ethos. |
| Lead connector | **Jira** | Richer issue model stresses the framework harder up front. |
| Auth | **API token first**; OAuth2 3LO deferred | Jira Cloud accepts email + API token (HTTP Basic). No redirect/refresh machinery. YAGNI for a self-hosters-first product. |
| Conflict policy | **External-wins** on scalars; comments append-only union | Jira owns scalar state; ManyForge is the AI/triage layer on top. |
| Agent capability | **Read + gated write** connector tools (US6) | Demo requires the autonomy gate exercised on a real external action. |

### Out of scope (deferred — filed as follow-ups, not built now)

- OAuth2 3LO (authorize redirect, state/PKCE, refresh rotation + scheduler).
- Vault **key rotation** (the `crypto.Sealer` is single-master-key today).
- **Passive native→external scalar push** (external is source-of-truth; native scalar
  edits are advisory — to change Jira scalars you take a deliberate gated action, §5.3).
- A deo.11-style guard: a future `UpdateConnectorCredential` query **must** carry
  `allow_private_base_url`.

## 2. Architecture & module layout

Two new packages, both already named in the constitution's module map.

### `internal/platform/secrets` — the vault (SL-B)

A generic envelope-encrypted secret store wrapping the existing
`internal/platform/crypto/sealer.go` (`Sealer.Seal/Open`, AES-256-GCM, single master key).
Typed API: `Put(ctx, tx, business, scope, ref, plaintext) → id`, `Resolve(ctx, business, scope, ref) → []byte`, `Delete(...)`. RLS-scoped, every write audited, **secrets never logged**.
Backed by a `secret` table holding only sealed values. First consumer is connectors;
Spec 007's repo connector reuses it unchanged.

### `internal/connectors` — the framework

- `TicketingConnector` interface (capability-typed): the seam every concrete connector
  implements. Methods cover: fetch issue + comments, post comment, transition status,
  list issues updated-since (reconcile), decode + verify webhook.
- `Registry`: resolves an enabled `connector` row → a live client bound to vault-resolved
  credentials and an SSRF-safe HTTP client.
- `/jira` — Jira Cloud implementation (US3/US4).
- `/zendesk` — Zendesk implementation (US5).
- `SyncService` — inbound + outbound sync handlers (outbox subscribers) + the
  reconcile poller.
- `webhook.go` — public ingest handler (mirrors `internal/inbox/webhook.go`).
- `credential.go` — connector-credential service (validate → seal → store → audit).
- `tools.go` — the connector agent-tools (US6).

### Sync transport = the existing outbox, not a new daemon

Inbound webhook ingest and native ticket changes both `Enqueue` onto the existing
`internal/platform/events` outbox **in their own transaction**; the existing `Worker`
dispatches to `SyncService` handlers registered via `Bus.Subscribe`. This reuses the
transactional, at-least-once, `FOR UPDATE SKIP LOCKED` machinery already proven by
`ticket.created` / `ticket.replied`, instead of inventing a parallel sync loop.

All outbound HTTP goes through `internal/platform/netsafe` (`NewClientWithOptions`):
metadata IPs are always blocked; an on-prem Jira Data Center on loopback/RFC1918 is
reachable **only** via a per-connector `allow_private_base_url` trust flag, validated at
create-time and audited in the create tx — exactly the deo.9 pattern.

## 3. Data model (migrations 0040+)

- **`connector`** `(id, business_id, tenant_root_id, type ['jira'|'zendesk'], display_name,
  base_url, allow_private_base_url bool, config jsonb, status ['enabled'|'disabled'],
  created_at, updated_at)` — RLS policy, `tenant_root` immutable trigger, composite FK
  `(business_id, tenant_root_id) → business`. `config` holds connector-specific settings
  (e.g. Jira project key, default issue type).
- **Credential** — the **vault** holds the sealed `email:api_token`; the `connector` row
  carries a `secret_ref` into the vault. No plaintext secret column exists anywhere.
- **`ticket`** `+ connector_id uuid NULL` (FK), `+ external_id text NULL`,
  `+ external_url text NULL`; partial `UNIQUE(connector_id, external_id) WHERE connector_id IS NOT NULL`.
- **`ticket_message`** `+ connector_id uuid NULL` (FK), `+ external_id text NULL`;
  partial `UNIQUE(connector_id, external_id)` → comment dedupe.
- **`connector_sync_state`** `(ticket_id, connector_id, external_id, snapshot jsonb,
  external_updated_at, synced_at)` — last-synced scalar snapshot; drives external-wins
  change detection and the reconcile cursor.
- **`connector_webhook_delivery`** `(connector_id, external_delivery_id, received_at)`
  `UNIQUE(connector_id, external_delivery_id)` → replay/idempotency protection.

Mirror every new column into `db/schema.sql` (sqlc expands `*`); strip `DEFAULT`.
Every new table: RLS + composite FK + `tenant_root` immutable trigger.

## 4. Mapping (Jira ⇄ native)

| Native | Jira | Notes |
|--------|------|-------|
| `ticket` | issue | linked by `external_id` = issue key/id |
| `ticket.subject` | summary | |
| `ticket.status` | status (via transitions) | external-wins inbound |
| `ticket.priority` | priority | external-wins inbound |
| `ticket_message` (direction=note/outbound/inbound) | comment | dedupe by `external_id` |
| `requester` | reporter | best-effort, by email |

Zendesk (US5): ticket↔ticket, comment↔ticket_message, same shape via the interface.

## 5. Sync engine & semantics

### 5.1 Inbound (Jira → native)

Public route `POST /connectors/webhook/{type}/{connectorRef}` (no JWT), wrapped by the
per-IP rate-limit middleware:

1. **Body cap** via `http.MaxBytesReader` → 413 if over.
2. **Signature verify** — constant-time HMAC against the connector's webhook secret;
   missing/forged → 401.
3. **Rate-limit before routing** — keyed by `connectorRef`, applied *before* resolving the
   connector, so unknown ≡ known (no existence oracle).
4. **Dedupe** the delivery id (`connector_webhook_delivery` unique insert); replay → no-op.
5. **Enqueue** `connector.inbound.sync` (raw payload + connectorRef) in the ingest tx.
6. Return uniform **202** for routed / unknown / duplicate (no oracle).

Worker handler `SyncService.handleInbound`: resolves the connector (RLS), upserts the
native ticket + comments by `external_id` (idempotent), applies **external-wins** scalars,
refreshes `connector_sync_state.snapshot`, audits.

### 5.2 Comments — append-only union

Comments/messages never conflict: both directions union, deduped by
`(connector_id, external_id)`. Idempotent by construction.

### 5.3 Outbound (native → Jira)

A native reply or a new connector-linked ticket enqueues `connector.outbound.sync` **in the
source write's tx**. Worker posts a Jira comment / creates the issue via the SSRF-safe
client, writes the resulting `external_id` back onto the message/ticket, audits.

Passive native **scalar** edits are **not** pushed (external is source-of-truth). Changing a
Jira scalar is a deliberate **gated action** (§6) — an explicit, audited write that becomes
the new external truth on the next inbound pull. This prevents native edits from being
silently clobbered: the only way to move Jira is an explicit action, never a passive field edit.

### 5.4 Reconcile backstop

A periodic delta-poll per enabled connector (`list issues updated since cursor`) reuses the
§5.1 inbound upsert path, catching dropped/missed webhooks. Cursor stored per connector.

## 6. Agent tools (US6)

Registered in the Spec 003 tool registry alongside internal tools; the autonomy gate runs
**after RBAC, before execution**; every invocation audited via the existing tool-audit path.

| Tool | Effect | Perm | Behavior |
|------|--------|------|----------|
| `read_external_ticket` | `EffectRead` | `connectors.read` | Fetch Jira issue + comments as context (never mutates). |
| `add_external_comment` | `EffectExternal` | `connectors.write` | Gated; posts a comment to Jira. |
| `transition_external_status` | `EffectExternal` | `connectors.write` | Gated; transitions the Jira issue. |

The two `EffectExternal` actions are authoritative Jira writes (external truth refreshes on
the next pull). By mode: `assist`→queue/approve, `queue-writes`→approve, `autonomous`→exec
(per the existing `gate()` table). Demo: an agent reads an external ticket, then takes a
gated write action through the autonomy gate.

## 7. Security / regression contract (the pins)

One source-level + behavioral pin per item, in `internal/security_regression/` (one file per
finding id), run by `make sec-test`:

1. **Vault**: sealed-at-rest round-trip + **no-secret-in-logs** (log scan / redaction).
2. **Webhook signature**: valid passes, forged/missing → 401, uniform 202, no oracle.
3. **Sync idempotency**: re-delivered webhook → single message (delivery-dedupe + `external_id` unique).
4. **Conflict determinism**: external-wins; both-changed → external value applied **+ audited**.
5. **SSRF-safe outbound**: metadata always blocked; on-prem reachable only via trust flag,
   validated + audited (deo.9 pattern); redirect-to-metadata stays blocked.
6. **External actions gated + audited**: `EffectExternal` → gate by mode; every invocation
   audited (inputs/outputs/decision).
7. **Tenant isolation**: RLS + composite FK + `tenant_root` immutable on every new table;
   cross-tenant connector/ticket access returns not-found.

## 8. User-story breakdown

Each US = its own design + TDD plan, fresh implementer + 2-stage review, mirroring US7/US8.

| US | Scope | Depends |
|----|-------|---------|
| **US1** | `internal/platform/secrets` vault + `connector`/credential schema + credential service (validate→seal→store→audit, on-prem trust flag) + test-call validation. Pins: vault encryption, no-secret-in-logs. | — |
| **US2** | `TicketingConnector` interface + `Registry` + ext-id/`connector_id` columns + `connector_sync_state`/`connector_webhook_delivery` tables + outbox topics, proven against a **fake** in-memory connector (no real API yet). | US1 |
| **US3** | Jira **inbound**: Jira client (read issue/comments, list-updated-since), webhook adapter (verify + dedupe), inbound upsert (external-wins + snapshot), reconcile poll. One-way ext→native. Golden fixtures. | US2 |
| **US4** | Jira **outbound + full bidirectional**: native reply→Jira comment, new linked ticket→issue, conflict finalized, round-trip integration test. | US3 |
| **US5** | **Zendesk**: thin 2nd `TicketingConnector` + webhook adapter reusing US1–US4. Golden fixtures. | US4 |
| **US6** | **Agent tools**: `read_external_ticket` (read) + `add_external_comment` / `transition_external_status` (external, gated+audited). | US3 |

Sequencing: US1→US2→US3→US4. US5 and US6 can run in parallel after US4 (US6 needs only the
Jira client + gate from US3, but lands cleanly after US4's outbound path exists).

## 9. Testing strategy

- **Golden fixtures** in `testdata/` (Jira/Zendesk issue, comments, transitions, webhook
  payloads) → deterministic connector tests, mirroring `internal/platform/ai/testdata`.
- **Per-pin tests** (§7): vault round-trip + log-scan; webhook valid/forged/missing/replay +
  body-cap + rate-limit-before-routing; idempotent re-delivery; external-wins conflict;
  SSRF refusal + on-prem trust; gate queue/approve/exec by mode.
- **Integration** (`//go:build integration`, `make int-test`): full Jira round-trip against an
  `httptest` stub server behind the SSRF client; e2e config path
  (DB connector cred → vault resolve → client → transport).
- Full gate per US: `gofmt -l` clean + `make test` + `make contract-test` + `make lint`
  (golangci-lint at `~/go/bin`) + `make sec-test` + `make int-test`.

## 10. Demo (acceptance)

Connect a Jira project to a business → an external issue arrives via webhook and appears as a
native ticket → a 003 agent reads it and takes a gated write action (comment / transition)
through the autonomy gate → the change lands in Jira → a reply on the native ticket syncs back
as a Jira comment. Then repeat the connect+sync step for Zendesk to prove the framework
generalizes.
