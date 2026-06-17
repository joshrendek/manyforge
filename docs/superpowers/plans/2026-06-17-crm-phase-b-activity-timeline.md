# CRM Phase B — Activity Timeline — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development — implement task-by-task, fresh subagent per task, two-stage (spec then quality) review. Steps use `- [ ]`.

**Goal:** A durable per-contact activity timeline auto-populated from ticket/email events (+ one-time backfill), shown on the contact-detail page.

**Architecture:** New `activity_entry` tenant-wide table (RLS like Phase A's contact/company). `ActivityService.Record(tx, entry)` mirrors `audit.Write` — called inline in principal-scoped `WithPrincipal` txs (Triage/Reply/AddNote) and via a new SECURITY DEFINER helper (`crm_record_inbound_activity`) for the principal-less inbox path. Merge re-points activity. Backfill from `ticket`/`ticket_message`. Branch `005-crm-phase-b` (off master, which has Phase A).

**Tech Stack:** Go (pgx/v5, sqlc v1.27.0 bottle, chi), PostgreSQL (RLS, SECURITY DEFINER), Angular (signals), Vitest, Playwright.

## Design reference
`docs/superpowers/specs/2026-06-17-crm-activity-timeline-phase-b-design.md`. Read it first.

## Conventions (every task)
- `export PATH="$HOME/go/bin:$PATH"`. Dev DB DSN: `postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable` (already at Phase A migration 0060).
- sqlc regen ONLY with `/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate`; verify `git diff --stat internal/platform/db/dbgen/` minimal. Tables mirrored into `db/schema.sql`; functions are NOT.
- Reuse Phase A `internal/crm` helpers: `mapErr`, `resolveTenantRoot`, `clampLimit`, `trim`, `ptr`, `pgUUIDPtr`, and the generic cursor in `cursor.go` (add a `cursorActivity="a"` kind).
- Errors via `errs` sentinels; no-oracle 404; commits one-per-task, NO Co-Authored-By.
- **Do NOT modify `sync_inbound_external_issue`** (the connector ticket-create function) — `manyforge-edq` owns it on a parallel branch this slice. Live connector-source recording is a deferred follow-up; backfill covers historical connector tickets.
- Gates before PR: `go build`, `make test`, `make sec-test`, `make lint`, `go test -tags contract ./cmd/...`, web `npm run build && npm test && npm run e2e -- e2e/crm.spec.ts`.

---

## Task 1: migration — activity_entry table, RLS, indexes, dedup

**Files:** `migrations/0061_crm_activity_entry.up.sql` / `.down.sql`; mirror table+indexes into `db/schema.sql`.

- [ ] Write the up migration (confirm next number is 0061 via `ls migrations/`): the `activity_entry` table from the design's Data model section (cols id, tenant_root_id, business_id, contact_id, kind, occurred_at, actor, source_type, source_id, summary, metadata jsonb, created_at; `UNIQUE(id, tenant_root_id)`; composite FKs to `contact` and `business`). Indexes `activity_contact_time_idx (contact_id, occurred_at DESC, id DESC)`, `activity_business_time_idx (business_id, occurred_at DESC)`, and the dedup partial unique `activity_dedup_idx (tenant_root_id, source_type, source_id, kind) WHERE source_id IS NOT NULL`. RLS: `ENABLE ROW LEVEL SECURITY` + `activity_entry_rls` USING+WITH CHECK `tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal()))` (copy EXACTLY from migration 0057's contact_rls). `GRANT SELECT,INSERT,UPDATE,DELETE ON activity_entry TO manyforge_app` (match 0057). Add `activity_troot_immutable` BEFORE UPDATE trigger reusing `support_tenant_root_immutable()` (match 0057).
- [ ] down: drop trigger, policy, table.
- [ ] Mirror the table + 3 indexes into `db/schema.sql` (tables-only convention; bump the sync-range header).
- [ ] Verify: `migrate up` then `down 1` then `up` (round-trip; leave at 0061); `psql "$DSN" -c '\d activity_entry'` shows FKs + indexes + RLS + trigger; `/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate` produces no churn (schema parses).
- [ ] Commit: `feat(crm): migration 0061 — activity_entry table, RLS, indexes`.

