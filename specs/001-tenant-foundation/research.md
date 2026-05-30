# Phase 0 Research: Tenant Foundation

Resolves the plan-level decisions in [plan-inputs.md](./plan-inputs.md). Each maps to bd issue
`manyforge-5zt.N`. Incorporates the Codex second-pass review of the plan (2026-05-29). No
`NEEDS CLARIFICATION` remain.

---

## R1. Hierarchy storage & tenant identity (manyforge-5zt.1)

**Decision**: `business` carries `parent_id` (nullable; null = master) and an immutable
`tenant_root_id` (the master's id; a master's `tenant_root_id` = its own id). `business` has a
`UNIQUE (id, tenant_root_id)` so other tables can use **composite foreign keys** to prove same-tenant
membership at the DB level. A `business_closure` table `(ancestor_id, descendant_id, depth,
tenant_root_id)` materializes every ancestor→descendant pair including self (depth 0). Subtree
reads/scoping join the closure table.

**Move algorithm** (one transaction, under the tenant advisory lock — R5):
1. Reject if `new_parent` is the node itself or any descendant (cycle): `EXISTS closure WHERE
   ancestor_id = :node AND descendant_id = :new_parent`.
2. Reject if `new_parent.tenant_root_id <> node.tenant_root_id` (no cross-tenant move).
3. Reject if resulting depth (`new_parent.depth + subtree_height`) exceeds the configured max.
4. Delete cross-boundary closure rows: pairs `(a, d)` where `d ∈ moved-subtree` and
   `a ∉ moved-subtree` (the moved subtree's old ancestor links).
5. Insert new links: for each `a ∈ ancestors(new_parent) ∪ {new_parent}` × each `d ∈
   moved-subtree`, insert `(a, d, depth(a→new_parent)+1+depth(node→d), tenant_root_id)`.
6. Subtree-internal closure rows are untouched. Update `business.parent_id`.

**Rationale**: closure tables give O(matches) subtree/ancestor reads via indexed joins (no per-read
recursive CTE), make depth explicit (cap = `depth <= N`), and meet SC-007. `tenant_root_id` on every
tenant row makes both the app predicate and RLS O(1) and enables composite-FK enforcement (R-id).

**Alternatives considered**: recursive CTE (read cost), `ltree` (move rewrites many paths, escaping),
adjacency list + app recursion (N+1). All rejected for our read-heavy access-check workload.

---

## R2. Tenant isolation: app predicate + self-deriving RLS (manyforge-5zt.2)

**Decision**: dual, independent enforcement — and **RLS does not trust an app-supplied subtree**.
1. **Application predicate**: every tenant-scoped sqlc query filters by `tenant_root_id` and the
   authorized business-id set computed in Go — never a handler-only check.
2. **Postgres RLS (self-deriving)**: every tenant-owned table has RLS + `FORCE ROW LEVEL SECURITY`.
   The app sets **only** `SET LOCAL manyforge.principal_id = <uuid>` per transaction. Policies derive
   authorization themselves:
   ```sql
   USING (EXISTS (
     SELECT 1 FROM membership m
     JOIN business_closure c ON c.ancestor_id = m.business_id
     WHERE m.principal_id = current_setting('manyforge.principal_id')::uuid
       AND c.descendant_id = <table>.business_id))
   ```
   The app connects as a **non-superuser, non-BYPASSRLS** role. A connection-acquire hook asserts no
   residual GUC. Tests assert rows return empty / error when `manyforge.principal_id` is unset
   (fail closed).

**Why the change from a trusted `authorized_subtree` GUC**: trusting an app-supplied subtree means an
application bug that sets an overbroad scope silently defeats RLS — it stops being independent
defense-in-depth. Deriving from `principal_id` alone makes RLS a true second wall: even a wrong app
predicate cannot widen what RLS returns. The Go layer still computes its own scope for the app
predicate and for efficient queries; the two layers share no trusted input beyond `principal_id`.

**Cost**: the RLS `EXISTS` adds a membership+closure semijoin per row-set; indexes
`membership(principal_id, business_id)` and `business_closure(ancestor_id, descendant_id)` keep it an
index-only probe. Benchmarked with RLS **enabled** (R-perf), not bare queries.

**Alternatives considered**: app-predicate-only (one forgotten WHERE = leak); RLS trusting a subtree
GUC (defeatable by app bug — rejected per this review); a SECURITY DEFINER access function (viable
alternative if the inline `EXISTS` underperforms — documented as the fallback).

---

## R3. RBAC: catalog, roles, principals, resolution (manyforge-5zt.3)

**Permission naming (frozen)**: `<module>.<action>` for module-level capabilities
(`members.manage`, `roles.manage`, `hierarchy.manage`, `business.delete`, `audit.read`,
`ownership.transfer`) and `<module>.<resource>.<action>` where a resource exists in later modules
(`crm.contact.read`). This convention is fixed before migrations seed the catalog (FR-007).

**Model**:
- `permission(key, module, description)` — system catalog (`-- security: system catalog`), seeded,
  immutable to tenants. Removed keys deny by default (FR-025).
- `role(id, tenant_root_id NULL=preset, key, name, is_locked)` + `role_permission`. Presets
  (Owner/Admin/Member/Viewer) are global; Owner `is_locked` ⇒ all permissions. Custom roles carry
  `tenant_root_id` (FR-019), RLS-scoped, unique `(tenant_root_id, key)`.
- `principal(id, kind, account_id?, home_business_id?)`. Human = ⚙ global. Agent = tenant-bound.
- `membership(principal_id, business_id, role_id, tenant_root_id, …)`, unique `(principal_id,
  business_id)` — direct grant. Composite FK `(business_id, tenant_root_id) → business(id,
  tenant_root_id)` and `(role_id, tenant_root_id)` for custom roles enforce same-tenant at the DB.
- **Effective permissions** for (P, B) = union of `role_permission` over every membership P holds on
  B **or any non-archived ancestor of B** (closure join). Computed per request via one indexed query;
  **not** cached across requests (FR-013/FR-025 immediate effect).

**Escalation control re-checked at every grant point** (FR-023, fixes review #7):
- **Assignment** (invite create, role change): target role's permission set ⊆ actor's effective set;
  assigning Owner / `ownership.transfer` requires actor Owner; agents rejected for admin permissions.
- **Custom-role edit**: re-validate that the editor still holds every permission the edited role
  grants; an edit that would let an agent-held role gain admin permissions is rejected.
- **Invitation acceptance**: re-validate at accept time — reject if the inviter has since lost the
  authority, the role changed incompatibly, or the target business is archived/deleted. (Authority is
  bound to the invite, re-checked on consume, not just at creation.)

**Rationale**: one authorization vocabulary for humans + agents (Principle IV); union-over-closure
matches downward-only inheritance; per-request resolution keeps "immediate effect" honest; re-checking
at edit/accept closes the time-of-check gaps the review flagged.

**Alternatives considered**: cached effective permissions (stale-grant risk — rejected for v1).

---

## R4. Authentication, sessions, oracle hardening (manyforge-5zt.4)

**Decision** — complete flows (fixes review #13):
- **Access token**: 15-min JWT, **EdDSA (Ed25519)**, parser pinned `WithValidMethods(["EdDSA"])` +
  `WithIssuer` + `WithAudience`. A **key ring** with `kid` header enables rotation; unknown/unpinned
  `kid` is rejected. Claims: `iss`, `aud`, `sub` (principal id), `kid`, `exp`.
- **Refresh token**: opaque 256-bit, stored hashed (SHA-256) with `family_id`, `parent_id`, `used_at`,
  `revoked_at`, `expires_at`. Rotation in a `FOR UPDATE` tx; reuse of a used token ⇒ revoke whole
  family (recursive CTE on `parent_id`) + structured security log. **Logout** revokes the presented
  token's family.
- **Passwords**: argon2id (m=64MB,t=3,p=1; configurable). **Password reset**: request (uniform 202) →
  hashed single-use token → **consume** endpoint sets a new hash. **Magic-link login**: request →
  hashed single-use token → **consume** issues a token pair (works for password-less accounts).
- **Email verification & change**: both gated by hashed single-use tokens; an account can't act
  (create business / accept invite) until verified (FR-002); email change re-verifies the new address
  before it becomes the login identifier.
- **Existence-oracle hardening** (FR-026, SC-010): sign-in miss runs a fixed-cost dummy argon2 compare;
  sign-up / login / password-reset / invite responses use a uniform shape and never reveal whether an
  email is registered to another account; generic `INVALID_CREDENTIALS`; per-email + per-IP limits (R6).

**Rationale**: aligns with Constitution Principle II (alg/iss/aud pinning, key rotation, rotation+reuse
detection, fixed-cost comparisons). EdDSA + `kid` ring for rotation hygiene on a platform.

---

## R5. Concurrency for structural mutations (manyforge-5zt.6)

**Decision**: a single per-tenant **advisory xact lock** `pg_advisory_xact_lock(hashtext(tenant_root_id))`
guards **all** structural and ownership mutations within a tenant: business **create**, move, archive,
restore, delete, **ownership transfer, Owner assignment, Owner demotion, Owner revocation**. (Fixes
review #5 — create can race parent delete; two owner demotions can race.) One lock key per tenant ⇒ no
lock-ordering deadlock between structural ops (they all take the same single lock). Inside the lock:
re-check invariants, mutate closure / membership, commit. A caller whose precondition changed returns
`409 ErrConflict` deterministically.

**Rationale**: structural edits are rare; per-tenant serialization removes interleaving races
(orphaning, cycles, zero-Owner) and gives a deterministic, testable conflict response (FR-031), while
preserving cross-tenant parallelism. Single lock per tenant sidesteps multi-lock deadlock entirely.

**Alternatives considered**: `SELECT … FOR UPDATE` on subtree roots (fiddlier across closure rewrites,
multi-row lock ordering risk); optimistic version + retry (more moving parts for rare ops). Rejected.

---

## R6. Lifecycle, GDPR, abuse limits (manyforge-5zt.5)

**Decision**:
- **Soft delete + retention**: `business.deleted_at`, `account.deleted_at`; hidden from normal queries;
  purge job hard-deletes after a configurable window (default 30d); business delete refused while it
  has active sub-businesses (FR-017) and requires explicit confirmation (R-contract).
- **GDPR erasure vs append-only audit** (fixes review #10): the app role has **no** UPDATE/DELETE on
  `audit_entry`. Erasure runs through a **separate, restricted `erasure` role** (or `SECURITY DEFINER`
  procedure) that may only redact designated PII json keys, writing an immutable redacted snapshot;
  the row, its id, action, and timestamps are retained (Principle VI). `audit_entry.business_id` is
  **nullable** with `ON DELETE SET NULL` (plus a retained `tenant_root_id`) so purging a business
  never orphans or blocks audit history. Global/account/security events (login, signup, family-revoke)
  write `business_id = NULL`.
- **Data export**: account export endpoint returns the account + its memberships as JSON (FR-028).
- **Pagination**: keyset/cursor on **every** list incl. roles, permissions, invitations (fixes review
  #19); hard max page size 100 (configurable), silently capped; deterministic sort keys (e.g.
  `(created_at DESC, id)`).
- **Rate limits**: token-bucket per IP and per account/email on sign-up, sign-in, verification,
  password-reset, refresh, invite create, invite accept, and hierarchy mutations; `429` surfaced in the
  contract (fixes review #20). **Trusted-proxy** config decides the client IP (never raw
  `X-Forwarded-For`); counters in Postgres with atomic upsert + periodic cleanup. No Redis dependency
  (self-hostable); pluggable Redis backend later.
- **Email**: `Mailer` interface; SMTP (prod) / log (dev); `email_suppression` for hard bounces; resend
  throttle. Any user/agent-influenced outbound URL uses the SSRF-guarded client (Principle II).

---

## R7. Agent principal containment + autonomy gate (manyforge-5zt.7)

**Decision** — enforced structurally, not just documented (fixes review #8):
- DB constraints/triggers: an `agent` principal may hold **exactly one** membership, and only on its
  `home_business_id`; `membership.tenant_root_id` must equal the home business's; a trigger rejects
  assigning any admin-class permission (`members.manage`, `roles.manage`, `hierarchy.manage`,
  `business.delete`, `ownership.transfer`, `audit.read`-write) to an agent-held role binding.
- The effective-permission query special-cases `kind='agent'` to **direct-only** (no inheritance) — in
  addition to the constraint, so both layers agree.
- **Autonomy policy record** + server-side **fail-closed gate** middleware runs **after** the RBAC
  check and before execution; default Mode 1 (sandboxed + approval for external/irreversible actions).
  Every agent action is audited with inputs/outputs/decision + correlation id (R-audit).
- Agent **lifecycle** (create/configure/run) is the separate agents feature; this plan ships the
  constrained principal model, the constraints, and the gate hook contract only.

**Rationale**: reconciles the spec with Principle IV and makes containment a DB invariant, so the
agents feature plugs into an already-bounded model.

---

## Cross-cutting choices

- **Errors**: `internal/platform/errs` sentinels; handlers map via `errors.Is`; unauthorized + unknown
  both → 404 (no oracle, FR-026); never echo wrapped errors. No `403` is ever returned for
  ownership/authorization on tenant resources (fixes review #12).
- **Migrations**: `golang-migrate`, forward-only, CI-run; sqlc generates query code (never hand-edited).
- **Observability**: `slog` JSON with `request_id` (correlation id), `principal_id`, `tenant_root_id`,
  `business_id`; `/healthz`, `/readyz`, `/metrics`.
- **Config**: 12-factor env; secrets never committed; trusted-proxy + token-key paths via config.
