# Connectors Management API + UI — Design

- **Status:** Approved 2026-06-12
- **Issue:** `manyforge-4zs.3` (parent epic `manyforge-4zs` — UI Redesign + Design System, Stream 2 spillover)
- **Builds on:** Spec 004 External Connectors (`docs/superpowers/specs/2026-06-06-external-connectors-design.md`, epic `manyforge-a7j`)
- **Size:** M (one phased spec: backend lands first, UI second)

## Summary

Spec 004 built the entire connectors *engine* — credential vault, inbound webhook handler,
outbound dispatcher, reconcile poller, Jira + Zendesk clients, and the `connector` data model —
but exposed **no management HTTP API**: the only public route is the inbound webhook
(`POST /connectors/{type}/{connectorID}/webhook`). A connector today can only be created from Go
code / tests, never from the product. This feature adds the **human-facing management surface**:
a `connectors.manage`-gated CRUD API (list, create, edit, rotate credentials, test, delete) plus an
Angular UI on the Stream-1 design system, so an owner/admin can connect Jira/Zendesk, watch sync
health, rotate credentials, and disconnect — all without leaking sealed credentials.

## Context: what exists vs. what's missing

**Already built (Spec 004):**
- `connector` table: `id, business_id, tenant_root_id, type(jira|zendesk), display_name, base_url,
  allow_private_base_url, secret_ref→secret, config(jsonb), status(enabled|disabled),
  created_at, updated_at, last_reconciled_at`. `UNIQUE(business_id, type, base_url)`. RLS-scoped.
- Vault (`internal/platform/secrets`): AES-256-GCM seal/open; `secret` table holds sealed blobs.
- `internal/connectors/service.go`: `Create` (validate → optional verify → seal → insert → audit)
  and `Resolve` (load + unseal). sqlc: `InsertConnector`, `GetConnector`, `InsertSecret`,
  `GetSecret`, `DeleteSecret`, plus outbound enqueue queries.
- Inbound webhook handler, outbound dispatcher, reconcile poller, Jira + Zendesk `TicketingConnector`
  clients, agent tools (gated by the *separate* `connectors.read`/`connectors.write` perms).

**Missing (this feature):**
- `connectors.manage` permission (human management; distinct from the agent-tool perms).
- Protected CRUD routes + handlers under `/api/v1/businesses/{id}/connectors`.
- Service methods: `List`, `Get`, `Update`, `RotateCredential`, `Test`, `Delete`.
- sqlc queries: `ListConnectors`, `GetConnectorMeta`, `UpdateConnector`, the delete-detach statements,
  and sync-health aggregates.
- OpenAPI contract — spec 004 never got a `specs/004-…/` dir; this establishes
  `specs/004-external-connectors/contracts/openapi.yaml` (per the 001–003 convention).
- The entire Angular connectors UI (service, list, setup/rotate form, nav item + badge, e2e).

## Decisions (locked)

1. **Full CRUD + credential rotation + hard delete** (the largest scope option). This deliberately
   pulls in the two pieces Spec 004 explicitly deferred — the credential-rotation guard and
   hard-delete cascades — and treats both as first-class with their own security tests.
2. **Reconnect lifecycle = disable ⇄ enable.** Toggling `status` preserves the connector's identity,
   all `connector_id`/`external_id` ticket linkage, sync_state, and the sealed secret, so re-enabling
   resumes sync with **zero duplicates**. This — not delete+recreate — is the supported
   "temporarily disconnect / reconnect" path.
3. **Hard delete is terminal but non-destructive to customer data.** In one transaction: NULL
   `connector_id` on linked `ticket` + `ticket_message` rows **while preserving `external_id` and
   `external_url`** as read-only historical metadata (permitted by
   `CHECK(connector_id IS NULL OR external_id IS NOT NULL)` — the NULL-connector clause passes);
   cascade-delete `connector_sync_state`, `connector_webhook_delivery`, `connector_outbound_op`;
   delete the sealed `secret`; delete the `connector`; write an audit row.
