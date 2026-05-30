# Implementation Plan: Tenant Foundation

**Branch**: `001-tenant-foundation` | **Date**: 2026-05-29 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `specs/001-tenant-foundation/spec.md`; plan-level decisions from [plan-inputs.md](./plan-inputs.md)

## Summary

Build the multi-tenant foundation every ManyForge capability depends on: accounts + email-based
auth, a master→sub-business hierarchy, capability-catalog RBAC with tenant-scoped custom roles,
AI agents modeled as constrained first-class principals, structural tenant isolation, and an
append-only audit trail. Delivered as a Go modular monolith (`internal/` modules) over PostgreSQL
with sqlc, dual-enforced isolation (application predicate + Row-Level Security), and a closure-table
hierarchy. The Angular dashboard consumes the JSON API defined in `contracts/`.

## Technical Context

**Language/Version**: Go 1.23+ (single deployable from `cmd/manyforge`)

**Primary Dependencies**: `chi` (router/middleware), `pgx v5` (driver/pool), `sqlc` (type-safe
queries), `golang-migrate` (forward-only migrations), `golang-jwt/jwt v5` (access tokens, alg-pinned),
`golang.org/x/crypto/argon2` (password + token hashing). Frontend: Angular (latest), TypeScript,
under `web/` — consumes the API; not the focus of this backend plan.

**Storage**: PostgreSQL 16. System of record. Closure table for hierarchy; RLS on all tenant tables.

**Testing**: `go test` + `testcontainers-go` (ephemeral Postgres) for integration/contract tests;
Playwright (`web/e2e/`) for the Angular flows; `internal/security_regression/` for isolation,
privilege-escalation, and existence-oracle pins. Merge gate: `make test` + `make lint` + `make sec-test`.

**Target Platform**: Linux server (single binary, self-hostable); Postgres 16.

**Project Type**: Web service (Go API) + Angular SPA. This plan covers the backend foundation +
API contracts; the SPA implementation tracks the contracts.

**Performance Goals**: business-listing and access-checks p95 < 200 ms at 1,000 businesses / 10 levels
per tenant (SC-007). Auth endpoints fixed-cost to avoid timing oracles (SC-010).

**Constraints**: no cross-tenant leakage (Principle I); self-hostable with no mandatory external
services beyond Postgres + SMTP; rate-limiting and pagination caps built in; agents bounded to one
home business.

**Scale/Scope**: ≥1,000 businesses and ≥10 nesting levels per tenant; many tenants per deployment;
foundation only (no CRM/ticketing/inbox/feedback verticals).

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

Evaluated against the seven principles and the per-plan gates in the constitution's
*Development Workflow & Quality Gates*:

| Gate (principle) | How this plan satisfies it | Status |
|---|---|---|
| Tenant isolation (I) | `business_id` + `tenant_root_id` on every tenant table; closure-table subtree scoping; app predicate **and** RLS independently fail closed; cross-tenant move rejected. | PASS |
| Security & privacy (II) | Service-layer ownership predicates; typed `errs` sentinels; alg/iss/aud-pinned JWT; argon2id; refresh rotation + reuse detection; existence-oracle hardening; rate limits; SSRF-guarded outbound. | PASS |
| Test-first (III) | Test plan below + in `quickstart.md`: contract tests from `contracts/`, integration via testcontainers, `security_regression/` pins, Playwright for SPA. TDD per task. | PASS |
| Bounded agents (IV) | Agents are constrained principals (one home business, no inheritance, no admin perms, autonomy gate after RBAC); every agent action audited. Model built now; lifecycle is the agents feature. | PASS |
| Modular monolith (V) | One binary; `internal/{platform,account,tenancy,authz,invitations}` bounded modules; thin handlers → services; sqlc; `ee/` untouched. | PASS |
| Observability & audit (VI) | Append-only `audit_entry` written in the same tx as the change; slog with request/principal/business correlation IDs; health/metrics endpoints. | PASS |
| Open-core (VII) | All foundation code in `internal/` (MIT); no `ee/` dependency; secrets via env, never committed. | PASS |

**Result**: PASS (no violations). Complexity Tracking intentionally empty.

## Project Structure

### Documentation (this feature)

```text
specs/001-tenant-foundation/
├── plan.md              # This file
├── spec.md              # Feature spec (31 FRs, 11 SCs)
├── plan-inputs.md       # Deferred HOW decisions (consumed here)
├── research.md          # Phase 0 — decisions
├── data-model.md        # Phase 1 — schema & entities
├── quickstart.md        # Phase 1 — run + validation walkthrough
├── contracts/
│   └── openapi.yaml     # Phase 1 — HTTP API contract
└── checklists/
    └── requirements.md  # Spec quality checklist
```

