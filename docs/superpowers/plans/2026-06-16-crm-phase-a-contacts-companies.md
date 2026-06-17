# CRM Phase A — Contacts, Companies & Inbox Seam — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship tenant-wide Contacts + Companies (CRUD, dedup-by-email, domain-derived companies, merge) with the inbound-email seam that auto-links new senders to contacts, plus a one-time backfill — the first demoable slice of Spec 005 (`manyforge-nwr`).

**Architecture:** New `internal/crm` package mirroring `internal/ticketing` (Service + thin Handler + `db/query/crm.sql` + migration + RLS). Contacts/companies are tenant-wide (deduped by `(tenant_root_id, email)` / `(tenant_root_id, domain)`), accessed via a tenant-wide RLS policy. New email is linked inside the existing `ingest_inbound_message` `SECURITY DEFINER` function (principal-less path); the free-email denylist stays in Go. Phase B (activity timeline) is a separate plan/PR.

**Tech Stack:** Go (pgx/v5, sqlc v1.27.0 bottle, chi, google/uuid), PostgreSQL (RLS, citext, SECURITY DEFINER functions), Angular standalone (signals), Vitest, Playwright.

---

## Design reference

Spec: `docs/superpowers/specs/2026-06-16-crm-contacts-timeline-design.md`. Read it before starting.

## Conventions (apply to every task)

- **Env:** `export PATH="$HOME/go/bin:$PATH"`. Dev DB: `postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable`.
- **sqlc:** regenerate ONLY with the v1.27.0 bottle: `/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate` (the `sqlc` on PATH is v1.31.1 and churns 24 files). After regen, `git diff --stat internal/platform/db/dbgen/` MUST be a minimal diff. sqlc reads `db/schema.sql`, not migrations — every column needs BOTH a migration AND a `schema.sql` edit.
- **Gates before each commit:** `go build ./...`, `make test`. Before the PR: also `make sec-test`, `make lint` (vet + staticcheck), `go test -tags contract ./cmd/...`, and (web) `npm run build && npm test`.
- **Errors:** wrap with `errs.ErrNotFound|ErrValidation|ErrConflict` (`internal/platform/errs`); foreign/unknown UUID → `ErrNotFound` (no oracle). Handlers map via `httpx.WriteError`.
- **Commits:** one per task (or per red/green pair). No Co-Authored-By trailer (per user CLAUDE.md).
- **Migrations are authoritative; mirror table shapes into `db/schema.sql` in the same task.**

---

## Task 1: Migration 0057 — contact + company tables, RLS, requester FK

**Files:**
- Create: `migrations/0057_crm_contacts_companies.up.sql`
- Create: `migrations/0057_crm_contacts_companies.down.sql`
- Modify: `db/schema.sql` (append the two tables + the requester FK; mirror the migration)

- [ ] **Step 1: Write the up migration**

```sql
-- migrations/0057_crm_contacts_companies.up.sql
-- Spec 005 (manyforge-nwr) Phase A: tenant-wide CRM contacts + companies.

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

-- Promote the existing requester.contact_id stub (migrations/0013) to a real FK.
ALTER TABLE requester
    ADD CONSTRAINT requester_contact_fk
    FOREIGN KEY (contact_id, tenant_root_id) REFERENCES contact (id, tenant_root_id);
CREATE INDEX requester_contact_idx ON requester (contact_id);

-- Tenant-wide RLS: a row is visible when its tenant_root_id is one of the
-- principal's authorized tenants (mirrors role_rls in migrations/0007).
ALTER TABLE company ENABLE ROW LEVEL SECURITY;
CREATE POLICY company_rls ON company FOR ALL
    USING (tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())))
    WITH CHECK (tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())));

ALTER TABLE contact ENABLE ROW LEVEL SECURITY;
CREATE POLICY contact_rls ON contact FOR ALL
    USING (tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())))
    WITH CHECK (tenant_root_id IN (SELECT tenant_root_id FROM authorized_tenants(current_principal())));
```

> NOTE for the implementer: `authorized_tenants(current_principal())` and `current_principal()` are the helpers used by `role_rls` in `migrations/0007_rls.up.sql:101-103` — open that file and copy the EXACT helper names/signatures it uses. If `authorized_tenants` does not exist, use the same construct `role_rls` uses for tenant scoping. Do NOT invent a helper.

- [ ] **Step 2: Write the down migration**

```sql
-- migrations/0057_crm_contacts_companies.down.sql
ALTER TABLE requester DROP CONSTRAINT IF EXISTS requester_contact_fk;
DROP INDEX IF EXISTS requester_contact_idx;
DROP POLICY IF EXISTS contact_rls ON contact;
DROP POLICY IF EXISTS company_rls ON company;
DROP TABLE IF EXISTS contact;
DROP TABLE IF EXISTS company;
```

- [ ] **Step 3: Mirror into `db/schema.sql`**