4. **Auto-re-adopt-on-reconnect is deferred** to a follow-up bd issue. Because delete preserves
   `external_id`/`external_url`, a future connect-time re-adoption (match orphans by `external_url`
   host → relink by `external_id`) remains possible; v1 does not auto-relink. A connector created
   after a delete is a new identity that re-imports fresh.
5. **`base_url` and `type` are immutable** on `PATCH`. They are part of the connector's identity
   (`UNIQUE(business_id, type, base_url)`); changing the target is "reconnect to a different system"
   = delete + recreate. This avoids SSRF re-validation drift and identity confusion.
6. **Rotation re-verifies by default** (kept default a): a credential rotation runs the live verify
   call *before* sealing and refuses to persist a credential that does not authenticate.
7. **Sync health = moderate** (kept default b): per connector surface `status`, `last_reconciled_at`,
   `linked_ticket_count`, `pending_outbound_ops`, `failed_outbound_ops`, and `last_error` (redacted
   reason of the most-recent failed op). Drives a Healthy / Degraded / Disabled pill + the nav badge.
8. **API-token auth only; no OAuth2.** The bd issue text mentioned "OAuth2/API-key," but the backend
   only supports API tokens and Spec 004 deferred OAuth2 (YAGNI for self-hosters). OAuth2 is out of
   scope; the setup form collects email + API token (+ webhook secret).

## Out of scope

- OAuth2 / 3LO connector setup.
- Auto-re-adoption of detached tickets on reconnect (deferred follow-up).
- Vault master-key rotation.
- Partial credential rotation (rotate only `webhook_secret` without re-supplying email/api_token) —
  rotation replaces the full bundle.
- New connector *types* beyond the existing Jira + Zendesk.

## Permission model

- Migration `0048_connector_manage_permission`:
  `INSERT INTO permission (key, module, description) VALUES
   ('connectors.manage','connectors','Create, configure, rotate credentials for, and delete external connectors');`
  Grant to preset roles **owner + admin** (`role.tenant_root_id IS NULL AND role.key IN ('owner','admin')`).
- Wired in `cmd/manyforge/main.go` as
  `connectorsManage := httpx.RequirePermission(db, permResolve, "connectors.manage", businessIDFromPath)`
  and applied to the route group (mirrors `agentsConfigure`).
- Distinct from `connectors.read`/`connectors.write` (those gate agent tools, migration 0047).

## HTTP API

Base: `/api/v1/businesses/{id}/connectors`, all gated by `connectors.manage`. Mirrors the
`agents` CRUD conventions (chi `ProtectedRoutes`, thin handlers, wire DTOs separate from domain,
`WithPrincipal` + explicit `business_id` predicate, typed `errs` → HTTP).

| Method | Path | Action | Success | Errors |
|---|---|---|---|---|
| `GET`    | `/`               | List connectors (metadata + health) | `200` | `404` |
| `POST`   | `/`               | Create / connect | `201` | `400` (incl. verify-failed, safe msg), `404`, `409` (dup type+base_url) |
| `GET`    | `/{cid}`          | Get one (metadata + health) | `200` | `404` |
| `PATCH`  | `/{cid}`          | Edit `display_name`/`config`/`status` | `200` | `400`, `404` |
| `PUT`    | `/{cid}/credential` | Rotate sealed credential (full bundle) | `200` | `400` (verify failed), `404` |
| `POST`   | `/{cid}/test`     | Re-verify stored credential | `200 {ok, detail}` | `404` |
| `DELETE` | `/{cid}`          | Terminal delete (detach + cascade) | `204` | `404` |

**Request shapes**
- Create: `{ type, display_name, base_url, allow_private_base_url?, email, api_token, webhook_secret, config? }`
- Patch (all optional / pointer-typed; omitted = unchanged): `{ display_name?, config?, status? }`
- Rotate credential: `{ email, api_token, webhook_secret }` (full bundle).

