# US5 — Scoped, Isolated, Audited Support — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. TDD is mandatory (project gate): write the failing test, watch it fail, implement, watch it pass, commit.

**Goal:** Prove and complete the support desk's isolation/permission/audit guarantees (SC-004/005/006/009, FR-014/015/016/017) and add `tickets.delete` soft-delete/redact — closing `specs/002-support-desk/tasks.md` T062–T067.

**Architecture:** Five of the six tasks are *verification* tasks that pin behavior already built across US1–US4 (RLS on 7 tables, the 6-permission middleware, in-tx audit on every mutation, no-oracle 404 collapse) with new behavioral/integration tests + fast source pins. The one *feature* task (T066) is `tickets.delete` as a **redact-in-place** under the caller's RLS context: blank the ticket's own PII (subject) + its messages' bodies + attachment filenames, set `ticket.redacted_at`, schedule attachment-blob purge via the transactional outbox, audit in-tx — never a hard `DELETE` (Principle VI / FR-014), and never the shared `requester` row (that is the account/requester-erasure path, out of scope here).

**Tech Stack:** Go (`internal/ticketing`, `internal/inbox`, `internal/security_regression`), pgx/v5 + RLS, sqlc (`db/query/*.sql` → `make generate`), testcontainers integration tests (`-tags integration`, `make int-test`, `-p 1`), source pins in `internal/security_regression` (run in `make test` + `make sec-test`), golang-migrate.

**Pre-flight (current state — verified by recon, do NOT re-discover):**
- 7 RLS tables (migration 0014, identical policy): `ticket, ticket_message, requester, ticket_tag, attachment, email_domain, inbound_address` — `USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))`.
- 6 permissions (migration 0015): `tickets.read/reply/write/assign/delete`, `inbox.manage`; presets in `data-model.md` (delete = owner/admin only). All routes gated by `httpx.RequirePermission(...)` in `cmd/manyforge/main.go`; `WriteError` maps `ErrNotFound`+`ErrForbidden`→404 (no oracle).
- Audit: EVERY existing mutation writes an in-tx `audit_entry` (reply `ticket.replied`, note `ticket.noted`, triage `ticket.status_changed/priority_changed/tags_changed/assigned`, identity `email_domain.created/verified`, `inbound_address.created`, ingest `ticket.created`/`ticket.message.received` + reopen `ticket.status_changed` in the migration-0024 DEFINER, loop `ticket.loop_suppressed`). The ONLY unaudited/unbuilt mutation is redact (T066).
- `redacted_at IS NULL` is already filtered in `ListTickets/ListTicketsAfter/GetTicket` (db/query/ticketing.sql) and the ingest DEFINER; it is NOT yet filtered in `ListMessages` (T066 must add it).
- Test harness: `internal/security_regression/rls_matrix_test.go` (001-table RLS matrix pattern), `internal/ticketing/read_integration_test.go` (`seedReadTenant`/`seedTicket`/`newReadService`/`startReadDB`), `internal/platform/db/testdb` (`Start`, `Super`, `App`, `AppDSN`). `mustRead(t, path)` helper in `security_regression`.

---

## File Structure

| File | Create/Modify | Responsibility |
|---|---|---|
| `internal/security_regression/support_isolation_test.go` | Create | T062: behavioral RLS matrix over the 7 tables (read+write deny for absent/malformed/sideways/cross-root principal) |
| `internal/ticketing/permissions_integration_test.go` | Create | T063: six-permission enforcement matrix (each grants its set, denies the rest; 404 not 403) |
| `internal/ticketing/audit_integration_test.go` | Create | T064: consolidated audit matrix — one assertion per mutation kind |
| `internal/security_regression/audit_source_pin_test.go` | Create | T065: pin the principal-less ingest source label + that every mutation audits |
| `db/query/ticketing.sql` | Modify | T066: `RedactTicket` (+ blank messages), add `redacted_at` filter to `ListMessages` |
| `migrations/0025_attachment_purge.up.sql` / `.down.sql` | Create | T066: `attachment.purge` is handled in Go (no schema), but if a DEFINER is needed for cross-message blanking add it here; otherwise skip |
| `internal/ticketing/redact.go` | Create | T066: `Service.RedactTicket` (redact-in-place under WithPrincipal, audit, enqueue purge) |
| `internal/ticketing/handler.go` | Modify | T066: `DELETE /businesses/{id}/tickets/{tid}` handler + `DeleteRoutes` group |
| `internal/platform/blob/blob.go` | Modify (if absent) | T066: `Store.Delete(ctx, key)` for the purge consumer |
| `internal/ticketing/attachment_purge_subscriber.go` | Create | T066: outbox consumer for `attachment.purge` → `blob.Delete` |
| `cmd/manyforge/main.go` | Modify | T066: `ticketsDelete` middleware + mount `DeleteRoutes`; subscribe the purge consumer |
| `specs/002-support-desk/contracts/openapi.yaml` + `cmd/manyforge/drift_002_test.go` | Modify | T066: document `DELETE .../tickets/{tid}` + drift in-scope |
| `internal/ticketing/redact_integration_test.go` | Create | T066: redact behavior (excluded from list/get→404, bodies blanked, purge enqueued, audited) |
| `internal/security_regression/redact_pin_test.go` | Create | T066: pin redact = soft (no hard DELETE), audited, list/get exclude |
| `specs/002-support-desk/tasks.md` | Modify | Check off T062–T067 as each lands |