Append the `company` and `contact` `CREATE TABLE` + index statements (same DDL as Step 1, minus the RLS policies — `schema.sql` is the sqlc shape input; check whether existing tables in `schema.sql` include `ENABLE ROW LEVEL SECURITY` and match that convention). Add `contact_id` is already present on `requester` in `schema.sql:190-203` — add the matching FK + `requester_contact_idx` line if `schema.sql` mirrors constraints (check how `requester`'s existing FKs appear there and match exactly).

- [ ] **Step 4: Apply + verify the migration round-trips**

Run:
```bash
migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" up
migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" down 1
migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" up
```
Expected: all succeed; final `up` re-creates the tables. Verify with `psql "$DSN" -c '\d contact'` showing the partial unique index + FK.

- [ ] **Step 5: Commit**

```bash
git add migrations/0057_crm_contacts_companies.up.sql migrations/0057_crm_contacts_companies.down.sql db/schema.sql
git commit -m "feat(crm): migration 0057 — contact + company tables, RLS, requester FK"
```

---

## Task 2: CRM permissions (crm.read / crm.write)

**Files:**
- Create: `migrations/0058_crm_permissions.up.sql`
- Create: `migrations/0058_crm_permissions.down.sql`
- Modify: `internal/authz/perms.go` (add constants)
- Modify: `internal/security_regression/perm_constants_pin_test.go` (add the new keys to the pinned list)

- [ ] **Step 1: Write the permissions migration**

```sql
-- migrations/0058_crm_permissions.up.sql
INSERT INTO permission (key, module, description) VALUES
    ('crm.read',  'crm', 'View contacts and companies'),
    ('crm.write', 'crm', 'Create, update, delete, and merge contacts and companies');

INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key IN ('crm.read', 'crm.write')
    WHERE r.tenant_root_id IS NULL AND r.key IN ('owner', 'admin');

-- Read-only for agent/member roles if they exist (match the precedent in
-- migrations/0027_agent_permissions.up.sql for which roles get read).
INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key = 'crm.read'
    WHERE r.tenant_root_id IS NULL AND r.key IN ('member');
```

> Open `migrations/0027_agent_permissions.up.sql` and `migrations/0003_rbac.up.sql` first — copy the EXACT `permission` columns and the EXACT built-in role keys that exist (`owner`/`admin`/`member`/etc.). Drop the `member` insert if no such role key exists.

- [ ] **Step 2: Write the down migration**

```sql
-- migrations/0058_crm_permissions.down.sql
DELETE FROM role_permission WHERE permission_key IN ('crm.read', 'crm.write');
DELETE FROM permission WHERE key IN ('crm.read', 'crm.write');
```

- [ ] **Step 3: Add Go constants**

In `internal/authz/perms.go`, in the `const (...)` block, add:
```go
	PermCRMRead  = "crm.read"
	PermCRMWrite = "crm.write"
```

- [ ] **Step 4: Update the perm-key pin test**

In `internal/security_regression/perm_constants_pin_test.go`, add `"crm.read"` and `"crm.write"` to the pinned key list (the list around line 39-44 the test asserts appear in the migrations catalog).

- [ ] **Step 5: Apply migration + run the pin test**

Run:
```bash
migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" up
go test ./internal/security_regression/ -run PermConstants -v
```
Expected: migration applies; pin test PASSES (constants ↔ catalog in sync).

- [ ] **Step 6: Commit**

```bash
git add migrations/0058_crm_permissions.up.sql migrations/0058_crm_permissions.down.sql internal/authz/perms.go internal/security_regression/perm_constants_pin_test.go
git commit -m "feat(crm): crm.read/crm.write permissions + role grants"
```

---

## Task 3: sqlc queries for contact + company

**Files:**
- Create: `db/query/crm.sql`
- Regenerate: `internal/platform/db/dbgen/` (do not hand-edit)

- [ ] **Step 1: Write the query file**

```sql
-- db/query/crm.sql

-- name: InsertContact :one
INSERT INTO contact (tenant_root_id, primary_email, display_name, company_id)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetContact :one
SELECT * FROM contact WHERE id = $1 AND deleted_at IS NULL;

-- name: ListContacts :many
SELECT * FROM contact
WHERE deleted_at IS NULL
  AND (sqlc.narg('after_email')::citext IS NULL OR primary_email > sqlc.narg('after_email'))
ORDER BY primary_email ASC, id ASC
LIMIT sqlc.arg('lim');

-- name: UpdateContact :one
UPDATE contact SET
  display_name = COALESCE(sqlc.narg('display_name'), display_name),
  company_id   = COALESCE(sqlc.narg('company_id'), company_id),
  updated_at   = now()
WHERE id = sqlc.arg('id') AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteContact :exec
UPDATE contact SET deleted_at = now(), updated_at = now()
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetContactByEmail :one
SELECT * FROM contact
WHERE tenant_root_id = $1 AND primary_email = $2 AND deleted_at IS NULL;

-- name: InsertContactByEmail :one
INSERT INTO contact (tenant_root_id, primary_email, display_name, company_id)
VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_root_id, primary_email) WHERE deleted_at IS NULL
DO UPDATE SET display_name = COALESCE(contact.display_name, EXCLUDED.display_name), updated_at = now()
RETURNING *;

-- name: ListRequestersForContact :many
SELECT * FROM requester WHERE contact_id = $1;

-- Merge: re-point a loser contact's requesters to the winner.
-- name: RepointRequesters :exec
UPDATE requester SET contact_id = sqlc.arg('winner_id'), updated_at = now()
WHERE contact_id = sqlc.arg('loser_id');

-- name: InsertCompany :one
INSERT INTO company (tenant_root_id, name, domain)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetCompany :one
SELECT * FROM company WHERE id = $1;

-- name: ListCompanies :many
SELECT * FROM company
WHERE (sqlc.narg('after_name')::text IS NULL OR name > sqlc.narg('after_name'))
ORDER BY name ASC, id ASC
LIMIT sqlc.arg('lim');

-- name: UpdateCompany :one
UPDATE company SET
  name   = COALESCE(sqlc.narg('name'), name),
  domain = COALESCE(sqlc.narg('domain'), domain),
  updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: DeleteCompany :exec
DELETE FROM company WHERE id = $1;

-- name: ResolveCompanyByDomain :one
INSERT INTO company (tenant_root_id, name, domain)
VALUES ($1, $2, $3)
ON CONFLICT (tenant_root_id, domain) WHERE domain IS NOT NULL
DO UPDATE SET updated_at = now()
RETURNING *;
```

> Confirm `sqlc.narg`/`sqlc.arg` cast syntax against `db/query/ticketing.sql` (it uses `sqlc.narg('status')::ticket_status`). Adjust casts to match what sqlc v1.27.0 accepts.

- [ ] **Step 2: Regenerate dbgen with the pinned bottle**

Run:
```bash
/opt/homebrew/Cellar/sqlc/1.27.0/bin/sqlc generate
git diff --stat internal/platform/db/dbgen/
```
Expected: NEW `crm.sql.go` (or additions to `models.go`/`querier.go`) ONLY — a minimal diff. If 20+ files churn, you used the wrong sqlc version; `git checkout internal/platform/db/dbgen/` and re-run with the bottle path.

- [ ] **Step 3: Build**

Run: `go build ./...`
Expected: compiles (dbgen has the new `Contact`/`Company` models + query methods).

- [ ] **Step 4: Commit**

```bash
git add db/query/crm.sql internal/platform/db/dbgen/
git commit -m "feat(crm): sqlc queries for contact + company"
```

---

## Task 4: ContactService — CRUD (TDD)

**Files:**
- Create: `internal/crm/types.go` (domain structs + input types)
- Create: `internal/crm/contact.go` (ContactService)
- Create: `internal/crm/cursor.go` (cursor helper — copy the pattern from `internal/ticketing/cursor.go`)
- Test: `internal/crm/contact_integration_test.go` (build tag `integration`)

- [ ] **Step 1: Write domain types**

```go
// internal/crm/types.go
package crm

import (
	"time"

	"github.com/google/uuid"
)

type Contact struct {
	ID           uuid.UUID  `json:"id"`
	TenantRootID uuid.UUID  `json:"tenant_root_id"`
	PrimaryEmail string     `json:"primary_email"`
	DisplayName  *string    `json:"display_name,omitempty"`
	CompanyID    *uuid.UUID `json:"company_id,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type ContactInput struct {
	PrimaryEmail string     // required on create
	DisplayName  *string    // nil = absent (PATCH preserves)
	CompanyID    *uuid.UUID // nil = absent
}

