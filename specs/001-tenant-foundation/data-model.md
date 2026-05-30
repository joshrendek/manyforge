# Phase 1 Data Model: Tenant Foundation

PostgreSQL 16. Ids `uuid` (v7). Timestamps `timestamptz`. Emails `citext`. Tenant identity is enforced
by the DB (composite FKs + triggers), not by convention. Every **tenant-owned** table carries
`tenant_root_id uuid NOT NULL` and has RLS + `FORCE ROW LEVEL SECURITY` with a **self-deriving** policy
(R2). **System-catalog** tables are seeded by migration and immutable to tenants
(`-- security: system catalog`). The app connects as a non-superuser, non-BYPASSRLS role.

Legend: 🔒 tenant-owned (RLS) · 🌐 system catalog · ⚙ global identity (cross-tenant by design).

`business` declares `UNIQUE (id, tenant_root_id)` so every 🔒 child can use a **composite FK**
`(…, tenant_root_id) → business(id, tenant_root_id)` to make "same tenant" a DB invariant.

---

## Entities

### ⚙ account — human identity (FR-001/002/028)
`id PK · email citext UNIQUE · email_verified_at? · password_hash? (argon2id) · display_name ·
status[active|deactivated] · deleted_at? · created_at · updated_at`.
Global (⚙): one account → many memberships across tenants. Not RLS-scoped; the API never exposes an
account's cross-tenant memberships (FR-030).

### principal — unifying actor (FR-021/022/027)
`id PK · kind[human|agent] · account_id? FK→account · home_business_id? FK→business · tenant_root_id? ·
created_at`.
- `kind='human'`: ⚙ global; `account_id NOT NULL`; `home_business_id/tenant_root_id NULL`.
- `kind='agent'`: 🔒 tenant-bound; `home_business_id NOT NULL`; `tenant_root_id` = home business's root
  (composite FK `(home_business_id, tenant_root_id) → business(id, tenant_root_id)`); RLS-scoped.
- CHECK enforces the kind/column pairing.

### 🔒 business — hierarchy node + tenancy unit (FR-003/004/005)
`id PK · parent_id? FK · tenant_root_id NOT NULL · name · status[active|archived] · deleted_at? ·
created_at · updated_at`. `UNIQUE (id, tenant_root_id)`. Composite FK
`(parent_id, tenant_root_id) → business(id, tenant_root_id)` proves parent shares the tenant. Trigger:
master (`parent_id IS NULL`) ⇒ `tenant_root_id = id`. `tenant_root_id` is immutable (trigger rejects
UPDATE). Indexes `(tenant_root_id)`, `(parent_id)`, partial `WHERE deleted_at IS NULL`.

### 🔒 business_closure — ancestor/descendant materialization (R1, FR-006, SC-007)
`ancestor_id FK · descendant_id FK · depth int · tenant_root_id NOT NULL · PK(ancestor_id,
descendant_id)`. Composite FKs on both endpoints → `business(id, tenant_root_id)` (same tenant). CHECK
`ancestor.tenant_root_id = descendant.tenant_root_id` via the shared `tenant_root_id` column. Indexes
`(descendant_id, ancestor_id)`, `(ancestor_id, depth)`, `(tenant_root_id)`. Self-row (depth 0) on
create; move rewrite per research R1 (delete cross-boundary rows, insert new-ancestor×subtree rows,
depth-cap checked before mutation), all under the tenant advisory lock (R5).

### 🌐 permission — capability catalog (FR-007)
`key PK (module.action | module.resource.action) · module · description`. System catalog; seeded;
tenants cannot insert. Removed keys deny by default (FR-025).

### 🌐/🔒 role + role_permission (FR-007/019/020/025)
`role(id PK · tenant_root_id? · key · name · is_locked bool · created_at)`.
- `tenant_root_id IS NULL` ⇒ built-in preset (🌐, read-only to tenants): `owner` (`is_locked`, ⇒ all
  permissions), `admin`, `member`, `viewer`.
- `tenant_root_id` set ⇒ custom role (🔒, FR-019), `UNIQUE (tenant_root_id, key)`, RLS-scoped, and only
  assignable within its `tenant_root_id` (membership composite FK below).
`role_permission(role_id FK, permission_key FK) PK(role_id, permission_key)`. RLS on `role_permission`
mirrors its role's tenant (policy joins `role`); preset rows read-only to tenants. Editing a role
mutates `role_permission` transactionally and re-checks escalation (R3); deleting a role still assigned
to any membership is refused (FR-025).

### 🔒 membership — direct grant (FR-008/010/013/024)
`id PK · principal_id FK · business_id FK · role_id FK · tenant_root_id NOT NULL · granted_by? FK→
principal · granted_at · UNIQUE(principal_id, business_id)`.
Composite FKs: `(business_id, tenant_root_id) → business(id, tenant_root_id)` and, for custom roles,
`(role_id, tenant_root_id) → role(id, tenant_root_id)` — so a tenant's custom role can never be assigned
outside its tenant. Inherited access is **derived** (closure join), never stored. Agent principals: a
trigger enforces ≤1 membership, on `home_business_id` only, and rejects admin-class permissions
(FR-027).

**Owner invariant (FR-014/024, deferred constraint trigger)**: direct ownership lives on the **tenant
root**. A `DEFERRABLE INITIALLY DEFERRED` constraint trigger checks at commit that each tenant root has
≥1 membership whose role is Owner — so an atomic transfer (revoke old + grant new in one tx) passes
while a bare last-Owner removal fails. All Owner mutations run under the tenant advisory lock (R5).