**Order:** T062 → T063 → T064 → T065 → T067 → T066. Do the verification tasks first (fast, high-confidence, they harden what exists); do T066 (the feature) last so its redact mutation can be folded into the T064 audit matrix and T062/T067 exclusion assertions as a final pass. File one bd issue for the whole US5 epic-child before starting; commit per task.

---

## Task T062: RLS matrix over the 7 support tables (SC-004/SC-006)

**Files:**
- Create: `internal/security_regression/support_isolation_test.go`
- Reference pattern: `internal/security_regression/rls_matrix_test.go`, `internal/security_regression/isolation_test.go`

- [ ] **Step 1: Read the 001 RLS matrix test to match house style**

Run: `sed -n '1,80p' internal/security_regression/rls_matrix_test.go` — copy its two-tenant seed + `WithPrincipal` deny pattern (how it sets nil / foreign-owner / unknown principal and asserts the app pool sees 0 rows while `Super` sees the row).

- [ ] **Step 2: Write the failing test (read + write isolation across all 7 tables)**

Seed two independent master tenants (t1, t2) each with one row in every 7 table (reuse direct `Super.Exec` inserts as `seedReadTenant`/`seedTicket` do). For each "attacker" principal context — `uuid.Nil` (absent), a random uuid (unknown), t2's owner (sideways/cross-root) — open `tdb.App.WithPrincipal(ctx, attacker, ...)` and assert:
  - READ: `SELECT count(*) FROM <table> WHERE id = <t1 row id>` returns 0 (RLS denies).
  - WRITE: `UPDATE <table> SET ... WHERE id = <t1 row id>` reports 0 rows affected; `DELETE ...` 0 rows.
  - Control: `tdb.Super` (RLS-exempt) DOES see the row (proves the row exists — so 0 under RLS is denial, not absence).

Table the 7 tables with a per-table (id, a harmless UPDATE column) so the loop is DRY. Build tag `//go:build integration`, package `security_regression`.

- [ ] **Step 3: Run it — expect FAIL only if a table is unprotected**

Run: `go test -tags integration -p 1 -run TestSupportTablesRLSMatrix ./internal/security_regression/ -v`
Expected: PASS if all 7 policies are intact (this test *characterizes* the wall). To prove it bites: temporarily `ALTER TABLE ticket DISABLE ROW LEVEL SECURITY` in a scratch psql against a throwaway container — out of band — confirm the ticket case fails; do NOT commit that.

- [ ] **Step 4: Commit**

```bash
git add internal/security_regression/support_isolation_test.go
git commit -m "test(002): T062 — RLS read+write isolation matrix across the 7 support tables (SC-004/SC-006)"
```

---

## Task T063: six-permission enforcement matrix (SC-009)

**Files:**
- Create: `internal/ticketing/permissions_integration_test.go`
- Reference: `internal/ticketing/read_integration_test.go` (`seedReadTenant` already seeds owner/member/noReader + preset roles), `internal/authz/resolver.go`

- [ ] **Step 1: Write the failing test (grant-exactly-its-set matrix)**