type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor,omitempty"`
}
```

- [ ] **Step 2: Write the failing integration test (Create + Get + dedup)**

```go
// internal/crm/contact_integration_test.go
//go:build integration

package crm_test

// Use the testdb harness from internal (mirror internal/inbox/ingest_integration_test.go:
// testdb.Start(ctx), tdb.App for app-role/RLS, tdb.Super for seeding).
// Seed a tenant_root business + a principal with crm.write, get its principalID,
// then exercise the service through db.WithPrincipal.

func TestContactCreateGetAndDedup(t *testing.T) {
	// 1. start testdb, seed tenant + principal (helper seedCRMTenant — see note)
	// 2. svc := &crm.ContactService{DB: tdb.App}
	// 3. c1, err := svc.Create(ctx, principalID, crm.ContactInput{PrimaryEmail: "ada@example.com"})
	//    require no err; c1.PrimaryEmail == "ada@example.com"
	// 4. got, err := svc.Get(ctx, principalID, c1.ID); require got.ID == c1.ID
	// 5. dup, err := svc.ResolveOrCreateByEmail(ctx, tx?, ...) — covered in Task 5; here assert
	//    Create with the same email returns errs.ErrConflict (UNIQUE violation mapped).
}
```

> Implementer: add a `seedCRMTenant` helper in this test file modeled on `internal/inbox/ingest_integration_test.go:37-83` (`seedIngestTenant`): insert a `business` (id=tenant_root), `business_closure`, a `principal`, and grant it `crm.read`/`crm.write` via the role tables so RLS + permission resolution work. Reuse `testdb` and `db.New(...)` exactly as inbox tests do.

- [ ] **Step 3: Run it — verify it fails to compile/fails**

Run: `go test -tags integration ./internal/crm/ -run TestContactCreateGetAndDedup -v`
Expected: FAIL (ContactService undefined).

- [ ] **Step 4: Implement ContactService CRUD**

```go
// internal/crm/contact.go
package crm

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"manyforge/internal/platform/db"          // adjust import path to module name
	"manyforge/internal/platform/db/dbgen"
	"manyforge/internal/platform/errs"
)

type ContactService struct {
	DB *db.DB
}

func (s *ContactService) Create(ctx context.Context, principalID uuid.UUID, in ContactInput) (Contact, error) {
	if in.PrimaryEmail == "" {
		return Contact{}, fmt.Errorf("primary_email required: %w", errs.ErrValidation)
	}
	var out Contact
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		// tenant_root_id derived from the principal's tenant — resolve it the same
		// way ticketing does (a helper or a query for the principal's tenant_root).
		trid, err := principalTenantRoot(ctx, tx, principalID)
		if err != nil {
			return err
		}
		row, err := q.InsertContact(ctx, dbgen.InsertContactParams{
			TenantRootID: db.PGUUID(trid),
			PrimaryEmail: in.PrimaryEmail,
			DisplayName:  in.DisplayName,
			CompanyID:    db.PGUUIDPtr(in.CompanyID),
		})
		if err != nil {
			return mapErr(err)
		}
		out = toContact(row)
		return nil
	})
	return out, err
}

func (s *ContactService) Get(ctx context.Context, principalID, id uuid.UUID) (Contact, error) {
	var out Contact
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		row, err := dbgen.New(tx).GetContact(ctx, db.PGUUID(id))
		if err != nil {
			return mapErr(err)
		}
		out = toContact(row)
		return nil
	})
	return out, err
}

// mapErr collapses pgx.ErrNoRows -> ErrNotFound and unique violations -> ErrConflict.
func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("duplicate: %w", errs.ErrConflict)
	}
	return err
}
```

Also implement `List` (cursor via `crm/cursor.go`, mirror `ticketing/cursor.go` with a `cursorContacts="c"` kind), `Update` (PATCH — pointer fields → `dbgen.UpdateContactParams` with `db.TextPtr`/narg helpers; confirm null-param helper names from `dbgen`), `SoftDelete`. Add `toContact(row)` mapping (`db.pgUUIDPtr` for nullable uuids — note that helper is unexported in ticketing; add an equivalent in crm) and `principalTenantRoot` (copy how ticketing/inbox resolves a principal's tenant_root; if a helper exists in `db` or `authz`, use it).

- [ ] **Step 5: Run the test — verify pass**

Run: `go test -tags integration ./internal/crm/ -run TestContactCreateGetAndDedup -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/crm/
git commit -m "feat(crm): ContactService CRUD"
```

---

## Task 5: ContactService — ResolveOrCreateByEmail + Merge (TDD)

**Files:**
- Modify: `internal/crm/contact.go`
- Test: `internal/crm/merge_integration_test.go`

- [ ] **Step 1: Write the failing tests**

```go
//go:build integration
package crm_test

func TestResolveOrCreateByEmailIsIdempotent(t *testing.T) {
	// resolve "ada@example.com" twice -> same contact ID (no duplicate).
}

