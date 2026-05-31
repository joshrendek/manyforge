# Quickstart: Tenant Foundation

How to run the foundation locally and validate it against the spec's acceptance scenarios.

## Prerequisites

- Go 1.23+
- PostgreSQL 16 (local or Docker)
- `make`, `sqlc`, `golang-migrate`, `node` (for the Angular `web/` workspace)

## Run

```bash
cp .env.example .env            # DB DSN, JWT keypair, SMTP (dev mailer logs to stdout)
make migrate                    # apply forward-only migrations
make generate                   # sqlc → generated query code (never hand-edit)
make dev                        # start the API on :8080
# web/ (separate): cd web && npm install && npm run start   # Angular dashboard → proxies /api
```

Health: `curl localhost:8080/healthz` → `200`. Metrics: `/metrics`. Readiness: `/readyz`.

## Test (the merge gate — Constitution Principle III)

```bash
make test       # unit tests (fast, no DB): source-level security pins + OpenAPI-drift check
make int-test    # ALL integration tests (testcontainers spins ephemeral Postgres; Docker required)
make sec-test    # internal/security_regression subset: isolation, oracle, escalation, agent containment
make lint        # vet + golangci-lint
cd web && npm run e2e   # Playwright foundation flows (real browser)
```

CI runs `make test && make int-test && make lint` (`int-test` ⊇ `sec-test`); all green required to merge.

## Validation walkthrough (maps to spec acceptance scenarios)

> Run against a fresh DB. Each step is also covered by an automated test; this is the manual smoke path.

1. **US1 — account + master business**
   - `POST /api/v1/auth/signup` → `202`; consume the verification token via `POST /auth/verify-email`.
   - `POST /auth/login` → token pair. `POST /businesses {name}` (no `parent_id`) → `201`; creator is Owner.
   - ✅ Expect: business has `parent_id=null`, `tenant_root_id=id`; caller holds the Owner role.

2. **US2 — hierarchy**
   - `POST /businesses {name, parent_id}` (nest 2–3 levels). `POST /businesses/{id}/move` to reparent.
   - Try `move` under a descendant → `409` (cycle, FR-006). Try move into another tenant → `404/409`.
   - `POST /businesses/{id}/archive` then `/restore`.
   - ✅ Expect: tree reflects changes; archived branch hidden from `GET /businesses`; cycle/cross-tenant refused.

3. **US3 — invite + scoped access**
   - As a member with `members.manage`: `POST /businesses/{sub}/invitations {email, role_id}` → `202`.
   - Accept via `POST /invitations/accept {token}` (register first if new) → membership granted.
   - Re-accept the same token → `410` (single-use, FR-009). Invite a role above your own → refused.
   - ✅ Expect: invitee sees only `{sub}` + its descendants on login; nothing above/beside.

4. **US4 — isolation (the critical one)**
   - Create a second, unrelated tenant under a different account.
   - From tenant A, `GET /businesses/{B's id}` → `404` (indistinguishable from unknown, FR-011).
   - Revoke a member (`DELETE /businesses/{id}/members/{principalId}`) → their next call loses access.
   - ✅ Expect: 0% cross-tenant visibility (SC-003); revocation immediate (SC-004). `make sec-test` pins this.

5. **US5 — admin + audit**
   - `GET /businesses/{id}/members` shows each member's role + direct/inherited (+ ancestor).
   - Change a role; revoke another; transfer ownership; try to remove the last Owner → `409` (FR-014).
   - `GET /businesses/{id}/audit` shows an append-only entry for every change above.
   - ✅ Expect: every mutation audited (SC-005); last-Owner protected (SC-008).
   - **Account lifecycle (FR-028):** `GET /me/export` (data export), `POST /me/deactivate`
     (reversible), `POST /me/delete` → `202` (soft-delete + sessions revoked + 30-day erasure
     schedule). Deactivate/delete are refused `409` for the last Owner of any tenant.
   - **Auth flows:** `POST /auth/password-reset[/confirm]`, `POST /auth/magic-link[/consume]`
     (uniform `202`, no existence oracle), `POST /me/email-change[/confirm]`. All tokens single-use.

6. **RBAC + agent containment**
   - `POST /roles` to define a custom role with a narrow permission set; assign it; confirm the holder
     can do exactly those actions and is denied the rest (SC-009).
   - Agent containment is covered by `make sec-test` (SC-011) even though agent lifecycle ships later.

## Performance check (SC-007)

`go test -tags integration ./internal/tenancy -run TestSC007 -count=1` seeds 1,000 businesses / 10 levels
and asserts listing + access-check p95 < 200 ms (RLS enabled).