For each of the 6 permissions, seed a principal whose custom role grants ONLY that permission (insert a `role` + `role_permission` rows via `Super`, then a membership). Drive each action through `authz.Resolve(ctx, tx, pid, bid)` inside `WithPrincipal` and assert `perms.Has(<that perm>)` is true and `Has(<each other perm>)` is false. Then assert the preset matrix from `data-model.md`: owner→all 6, admin→all 6, member→read/reply/write/assign (not delete/inbox.manage), viewer→read only.

- [ ] **Step 2: Add the human-vs-agent uniformity pin**

`authz.Resolve` is principal-kind-agnostic (it keys on membership, not kind). Add to the same file a sub-test (or a `security_regression` source pin) asserting `internal/authz/resolver.go` resolves by membership with no `kind` branch — quote a fragment (`HasOwnerRole`/`EffectivePermissions`) so a future kind-gate is caught. (Agent *memberships* are spec-003; this pins that the permission layer will treat them uniformly — FR-016.)

- [ ] **Step 3: Run — expect PASS (characterizes the matrix); prove it bites by flipping one expected grant in the test to the wrong value, see red, revert**

Run: `go test -tags integration -p 1 -run TestSupportPermissionMatrix ./internal/ticketing/ -v`

- [ ] **Step 4: Commit**

```bash
git add internal/ticketing/permissions_integration_test.go
git commit -m "test(002): T063 — six-permission enforcement matrix, human+agent-uniform (SC-009)"
```

---

## Task T064: consolidated audit matrix (SC-005/FR-014)

**Files:**
- Create: `internal/ticketing/audit_integration_test.go`
- Reference: existing per-mutation audit assertions in `reply_integration_test.go`, `note_integration_test.go`, `triage_integration_test.go`, `identity_integration_test.go`, `inbox/reopen_integration_test.go`

- [ ] **Step 1: Write the failing test (one row per mutation kind)**

Drive each mutation through its service method (Reply, AddNote, Triage status/priority/tags/assignee, IdentityService CreateEmailDomain/VerifyEmailDomain/CreateInboundAddress, inbox Ingest new+append+reopen) and after each assert exactly one new `audit_entry` (via `Super`) with the expected `action`, `target_type`, `actor_principal_id` (NULL for ingest, the caller for the rest), and that `old_value`/`new_value` are populated where applicable. This is the SC-005 "100% of mutations audited" proof in one place.

- [ ] **Step 2: Run — expect PASS for all built mutations**

Run: `go test -tags integration -p 1 -run TestSupportAuditMatrix ./internal/ticketing/ -v` (and the inbox portion under `./internal/inbox/` if split). Redact (T066) is added to this matrix in T066's final step.

- [ ] **Step 3: Commit**

```bash
git add internal/ticketing/audit_integration_test.go
git commit -m "test(002): T064 — consolidated audit matrix for every support mutation (SC-005)"
```

---

## Task T065: principal-less ingest source label + audit-sweep pin (FR-014)

**Files:**
- Create: `internal/security_regression/audit_source_pin_test.go`

- [ ] **Step 1: Write the source-label + sweep pin**