### Source Code (repository root)

```text
cmd/manyforge/
└── main.go                     # wire config, db pool, router, modules; serve

internal/
├── platform/
│   ├── config/                 # env config
│   ├── db/                     # pgx pool, tx helpers, RLS context (SET LOCAL), migrate runner
│   ├── errs/                   # typed sentinels (ErrNotFound/Forbidden/Validation/Conflict)
│   ├── httpx/                  # chi router, middleware (auth, ratelimit, recover, reqid), error mapping
│   ├── auth/                   # JWT (alg-pinned), argon2id, refresh-token rotation/reuse
│   ├── audit/                  # append-only audit writer (same-tx)
│   ├── mailer/                 # outbound email iface + SMTP/dev impls, bounce suppression
│   └── ratelimit/              # per-IP + per-account limiter
├── account/                    # accounts: signup, email verify, profile, lifecycle (deactivate/delete/export)
├── tenancy/                    # business + closure hierarchy, membership, isolation/subtree scoping
├── authz/                      # principal, permission catalog, roles, role_permissions, effective-perm resolution
└── invitations/                # invitations: create/accept/expire/revoke

migrations/                     # golang-migrate, forward-only
db/query/                       # sqlc .sql query files → generated into internal/*/store
sqlc.yaml

web/                            # Angular dashboard (consumes API) — separate workstream
└── e2e/                        # Playwright specs for the foundation flows

internal/security_regression/   # cross-tenant, privilege-escalation, oracle pins (make sec-test)
```

**Structure Decision**: Modular monolith per Constitution Principle V. The foundation occupies five
`internal/` modules plus `platform/`. `tenancy` owns the business hierarchy + isolation; `authz` owns
the principal/RBAC model; `account` owns human identity + lifecycle; `invitations` is its own bounded
flow; cross-module access is via service interfaces only. `ee/` is not introduced by this feature.

## Test Plan (Constitution Principle III)

| Layer | Coverage | Location |
|---|---|---|
| Contract | Every `contracts/openapi.yaml` operation: shape, status codes, error envelope, pagination caps; assert no `403` is returned for tenant authorization | `internal/*/**_contract_test.go` (testcontainers) |
| Integration | User-story acceptance scenarios (US1–US5) end-to-end through services + DB | `internal/*/**_test.go` |
| **RLS matrix** | For **every** tenant-owned table × CRUD: rows are invisible/blocked when `manyforge.principal_id` is **absent, malformed, sideways (other tenant), or cross-root**; app predicate and self-deriving RLS tested **separately** so each fails closed on its own. Run as the **non-bypass app role** against adversarially seeded tenants. | `internal/security_regression/rls_matrix_test.go` |
| Security regression | Cross-tenant isolation (SC-002/003), existence oracle (SC-010), privilege escalation at assign/edit/accept (FR-023), agent containment (SC-011) | `internal/security_regression/` (`make sec-test`) |
| **Oracle timing** | Response **shape AND latency distribution** are indistinguishable across: login miss, wrong password, deactivated account, unverified account, invalid/expired/reused invite (SC-010) | `internal/security_regression/oracle_test.go` |
| **Concurrency matrix** | Parallel pairs stay acyclic + non-orphaning with deterministic `409`: create+delete, move+move on overlapping branches, move+archive, restore+delete, concurrent Owner demotions; closure-table integrity asserted after each (FR-031) | `internal/tenancy/concurrency_test.go` |
| Performance | Listing + access-check p95 < 200 ms at 1,000 businesses / 10 levels, **with RLS enabled** (SC-007) | `internal/tenancy/bench_test.go` |
| E2E (UI) | Sign-up→master business→sub-business→invite→scoped login (real browser) | `web/e2e/*.spec.ts` (Playwright) |

**Primary isolation verification is behavioral**: integration tests executed as the actual
non-superuser, non-BYPASSRLS app role against seeded adversarial tenants (above). Source-level pins
(`strings.Contains` / signature reflection on the ownership predicate + RLS policy SQL) are a **cheap
CI backstop only** — they prove the control wasn't silently deleted in a refactor; they do not prove
correctness and never substitute for the behavioral tests.

## Complexity Tracking

> No Constitution Check violations — table intentionally empty.

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| — | — | — |

## Phase 2 note

`/speckit-tasks` will generate `tasks.md` from this plan, the spec's user stories, `data-model.md`,
and `contracts/`. Test tasks are mandatory. The seven `manyforge-5zt.*` bd planning issues map onto
the Phase-0/1 decisions recorded in `research.md`.