**Response shape (`connectorResp`) — credentials never present**
`{ id, business_id, type, display_name, base_url, allow_private_base_url, config, status,
   created_at, updated_at, last_reconciled_at,
   health: { state: "healthy"|"degraded"|"disabled", linked_ticket_count, pending_outbound_ops,
             failed_outbound_ops, last_error } }`
The wire DTO has **no** `email`/`api_token`/`webhook_secret` field — write-only by construction.

## Service layer + data layer

New service methods in `internal/connectors/service.go` (all take `(ctx, principalID, businessID, …)`,
wrap DB in `DB.WithPrincipal`, include explicit `business_id` predicates, map `pgx.ErrNoRows` →
`errs.ErrNotFound`, SQLSTATE 23505 → `errs.ErrConflict`):

- **`List`** → `ListConnectors(business_id)` + a `ConnectorHealth(business_id)` aggregate
  (counts from `connector_outbound_op` grouped by status, linked-ticket count, last failed-op reason).
- **`Get`** → `GetConnectorMeta(id, business_id)` (no secret).
- **`Update`** → `UpdateConnector` with `COALESCE(NULLIF($n,''), col)` / pointer params for partial
  update of `display_name`, `config`, `status`; `base_url`/`type` untouched.
- **`RotateCredential`** → verify new bundle *before* tx (never hold a tx across network, mirroring
  `Create`); then one tx: `InsertSecret` (seal) → `UpdateConnectorSecretRef` (swap) → `DeleteSecret`
  (old) → audit.
- **`Test`** → `Resolve` + run the connector's `Verify`; returns `{ ok, detail }` (detail is a safe,
  non-leaking status string).
- **`Delete`** → one tx executing the detach + cascade of Decision 3, then audit.

No new tables; only the permission migration (0048). The delete-detach is raw SQL (multiple
statements in one `WithPrincipal` tx), not sqlc-generated, because it spans several tables.

## Security invariants (each gets a regression pin)

- **Credentials write-only / never returned** — structural (DTO has no cred fields) + test asserting
  no response body on any endpoint contains the seeded api_token/webhook_secret.
- **Ownership predicate in SQL** — every method filters by `business_id` *and* runs under
  `WithPrincipal` RLS. Foreign/unknown `cid` → `404` (no existence oracle; matches `RequirePermission`
  returning `404` on perm-miss).
- **Validate all caller UUIDs** — `cid` path param and any body-supplied IDs verified before mutate.
- **Atomicity** — delete-detach and credential-rotation each run in a single transaction; partial
  failure rolls back. Verify network calls happen *outside* the tx.
- **Audit every mutation** — create/update/rotate/enable/disable/delete each write an `audit` row
  (actor, target connector, action, safe before/after metadata — never secrets).
- **SSRF** — `base_url` validated via `netsafe` on create (existing `Create` path); immutable
  thereafter, so no re-validation gap.
- **Source-level pin** — `strings.Contains` test that the DELETE SQL NULLs `connector_id` but does
  **not** null `external_id`/`external_url` (guards Decision 3 against a future refactor).

## Frontend (Angular, Stream-1 design system)

- `web/src/app/core/connectors.service.ts` — mirror `approvals.service.ts`; business-scoped
  `/api/v1/businesses/{id}/connectors` calls; `pendingCount` signal = # degraded/disabled → nav badge.
- `web/src/app/pages/connectors/list.ts` — mirror `approvals/queue.ts`: business filter, `.mf-table`
  rows (DIV-flex; fixed-width `<span>` columns), a health `mf-status-pill`
  (healthy→success / degraded→warn / disabled→neutral), row actions Test / Enable·Disable / Edit /
  Delete. Loading / empty (`mf-empty-state`) / error states; stale-response guard; polling with
  `clearInterval` on destroy.