func TestMergeRepointsRequestersAndSoftDeletesLoser(t *testing.T) {
	// seed two contacts (winner, loser), attach a requester row to loser,
	// svc.Merge(ctx, principalID, winnerID, loserID):
	//   - loser.deleted_at set (Get(loser) -> ErrNotFound)
	//   - the requester now has contact_id == winnerID
	//   - an audit_entry with action "contact.merged" exists (query via Super)
}
```

- [ ] **Step 2: Run — verify fail**

Run: `go test -tags integration ./internal/crm/ -run 'TestResolveOrCreate|TestMerge' -v`
Expected: FAIL (methods undefined).

- [ ] **Step 3: Implement ResolveOrCreateByEmail + Merge**

```go
// ResolveOrCreateByEmail operates inside the CALLER's tx (no WithPrincipal wrapper),
// so the principal-less inbox path can reuse it. tenantRootID is passed explicitly.
func (s *ContactService) ResolveOrCreateByEmail(ctx context.Context, tx pgx.Tx, tenantRootID uuid.UUID, email string, displayName *string, companyID *uuid.UUID) (Contact, error) {
	q := dbgen.New(tx)
	row, err := q.InsertContactByEmail(ctx, dbgen.InsertContactByEmailParams{
		TenantRootID: db.PGUUID(tenantRootID),
		PrimaryEmail: email,
		DisplayName:  displayName,
		CompanyID:    db.PGUUIDPtr(companyID),
	})
	if err != nil {
		return Contact{}, mapErr(err)
	}
	return toContact(row), nil
}

func (s *ContactService) Merge(ctx context.Context, principalID, winnerID, loserID uuid.UUID) error {
	if winnerID == loserID {
		return fmt.Errorf("cannot merge a contact into itself: %w", errs.ErrValidation)
	}
	return s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		winner, err := q.GetContact(ctx, db.PGUUID(winnerID)) // RLS + existence
		if err != nil {
			return mapErr(err)
		}
		if _, err := q.GetContact(ctx, db.PGUUID(loserID)); err != nil {
			return mapErr(err)
		}
		if err := q.RepointRequesters(ctx, dbgen.RepointRequestersParams{WinnerID: db.PGUUID(winnerID), LoserID: db.PGUUID(loserID)}); err != nil {
			return err
		}
		if err := q.SoftDeleteContact(ctx, db.PGUUID(loserID)); err != nil {
			return err
		}
		tt := "contact"
		return audit.Write(ctx, tx, audit.Entry{
			TenantRootID:     ptrUUID(uuid.UUID(winner.TenantRootID.Bytes)),
			ActorPrincipalID: &principalID,
			Action:           "contact.merged",
			TargetType:       &tt,
			TargetID:         &winnerID,
			NewValue:         map[string]any{"winner_id": winnerID, "loser_id": loserID},
		})
	})
}
```

> Phase B will also re-point `activity_entry`; that table does not exist yet, so Merge here only re-points requesters + company association. Add a `// TODO(phase-b): re-point activity_entry` comment so the Phase B plan picks it up.

- [ ] **Step 4: Run — verify pass**

Run: `go test -tags integration ./internal/crm/ -run 'TestResolveOrCreate|TestMerge' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/crm/
git commit -m "feat(crm): ContactService ResolveOrCreateByEmail + Merge"
```

---

## Task 6: CompanyService — CRUD + ResolveOrCreateByDomain + free-email denylist (TDD)

**Files:**
- Create: `internal/crm/company.go`
- Create: `internal/crm/freemail.go` (denylist + `IsFreeEmailDomain`)
- Test: `internal/crm/freemail_test.go` (plain unit, no DB), `internal/crm/company_integration_test.go`

- [ ] **Step 1: Write the denylist unit test (no DB)**

```go
// internal/crm/freemail_test.go
package crm

import "testing"

func TestIsFreeEmailDomain(t *testing.T) {
	free := []string{"gmail.com", "GMAIL.COM", "outlook.com", "yahoo.com", "icloud.com", "proton.me", "hotmail.com"}
	notFree := []string{"acme.com", "atlassian.net", "manyforge.test"}
	for _, d := range free {
		if !IsFreeEmailDomain(d) {
			t.Errorf("expected %q to be free-email", d)
		}
	}
	for _, d := range notFree {
		if IsFreeEmailDomain(d) {
			t.Errorf("expected %q to NOT be free-email", d)
		}
	}
}
```

- [ ] **Step 2: Run — verify fail**

Run: `go test ./internal/crm/ -run TestIsFreeEmailDomain -v`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement the denylist**

```go
// internal/crm/freemail.go
package crm

import "strings"

var freeEmailDomains = map[string]struct{}{
	"gmail.com": {}, "googlemail.com": {}, "outlook.com": {}, "hotmail.com": {},
	"live.com": {}, "msn.com": {}, "yahoo.com": {}, "ymail.com": {}, "icloud.com": {},
	"me.com": {}, "mac.com": {}, "aol.com": {}, "proton.me": {}, "protonmail.com": {},
	"gmx.com": {}, "gmx.net": {}, "mail.com": {}, "zoho.com": {}, "yandex.com": {},
}

// IsFreeEmailDomain reports whether domain is a public/free mailbox provider
// (so it should NOT auto-create a company).
func IsFreeEmailDomain(domain string) bool {
	_, ok := freeEmailDomains[strings.ToLower(strings.TrimSpace(domain))]
	return ok
}

// DomainFromEmail returns the lowercased domain part, or "" if malformed.
func DomainFromEmail(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(email[at+1:]))
}
```

- [ ] **Step 4: Run denylist test — verify pass**

Run: `go test ./internal/crm/ -run TestIsFreeEmailDomain -v`
Expected: PASS.

- [ ] **Step 5: Write CompanyService integration test + implement**

Test `company_integration_test.go`: `ResolveOrCreateByDomain` twice with "acme.com" → same company; and a free-email domain is never passed here (denylist is applied by the caller). Implement `CompanyService` (struct `{DB *db.DB}`) with `Create`/`Get`/`List`/`Update`/`Delete` (mirror ContactService) and:
```go
// ResolveOrCreateByDomain operates inside the caller's tx (principal-less safe).
// Caller MUST have already excluded free-email domains via IsFreeEmailDomain.
func (s *CompanyService) ResolveOrCreateByDomain(ctx context.Context, tx pgx.Tx, tenantRootID uuid.UUID, domain string) (Company, error) {
	row, err := dbgen.New(tx).ResolveCompanyByDomain(ctx, dbgen.ResolveCompanyByDomainParams{
		TenantRootID: db.PGUUID(tenantRootID),
		Name:         domain, // default name = domain; user can rename later
		Domain:       pgCitext(domain),
	})
	if err != nil {
		return Company{}, mapErr(err)
	}
	return toCompany(row), nil
}
```

- [ ] **Step 6: Run + commit**

