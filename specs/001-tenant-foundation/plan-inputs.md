# Plan Inputs: Tenant Foundation

**Purpose**: Implementation-level (HOW) decisions surfaced by the Codex second-pass review
(2026-05-29) that are intentionally **out of the spec** (which stays WHAT-only) but MUST be
resolved during `/speckit-plan`. Each item maps to a spec requirement it operationalizes.

> Treat every item here as a required decision in `plan.md` / `research.md` / `data-model.md`.
> The Constitution Check gate must show how each is satisfied.

## Data model & hierarchy

- **Tenant root identity (FR-006, FR-012, Principle I)**: add an immutable `tenant_root_id`
  (the master business id) on every tenant-owned row. Subtree moves MUST preserve
  `tenant_root_id`; cross-root moves are rejected. Decide promotion/demotion (can a
  sub-business become a master? default: no in v1).
- **Hierarchy storage (FR-004, FR-005, FR-006, SC-007)**: closure table
  `(ancestor_id, descendant_id, depth, tenant_root_id)` + direct `parent_id` on `business`.
  Define index set, the transactional closure-row rewrite on move, and the cycle check
  performed inside the same transaction. Validate p95 < 200 ms at 1,000 businesses / 10 levels.
- **Tenant-owned table classification (Principle I, FR-012)**: explicitly list every table as
  tenant-owned (carries `business_id` + `tenant_root_id`) or system-catalog
  (`business_id IS NULL`, `// security: system catalog`): `business`, closure rows,
  `membership`, `principal`, `agent` (later), `role`, `role_permission`, `permission`
  (catalog = system), `invitation`, `audit_entry`.
- **Concurrency (FR-031)**: choose the serialization mechanism for move/archive/restore/delete
  on overlapping branches (e.g., `SELECT … FOR UPDATE` on the affected subtree root, or
  advisory locks keyed by `tenant_root_id`). Define the deterministic conflict response
  (409/`ErrConflict`).

## Authorization & RLS

- **RLS operational contract (Principle I, FR-011, FR-012)**: how Postgres receives the
  principal + authorized-subtree context (`SET LOCAL app.principal_id` / `app.subtree`),
  transaction boundary discipline, connection-pool hygiene (reset on checkin),
  `FORCE ROW LEVEL SECURITY`, a non-superuser app role that cannot bypass RLS, and tests
  asserting queries **fail closed** when context is missing. Application predicate AND RLS
  must each independently deny.
- **Permission-resolution algorithm (FR-010, FR-019, FR-021, FR-023)**: define effective-
  permission computation = union of permissions from all applicable direct + inherited
  (ancestor) grants, archived-ancestor handling, catalog namespacing, built-in preset
  evolution, and the query strategy that stays within SC-007 at scale. Specify the
  "no-grant-above-your-own-level" check (FR-023) precisely.
- **Agent policy gate (FR-027, Principle IV)**: the autonomy-mode gate (Mode 1 default) runs
  server-side AFTER the RBAC check; define where it lives in the request path and how Mode
  1/2/3 map to gated actions.

## Authentication & sessions

- **Token/session details (FR-001, FR-026, Constitution auth rules)**: access-token TTL,
  refresh-token rotation + reuse-detection response, session/logout revocation mechanism,
  password hashing (argon2id params) / magic-link token entropy + hashing at rest,
  email-change re-verification flow, account recovery. (Spec fixes the behaviors; plan fixes
  the parameters.)
- **Existence-oracle hardening (FR-026, SC-010)**: uniform response shapes + a fixed-cost
  path (dummy hash compare / constant work) for sign-in and sign-up misses; per-email +
  per-IP rate limiting (FR-029).

## Lifecycle, retention, abuse

- **Deletion/erasure (FR-028)**: soft-delete + retention window + purge job design; GDPR
  erasure that anonymizes PII while keeping a pseudonymized append-only audit row; legal-hold
  handling.
- **Pagination & rate limits (FR-029)**: cursor pagination + hard max page size for every
  list; concrete rate-limit buckets for auth/verify/invite/accept/hierarchy endpoints.
- **Email deliverability (FR-008, FR-009, dependency)**: outbound-email provider abstraction,
  hard-bounce suppression list, resend throttles, self-hosted SMTP config contract.

## Test plan hooks (Constitution Principle III)

- Isolation/oracle test matrix across endpoints (SC-002, SC-003, SC-010).
- Agent-containment tests (SC-011) even though agent lifecycle is a later feature — the
  principal model must already enforce FR-027.
- Concurrency tests for FR-031 (parallel overlapping moves).
- `internal/security_regression/` entries for: cross-tenant access, privilege escalation
  (FR-023), existence oracle (FR-026).
