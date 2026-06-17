# Lite CRM — Contacts, Companies & Activity Timeline — Design

- **Date:** 2026-06-16
- **Status:** Approved (brainstorm) — pending implementation plan
- **bd epic:** `manyforge-nwr` (Spec 005 — Lite CRM + Activity Timeline)
- **Relates to:** Builds on Native Support Desk (Spec 002 — inbox + ticketing), Agent Runtime (Spec 003), External Connectors (Spec 004). New package `internal/crm`, mirroring `internal/ticketing`.

## Goal

Give operators a tenant-wide view of **who** they talk to (contacts), **where those people work** (companies), and **everything that has happened with a person** across sources (a durable activity timeline). This is the first, demoable slice of the larger Spec 005 CRM.

## Scope

**In scope (slice #1 + #2):**
- **Contacts** — tenant-wide people, deduped by email, with CRUD + dedup **merge**.
- **Companies** — auto-derived from a contact's email domain (with a free-email denylist) + manual override/CRUD.
- **Activity timeline** — a durable `activity_entry` table written **in-tx at each source mutation**, plus a one-time backfill from existing tickets/messages; a per-contact chronological timeline view.
- The `requester → contact` seam wired into the inbound-email path (resolve-or-create contact, set `requester.contact_id`, resolve-or-create company by domain).
- New `crm.read` / `crm.write` permissions, business-scoped API endpoints, OpenAPI documentation, Angular UI (Contacts list, Contact detail + timeline, Companies list/detail + nav), and the security-regression + contract tests.

**Out of scope (deferred to later Spec 005 slices — keep the first slice demoable):**
- **Deals / pipeline** (stages, value, forecasting).
- **AI enrichment / draft follow-ups** (the autonomy-gated 003 integration).
- **Outbound CRM connectors** (HubSpot / Salesforce sync, reusing SL-B).
- Custom fields, contact tags/segments, bulk import/export.

## Background (verified against `HEAD` = `3ccc7f4`)

- **The `requester.contact_id` seam already exists** as a nullable stub in BOTH `migrations/0013_support_desk.up.sql:74-92` and `db/schema.sql:190-203` (sqlc reads `schema.sql`). `requester` is `(id, business_id, tenant_root_id, email citext, display_name, contact_id uuid /* stub, no FK */)` with `UNIQUE(tenant_root_id, email)` and `UNIQUE(id, tenant_root_id)`. → contacts can reuse the existing tenant-wide email dedup; Phase A *promotes* `contact_id` to a real FK rather than restructuring.
- **In-tx audit pattern** is `audit.Write(ctx context.Context, tx pgx.Tx, e Entry) error` (`internal/platform/audit/audit.go:45-78`), inserting via `dbgen.New(tx).InsertAuditEntry(...)`. `ActivityService.Record(ctx, tx, entry)` mirrors this exactly — same-tx, no async outbox.
- **`audit_entry`** (`db/schema.sql:131-146`) and **`outbox`** (`db/schema.sql:277-286`) give the column-shape templates; `activity_entry` mirrors `audit_entry`'s tenant/business/jsonb shape.
- **Service/handler layering** (`internal/ticketing/{service,handler}.go`): a `Service` struct holds `DB *db.DB` (+ deps); methods take `(ctx, principalID, businessID, …)` and run inside `s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error { q := dbgen.New(tx); … })`. Handlers resolve permissions via `httpx.PermissionResolver` and map typed sentinels (`errs.ErrNotFound`, …) to HTTP via `httpx.WriteError` — foreign/unknown UUID both collapse to 404 (no oracle).
- **RLS** is enforced by `db.WithPrincipal(ctx, principalID, fn)` (`internal/platform/db/db.go:75-93`), which sets a **tx-local GUC** `manyforge.principal_id` via `set_config(..., true)`; RLS policies read that GUC to scope rows to the principal's authorized businesses.
- **citext** is already an installed extension (`migrations/0001_identity.up.sql:2`, `db/schema.sql:6`) and is the established type for email/domain columns.
- **Next migration number is `0057`** (latest is `0056_ai_provider_openrouter`). `db/schema.sql` is hand-edited alongside each migration (migrations are authoritative; `schema.sql` mirrors table shapes for sqlc).

## Locked decisions (approved)

1. **Contact = tenant-wide, deduped by email.** Matches the existing `requester UNIQUE(tenant_root_id, email)`. Access is **tenant-scoped**: visible to any principal who is a member of *any* business under that `tenant_root` — a deliberately wider grant than per-business membership. This needs a **tenant-wide RLS variant** (see "Access model"), hardened with security-regression tests; **tenant isolation is regression contract #1**.
2. **Timeline = durable `activity_entry` table**, written **in-tx at each source mutation** (the `audit.Write` pattern, *not* an async outbox), plus a one-time **backfill** from existing tickets / `ticket_message`. Chosen over read-time UNION for clean cross-source ordering/attribution, extensibility, and roadmap "SL-C hardened"; the cost is a one-time backfill.
3. **Companies = auto-by-email-domain** (free-email denylist: gmail/outlook/yahoo/icloud/proton/…) **+ manual override** (rename, reassign a contact's company, create/edit/delete).
4. **Merge included.** `ContactService.Merge(winner, loser)` re-points the loser's requesters + activity entries + company association to the winner, soft-deletes the loser, in **one tx + audit**. Dedup/merge correctness is a regression contract.

## Access model (tenant-wide RLS)

CRM tables are tenant-wide (`tenant_root_id`, no per-row `business_id` gate for contacts/companies). The RLS policy must grant a principal access to a CRM row when the row's `tenant_root_id` matches the tenant_root of **any business the principal belongs to** — i.e. derive the principal's tenant(s) from membership and compare to the row's `tenant_root_id`, rather than matching `business_id` directly.

- Implementation detail to confirm in Phase A: whether to (a) add a tenant-wide RLS policy keyed off a subquery against the membership table using the existing `manyforge.principal_id` GUC, or (b) set an additional tenant GUC. Prefer (a) — single source of truth, no new GUC plumbing.
- `activity_entry` carries **both** `tenant_root_id` (the access gate) and `business_id` (the source business, for the per-business timeline filter + attribution). Its RLS uses the same tenant-wide predicate.
- **Security-regression tests (regression contract #1):** a principal in tenant A must never read/merge a contact, company, or activity entry in tenant B — across list, get, merge, and the inbound-email resolve path. One file per finding-style assertion in `internal/security_regression/`, plus source-level pins on the RLS policy SQL so a refactor that drops it fails CI.

## Data model

All tables are tenant-wide and expose a `UNIQUE(id, tenant_root_id)` composite key so child tables can use composite FKs (the pattern `requester` already uses).

```sql
-- company
CREATE TABLE company (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_root_id uuid NOT NULL,
    name           text NOT NULL,
    domain         citext,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id)
);
CREATE UNIQUE INDEX company_tenant_domain_uq
    ON company (tenant_root_id, domain) WHERE domain IS NOT NULL;

-- contact
CREATE TABLE contact (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_root_id uuid NOT NULL,
    primary_email  citext NOT NULL,
    display_name   text,
    company_id     uuid,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    deleted_at     timestamptz,
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (company_id, tenant_root_id) REFERENCES company (id, tenant_root_id)
);
CREATE UNIQUE INDEX contact_tenant_email_uq
    ON contact (tenant_root_id, primary_email) WHERE deleted_at IS NULL;

-- promote the existing requester.contact_id stub to a real FK
ALTER TABLE requester
    ADD CONSTRAINT requester_contact_fk
    FOREIGN KEY (contact_id, tenant_root_id) REFERENCES contact (id, tenant_root_id);
CREATE INDEX requester_contact_idx ON requester (contact_id);

-- activity_entry (Phase B)
CREATE TABLE activity_entry (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_root_id uuid NOT NULL,
    business_id    uuid NOT NULL,
    contact_id     uuid NOT NULL,
    kind           text NOT NULL,        -- ticket_created | ticket_status_changed | email_received | email_sent | …
    occurred_at    timestamptz NOT NULL,
    actor          text,                 -- principal / system / requester email, for attribution
    source_type    text NOT NULL,        -- ticket | ticket_message | …
    source_id      uuid,
    summary        text NOT NULL,
    metadata       jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (contact_id, tenant_root_id) REFERENCES contact (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id)
);
CREATE INDEX activity_contact_time_idx  ON activity_entry (contact_id, occurred_at DESC);
CREATE INDEX activity_business_time_idx ON activity_entry (business_id, occurred_at DESC);
```

- A contact has **1..\* requesters**; the contact's "all emails" = its requesters' emails; `primary_email` is the canonical/dedup key.
- `kind` is an open text set (not a DB enum) so new sources don't require an `ALTER TYPE` lockstep — validated at the service boundary.
- Both new migrations must be mirrored into `db/schema.sql` in the same change (sqlc input). RLS policies + `ENABLE ROW LEVEL SECURITY` go in the migration alongside the tables.

## Services (`internal/crm`)

Mirror `internal/ticketing`: a `Service` per aggregate holding `DB *db.DB`, methods taking `(ctx, principalID, …)` and running inside `WithPrincipal`; typed `errs` sentinels; audit writes in-tx.

- **ContactService:** `Create`, `Get`, `List(cursor)`, `Update` (PATCH-style, pointer fields preserve omitted), `Delete` (soft), `ResolveOrCreateByEmail(ctx, tx, tenantRootID, email, displayName)` (used by the inbox seam — operates inside the caller's tx), and `Merge(ctx, principalID, winnerID, loserID)` (one tx: re-point requesters + activity + company, soft-delete loser, audit).
- **CompanyService:** `Create`, `Get`, `List(cursor)`, `Update`, `Delete`, `ResolveOrCreateByDomain(ctx, tx, tenantRootID, domain)` (skips free-email denylist domains → returns no company).
- **ActivityService:** `Record(ctx, tx, entry)` (in-tx, mirrors `audit.Write`) and `ListForContact(ctx, principalID, contactID, cursor)`.
- **Free-email denylist:** a small in-package set (gmail.com, googlemail.com, outlook.com, hotmail.com, live.com, yahoo.com, icloud.com, me.com, proton.me, protonmail.com, aol.com, gmx.com, …). Domains in the set → contact gets no auto-company.

## Hooks / seams

- **Inbound email** (`internal/inbox/service.go`): inside the existing tx that creates/updates the `requester`, call `ContactService.ResolveOrCreateByEmail` → set `requester.contact_id`; `CompanyService.ResolveOrCreateByDomain(email-domain)` → set `contact.company_id` when the contact is new and the domain isn't free-email.
- **Activity recording (Phase B):** `ticket_created`, `ticket_status_changed`, `email_received`, `email_sent` each call `ActivityService.Record(tx, entry)` inside their **existing** tx (resolve the entry's `contact_id` from the ticket's requester). If recording fails, propagate + roll back (no `_ = ` swallow) — the timeline is part of the source-of-truth write.

## API

- **Permissions:** new `crm.read` / `crm.write` in the authz catalog (catalog migration + role grants; follow the `agents.*` precedent). Update `internal/security_regression` perm-key pins if the catalog is grep-pinned.
- **Endpoints** (business-scoped URL for permission resolution + nav, returning tenant-wide CRM data, RLS-scoped, server-gated):
  - `GET/POST   /api/v1/businesses/{id}/contacts`
  - `GET/PATCH/DELETE /api/v1/businesses/{id}/contacts/{contactID}`
  - `POST      /api/v1/businesses/{id}/contacts/{contactID}/merge` (body `{loser_id}`) — `crm.write`
  - `GET       /api/v1/businesses/{id}/contacts/{contactID}/activity` (Phase B)
  - `GET/POST   /api/v1/businesses/{id}/companies`
  - `GET/PATCH/DELETE /api/v1/businesses/{id}/companies/{companyID}`
- **OpenAPI:** add schemas + paths to the contracts openapi.yaml in the **same change** — `go test -tags contract ./cmd/...` fails on undocumented routes while package tests stay green.

## UI (Angular, `web/src/app`)

Mirror the connectors/agents pages (service in `core/`, page components, `.mf-table` div/flex tables, `mf-empty-state`):
- **Contacts list** (`/crm/:businessId/contacts`) — table, search, create; row → detail.
- **Contact detail + timeline** — header (name, primary email, all requester emails, company), edit, **merge** action (pick loser), and the activity timeline (Phase B; chronological, cursor-paged).
- **Companies list + detail** — list, create/edit, contacts-at-company.
- **Nav** entry for CRM (mirror connectors/agents nav).
- `CrmService` (+ split services if cleaner) in `core/`.
- **Verification:** Vitest unit specs + a real-browser pass (gstack/Playwright) + a Playwright e2e spec under `web/e2e/` (page.route-mocked) — per the "drive a real browser for frontend changes" rule.

## Backfill

- **Contacts/companies (Phase A):** one-time backfill — for each existing `requester`, resolve-or-create a contact by `(tenant_root_id, email)`, set `requester.contact_id`; auto-create companies by domain (denylist-filtered). Idempotent (safe to re-run).
- **Timeline (Phase B):** one-time backfill of `activity_entry` from existing `ticket` (created/status) + `ticket_message` (email in/out), ordered by source timestamps. Idempotent (dedupe on `(source_type, source_id, kind)`).
- Backfill runs as a guarded migration step or a one-shot command (confirm in plan); must be safe against the live dev DB.

## Phasing (large → two PRs, one branch at a time)

- **Phase A — Contacts + Companies + seam** (PR 1): migration 0057 (contact, company, requester FK, RLS, perms catalog) + schema.sql mirror + sqlc + ContactService/CompanyService + handlers + OpenAPI + inbox seam + contacts/companies backfill + CRM UI (lists/detail/nav, no timeline) + security-regression (tenant isolation, merge) + contract test.
- **Phase B — Activity timeline** (PR 2): migration 0058 (activity_entry + RLS) + schema.sql + sqlc + ActivityService + recording hooks in inbox/ticketing + timeline backfill + timeline endpoint + OpenAPI + timeline UI on contact detail + tests.

Branch is `005-crm-contacts-timeline` (already created off master). One branch at a time; PR 2 branches fresh off updated master after PR 1 merges.

## Test plan

- **Unit (Go):** ContactService dedup (`ResolveOrCreateByEmail` returns existing on duplicate email), Merge (requesters/activity/company re-pointed, loser soft-deleted, audit written), CompanyService domain resolution + free-email denylist (no company for gmail etc.), PATCH partial-update preserves omitted fields, ActivityService.Record in-tx + ListForContact ordering.
- **Integration (`-tags integration`):** inbox seam creates/links contact + company on inbound email; activity recording fires in-tx on ticket create/status/email; backfill idempotency.
- **Security-regression (`make sec-test`, `internal/security_regression/`):** tenant-isolation across list/get/merge/activity/inbox-resolve (contract #1); merge correctness (contract); foreign/unknown contact UUID → 404 (no oracle); source-level pins on the RLS policy SQL + the in-tx `Record` call (so a refactor dropping either fails CI).
- **Contract:** `go test -tags contract ./cmd/...` green (new paths documented in openapi.yaml).
- **Frontend:** Vitest specs for `CrmService` + each page component; Playwright e2e (`web/e2e/`) for contacts list → detail → merge and companies; a real-browser pass before "done".
- **Gates (all green before each PR):** `go build ./...`, `make test`, `make sec-test`, `make lint` (vet + staticcheck), `go test -tags contract ./cmd/...`, `cd web && npm run build && npm test`, and the targeted e2e specs.

## Regression contracts (Spec 005)

1. **Tenant isolation** — no cross-tenant read/merge of contacts, companies, activity (the headline contract; the tenant-wide grant makes this the highest-risk surface).
2. **Contact dedup/merge correctness** — resolve-or-create never duplicates on email; merge re-points everything and leaves no orphans.
3. **Cross-source timeline ordering/attribution** — entries ordered by `occurred_at`, correct `actor`/`source` attribution.

## Foundation references (for the implementation plan)

- `migrations/0013_support_desk.up.sql:74-92` — `requester` table + `contact_id` stub + `UNIQUE(tenant_root_id, email)`.
- `db/schema.sql:131-146` (audit_entry), `:190-203` (requester), `:277-286` (outbox) — column-shape templates + sqlc input.
- `internal/platform/audit/audit.go:45-78` — `Write(ctx, tx, Entry)` in-tx pattern to mirror for `ActivityService.Record`.
- `internal/ticketing/service.go` (Service struct + `WithPrincipal` method pattern), `internal/ticketing/handler.go` (PermissionResolver + `errs` → HTTP mapping) — layering template.
- `internal/platform/db/db.go:75-93` — `WithPrincipal` GUC scoping; basis for the tenant-wide RLS variant.
- `internal/inbox/service.go` — inbound-email tx where the contact/company seam hooks in.

## Verification gotchas (carry-over)

- **sqlc** is the Homebrew bottle `/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate` (the v1.31.1 on PATH churns 24 files). After regen, `git diff --stat internal/platform/db/dbgen/` must be minimal. sqlc reads `db/schema.sql`, not migrations — every column needs BOTH.
- **OpenAPI contract test** (`go test -tags contract ./cmd/...`) and **staticcheck** (in `make lint`) are NOT caught by per-package `go test` — run them explicitly.
- Migrate dev DB: `migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" up`.
- Frontend runner is **Vitest** (`import … from 'vitest'`, `vi.spyOn`); services live in `core/`.