Run: `go test -tags integration ./internal/crm/ -run TestCompany -v` → PASS.
```bash
git add internal/crm/
git commit -m "feat(crm): CompanyService CRUD + ResolveOrCreateByDomain + free-email denylist"
```

---

## Task 7: HTTP handler — contacts endpoints + wiring

**Files:**
- Create: `internal/crm/handler.go`
- Modify: `cmd/manyforge/main.go` (construct services + handler, mount routes, add `crmRead`/`crmWrite` gates)

- [ ] **Step 1: Write the handler**

Mirror `internal/ticketing/handler.go:26-76`:
```go
// internal/crm/handler.go
package crm

type Handler struct {
	contacts  *ContactService
	companies *CompanyService
	db        *db.DB
	resolve   httpx.PermissionResolver
}

func NewHandler(c *ContactService, co *CompanyService, database *db.DB, resolve httpx.PermissionResolver) *Handler {
	return &Handler{contacts: c, companies: co, db: database, resolve: resolve}
}

func (h *Handler) ReadRoutes(r chi.Router) {
	r.Get("/businesses/{id}/contacts", h.listContacts)
	r.Get("/businesses/{id}/contacts/{cid}", h.getContact)
	r.Get("/businesses/{id}/companies", h.listCompanies)
	r.Get("/businesses/{id}/companies/{coid}", h.getCompany)
}

func (h *Handler) WriteRoutes(r chi.Router) {
	r.Post("/businesses/{id}/contacts", h.createContact)
	r.Patch("/businesses/{id}/contacts/{cid}", h.updateContact)
	r.Delete("/businesses/{id}/contacts/{cid}", h.deleteContact)
	r.Post("/businesses/{id}/contacts/{cid}/merge", h.mergeContact)
	r.Post("/businesses/{id}/companies", h.createCompany)
	r.Patch("/businesses/{id}/companies/{coid}", h.updateCompany)
	r.Delete("/businesses/{id}/companies/{coid}", h.deleteCompany)
}
```

Implement each handler method following `ticketing/handler.go:427-459`: `httpx.PrincipalFromContext`, `pathUUID(r, "cid")` (copy the `pathUUID` helper into crm or reuse if exported), `httpx.DecodeJSON` for bodies, call the service, `httpx.WriteError(w, r, err)`, `httpx.WriteJSON(w, status, resp)`. Define request/response DTO structs + `toContactResp`/`toCompanyResp`. The merge body is `{ "loser_id": "<uuid>" }`.

- [ ] **Step 2: Wire into main.go**

Near the ticketing wiring (`cmd/manyforge/main.go:117-140`):
```go
crmContacts := &crm.ContactService{DB: database}
crmCompanies := &crm.CompanyService{DB: database}
crmH := crm.NewHandler(crmContacts, crmCompanies, database, permResolve)
```
Add permission-gate middleware mirroring `h.ticketsRead` (which wraps `httpx.RequirePermission(database, permResolve, authz.PermTicketsRead, bizIDFromPath)`): create `crmRead := httpx.RequirePermission(database, permResolve, authz.PermCRMRead, bizID)` and `crmWrite := ...PermCRMWrite...`. In `mountAPIRoutes` add two groups after the ticketing groups (around `main.go:806`):
```go
pr.Group(func(cr chi.Router) { cr.Use(crmRead); crmH.ReadRoutes(cr) })
pr.Group(func(cw chi.Router) { cw.Use(crmWrite); crmH.WriteRoutes(cw) })
```

> Match the EXACT way ticketing builds its `ticketsRead`/businessID-extractor middleware in `main.go` — copy that closure, swapping the permission constant.

- [ ] **Step 3: Build + run existing tests**

Run: `go build ./... && make test`
Expected: compiles, existing tests pass.

- [ ] **Step 4: Smoke-test against the dev DB**

Run (backend on :8081, demo login):
```bash
# Acquire a token via the demo login, then:
curl -s -H "Authorization: Bearer $TOK" \
  http://localhost:8081/api/v1/businesses/7bbeb32e-7c98-4c8f-966b-70acdb440dce/contacts | head
```
Expected: `{"items":[...]}` (empty or seeded), HTTP 200 — not 404/500.

- [ ] **Step 5: Commit**

```bash
git add internal/crm/handler.go cmd/manyforge/main.go
git commit -m "feat(crm): contacts + companies HTTP handlers + route wiring"
```

---

## Task 8: OpenAPI contract — document the new paths

**Files:**
- Modify: the contracts openapi.yaml that the contract test reads (find via `go test -tags contract ./cmd/...` failure output; likely `specs/003-agent-runtime/contracts/openapi.yaml` — confirm which file the loader uses).

- [ ] **Step 1: Run the contract test to see the gap**

Run: `go test -tags contract ./cmd/... 2>&1 | head -40`
Expected: FAIL listing the new `/contacts` + `/companies` routes as undocumented.

- [ ] **Step 2: Add schemas + paths**

Add `Contact`, `Company`, `ContactList`, `CompanyList`, `MergeRequest` schemas and the paths:
`/api/v1/businesses/{id}/contacts` (GET, POST), `/contacts/{cid}` (GET, PATCH, DELETE), `/contacts/{cid}/merge` (POST), `/companies` (GET, POST), `/companies/{coid}` (GET, PATCH, DELETE) — matching the field names in the handler DTOs and the existing yaml's style (copy a ticketing path block as the template).

- [ ] **Step 3: Run contract test — verify pass**

Run: `go test -tags contract ./cmd/...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add specs/*/contracts/openapi.yaml
git commit -m "docs(api): document CRM contacts + companies endpoints (openapi)"
```

---

## Task 9: Inbox seam — link new email to contact + company (TDD)

**Files:**
- Create: `migrations/0059_ingest_link_contact.up.sql` / `.down.sql` (CREATE OR REPLACE `ingest_inbound_message` with a `p_company_domain` param + contact/company resolution)
- Modify: `db/schema.sql` (mirror the function body if functions live there; otherwise functions are migration-only — confirm)
- Modify: `internal/inbox/service.go` (extract sender domain, apply denylist, pass `p_company_domain`)
- Test: `internal/inbox/contact_seam_integration_test.go`

- [ ] **Step 1: Write the failing integration test**