## Task 2: sqlc queries for activity

**Files:** `db/query/crm.sql` (append); regen dbgen (bottle).

- [ ] Append queries (match Phase A style; tenant-scoped):
  - `InsertActivityEntry :exec` — `INSERT INTO activity_entry (id, tenant_root_id, business_id, contact_id, kind, occurred_at, actor, source_type, source_id, summary, metadata, created_at) VALUES ($1,...,now()) ON CONFLICT (tenant_root_id, source_type, source_id, kind) WHERE source_id IS NOT NULL DO NOTHING` (idempotent recorder).
  - `ListActivityForContact :many` — `WHERE tenant_root_id=$1 AND contact_id=$2 ORDER BY occurred_at DESC, id DESC LIMIT $3` (first page).
  - `ListActivityForContactAfter :many` — keyset: `... AND (occurred_at, id) < (sqlc.arg('cur_occurred')::timestamptz, sqlc.arg('cur_id')::uuid) ORDER BY occurred_at DESC, id DESC LIMIT sqlc.arg('lim')` (DESC → use `<`; copy ticketing's keyset direction).
  - `RepointActivityEntries :exec` — `UPDATE activity_entry SET contact_id=sqlc.arg('winner') WHERE contact_id=sqlc.arg('loser') AND tenant_root_id=sqlc.arg('tenant_root_id')`.
- [ ] Regen with the bottle; confirm minimal diff; `go build ./...`.
- [ ] Commit: `feat(crm): sqlc queries for activity_entry`.

## Task 3: ActivityService (Record + ListForContact) — TDD

**Files:** `internal/crm/activity.go`, `internal/crm/types.go` (+ActivityEntry/ActivityInput), `cursor.go` (+cursorActivity), `internal/crm/activity_integration_test.go`.

- [ ] types.go: `ActivityEntry{ID, TenantRootID, BusinessID, ContactID uuid.UUID; Kind string; OccurredAt time.Time; Actor *string; SourceType string; SourceID *uuid.UUID; Summary string; Metadata json.RawMessage; CreatedAt time.Time}` (+ JSON tags); `ActivityInput{BusinessID, ContactID uuid.UUID; Kind string; OccurredAt time.Time; Actor *string; SourceType string; SourceID *uuid.UUID; Summary string; Metadata []byte}`.
- [ ] cursor.go: add `cursorActivity="a"` + `encodeActivityCursor(occurredAt, id)` / `decodeActivityCursor` (the keyset key is the RFC3339Nano occurred_at; reuse the generic encode/decode — note it stores a string key + uuid, so format occurred_at as the key).
- [ ] activity.go: `ActivityService{DB *db.DB}`:
  - `Record(ctx, tx pgx.Tx, tenantRootID uuid.UUID, in ActivityInput) error` — principal-less, caller's tx (mirrors `audit.Write`); validate Kind/SourceType non-empty (ErrValidation); `dbgen.New(tx).InsertActivityEntry(...)` with `db.PGUUID(uuid.New())` id; map errors. Doc: caller supplies trusted tenantRootID + runs on an RLS-set or RLS-exempt tx.
  - `ListForContact(ctx, principalID, businessID, contactID uuid.UUID, cursor string, limit int) (Page[ActivityEntry], error)` — WithPrincipal → resolveTenantRoot(businessID) → first page or keyset → trim → NextCursor from last (occurred_at,id). Mirror Phase A ContactService.List.
- [ ] TDD (`//go:build integration`, reuse `seedCRMTenant` from `contact_integration_test.go`): `TestActivityRecordAndList` — seed tenant + a contact; Record 3 entries with different occurred_at; ListForContact returns them newest-first; cursor paginates disjoint. `TestActivityRecordIdempotent` — Record the same (source_type, source_id, kind) twice → one row. Run RED → implement → GREEN.
- [ ] Verify: `go test -tags integration ./internal/crm/ -run TestActivity -v`; `go build`; `go vet`.
- [ ] Commit: `feat(crm): ActivityService Record + ListForContact`.

## Task 4: principal-scoped recording hooks (Triage / Reply / AddNote) — TDD

**Files:** `internal/ticketing/service.go` (+ `Activity *crm.ActivityService` field, or a minimal recorder interface to avoid an import cycle — CHECK: does `crm` import `ticketing`? It must NOT; if it does, define a small `ActivityRecorder` interface in ticketing and pass crm's service); `cmd/manyforge/main.go` (wire it); `internal/ticketing/*_integration_test.go` (assert recording).

- [ ] **Import-cycle check first:** `grep -rn manyforge/internal/ticketing internal/crm/` — if crm imports ticketing, do NOT add a crm field to ticketing; instead declare `type ActivityRecorder interface { Record(ctx, tx, tenantRootID, ActivityInput) error }` in ticketing and pass the crm service (which satisfies it). Otherwise a direct `*crm.ActivityService` field is fine. (crm currently imports only db/dbgen/errs/audit — no ticketing — so a direct field should be safe; verify.)
- [ ] In `Triage` (status-change branch, right after `writeStatusAudit`): resolve the ticket's contact_id (the loaded ticket `tk` has requester_id; load `requester.contact_id` — add a tiny query `GetContactIDForTicket` or reuse GetRequesterForTicket; if contact_id is null, skip recording). Record `kind="ticket_status_changed"`, source_type="ticket", source_id=ticketID, actor=principalID.String(), occurred_at=now(), summary e.g. "status: old → new", metadata {old,new}. Same-tx; propagate errors.
- [ ] In `Reply` (after `audit.Write("ticket.replied")`): Record `kind="email_sent"`, source_type="ticket_message", source_id=msgID, actor=principalID, occurred_at=now(), summary "Replied". Same tx.
- [ ] In `AddNote` (optional but cheap): Record `kind="note_added"`, source_type="note". (If AddNote's target isn't contact-linked, skip.)
- [ ] Wire `ActivityService` into the ticketing Service construction in `main.go` (alongside the existing ticketSvc fields).
- [ ] TDD: extend/`add internal/ticketing/activity_recording_integration_test.go` — Triage a ticket whose requester has a contact → an `activity_entry(kind=ticket_status_changed, contact_id=...)` exists (query via Super); Reply → an `email_sent` row exists. RED → implement → GREEN. Don't regress existing ticketing tests.
- [ ] Verify: `go test -tags integration ./internal/ticketing/...`; build/vet.
- [ ] Commit: `feat(crm): record ticket status-change + reply activity (principal-scoped)`.

## Task 5: native-inbox activity recorder (SECURITY DEFINER) — TDD

**Files:** `migrations/0062_crm_inbound_activity.up.sql`/`.down.sql`; `internal/inbox/service.go`; `internal/inbox/*_integration_test.go`.

- [ ] up migration: `CREATE FUNCTION crm_record_inbound_activity(p_tenant_root_id uuid, p_business_id uuid, p_ticket_id uuid, p_message_id uuid, p_created boolean, p_occurred_at timestamptz) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$ DECLARE v_contact_id uuid; BEGIN SELECT r.contact_id INTO v_contact_id FROM ticket t JOIN requester r ON r.id = t.requester_id WHERE t.id = p_ticket_id; IF v_contact_id IS NULL THEN RETURN; END IF; IF p_created THEN INSERT INTO activity_entry (id, tenant_root_id, business_id, contact_id, kind, occurred_at, actor, source_type, source_id, summary, created_at) VALUES (gen_random_uuid(), p_tenant_root_id, p_business_id, v_contact_id, 'ticket_created', p_occurred_at, 'system', 'ticket', p_ticket_id, 'Ticket created from inbound email', now()) ON CONFLICT (tenant_root_id, source_type, source_id, kind) WHERE source_id IS NOT NULL DO NOTHING; END IF; IF p_message_id IS NOT NULL THEN INSERT INTO activity_entry (...) VALUES (..., 'email_received', p_occurred_at, 'system', 'ticket_message', p_message_id, 'Inbound email received', now()) ON CONFLICT ... DO NOTHING; END IF; END; $$;` + `REVOKE ALL ... FROM PUBLIC; GRANT EXECUTE ... TO manyforge_app;` (copy the prelude/grant from `crm_link_inbound_sender` migration 0059). down: `DROP FUNCTION IF EXISTS crm_record_inbound_activity(uuid,uuid,uuid,uuid,boolean,timestamptz);`. No schema.sql change (function).
- [ ] Go: in `internal/inbox/service.go`, after the ingest `QueryRow` returns `out_ticket_id/out_message_id/out_created` (and after `crm_link_inbound_sender`), in the SAME tx call `SELECT crm_record_inbound_activity($1,$2,$3,$4,$5,$6)` with `r.tenantRootID, r.businessID, scTicket (if valid), scMessage (if valid), out.Created, now()`. Skip when ticket id is null/suppressed. Propagate errors.
- [ ] TDD: `internal/inbox/activity_seam_integration_test.go` — ingest a new inbound email from a sender → assert `activity_entry` has a `ticket_created` (contact-linked) + an `email_received` row; a SECOND inbound on the same ticket (reply via reply-token) → adds another `email_received` but NOT a second `ticket_created`. RED → implement → GREEN; full inbox suite green.
- [ ] Verify: migrate round-trip; `go test -tags integration ./internal/inbox/...`.
- [ ] Commit: `feat(crm): record inbound ticket-created + email-received activity (DEFINER)`.

## Task 6: Merge re-points activity_entry — TDD

**Files:** `internal/crm/contact.go` (Merge); `internal/crm/merge_integration_test.go`.

- [ ] In `ContactService.Merge`'s tx, after `RepointRequesters` + before/after `SoftDeleteContact`, add `q.RepointActivityEntries(ctx, {Winner: winnerID, Loser: loserID, TenantRootID: trid})`. Remove the Phase A `TODO(phase-b)` comment.
- [ ] Extend `TestMergeRepointsRequestersAndSoftDeletesLoser` (or add `TestMergeRepointsActivity`): seed an activity_entry on the loser (via ActivityService.Record in a WithPrincipal tx, or Super insert), Merge, assert the row's contact_id == winner. RED (re-point query absent) → implement → GREEN.
- [ ] Verify: `go test -tags integration ./internal/crm/ -run TestMerge -v`.
- [ ] Commit: `feat(crm): merge re-points activity_entry to the winner`.

## Task 7: backfill activity from tickets + messages

**Files:** `migrations/0063_crm_activity_backfill.up.sql`/`.down.sql`.

- [ ] up (idempotent): insert `ticket_created` from every `ticket` (occurred_at=created_at, contact via `requester.contact_id`, actor='system', source_type='ticket', source_id=ticket.id, summary='Ticket created') WHERE the requester has a contact_id; `ON CONFLICT (tenant_root_id, source_type, source_id, kind) WHERE source_id IS NOT NULL DO NOTHING`. Then from `ticket_message`: direction='inbound' → `email_received` (actor='system'); direction='outbound' → `email_sent` (actor = author_principal_id::text). Join ticket→requester→contact for contact_id; skip null contact_id. down: no-op comment.
- [ ] Verify on dev DB: before/after counts (`SELECT kind, count(*) FROM activity_entry GROUP BY kind`); re-run the up SQL via psql → 0 new rows (idempotent); spot-check a known contact has its ticket_created + message events; leave DB migrated.
- [ ] Commit: `feat(crm): backfill activity timeline from tickets + messages`.

## Task 8: API — GET contact activity + OpenAPI

**Files:** `internal/crm/handler.go` (+ activity DTO + route); `cmd/manyforge/main.go` (handler needs ActivityService — add to crm.NewHandler); `specs/005-crm-contacts-timeline/contracts/openapi.yaml`.

- [ ] Add `h.contacts`... wait: add `activity *ActivityService` to the crm Handler + NewHandler; in `ReadRoutes` add `r.Get("/businesses/{id}/contacts/{cid}/activity", h.listActivity)`; handler reads pid+bid+cid, `?cursor=&?limit=`, calls `activity.ListForContact`, WriteJSON Page. DTO `activityResp` (snake_case, matching ActivityEntry). Wire ActivityService into NewHandler in main.go.
- [ ] OpenAPI: add the `/contacts/{cid}/activity` GET path + `ActivityEntry`/`ActivityList` schemas (match the DTO + Phase A style). Run `go test -tags contract ./cmd/...` → was failing (undocumented) → green after.
- [ ] Verify: `go build`, `make test`, contract green; smoke `curl` the route on :8081 (200 with backfilled data).
- [ ] Commit: `feat(crm): GET contact activity endpoint + openapi`.

## Task 9: security regression — timeline isolation + ordering/attribution

**Files:** `internal/security_regression/crm_activity_*_test.go`.

- [ ] Source pins: `activity_entry_rls` exists in 0061 with `authorized_tenants(current_principal())`; the DEFINER `crm_record_inbound_activity` (0062) has `SECURITY DEFINER` + `SET search_path`; `ListActivityForContact*` queries carry `tenant_root_id =`.
- [ ] Behavioral (`//go:build integration`): `TestActivityTenantIsolation` — principal A can't `ListForContact` tenant B's contact activity (ErrNotFound or empty incl. foreign-business-URL), mirroring the Phase A matrix. `TestActivityCrossSourceOrdering` (regression contract #3) — a contact with ticket_created + email_received + email_sent + status_changed at distinct occurred_at lists them in correct DESC order with correct actor/source attribution.
- [ ] Verify: `make sec-test` green.
- [ ] Commit: `test(sec): activity timeline isolation + cross-source ordering`.

## Task 10: UI — timeline on contact detail — TDD (Vitest)

**Files:** `web/src/app/core/crm.service.ts` (+ListActivity/ActivityEntry); `web/src/app/pages/crm/contact-detail.ts` (+timeline section); `contact-detail.spec.ts`.

- [ ] crm.service.ts: `ActivityEntry` interface + `listActivity(businessId, contactId, cursor?) : Observable<Page<ActivityEntry>>` → `GET .../contacts/{id}/activity`.
- [ ] contact-detail.ts: add an `activity` signal; on init call `listActivity`; render a **Timeline** section (`data-testid="activity-timeline"`, rows `activity-row` with kind label + summary + occurred_at, empty state `activity-empty`, optional "load more" via next_cursor). Mirror the existing load pattern.
- [ ] Spec: flush the activity GET with 2 entries; assert timeline rows render + empty state. Update any exact-match expectations. RED → implement → GREEN.
- [ ] Verify: `cd web && npx ng test --include 'src/app/pages/crm/contact-detail.spec.ts' --no-watch`; full `npm test`; `npm run build`.
- [ ] Commit: `feat(web): activity timeline on contact detail`.

## Task 11: e2e + real-browser

**Files:** `web/e2e/crm.spec.ts` (extend).

- [ ] Extend the contact-detail e2e: mock `**/contacts/c1/activity` → entries; assert `activity-timeline` + `activity-row` render. `npm run e2e -- e2e/crm.spec.ts` green.
- [ ] Real-browser pass (Playwright MCP / gstack) against the live app (:4300/:8081 with backfilled activity): open a contact detail, confirm the timeline shows backfilled ticket/email events in order, no console errors; screenshot.
- [ ] Commit: `test(web): CRM activity timeline e2e`.

## Final verification (before PR)
- [ ] `go build ./...`; `make test`; `go test -tags integration ./internal/crm/... ./internal/inbox/... ./internal/ticketing/... ./internal/security_regression/...`; `make sec-test`; `make lint`; `go test -tags contract ./cmd/...`; `git diff --stat internal/platform/db/dbgen/` minimal.
- [ ] web: `npm run build && npm test && npm run e2e -- e2e/crm.spec.ts`; real-browser pass done.
- [ ] Open PR into master (single Phase B PR). bd: note Phase B complete on `manyforge-nwr`; file the deferred follow-up (live connector-source activity recording, after edq).

## Spec-coverage self-check
activity_entry+RLS → T1; sqlc → T2; ActivityService → T3; principal-scoped recording → T4; inbox DEFINER recording → T5; merge re-point → T6; backfill → T7; API+OpenAPI → T8; sec-regression (isolation + ordering, contract #3) → T9; UI → T10; e2e+real-browser → T11. Deferred (noted): live connector-ticket recording (edq overlap).