- `web/src/app/pages/connectors/setup.ts` — mirror `inbox-settings.ts` (template-driven `FormsModule`,
  no reactive forms): type selector (Jira/Zendesk), `base_url`, `display_name`, `config`
  (`project_key`/`issue_type`), and **`type="password"`** credential fields with a "never shown again"
  hint. The same form is reused for **rotate credential**.
- Nav item `{ label: 'Connectors', route: '/connectors', testid: 'nav-connectors' }` in `ui/nav.ts`;
  lazy route in `app.routes.ts` behind `authGuard`; badge stamped in `app.ts` like Approvals.
- **Destructive delete UX:** a type-to-confirm modal naming the connector and showing the
  linked-ticket count that will be detached to native.

## Testing plan

- **Backend unit** (`internal/connectors/handler_test.go`): fake service behind real `RequireAuth`;
  asserts request validation, error→status mapping, and that response DTOs carry no credentials.
- **Backend integration** (`//go:build integration`, testcontainers): full CRUD against a real DB;
  the `connectors.manage` permission matrix per role (owner/admin allowed, member/viewer → 404);
  RLS cross-tenant isolation; create→test→rotate→delete happy path; delete-detach correctness
  (tickets survive as native with `external_id` preserved; bookkeeping + secret gone).
- **Security regression** (`internal/security_regression/connectors_manage_<id>_test.go`, one file,
  finding-ID header): the six pins listed above.
- **Frontend unit** (Vitest, `cd web && npm test`): list render, health pill mapping, optimistic
  enable/disable, credential field is `type=password`.
- **Frontend e2e** (`web/e2e/connectors.spec.ts`, Playwright): list → create → test → delete-confirm
  flows with `page.route` mocks; light + dark theme eyes-on (undefined CSS classes render silently).
- **Dev-env setup:** add `MANYFORGE_CONNECTOR_MASTER_KEY` to `.air.env` so the backend enables
  connectors for manual/e2e verification (today it logs "external connectors disabled").
- CI: `make test` + `make sec-test` + lint, plus the frontend gate, must all pass.

## Build sequence (phased)

1. Migration `0048` (`connectors.manage`) + main.go wiring.
2. sqlc queries (`ListConnectors`, `ConnectorHealth`, `GetConnectorMeta`, `UpdateConnector`,
   `UpdateConnectorSecretRef`) + `make generate`.
3. Service methods: `List`, `Get`, `Update`, `RotateCredential`, `Test`, `Delete`.
4. Handlers + routes + `specs/004-external-connectors/contracts/openapi.yaml`.
5. Backend tests (unit + integration) + security-regression pins; `make test` + `make sec-test` green.
6. Frontend service + list page + nav item + route + badge.
7. Setup/rotate form + delete-confirm modal.
8. Frontend Vitest + Playwright; add `MANYFORGE_CONNECTOR_MASTER_KEY` to `.air.env`; manual e2e in
   browser (light + dark) with `live-demo@manyforge.test`.

## Acceptance / demo

As owner of *Acme Holdings*: open **Connectors**, connect a Jira instance (form validates + live-verifies
the API token), see it land as **Healthy**; **Test** re-verifies; **Disable** then **Enable** resumes
sync with no duplicate tickets; **Rotate credential** swaps the token (re-verified, old secret deleted);
**Delete** with type-to-confirm detaches its synced tickets to native (they remain, `external_id`
preserved) and removes the connector + secret. Throughout, no API response ever contains the token,
and every mutation is audited.

## Follow-ups (file as bd issues)

- Auto-re-adopt detached tickets on reconnect (match by `external_url` host → relink by `external_id`).
- `manyforge-a7j.4.9` (outbound dispatcher stale-op reaper) feeds `failed_outbound_ops` health accuracy.
- OAuth2 connector setup (if/when a hosted-SaaS customer needs it).