```go
//go:build integration
package inbox

func TestIngestLinksContactAndCompany(t *testing.T) {
	// seed tenant + inbound address (seedIngestTenant).
	// Ingest an email from "ada@acme.com":
	//   - a contact row (tenant_root_id, primary_email="ada@acme.com") exists (query Super)
	//   - the created requester has contact_id == that contact's id
	//   - a company row (domain="acme.com") exists and contact.company_id points to it
	// Ingest a second email from "bob@gmail.com" (free-email):
	//   - a contact exists; its company_id IS NULL (denylist suppressed the company)
}
```

- [ ] **Step 2: Run — verify fail**

Run: `go test -tags integration ./internal/inbox/ -run TestIngestLinksContactAndCompany -v`
Expected: FAIL (contact_id NULL / no contact).

- [ ] **Step 3: CREATE OR REPLACE the DEFINER function**

In `migrations/0059_ingest_link_contact.up.sql`, copy the FULL current `CREATE FUNCTION ingest_inbound_message(...)` body from `migrations/0024_loop_guard.up.sql` (and any later migration that replaced it — `grep -rl 'FUNCTION ingest_inbound_message' migrations/` and use the LATEST), change to `CREATE OR REPLACE FUNCTION`, add a trailing param `p_company_domain citext DEFAULT NULL`, and insert this block BEFORE the requester upsert (so `v_contact_id` is set), then add `contact_id` to the requester upsert:

```sql
    -- CRM seam (spec 005): resolve company (by domain, caller pre-filtered free-email) + contact.
    v_company_id := NULL;
    IF p_company_domain IS NOT NULL THEN
        INSERT INTO company (tenant_root_id, name, domain)
            VALUES (p_tenant_root_id, p_company_domain, p_company_domain)
            ON CONFLICT (tenant_root_id, domain) WHERE domain IS NOT NULL
            DO UPDATE SET updated_at = now()
            RETURNING id INTO v_company_id;
    END IF;

    INSERT INTO contact (tenant_root_id, primary_email, display_name, company_id)
        VALUES (p_tenant_root_id, p_sender_email, p_sender_name, v_company_id)
        ON CONFLICT (tenant_root_id, primary_email) WHERE deleted_at IS NULL
        DO UPDATE SET display_name = COALESCE(contact.display_name, EXCLUDED.display_name),
                      company_id   = COALESCE(contact.company_id, EXCLUDED.company_id),
                      updated_at   = now()
        RETURNING id INTO v_contact_id;
```
Then change the requester upsert (currently lines 72-79) to include `contact_id`:
```sql
    INSERT INTO requester (business_id, tenant_root_id, email, display_name, contact_id)
        VALUES (p_business_id, p_tenant_root_id, p_sender_email, p_sender_name, v_contact_id)
        ON CONFLICT (tenant_root_id, email) DO UPDATE
            SET last_seen_at = now(),
                display_name = COALESCE(EXCLUDED.display_name, requester.display_name),
                contact_id   = COALESCE(requester.contact_id, EXCLUDED.contact_id),
                updated_at   = now()
        RETURNING id INTO v_requester_id;
```
Declare `v_company_id uuid; v_contact_id uuid;` in the `DECLARE` block. The `.down.sql` re-creates the prior function signature (copy it verbatim from 0024/latest).

- [ ] **Step 4: Pass `p_company_domain` from Go**

In `internal/inbox/service.go` near `senderEmail` (lines 139-143), compute:
```go
var companyDomain *string
if d := crm.DomainFromEmail(senderEmail); d != "" && !crm.IsFreeEmailDomain(d) {
	companyDomain = &d
}
```
Add `companyDomain` as the new last arg ($20) to the `ingest_inbound_message(...)` call and bump the SQL placeholder list. Import the `crm` package.

- [ ] **Step 5: Apply migration + run the test — verify pass**

Run:
```bash
migrate -path migrations -database "$DSN" up
go test -tags integration ./internal/inbox/ -run TestIngestLinksContactAndCompany -v
```
Expected: PASS.

- [ ] **Step 6: Run full inbox integration suite (no regressions)**

