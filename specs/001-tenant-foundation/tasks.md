---
description: "Task list for Tenant Foundation"
---

# Tasks: Tenant Foundation

**Input**: Design documents from `specs/001-tenant-foundation/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/openapi.yaml

**Tests**: MANDATORY per Constitution Principle III (Test-First, NON-NEGOTIABLE) and the project's
global engineering rules. Every user story's test tasks are written FIRST and MUST FAIL before its
implementation tasks (Red-Green-Refactor). UI-bearing work adds a real-browser Playwright spec.

**Organization**: by user story (US1–US5) for independent implementation/testing. This is a
*foundation* feature, so Phase 2 (Foundational) is large — it carries the shared schema, isolation,
auth, and RBAC machinery every story sits on.

## Format: `[ID] [P?] [Story] Description`
- **[P]**: parallelizable (different files, no incomplete dependency)
- **[Story]**: US1–US5 (user-story phases only)
- Paths follow the modular-monolith layout in plan.md

---

## Phase 1: Setup (Shared Infrastructure)

- [X] T001 Initialize Go module + `cmd/manyforge/main.go` skeleton (Go 1.23) in `go.mod`, `cmd/manyforge/main.go`
- [ ] T002 [P] Add dependencies (chi, pgx v5, sqlc, golang-migrate, golang-jwt/jwt v5, x/crypto/argon2) in `go.mod`
- [X] T003 [P] `Makefile` with targets: `dev test lint sec-test generate migrate`
- [X] T004 [P] `.golangci.yml` + `.editorconfig`
- [X] T005 [P] `sqlc.yaml` (db/query → internal/*/store) + create `migrations/` and `db/query/`
- [X] T006 [P] `.env.example` (DB DSN, EdDSA key paths, SMTP, trusted-proxy CIDRs)
- [X] T007 [P] `docker-compose.yml` (Postgres 16) + testcontainers helper in `internal/platform/db/testdb/testdb.go`
- [X] T008 [P] CI workflow `.github/workflows/ci.yml` running `make test && make sec-test && make lint`
- [X] T009 [P] Config loader in `internal/platform/config/config.go`
- [X] T010 [P] Typed error sentinels (`ErrNotFound/ErrForbidden/ErrValidation/ErrConflict`) in `internal/platform/errs/errs.go`
- [X] T011 [P] Structured logging + `/healthz` `/readyz` `/metrics` in `internal/platform/observability/observability.go`

---

## Phase 2: Foundational (Blocking Prerequisites)

**⚠️ CRITICAL**: No user-story work begins until this phase is complete.

### Schema & migrations (forward-only)
- [X] T012 Migration: extensions (`citext`,`pgcrypto`) + `account` + `principal` (kind CHECK) in `migrations/0001_identity.sql`
- [X] T013 Migration: `business` (UNIQUE(id,tenant_root_id); master-root + immutability triggers) + `business_closure` (composite FKs, indexes) in `migrations/0002_hierarchy.sql`
- [X] T014 Migration: `permission` (seed catalog, frozen `module.action` naming) + `role` (+ presets seed) + `role_permission` in `migrations/0003_rbac.sql`
- [X] T015 Migration: `membership` (composite FKs to business + custom role, uniqueness) + agent-containment trigger + DEFERRABLE last-Owner constraint trigger in `migrations/0004_membership.sql`
- [X] T016 Migration: `invitation` + `refresh_token` + `email_suppression` in `migrations/0005_invitations_sessions.sql`
- [X] T017 Migration: `audit_entry` (nullable business/tenant, agent + correlation fields, append-only grants) + `erasure` role & redaction proc in `migrations/0006_audit.sql`
- [X] T018 Migration: enable RLS + `FORCE ROW LEVEL SECURITY` + self-deriving (`principal_id`) policies on every tenant table; create non-superuser app role; revoke BYPASSRLS in `migrations/0007_rls.sql`

### Platform services
- [X] T019 pgx pool + `WithTx` helper + `SET LOCAL manyforge.principal_id` context + acquire-time GUC assertion in `internal/platform/db/db.go`
- [ ] T020 Migration runner wired to `make migrate` + `cmd/manyforge` in `internal/platform/db/migrate.go`
- [X] T021 [P] sqlc query files for all tables in `db/query/*.sql`; run `make generate`
- [X] T022 [P] argon2id hash/verify + fixed-cost dummy-compare helper in `internal/platform/auth/password.go`
- [X] T023 [P] EdDSA key ring (`kid`) + JWT sign/verify pinning alg/iss/aud in `internal/platform/auth/jwt.go`
- [ ] T024 Refresh-token store: hash, family, rotation (`FOR UPDATE`), reuse→family revoke in `internal/platform/auth/refresh.go`
- [ ] T025 HTTP router + middleware (request-id, recover, slog, auth→principal context) + error mapping (unauthorized+unknown→404) in `internal/platform/httpx/`
- [ ] T026 Append-only audit writer (same-tx, correlation id) in `internal/platform/audit/audit.go`
- [ ] T027 [P] Mailer interface + dev(log) impl + suppression check in `internal/platform/mailer/mailer.go`
- [ ] T028 [P] Rate limiter (per-IP + per-account, Postgres-backed) + trusted-proxy IP resolution in `internal/platform/ratelimit/ratelimit.go`
- [X] T029 [P] Cursor pagination helper with max-page-size cap in `internal/platform/httpx/page.go`
- [X] T030 [P] SSRF-guarded HTTP client in `internal/platform/netsafe/client.go`
- [ ] T031 Closure-table maintenance (self-row on create; move rewrite under `pg_advisory_xact_lock`) in `internal/tenancy/closure.go`
- [X] T032 Effective-permission resolver (union over non-archived-ancestor closure; agent direct-only) in `internal/authz/resolver.go`
- [ ] T033 `RequirePermission` authz middleware using the resolver in `internal/platform/httpx/authz.go`
- [X] T034 Security-regression harness: seed adversarial tenants as the non-bypass app role in `internal/security_regression/harness_test.go`

**Checkpoint**: foundation ready — user stories can proceed.

---

## Phase 3: User Story 1 - Establish account & master business (P1) 🎯 MVP

**Goal**: sign up → verify → log in → create a master business as its Owner.
**Independent Test**: register a fresh user, verify, log in, create a master business; confirm `parent_id=null`, `tenant_root_id=id`, creator is Owner, and an audit entry exists.

### Tests (write first, MUST fail)
- [ ] T035 [P] [US1] Contract tests for `/auth/signup,/auth/verify-email,/auth/login,/auth/refresh,/auth/logout` in `internal/account/auth_contract_test.go`
- [ ] T036 [P] [US1] Contract test for `POST /businesses` (master) in `internal/tenancy/business_contract_test.go`
- [ ] T037 [P] [US1] Integration: signup→verify→login→create master→Owner recorded→audit row in `internal/account/signup_flow_test.go`

### Implementation
- [ ] T038 [P] [US1] Account store + signup service (argon2id) + hashed single-use verification token in `internal/account/service.go`, `internal/account/store.go`
- [ ] T039 [US1] Auth service: login (fixed-cost), token-pair issue, refresh, logout (family revoke) in `internal/account/auth.go` (depends T022–T024)
- [ ] T040 [US1] Account HTTP handlers (signup/verify/login/refresh/logout) wired to router in `internal/account/handler.go`
- [ ] T041 [P] [US1] Tenancy service: create master business (tenant_root_id=id, self closure row, Owner membership, audit) in one tx in `internal/tenancy/service.go`
- [ ] T042 [US1] `POST /businesses` (master) handler + validation in `internal/tenancy/handler.go`
- [ ] T043 [US1] `GET /me`, `PATCH /me` handlers in `internal/account/handler.go`
- [ ] T044 [US1] Verification gate (unverified accounts can't create business/accept invite, FR-002) in `internal/account/auth.go`

**Checkpoint**: US1 independently functional — MVP demoable.

---

## Phase 4: User Story 2 - Build the business hierarchy (P1)

**Goal**: create/nest sub-businesses; rename, move, archive, restore.
**Independent Test**: nest sub-businesses, move one, archive/restore; tree reflects changes; cycle & cross-tenant moves refused.

### Tests (write first, MUST fail)
- [ ] T045 [P] [US2] Contract tests: `POST /businesses` (sub), `GET /businesses`, `GET/PATCH /businesses/{id}`, move, archive, restore in `internal/tenancy/hierarchy_contract_test.go`
- [ ] T046 [P] [US2] Integration: nest/move/archive/restore tree correctness in `internal/tenancy/hierarchy_test.go`
- [ ] T047 [P] [US2] Concurrency matrix: create+delete, move+move overlap, move+archive, restore+delete — acyclic, non-orphaning, deterministic 409 (FR-031) in `internal/tenancy/concurrency_test.go`

### Implementation
- [ ] T048 [US2] Create sub-business (closure insert, depth cap, composite FK) under advisory lock in `internal/tenancy/service.go`
- [ ] T049 [US2] Move (cycle check, cross-tenant reject, closure rewrite) under advisory lock in `internal/tenancy/service.go`
- [ ] T050 [US2] Archive/restore subtree in `internal/tenancy/service.go`
- [ ] T051 [US2] Scoped `GET /businesses` (list) + `GET/PATCH /businesses/{id}` handlers in `internal/tenancy/handler.go`
- [ ] T052 [US2] Soft-delete business (Owner-only, confirm body, refuse active children, FR-017) in `internal/tenancy/service.go`, `handler.go`

**Checkpoint**: US1+US2 work independently.

---

## Phase 5: User Story 3 - Invite members & assign roles (P2)

**Goal**: invite by email with a role ≤ inviter's; accept (auth-bound) → scoped access; custom roles.
**Independent Test**: invite to a sub-business as Member → accept → see only that subtree; role-above-own refused; reused token → 410; custom role grants exactly its permissions (SC-009).

### Tests (write first, MUST fail)
- [ ] T053 [P] [US3] Contract tests: invitations create/list/revoke/resend, `/invitations/accept`, roles CRUD, `/permissions` in `internal/invitations/contract_test.go`, `internal/authz/roles_contract_test.go`
- [ ] T054 [P] [US3] Integration: invite→accept→scoped access; role-above-own refused (FR-023); reused token 410 in `internal/invitations/accept_test.go`
- [ ] T055 [P] [US3] Integration: custom role → assign → exactly-permitted/denied (SC-009) in `internal/authz/role_enforce_test.go`
- [ ] T056 [P] [US3] Security regression: escalation refused at assign/edit/accept (FR-023) in `internal/security_regression/escalation_test.go`

### Implementation
- [ ] T057 [P] [US3] Permission catalog seed check + `GET /permissions` (paginated) in `internal/authz/permission.go`
- [ ] T058 [P] [US3] Role service (presets + custom CRUD; tenant_root_id derived from path; superset/escalation check; delete-in-use refused, FR-025) in `internal/authz/role.go`
- [ ] T059 [US3] Roles handlers under `/businesses/{id}/roles` in `internal/authz/handler.go`
- [ ] T060 [P] [US3] Invitations store+service: create (role ≤ inviter, hashed token), list, revoke, resend (throttled) in `internal/invitations/service.go`
- [ ] T061 [US3] Accept service: auth-bound + verified + email-match + re-validate authority/role/active + single-use + membership + audit in `internal/invitations/accept.go`
- [ ] T062 [US3] Invitations + accept handlers in `internal/invitations/handler.go`
- [ ] T063 [US3] Change-member-role handler+service (members.manage, escalation, immediate) in `internal/tenancy/members.go`

**Checkpoint**: US1–US3 work independently.

---

## Phase 6: User Story 4 - Enforce scoped, isolated access (P2)

**Goal**: callers see/act only within authorized subtree; unrelated tenants invisible; revocation immediate; no oracle.
**Independent Test**: two unrelated tenants → 0% cross-visibility, cross-tenant fetch → 404; revoke → access gone next action.

### Tests (write first, MUST fail) — the trust guarantee
- [ ] T064 [P] [US4] RLS matrix: every tenant table × CRUD with absent/malformed/sideways/cross-root principal context; app-predicate AND RLS separately; as non-bypass app role in `internal/security_regression/rls_matrix_test.go`
- [ ] T065 [P] [US4] Integration: two tenants 0% cross-visibility; cross-tenant GET → 404 (SC-002/003) in `internal/security_regression/isolation_test.go`
- [ ] T066 [P] [US4] Integration: revoke member → access gone next action (SC-004) in `internal/security_regression/revocation_test.go`
- [ ] T067 [P] [US4] Oracle-timing: shape+latency indistinguishable across login miss/wrong-pw/deactivated/unverified/invalid/expired/reused invite (SC-010) in `internal/security_regression/oracle_test.go`

### Implementation
- [ ] T068 [US4] Access list `GET /businesses/{id}/members` with direct/inherited provenance (grants[] union), no cross-tenant PII (FR-030) in `internal/tenancy/members.go`
- [ ] T069 [US4] Revoke membership (members.manage, last-owner protected, immediate) in `internal/tenancy/members.go`
- [ ] T070 [US4] Leave business (FR-018, last-owner protected) in `internal/tenancy/members.go`
- [ ] T071 [US4] Oracle hardening: uniform responses + fixed-cost paths across auth/invite + rate-limit wiring in `internal/account/`, `internal/invitations/`

**Checkpoint**: isolation guarantees proven by `make sec-test`.

---

## Phase 7: User Story 5 - Manage members & audit trail (P3)

**Goal**: review access (direct/inherited), change roles, revoke, transfer ownership; every change audited.
**Independent Test**: change role/revoke/transfer; last-Owner removal → 409; every mutation produces an audit entry.

### Tests (write first, MUST fail)
- [ ] T072 [P] [US5] Contract tests: `transfer-ownership` (tenant-root only), `GET /businesses/{id}/audit` in `internal/tenancy/ownership_contract_test.go`
- [ ] T073 [P] [US5] Integration: change/revoke/transfer atomic; last-Owner → 409 (FR-014/024); all mutations audited (SC-005) in `internal/tenancy/ownership_test.go`
- [ ] T074 [P] [US5] Security regression: agent-principal containment — no cross-business reach, no admin perms (SC-011/FR-027) in `internal/security_regression/agent_containment_test.go`

### Implementation
- [ ] T075 [US5] Transfer ownership (tenant-root only, atomic under lock, deferred Owner trigger) in `internal/tenancy/ownership.go`
- [ ] T076 [US5] Audit read `GET /businesses/{id}/audit` (paginated, audit.read) in `internal/audit/handler.go` (or `internal/tenancy/audit_handler.go`)
- [ ] T077 [US5] Account lifecycle: deactivate, delete (erasure schedule + last-Owner refuse), export (FR-028) in `internal/account/lifecycle.go`
- [ ] T078 [US5] Complete auth flows: email-change request/confirm, password-reset confirm, magic-link request/consume in `internal/account/auth.go`, `handler.go`

**Checkpoint**: all user stories independently functional.

---

## Phase 8: Polish & Cross-Cutting

- [ ] T079 [P] Performance bench: listing + access-check p95 < 200 ms at 1,000 businesses / 10 levels, RLS enabled (SC-007) in `internal/tenancy/bench_test.go`
- [ ] T080 [P] Source-level CI backstop pins (ownership predicate + RLS SQL via strings.Contains) in `internal/security_regression/pins_test.go`
- [ ] T081 [P] Angular Playwright e2e: signup→master→sub→invite→scoped login in `web/e2e/foundation.spec.ts`
- [ ] T082 [P] OpenAPI drift check (generated types vs `contracts/openapi.yaml`) in CI
- [ ] T083 Docs: `README.md` (run/test), `ARCHITECTURE.md` (module map), refresh `quickstart.md` validation
- [ ] T084 Wire all modules in `cmd/manyforge/main.go`; full `make test && make sec-test && make lint` green
- [ ] T085 [P] Mark bd planning issues `manyforge-5zt.1`–`.7` resolved (decisions landed in research.md/data-model.md)

---

## Dependencies & Execution Order

- **Setup (P1)** → no deps.
- **Foundational (P2)** → depends on Setup; **blocks all user stories**. Within P2: migrations T012–T018 in order; platform services T019–T034 after their migration (many [P]).
- **US1 (P3)** → after Foundational. MVP.
- **US2 (P4)** → after Foundational (uses closure T031); independent of US1 but shares tenancy module.
- **US3 (P5)** → after Foundational; uses resolver/authz (T032/T033) + invitations.
- **US4 (P6)** → after Foundational; validates isolation built in P2; touches members (shared with US3 — coordinate `internal/tenancy/members.go`).
- **US5 (P7)** → after Foundational; ownership + audit read + lifecycle.
- **Polish (P8)** → after desired stories.

### Within each story
Tests (MUST fail first) → models/stores → services → handlers → integration.

### Parallel opportunities
- Setup: T002–T011 mostly [P].
- Foundational platform services: T021–T030 largely [P] once T019/T020 land.
- Each story's test tasks ([P]) run together; `[P]` impl tasks touch distinct files.
- Stories US2/US3/US5 can be staffed in parallel after Foundational; US4 and US3 both edit `internal/tenancy/members.go` — serialize those.

## Parallel Example: User Story 1
```
# tests first, together:
T035 [US1] auth contract tests
T036 [US1] business (master) contract test
T037 [US1] signup-flow integration test
# then [P] impl on distinct files:
T038 account service   |   T041 tenancy create-master service
```

## Implementation Strategy

- **MVP** = Phase 1 + Phase 2 + Phase 3 (US1). Stop, run `make test && make sec-test`, demo.
- **Incremental**: add US2 → US3 → US4 → US5, each independently testable, each adding value without breaking prior stories.
- **Test-first is non-negotiable** (Constitution III): no impl task starts until its story's tests exist and fail.

## Notes
- `[P]` = different files, no incomplete dependency.
- Isolation/oracle/escalation/agent-containment tests live in `internal/security_regression/` (`make sec-test`) — they are the merge gate for Principle I/II/IV.
- The seven `manyforge-5zt.*` bd issues are the design decisions; `/speckit-taskstoissues` can sync these tasks to bd/GitHub if desired.