### 🔒 invitation (FR-008/009)
`id PK · business_id FK · tenant_root_id NOT NULL · email citext · role_id FK · token_hash · status
[pending|accepted|expired|revoked] · created_by FK→principal · expires_at · accepted_at? · created_at`.
Composite FK to `business(id, tenant_root_id)`. State: `pending → accepted | expired | revoked`.
Acceptance is single-use and **re-validated** (R3): `UPDATE … WHERE status='pending' AND expires_at >
now()` (rows-affected gate) **and** inviter still authorized + role still ≤ inviter level + business
active; binds to the **authenticated, verified** account whose email matches (fixes review #11).

### ⚙ refresh_token — session rotation (R4)
`id PK · principal_id FK · token_hash · family_id · parent_id? · used_at? · revoked_at? · expires_at ·
created_at`. Rotation in `FOR UPDATE` tx; reuse ⇒ family revoke (recursive CTE on `parent_id`) + log.
Logout revokes the presented family.

### ⚙ email_suppression — bounce list (R6)
`email citext PK · reason · created_at`. Mailer consults before sending.

### 🔒/global audit_entry — append-only trail (FR-015/016, VI; fixes review #9/#10)
`id PK · business_id? FK→business ON DELETE SET NULL · tenant_root_id? · actor_principal_id FK→principal
· action · target_type · target_id? · inputs jsonb? · outputs jsonb? · decision text? · correlation_id
(request id) · old_value jsonb? · new_value jsonb? · created_at`.
- `business_id`/`tenant_root_id` **nullable** so global/account/security events (login, signup,
  family-revoke) are representable; business-scoped rows are RLS-scoped, global rows are admin-only.
- `inputs`/`outputs`/`decision` capture agent actions (Principle IV).
- **Append-only**: app role has no UPDATE/DELETE. Erasure runs via a restricted `erasure` role /
  `SECURITY DEFINER` proc that redacts only designated PII json keys and writes an immutable snapshot;
  ids, action, timestamps retained (FR-028). `ON DELETE SET NULL` lets a business purge proceed without
  orphaning audit history.

---

## Key relationships

```
account ─1:1─ principal(human) ─< membership >─ business ─self(parent_id)─ business
                                          │            └─< business_closure >─┘
principal(agent) ─home,1membership─ business        business UNIQUE(id, tenant_root_id) ← composite FKs
role ─< role_permission >─ permission     membership ─(role_id,tenant_root_id)→ role(custom)
business ─< invitation     business ─< audit_entry(business_id NULLABLE)     principal ─< refresh_token
```

## Invariants (DB-enforced unless noted)

- **Isolation (I)**: every 🔒 query filters `tenant_root_id` (app predicate) **and** self-deriving RLS
  on `principal_id` (R2) — independent.
- **Same-tenant**: composite FKs prove parent, closure endpoints, membership business/role, invitation,
  agent home all share `tenant_root_id`. Cross-tenant references are unrepresentable.
- **Hierarchy (FR-006)**: in-tx descendant check (no cycle); no cross-root move; depth ≤ configured max
  (≥10, FR-004).
- **Owner (FR-014/024)**: deferred constraint trigger ⇒ each tenant root keeps ≥1 Owner; transfer atomic
  under the tenant lock; ownership/transfer exposed only at the tenant root (contract).
- **No escalation (FR-023)**: superset check at assign **and** role-edit **and** invite-accept (R3).
- **Agent containment (FR-027)**: ≤1 membership on home business, no admin permissions — trigger +
  query special-case.
- **Immediate effect (FR-013/025)**: effective permissions resolved per request (no stale cache).
- **Oracle boundary (FR-026)**: unknown + unauthorized → identical 404; auth misses fixed-cost.

## Migrations (forward-only, golang-migrate)

1. extensions (`citext`, `pgcrypto`); `account`; `principal` (+ kind/column CHECK).
2. `business` (+ `UNIQUE(id, tenant_root_id)`, master/root + immutability triggers); `business_closure`
   (+ composite FKs, indexes).
3. `permission` (seed catalog, frozen naming); `role` (+ presets seed); `role_permission`.
4. `membership` (+ composite FKs to business & custom role, uniqueness; agent-containment trigger;
   deferred Owner-count constraint trigger).
5. `invitation`; `refresh_token`; `email_suppression`.
6. `audit_entry` (nullable business/tenant, agent + correlation fields; append-only grants; `erasure`
   role + redaction proc).
7. RLS `ENABLE` + self-deriving (`principal_id`-only) policies via `SECURITY DEFINER` authorization
   functions on every 🔒 table; create the non-superuser, non-BYPASSRLS app role + restricted
   `erasure` role. `FORCE ROW LEVEL SECURITY` is intentionally omitted: the app role is never a table
   owner (so `ENABLE` already applies to it), and `FORCE` would subject the `SECURITY DEFINER`
   authorization functions to RLS (policy recursion) unless their owner has BYPASSRLS. Migrations
   therefore run as a superuser/owner.

## Implementation notes (deltas found while building)

- **`one_time_token` table** (migration 0008): single-use hashed tokens for email
  verification / password reset / email change / magic link (`purpose` column).
  Auth-internal, account-level, not RLS-scoped. (Was implicit in research R4; now explicit.)
- **`principal` is intentionally NOT RLS-scoped.** Auth flows (signup, login, refresh)
  read/write principals before any principal context exists, so RLS there breaks the
  bootstrap. Cross-tenant principal exposure is prevented at the query layer instead:
  access lists join the RLS-scoped `membership` table (FR-030). `account`,
  `refresh_token`, `one_time_token` are likewise auth-internal and unscoped.
- **Tenant-table inserts avoid `INSERT ... RETURNING`.** Under RLS, RETURNING applies the
  SELECT/`USING` policy to the returned row; a freshly-created master business is not yet
  visible to its creator (no membership at insert time), so a RETURNING insert fails with
  42501. Such inserts use `:exec` and the service builds the result from inputs.