Run: `go test -tags integration ./internal/inbox/...`
Expected: PASS (the DEFINER change didn't break existing ingest/threading/loopguard tests).

- [ ] **Step 7: Commit**

```bash
git add migrations/0059_ingest_link_contact.up.sql migrations/0059_ingest_link_contact.down.sql internal/inbox/service.go db/schema.sql
git commit -m "feat(crm): link inbound email senders to contacts + companies"
```

---

## Task 10: Backfill existing requesters → contacts + companies

**Files:**
- Create: `migrations/0060_crm_backfill.up.sql` (idempotent data backfill) / `.down.sql` (no-op or best-effort)

- [ ] **Step 1: Write the backfill migration**

```sql
-- migrations/0060_crm_backfill.up.sql
-- Idempotent: create a contact per distinct (tenant_root_id, email) requester,
-- link the requester, and auto-create companies by domain (excluding free-email).
-- Free-email exclusion uses a NOT IN list mirroring crm/freemail.go.

-- 1. contacts from requesters missing a contact link
INSERT INTO contact (tenant_root_id, primary_email, display_name)
    SELECT DISTINCT r.tenant_root_id, r.email, NULL
    FROM requester r
    WHERE r.contact_id IS NULL
    ON CONFLICT (tenant_root_id, primary_email) WHERE deleted_at IS NULL DO NOTHING;

-- 2. link requesters to their contact
UPDATE requester r
    SET contact_id = c.id
    FROM contact c
    WHERE r.contact_id IS NULL
      AND c.tenant_root_id = r.tenant_root_id
      AND c.primary_email = r.email
      AND c.deleted_at IS NULL;

-- 3. companies by domain (skip free-email)
INSERT INTO company (tenant_root_id, name, domain)
    SELECT DISTINCT c.tenant_root_id, split_part(c.primary_email::text, '@', 2), split_part(c.primary_email::text, '@', 2)::citext
    FROM contact c
    WHERE c.company_id IS NULL
      AND split_part(c.primary_email::text, '@', 2) <> ''
      AND lower(split_part(c.primary_email::text, '@', 2)) NOT IN
          ('gmail.com','googlemail.com','outlook.com','hotmail.com','live.com','msn.com',
           'yahoo.com','ymail.com','icloud.com','me.com','mac.com','aol.com','proton.me',
           'protonmail.com','gmx.com','gmx.net','mail.com','zoho.com','yandex.com')
    ON CONFLICT (tenant_root_id, domain) WHERE domain IS NOT NULL DO NOTHING;

-- 4. link contacts to companies
UPDATE contact c
    SET company_id = co.id
    FROM company co
    WHERE c.company_id IS NULL
      AND co.tenant_root_id = c.tenant_root_id
      AND co.domain = split_part(c.primary_email::text, '@', 2)::citext;
```
`.down.sql`: a no-op comment (backfilled data is not reverted; the table drops in 0057.down handle teardown).

- [ ] **Step 2: Apply + verify on dev DB**

Run:
```bash
migrate -path migrations -database "$DSN" up
psql "$DSN" -c "SELECT count(*) FROM requester WHERE contact_id IS NULL;"   # expect 0
psql "$DSN" -c "SELECT count(*) FROM contact; SELECT count(*) FROM company;"
```
Expected: every requester linked; contacts/companies populated. Re-run `migrate down 1 && migrate up` (the backfill block) → counts stable (idempotent).

- [ ] **Step 3: Commit**

```bash
git add migrations/0060_crm_backfill.up.sql migrations/0060_crm_backfill.down.sql
git commit -m "feat(crm): backfill contacts + companies from existing requesters"
```

---

## Task 11: Security-regression — tenant isolation + merge

**Files:**
- Create: `internal/security_regression/crm_tenant_isolation_test.go`

- [ ] **Step 1: Write the regression tests**

```go
//go:build integration
package security_regression

// Finding contract #1: tenant isolation. A principal in tenant A must not read,
// update, delete, or merge a contact/company in tenant B.
func TestCRMTenantIsolation(t *testing.T) {
	// seed tenant A (principal pA) and tenant B (principal pB, contact cB).
	// 1. ContactService.Get(ctx, pA, cB.ID)  -> errs.ErrNotFound (NOT the row)
	// 2. ContactService.List(ctx, pA, ...)    -> does not include cB
	// 3. ContactService.Update(ctx, pA, cB.ID, ...) -> errs.ErrNotFound
	// 4. ContactService.Merge(ctx, pA, <A contact>, cB.ID) -> errs.ErrNotFound
	// 5. same matrix for CompanyService.
}

// Source-level pin: the tenant-wide RLS policy must exist (a refactor dropping it fails CI).
func TestCRMRLSPolicyPinned(t *testing.T) {
	// strings.Contains a migration file for "CREATE POLICY contact_rls" and
	// "CREATE POLICY company_rls" with "authorized_tenants(current_principal())".
}
```

- [ ] **Step 2: Run — verify pass (the fix is already in place from Task 1)**

Run: `go test -tags integration ./internal/security_regression/ -run TestCRM -v`
Expected: PASS (RLS already enforces isolation). If any sub-case returns tenant B's row, STOP — the RLS policy is wrong; fix Task 1's policy before continuing.

- [ ] **Step 3: Run the fast source-pin subset (no DB)**

Run: `make sec-test` (or `go test ./internal/security_regression/ -run Pinned`)
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/security_regression/crm_tenant_isolation_test.go
git commit -m "test(sec): CRM tenant isolation + RLS source pins"
```

---

## Task 12: Frontend — CrmService + Contacts list page (TDD)

**Files:**
- Create: `web/src/app/core/crm.service.ts`
- Create: `web/src/app/pages/crm/contacts-list.ts`
- Create: `web/src/app/pages/crm/contacts-list.spec.ts`
- Modify: `web/src/app/app.routes.ts`, `web/src/app/ui/nav.ts`

- [ ] **Step 1: Write the service**

```ts
// web/src/app/core/crm.service.ts
import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';

export interface Contact {
  id: string; tenant_root_id: string; primary_email: string;
  display_name?: string; company_id?: string; created_at: string; updated_at: string;
}
export interface Company { id: string; tenant_root_id: string; name: string; domain?: string; created_at: string; updated_at: string; }

@Injectable({ providedIn: 'root' })
export class CrmService {
  private http = inject(HttpClient);
  private base(b: string) { return `/api/v1/businesses/${b}`; }

  listContacts(b: string): Observable<{ items: Contact[]; next_cursor?: string }> {
    return this.http.get<{ items: Contact[]; next_cursor?: string }>(`${this.base(b)}/contacts`);
  }
  getContact(b: string, id: string) { return this.http.get<Contact>(`${this.base(b)}/contacts/${id}`); }
  createContact(b: string, body: { primary_email: string; display_name?: string }) { return this.http.post<Contact>(`${this.base(b)}/contacts`, body); }
  updateContact(b: string, id: string, body: Partial<{ display_name: string; company_id: string }>) { return this.http.patch<Contact>(`${this.base(b)}/contacts/${id}`, body); }
  deleteContact(b: string, id: string) { return this.http.delete<void>(`${this.base(b)}/contacts/${id}`); }
  mergeContact(b: string, winnerId: string, loserId: string) { return this.http.post<void>(`${this.base(b)}/contacts/${winnerId}/merge`, { loser_id: loserId }); }
  listCompanies(b: string) { return this.http.get<{ items: Company[]; next_cursor?: string }>(`${this.base(b)}/companies`); }
  getCompany(b: string, id: string) { return this.http.get<Company>(`${this.base(b)}/companies/${id}`); }
  createCompany(b: string, body: { name: string; domain?: string }) { return this.http.post<Company>(`${this.base(b)}/companies`, body); }
  updateCompany(b: string, id: string, body: Partial<{ name: string; domain: string }>) { return this.http.patch<Company>(`${this.base(b)}/companies/${id}`, body); }
}
```

- [ ] **Step 2: Write the failing list-page spec**

Model on `web/src/app/pages/agents/list.spec.ts`: mount the component, flush `/api/v1/businesses` then `/api/v1/businesses/b1/contacts` with one contact, assert `componentInstance.items().length === 1` and the row renders. Use `provideHttpClient()`, `provideHttpClientTesting()`, `provideRouter([])`, imports from `vitest`.

- [ ] **Step 3: Run — verify fail**

Run: `cd web && npm test -- contacts-list`
Expected: FAIL (component undefined).

- [ ] **Step 4: Implement the list page**

Mirror `web/src/app/pages/agents/list.ts`: standalone component, `imports: [FormsModule, PageHeader, EmptyState, Spinner]`, business-selector + `signal`s (`items`, `loading`, `error`, `businessId`), `ngOnInit` loads businesses then `reload()` → `crm.listContacts(b)`. Template: `.mf-table` with rows (`data-testid="contact-row"`, cells `contact-email-cell`, `contact-name-cell`), a "New contact" inline form (`data-testid="contact-new"`), and `<mf-empty-state title="No contacts yet" data-testid="contacts-empty">`. Row links to `/crm/:businessId/contacts/:id`.

- [ ] **Step 5: Register route + nav**

`app.routes.ts`: add `{ path: 'crm/contacts', canActivate: [authGuard], loadComponent: () => import('./pages/crm/contacts-list').then(m => m.ContactsListComponent) }`. `ui/nav.ts`: add `{ label: 'Contacts', route: '/crm/contacts', testid: 'nav-crm-contacts' }`.

- [ ] **Step 6: Run spec + build — verify pass**

Run: `cd web && npm test -- contacts-list && npm run build`
Expected: PASS + build OK.

- [ ] **Step 7: Commit**

```bash
git add web/src/app/core/crm.service.ts web/src/app/pages/crm/contacts-list.ts web/src/app/pages/crm/contacts-list.spec.ts web/src/app/app.routes.ts web/src/app/ui/nav.ts
git commit -m "feat(web): CRM service + contacts list page + nav"
```

---

## Task 13: Frontend — Contact detail (+ merge) + Companies pages (TDD)

**Files:**
- Create: `web/src/app/pages/crm/contact-detail.ts` + `.spec.ts`
- Create: `web/src/app/pages/crm/companies-list.ts` + `.spec.ts`
- Modify: `web/src/app/app.routes.ts`, `web/src/app/ui/nav.ts`

- [ ] **Step 1: Failing specs**

`contact-detail.spec.ts`: flush businesses + `/contacts/:id` + `/companies` (for the company picker), assert header renders email + name; assert the merge control (`data-testid="contact-merge"`) is present. `companies-list.spec.ts`: flush `/companies` with one row, assert it renders.

- [ ] **Step 2: Run — verify fail**

Run: `cd web && npm test -- contact-detail companies-list`
Expected: FAIL.

- [ ] **Step 3: Implement**

Contact detail: header (primary email, display name, company), edit form (`updateContact`), company assignment `<select>` from `listCompanies`, and a **merge** block (`data-testid="contact-merge"`: pick a loser contact by id/email → `mergeContact(thisId, loserId)` → toast + navigate back to list). Companies list: mirror contacts list (rows `data-testid="company-row"`, cells `company-name-cell`/`company-domain-cell`, new-company form, empty state). Routes: `crm/:businessId/contacts/:id` and `crm/companies` (+ `crm/:businessId/companies/:id` if a detail page is added — keep companies to list+inline-edit for slice #1). Nav: add `{ label: 'Companies', route: '/crm/companies', testid: 'nav-crm-companies' }`.

- [ ] **Step 4: Run specs + build — verify pass**

Run: `cd web && npm test -- contact-detail companies-list && npm run build`
Expected: PASS + build.

- [ ] **Step 5: Commit**

```bash
git add web/src/app/pages/crm/ web/src/app/app.routes.ts web/src/app/ui/nav.ts
git commit -m "feat(web): CRM contact detail (+ merge) + companies list"
```

---

## Task 14: Frontend — Playwright e2e + real-browser verification

**Files:**
- Create: `web/e2e/crm.spec.ts`

- [ ] **Step 1: Write the e2e spec**

Model on `web/e2e/connectors.spec.ts`: an `auth(page)` helper (localStorage token + mock `/me` + `/businesses`), then:
- Test "contacts: renders list" — mock `**/api/v1/businesses/b1/contacts` → one contact; `goto('/crm/contacts')`; assert `getByTestId('contact-email-cell')` contains the email.
- Test "contacts: merge flow" — mock contact detail + companies + a `POST **/contacts/*/merge` returning 200; drive the merge control; assert a success toast / navigation.
- Test "companies: renders list" — mock `**/companies`; `goto('/crm/companies')`; assert the row.

- [ ] **Step 2: Run the e2e spec**

Run: `cd web && npm run e2e -- e2e/crm.spec.ts`
Expected: PASS (web dev server on :4300 must be up).

- [ ] **Step 3: Real-browser pass (per CLAUDE.md UI rule)**

Drive the running app (gstack `Skill: browse`, or Playwright MCP) against :4300 with the demo login (`live-demo@manyforge.test` / `DevPassw0rd!`): open `/crm/contacts` (should show backfilled contacts from the dev DB), open a contact detail, open `/crm/companies`. Confirm no console/provider/overlay errors. Capture a screenshot.

- [ ] **Step 4: Commit**

```bash
git add web/e2e/crm.spec.ts
git commit -m "test(web): CRM e2e — contacts list, merge, companies"
```

---

## Final verification (before opening the PR)

- [ ] `export PATH="$HOME/go/bin:$PATH"`
- [ ] `go build ./...`
- [ ] `make test`
- [ ] `go test -tags integration ./internal/crm/... ./internal/inbox/... ./internal/security_regression/...`
- [ ] `make sec-test`
- [ ] `make lint` (go vet + staticcheck — NOT caught by per-package test)
- [ ] `go test -tags contract ./cmd/...` (OpenAPI drift)
- [ ] `cd web && npm run build && npm test && npm run e2e -- e2e/crm.spec.ts`
- [ ] `git diff --stat internal/platform/db/dbgen/` is minimal (sqlc v1.27.0 used)
- [ ] Real-browser pass done + screenshot
- [ ] `bd update manyforge-nwr` notes Phase A complete; open PR into `master`; after merge, branch fresh for Phase B.

## Spec-coverage self-check (this plan vs. design)

- Contacts CRUD + dedup → Tasks 4, 5. Merge → Task 5 (+ regression Task 11). Companies CRUD + auto-by-domain + denylist → Task 6 (+ backfill Task 10, seam Task 9). Inbox seam → Task 9. Tenant-wide RLS → Task 1 (+ regression Task 11). Permissions → Task 2. API + OpenAPI → Tasks 7, 8. UI (list/detail/companies/nav) → Tasks 12, 13. Backfill → Task 10. Tests (unit/integration/sec/contract/e2e/real-browser) → woven through + Final verification.
- **Deferred to Phase B (separate plan):** `activity_entry` table, in-tx recording hooks, timeline backfill, timeline UI, and Merge re-pointing activity rows (TODO marker left in Task 5).