`mustRead` the migration-0024 DEFINER and assert it records the ingest source (`'source', p_source` in `inputs`, `actor_principal_id NULL`). Also pin (source-level) that each mutation service method calls `audit.Write`/the DEFINER audit — assert `internal/ticketing/service.go` contains `audit.Write` for each action string (`ticket.replied`, `ticket.noted`, `ticket.status_changed`, `ticket.priority_changed`, `ticket.tags_changed`, `ticket.assigned`) and `identity.go` for the three identity actions. This makes a dropped audit fail `make test` loudly even without Docker (the project's pin rule). Run it; prove it bites by renaming one expected action fragment, see red, revert.

- [ ] **Step 2: Commit**

```bash
git add internal/security_regression/audit_source_pin_test.go
git commit -m "test(002): T065 — pin ingest source label + every-mutation audit-write (FR-014)"
```

---

## Task T067: no-oracle handler audit (SC-006/FR-015)

**Files:**
- Create/extend: a behavioral test in `internal/ticketing/permissions_integration_test.go` (or a new `oracle_integration_test.go`) + extend `internal/security_regression/support_isolation_pin_test.go`

- [ ] **Step 1: Write the failing behavioral no-oracle test**

Through the full HTTP router (build it like `cmd/manyforge` does, or call handlers with a chi context), issue three GETs for a ticket id: (a) unknown id (does not exist), (b) cross-tenant id (exists in t2), (c) id in a business where the caller lacks `tickets.read`. Assert all three return byte-identical 404 bodies (`{"code":"NOT_FOUND","message":"not found"}`) and the same status. (Timing equality is covered by the RLS design; assert response equality.)

- [ ] **Step 2: Extend the source pin**

Add fragments to `support_isolation_pin_test.go` asserting no handler references `StatusForbidden`/`http.StatusForbidden`/`403` in `internal/ticketing/*.go` + `internal/inbox/*.go` (grep-style: read each file, assert `!strings.Contains(src, "StatusForbidden")`). This locks the 404-collapse.

- [ ] **Step 3: Run + Commit**

Run: `go test -tags integration -p 1 -run 'TestNoOracle' ./internal/ticketing/` and `go test ./internal/security_regression/ -run TestOwnershipPredicatesPinned`.
```bash
git add internal/ticketing/oracle_integration_test.go internal/security_regression/support_isolation_pin_test.go
git commit -m "test(002): T067 — no-oracle 404 parity across handlers (SC-006); pin no-403 collapse"
```

---

## Task T066: `tickets.delete` soft-delete / redact (FR-014, research R7)

**Design:** A `tickets.delete`-holder redacts a ticket in-place under their RLS context (a principal IS present, unlike ingest, so NO SECURITY DEFINER is needed — the existing triage/reply paths already write audit + mutate under `WithPrincipal`). One tx: ownership-scoped `UPDATE ticket SET redacted_at = now(), subject = '' WHERE id=$1 AND business_id=$2 AND redacted_at IS NULL`; blank message bodies for that ticket; blank attachment filenames + enqueue `attachment.purge` per blob to the outbox; write `ticket.redacted` audit with `old_value` carrying the pre-redaction subject hash/snapshot id. **Do NOT touch the shared `requester` row** (deduped across tickets; requester/account erasure is the 001 path, out of scope). Idempotent: a re-redact (already `redacted_at IS NOT NULL`) updates 0 rows → `ErrNotFound` to the caller (already-gone, no oracle).

**Files:** `db/query/ticketing.sql`, `internal/ticketing/redact.go`, `internal/ticketing/handler.go`, `internal/platform/blob/blob.go` (if `Delete` absent), `internal/ticketing/attachment_purge_subscriber.go`, `cmd/manyforge/main.go`, `specs/002-support-desk/contracts/openapi.yaml`, `cmd/manyforge/drift_002_test.go`, tests.

- [ ] **Step 1: Read the dependencies, quote them**

Run: `sed -n '1,60p' migrations/0012_account_erasure.up.sql` (the 001 erasure pattern to mirror for blanking), `grep -n "func.*Store\|Delete\|Put\|interface" internal/platform/blob/blob.go` (confirm whether `Store.Delete(ctx, key)` exists), and `grep -n "Subscribe\|SendSubscriber" cmd/manyforge/main.go internal/platform/notify/sender_subscriber.go` (the outbox-consumer wiring + subscriber shape to mirror). Record the exact `blob.Store` interface + how `notify.SendSubscriber` is registered.

- [ ] **Step 2: RED — redact integration test**

Create `internal/ticketing/redact_integration_test.go`: seed a ticket with 2 messages + 1 attachment, call `svc.RedactTicket(ctx, ownerWithDelete, biz, ticketID)`, then assert: `GetTicket`→`ErrNotFound`; `ListTickets` omits it; `Super` shows `redacted_at IS NOT NULL`, `subject=''`, both message bodies blank, attachment filename blank; exactly one `audit_entry` `action='ticket.redacted'`; one `outbox` row `topic='attachment.purge'` per attachment blob; a second `RedactTicket` returns `ErrNotFound` (idempotent). Run → FAIL (method absent).

- [ ] **Step 3: GREEN — query + service**

Add to `db/query/ticketing.sql`: `RedactTicket :execrows` (the `UPDATE ticket SET redacted_at=now(), subject='' WHERE id=$1 AND business_id=$2 AND redacted_at IS NULL`), `BlankTicketMessages :exec`, `ListTicketAttachmentBlobs :many` (blob keys for the ticket), `BlankTicketAttachments :exec`, and add `AND <ticket not redacted>` to `ListMessages`. `make generate`. Implement `internal/ticketing/redact.go` `Service.RedactTicket` under `WithPrincipal`: run the updates (RedactTicket rows-affected 0 → `errs.ErrNotFound`), `audit.Write{Action:"ticket.redacted", OldValue:{subject…}}`, `events.Enqueue(tx, tenantRoot, "attachment.purge", {blob_key})` per blob. Run the test → GREEN.

- [ ] **Step 4: GREEN — purge consumer + blob.Delete**

Add `Store.Delete(ctx, key)` to `internal/platform/blob` if absent (file:// + s3:// impls). Create `internal/ticketing/attachment_purge_subscriber.go`: `Handle(ctx, tx, e)` decodes `{blob_key}` and calls `blob.Delete` (idempotent — missing key is success). Add a unit/integration test for the subscriber. Wire `eventBus.Subscribe("attachment.purge", purgeSub.Handle)` in `cmd/manyforge/main.go`.

- [ ] **Step 5: GREEN — HTTP surface**

`handler.go`: `DeleteRoutes(r)` mounts `r.Delete("/businesses/{id}/tickets/{tid}", h.deleteTicket)`; `deleteTicket` calls `svc.RedactTicket`, returns 204. `main.go`: add `ticketsDelete := httpx.RequirePermission(database, permResolve, "tickets.delete", businessIDFromPath)` (struct field + literal + mount group). `openapi.yaml`: document `delete:` on `/businesses/{id}/tickets/{tid}` (204/404, requires tickets.delete). `drift_002_test.go`: add `"DELETE /businesses/{}/tickets/{}"` to `inScope002Ops` and `ticketsDelete: noop` to the test `apiHandlers`. Run `make contract-test`.

- [ ] **Step 6: Fold redact into the T064 audit matrix + T062/T067 exclusion**

Add a `ticket.redacted` case to `audit_integration_test.go`; add to T062/T067 that a redacted ticket is 404 to a `tickets.read` caller (excluded from list/get). Create `internal/security_regression/redact_pin_test.go`: pin that `RedactTicket` SQL is an `UPDATE` (never `DELETE FROM ticket`), the handler is gated `tickets.delete`, and list/get filter `redacted_at IS NULL`.

- [ ] **Step 7: Full gate + commit**

Run: `make test`, `~/go/bin/golangci-lint run ./...`, `make int-test`, `make contract-test`.
```bash
git add -A
git commit -m "feat(002): T066 — tickets.delete soft-delete/redact (in-place, audited, attachment-purge via outbox; FR-014/R7)"
```

---

## Final: close out

- [ ] Check off T062–T067 in `specs/002-support-desk/tasks.md`.
- [ ] Run the full gate (`make test` + `make contract-test` + `make int-test` + `~/go/bin/golangci-lint run ./...` + web build/test/e2e if touched — US5 is backend-only).
- [ ] `bd close` the US5 issue; consider whether to advance `master` to the US5 HEAD per the milestone convention.
- [ ] `git pull --rebase && git push && git status` (must show up to date).

## Test plan summary (what proves US5)

- **T062** integration: 7 tables × {absent, unknown, sideways} principal × {read, write} all denied; Super control proves rows exist.
- **T063** integration: each of 6 perms grants exactly its set; preset matrix (owner/admin/member/viewer) holds; authz is kind-agnostic (pin).
- **T064** integration: one audit row per mutation kind, correct action/actor/before-after.
- **T065** pin: ingest source label + every-mutation audit-write (fast CI).
- **T067** integration + pin: unknown/cross-tenant/forbidden GET → byte-identical 404; no `StatusForbidden` in handlers.
- **T066** integration + pin: redact excludes from list/get (404), blanks PII, enqueues blob purge, audits `ticket.redacted`, idempotent, never hard-DELETEs.

## Open decisions captured (resolve at execution if needed)

1. **Requester redaction scope:** this plan does NOT blank the shared `requester` on per-ticket redact (it is deduped across tickets). If product wants requester PII erased when their LAST ticket is redacted, that is a separate reference-counted path — file a follow-up rather than expanding T066.
2. **Redact a SECURITY DEFINER vs service-under-WithPrincipal:** plan uses the latter (a principal is present; mirrors triage). If cross-table blanking hits an RLS edge (e.g. attachment policy), fall back to a small DEFINER `redact_ticket(...)` in migration 0025 — Step 1's read confirms which.
