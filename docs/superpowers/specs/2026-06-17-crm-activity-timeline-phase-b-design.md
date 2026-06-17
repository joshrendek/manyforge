# CRM Phase B — Activity Timeline — Design

- **Date:** 2026-06-17
- **Status:** Approved-scope (continues Spec 005); implementation plan to follow
- **bd epic:** `manyforge-nwr` (Spec 005). Builds on Phase A (`manyforge-yhe`, merged in PR #3).
- **Branch:** `005-crm-phase-b` (off master, which has Phase A).

## Goal

Give each contact a durable, chronological **activity timeline** — auto-populated from ticket + email events across sources — shown on the contact-detail page. Durable `activity_entry` table written **in-tx at each source mutation** (the `audit.Write` pattern), plus a one-time backfill from existing tickets/messages.

## Scope

**In scope (this slice):**
- `activity_entry` table (tenant-wide, RLS), with indexes for the per-contact + per-business timeline.
- `ActivityService`: `Record(ctx, tx, entry)` (in-tx, mirrors `audit.Write`) + `ListForContact(ctx, principalID, businessID, contactID, cursor)`.
- **Principal-scoped recording hooks** (inline in the existing `WithPrincipal` tx, next to the existing `audit.Write`): `ticket_status_changed` (`Triage`), `email_sent` (`Reply`), `note_added` (`AddNote`), `ticket_reopened`/status transitions.
- **Native-inbox recording** (principal-less): a new SECURITY DEFINER helper `crm_record_inbound_activity(...)` called from the inbox Go after ingest — records `ticket_created` (on create) + `email_received` (per inbound message).
- One-time **backfill** of `activity_entry` from existing `ticket` (created) + `ticket_message` (inbound→email_received, outbound→email_sent). Covers historical connector tickets too.
- **Merge completion:** `ContactService.Merge` re-points `activity_entry.contact_id` loser→winner (the Phase A TODO).
- API: `GET /businesses/{id}/contacts/{cid}/activity` (cursor-paged) + OpenAPI + drift test.
- UI: a timeline section on the contact-detail page + `CrmService.listActivity`.
- Security regression: timeline tenant isolation + cross-source ordering/attribution (regression contract #3).

**Out of scope (deferred follow-ups, filed as bd issues):**
- **Live recording for NEW connector (Jira/Zendesk) tickets** — `sync_inbound_external_issue` is being modified in parallel by `manyforge-edq`; touching it here would clobber that change. Historical connector tickets are covered by the backfill; live connector-source activity recording lands after edq merges. (bd follow-up.)
- `ticket_status_changed` originating from connector inbound *updates* (same reason).
- Timeline filtering/search, activity types beyond ticket/email (e.g. deal events), real-time push.

## Background (verified on `005-crm-phase-b`)

- **Recording sites + principal context:**
  - `ticket_created` (native) + `email_received`: inside the principal-less `ingest_inbound_message` SECURITY DEFINER (latest def — grep `migrations/` for the most recent `FUNCTION ingest_inbound_message`); the Go inbox caller (`internal/inbox/service.go`) gets back `out_ticket_id`, `out_message_id`, `out_created` from the ingest `QueryRow`. → record via a new DEFINER helper called post-ingest in the same tx.
  - `ticket_status_changed`: `ticketing.Service.Triage` (`internal/ticketing/service.go`), inside `WithPrincipal`; already calls `writeStatusAudit(tx, ...)` — record alongside it. Has principalID (actor), businessID, tenantRootID, ticketID.
  - `email_sent`: `ticketing.Service.Reply` (`internal/ticketing/service.go`), inside `WithPrincipal`; inserts the outbound message + `audit.Write("ticket.replied")` + enqueues the send to the outbox (actual send is async in a worker). **Record `email_sent` at Reply time** (enqueue time) — consistent with how the audit row is written there; the worker only delivers.
  - `note_added`: `AddNote` (principal-scoped) — optional `note_added` kind.
- **contact_id resolution:** `ticket.requester_id → requester.contact_id` (Phase A populated the FK + backfill). dbgen has `GetRequesterForTicket`. For the DEFINER recorder, resolve inline: `SELECT contact_id FROM requester WHERE id = ticket.requester_id`.
- **audit pattern:** `audit.Write(ctx, tx, audit.Entry)` (`internal/platform/audit/audit.go`). `ActivityService.Record` mirrors it exactly (in-tx, no async).
- **Backfill sources:** `ticket(id, business_id, tenant_root_id, requester_id, status, created_at, ...)`, `ticket_message(id, ticket_id, business_id, tenant_root_id, direction in/out, author_principal_id, created_at, ...)`.
- **UI:** `web/src/app/pages/crm/contact-detail.ts` (signals + `ngOnInit` + `reload()`); `web/src/app/core/crm.service.ts` (add `listActivity`).
- **Principal-less RLS constraint (Phase A learning):** writes to the tenant-wide RLS `activity_entry` from the inbox path require a SECURITY DEFINER function (search-path pinned), exactly like `crm_link_inbound_sender` (migration 0059). Principal-scoped sources write directly under `WithPrincipal`.

## Data model

```sql
CREATE TABLE activity_entry (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_root_id uuid NOT NULL,
    business_id    uuid NOT NULL,
    contact_id     uuid NOT NULL,
    kind           text NOT NULL,        -- ticket_created | ticket_status_changed | email_received | email_sent | note_added
    occurred_at    timestamptz NOT NULL,
    actor          text,                 -- principal id / 'system' / requester email, for attribution
    source_type    text NOT NULL,        -- ticket | ticket_message | note
    source_id      uuid,
    summary        text NOT NULL,
    metadata       jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (contact_id, tenant_root_id) REFERENCES contact (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
CREATE INDEX activity_contact_time_idx  ON activity_entry (contact_id, occurred_at DESC, id DESC);
CREATE INDEX activity_business_time_idx ON activity_entry (business_id, occurred_at DESC);
-- tenant-wide RLS, mirroring contact/company (migration 0057):
ALTER TABLE activity_entry ENABLE ROW LEVEL SECURITY;
CREATE POLICY activity_entry_rls ON activity_entry FOR ALL
  USING (tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())))
  WITH CHECK (tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())));
-- + GRANT to the app role (match 0057); no tenant_root_id immutability trigger needed (append-only, but add one for consistency with 0057).
```

- `kind` is open text (validated at the service boundary) — no DB enum (avoids the lockstep churn).
- Idempotent backfill dedupes on `(source_type, source_id, kind)` — add a partial unique index or `ON CONFLICT DO NOTHING` arbiter for backfill safety: `CREATE UNIQUE INDEX activity_dedup_idx ON activity_entry (tenant_root_id, source_type, source_id, kind) WHERE source_id IS NOT NULL;`
- The cursor sorts by `(occurred_at DESC, id DESC)` — row-value keyset, like the Phase A list cursors.

## Services (`internal/crm`)

- `ActivityEntry` domain struct + `ActivityInput` (the fields a recorder supplies: BusinessID, ContactID, Kind, OccurredAt, Actor, SourceType, SourceID, Summary, Metadata).
- `ActivityService{DB *db.DB}`:
  - `Record(ctx context.Context, tx pgx.Tx, in ActivityInput) error` — in-tx insert via dbgen, `ON CONFLICT (tenant_root_id, source_type, source_id, kind) WHERE source_id IS NOT NULL DO NOTHING` (idempotent). Mirrors `audit.Write` (takes a tx, no WithPrincipal). Caller supplies a trusted tenantRootID/contactID.
  - `ListForContact(ctx, principalID, businessID, contactID uuid.UUID, cursor string, limit int) (Page[ActivityEntry], error)` — `WithPrincipal` + resolveTenantRoot(businessID) + `tenant_root_id`+`contact_id` scoped query; reuse the Phase A cursor helper with a new `cursorActivity` kind on `(occurred_at, id)` DESC.
- sqlc queries in `db/query/crm.sql`: `InsertActivityEntry`, `ListActivityForContact` (first page) + `ListActivityForContactAfter` (keyset), and a backfill is migration-SQL (not sqlc). Re-point on merge: `RepointActivityEntries` (UPDATE activity_entry SET contact_id=winner WHERE contact_id=loser AND tenant_root_id=$).

## Recording hooks

- **Principal-scoped (direct):** in `Triage` (status change → `ticket_status_changed`), `Reply` (→ `email_sent`), `AddNote` (→ `note_added`), and the new→open transition. Each, inside the existing `WithPrincipal` tx, resolves the ticket's `contact_id` (from the loaded ticket's requester) and calls `activitySvc.Record(ctx, tx, ...)` right after its `audit.Write`. Wire `ActivityService` into `ticketing.Service` (new field) in `main.go`.
- **Native inbox (DEFINER):** new migration adds `crm_record_inbound_activity(p_tenant_root_id, p_business_id, p_ticket_id, p_message_id, p_created boolean, p_occurred_at timestamptz)` SECURITY DEFINER (search-path pinned, GRANT to app role) that resolves `contact_id` from the ticket's requester and inserts a `ticket_created` row (when `p_created`) + an `email_received` row (for the message), both `ON CONFLICT DO NOTHING`. Called from `internal/inbox/service.go` after the ingest `QueryRow`, in the same tx, passing `out_ticket_id`/`out_message_id`/`out_created` (skip when ticket_id is null / suppressed).
- **Merge:** add `q.RepointActivityEntries(winner, loser, tenantRoot)` to `ContactService.Merge`'s tx (remove the Phase A `TODO(phase-b)`), + extend the merge regression test to assert activity rows re-point.

## Backfill

Migration `00NN_crm_activity_backfill.up.sql` (idempotent): insert `ticket_created` from every `ticket` (occurred_at = created_at, contact via requester), and `email_received`/`email_sent` from every `ticket_message` (direction in/out; actor = author_principal_id for outbound). Skip tickets whose requester has no `contact_id` (shouldn't happen post-Phase-A backfill, but guard). Dedup via the `activity_dedup_idx` / `ON CONFLICT DO NOTHING`. Re-runnable. `.down.sql` no-op.

## API

- `GET /api/v1/businesses/{id}/contacts/{cid}/activity?cursor=&limit=` → `{ items: [ActivityEntry], next_cursor }`, gated `crm.read`, RLS + tenant-predicate scoped. Add to the crm Handler `ReadRoutes`. Document in `specs/005-crm-contacts-timeline/contracts/openapi.yaml` (+ the drift_005 contract test will enforce it).

## UI

- `CrmService.listActivity(businessId, contactId, cursor?) → Observable<Page<ActivityEntry>>`.
- On `contact-detail.ts`: a **Timeline** section below the edit/merge blocks — load on init (`listActivity`), render a chronological list (`data-testid="activity-timeline"`, rows `activity-row` with kind icon/label + summary + relative time), empty state (`activity-empty`), "load more" via cursor. Vitest spec + extend the Playwright e2e (`web/e2e/crm.spec.ts`) to assert the timeline renders.

## Test plan

- **Unit/integration (Go):** ActivityService.Record idempotency (ON CONFLICT) + ListForContact ordering (occurred_at DESC, cursor disjoint); recording hooks fire in-tx (Triage→ticket_status_changed, Reply→email_sent, inbox→ticket_created+email_received) via integration tests; Merge re-points activity; backfill idempotency + counts.
- **Security-regression (`make sec-test`):** timeline tenant isolation (principal A can't list tenant B's contact activity — same matrix as Phase A, incl. foreign-business-URL); cross-source ordering/attribution (regression contract #3: a contact with a ticket_created + inbound + outbound + status_change shows them in correct occurred_at order with correct actor); source pins on the activity RLS policy + the DEFINER search_path.
- **Contract:** `go test -tags contract ./cmd/...` (new activity path documented).
- **Frontend:** Vitest for listActivity + the timeline section; Playwright e2e timeline render; real-browser pass (timeline shows backfilled events for a real contact).
- **Gates:** build, `make test`, `make sec-test`, `make lint`, contract, web build+test+e2e, dbgen minimal (sqlc v1.27.0).

## Phasing

One PR (slice is cohesive). Task order: (1) migration (table+RLS+indexes+dedup) → (2) sqlc → (3) ActivityService.Record+List+cursor → (4) principal-scoped hooks (Triage/Reply/AddNote) → (5) inbox DEFINER recorder + Go wiring → (6) Merge re-point + test → (7) backfill migration → (8) API + OpenAPI → (9) security regression → (10) UI service+timeline+specs → (11) e2e + real-browser. Each task: TDD + two-stage review (subagent-driven-development), as in Phase A.

## Pointers / carry-over

- Principal-less RLS → DEFINER recorder (see `[[crm-inbox-principal-less-constraint]]` bd memory; `crm_link_inbound_sender` migration 0059 is the template).
- sqlc v1.27.0 bottle (`/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate`); functions not mirrored in schema.sql; tables are.
- Contract test + staticcheck are the easily-missed gates.
- Do NOT touch `sync_inbound_external_issue` (edq owns it on a parallel branch this slice).
