# US2 — Agent Definitions CRUD — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax. The project is **TDD-mandatory** and **bd-tracked** (no TodoWrite). Commit per task. NO `Co-Authored-By` trailer. The repo HAS a remote — `git push` only at session end. `make lint` (golangci-lint 2.4.0: errcheck/govet/ineffassign/staticcheck/unused) is a MERGE GATE — 0 issues required.

**Goal:** Let an authorized human (holding `agents.configure`) CRUD business-bound **agent definitions** — provider/model/system-prompt/allowed-tools/autonomy-mode/enabled/budget — over the RLS DB, each bound to a freshly-created `kind='agent'` principal, exposed as a tenant-isolated HTTP+OpenAPI surface with no existence oracle.

**Architecture:** A new RLS-scoped `agent` table (mirroring `ai_provider_credential`/migration 0025) + an `AgentService` whose `Create` makes the agent's `kind='agent'` principal and the agent row in one `db.WithPrincipal` transaction (both via the `INSERT … SELECT FROM business` idiom so an invisible business yields a no-oracle 404). Thin chi handlers (mirroring `authz/handler.go`) gated by `httpx.RequirePermission(..., "agents.configure", businessIDFromPath)`, wired through `mountAPIRoutes`. A new `specs/003-agent-runtime/contracts/openapi.yaml` + a `drift_003_test.go` keep routes and contract in lockstep.

**Tech Stack:** Go 1.25 (`github.com/manyforge/manyforge`), `pgx/v5` + **sqlc** (`internal/platform/db/dbgen`, `make generate`), golang-migrate (`migrations/NNNN_*.{up,down}.sql` + `db/schema.sql` kept in sync — sqlc reads schema.sql, NOT migrations), chi router, `internal/platform/httpx` (errs→HTTP, `RequirePermission`, `DecodeJSON`), `internal/platform/errs` sentinels, testcontainers (`-tags integration`, `make int-test` is `-p 1`).

**Scope (locked):** Full HTTP+OpenAPI. `CreateAgent` creates the agent principal (NEW production path — none exists today). **Membership for the agent principal is deferred to US3** (the agent doesn't need to *act* yet; its principal simply has no RLS visibility until US3 binds a membership + role). `provider` reuses the existing `ai_provider` enum. A `name` column is added (the design's column list omitted it; a CRUD API needs a label). NOT in scope: the run loop, gate, approvals, credential HTTP (still service-only), any Angular UI.

---

## Background the engineer must not relearn

- **Mirror `ai_provider_credential` everywhere.** The credential store (US1a) is the template: `migrations/0025_ai_provider_credential.{up,down}.sql`, `db/query/ai.sql`, `internal/agents/credential.go`, `internal/agents/credential_integration_test.go`, `internal/agents/testsupport_integration_test.go` (the `seedAgentTenant` helper). Read these first.
- **The `INSERT … SELECT FROM business` idiom is load-bearing.** `business` is RLS-scoped; `principal` and the catalog are NOT. Inserting `SELECT b.tenant_root_id FROM business b WHERE b.id = $businessID` derives the tenant root AND gates on visibility in one shot: an invisible/foreign business returns no row → `:one` → `pgx.ErrNoRows` → `ErrNotFound` (no oracle). This is how `InsertAIProviderCredential` works (`db/query/ai.sql:11-26`).
- **`principal` table** (`migrations/0001_identity.up.sql:19-32`): `id, kind text CHECK (kind IN ('human','agent')), account_id, home_business_id, tenant_root_id, created_at`, with `principal_kind_pairing CHECK`: an **agent** ⇒ `account_id NULL AND home_business_id NOT NULL AND tenant_root_id NOT NULL`. There is **no production query that creates an agent principal** — only `CreateHumanPrincipal` exists (`db/query/account.sql`). US2 adds `CreateAgentPrincipal`. `principal` is NOT RLS-scoped (deliberately, `0007_rls.up.sql`) but IS granted to `manyforge_app`, so the insert runs fine inside `WithPrincipal`; visibility is enforced by the `SELECT FROM business` gate, not by RLS on `principal`.
- **RLS table recipe** (from 0025): `business_id`/`tenant_root_id NOT NULL`, `UNIQUE (id, tenant_root_id)`, composite FK `(business_id, tenant_root_id) REFERENCES business(id, tenant_root_id)`, index on `(business_id, tenant_root_id)`, `CREATE TRIGGER …_troot_immutable BEFORE UPDATE … EXECUTE FUNCTION support_tenant_root_immutable()` (fn defined in 0013), `GRANT … TO manyforge_app`, `ENABLE ROW LEVEL SECURITY`, `CREATE POLICY …_rls FOR ALL USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal()))) WITH CHECK (true)`. `.down.sql` reverses every step `IF EXISTS`.
- **sqlc ↔ schema.sql:** `make generate` runs `sqlc generate`, which reads `db/schema.sql` (bare table defs — NO `DEFAULT`/trigger/RLS/grant; see the credential entry at `db/schema.sql:291-306`) and `db/query/*.sql`. A new table needs BOTH `migrations/0026_*.up.sql` (runtime truth) AND a `db/schema.sql` entry (sqlc input) or sqlc won't see it.
- **sqlc type mappings:** `text[] NOT NULL` → `[]string`; `smallint` → `int16`; `integer` → `int32`; `text` → `string`; nullable `sqlc.narg('x')` → a pointer (`*string`, `*int16`, …); `ai_provider` enum → `dbgen.AiProvider`.
- **errs sentinels** (`internal/platform/errs`): `ErrNotFound`, `ErrForbidden`, `ErrValidation`, `ErrConflict`, `ErrRateLimited`. `httpx.WriteError` maps: Validation→400 (message surfaced), Conflict→409 ("conflict"), NotFound/Forbidden→**404** (no oracle), RateLimited→429, else→500 generic.
- **Permission gate:** `httpx.RequirePermission(database, permResolve, "agents.configure", businessIDFromPath)` (returns 404 — never 403 — on missing principal / invisible business / lacking perm). `businessIDFromPath` reads `chi.URLParam(r, "id")` (`cmd/manyforge/main.go:122-124`). The catalog row + role grants are seeded by a migration mirroring `0015_support_permissions.up.sql`.
- **gopls phantom diagnostics** on new files are STALE — trust `go build`/`go test`/`golangci-lint` only.

---

## File structure

| File | Responsibility |
|---|---|
| `migrations/0026_agent.up.sql` / `.down.sql` | The RLS `agent` table (mirrors 0025). |
| `migrations/0027_agent_permissions.up.sql` / `.down.sql` | `agents.configure` catalog row + owner/admin grants (mirrors 0015). |
| `db/schema.sql` (modify) | Bare `agent` table def appended (sqlc input). |
| `db/query/agent.sql` | sqlc: `CreateAgentPrincipal`, `CreateAgent`, `GetAgent`, `ListAgents`, `UpdateAgent`, `DeleteAgent`. |
| `internal/platform/db/dbgen/agent.sql.go` + `models.go` (regenerated) | **Generated** by `make generate` — never hand-edit. |
| `internal/agents/agent.go` | `AgentService`: `Create` (principal + row in one tx), `Get`/`List`/`Update`/`Delete`, `validate*`, `mapAgentErr`, `toAgent`, domain types. |
| `internal/agents/agent_test.go` | Unit: validation (pure, `DB: nil`). |
| `internal/agents/agent_integration_test.go` | `//go:build integration`: CRUD round-trip + cross-tenant no-oracle isolation via `testdb` + `seedAgentTenant`. |
| `internal/agents/agent_handler.go` | Thin chi handlers + request/response DTOs + `ProtectedRoutes` (mirrors `authz/handler.go`). |
| `internal/agents/agent_handler_test.go` | Handler-level tests (httptest): decode/validation/status mapping with a fake service. |
| `cmd/manyforge/main.go` (modify) | Construct `agents.NewHandler`, add `agentsConfigure` middleware + handler field to `apiHandlers`, mount the group in `mountAPIRoutes`. |
| `specs/003-agent-runtime/contracts/openapi.yaml` | New contract: agent paths + `Agent`/`CreateAgentRequest`/`UpdateAgentRequest` schemas. |
| `cmd/manyforge/drift_003_test.go` | `//go:build contract`: route↔contract drift + response-code pins (mirrors `drift_002_test.go`). |
| `internal/security_regression/agent_definition_ownership_pin_test.go` | Untagged source-level pin: agent queries scope by `business_id` + the table has the business-scoped RLS policy/grant (behavioral cross-tenant coverage lives in Task 6). |

---

## Task 1: Migration 0026 — the `agent` table + schema.sql entry

**Files:**
- Create: `migrations/0026_agent.up.sql`, `migrations/0026_agent.down.sql`
- Modify: `db/schema.sql`

- [ ] **Step 1: Write `migrations/0026_agent.up.sql`**

```sql
-- 0026: business-bound agent definitions (Spec 003 US2). Each agent has its own
-- kind='agent' principal (created alongside it by AgentService). RLS-scoped to the
-- owning business, mirroring ai_provider_credential (0025). provider reuses the
-- ai_provider enum from 0025.

CREATE TABLE agent (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    business_id          uuid NOT NULL,
    tenant_root_id       uuid NOT NULL,
    principal_id         uuid NOT NULL,
    name                 text NOT NULL,
    provider             ai_provider NOT NULL,
    model                text NOT NULL,
    system_prompt        text NOT NULL DEFAULT '',
    allowed_tools        text[] NOT NULL DEFAULT '{}',
    autonomy_mode        smallint NOT NULL DEFAULT 1 CHECK (autonomy_mode IN (1, 2, 3)),
    enabled              boolean NOT NULL DEFAULT true,
    monthly_budget_cents integer NOT NULL DEFAULT 0 CHECK (monthly_budget_cents >= 0),
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (business_id, name),       -- one agent name per business
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (principal_id) REFERENCES principal (id)
);
CREATE INDEX agent_business_idx ON agent (business_id, tenant_root_id);

-- tenant_root_id is immutable after insert (reuse the support trigger fn, 0013).
CREATE TRIGGER agent_troot_immutable
    BEFORE UPDATE ON agent
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

-- RLS: business-scoped, identical form to 0025 / the support tables (0014).
GRANT SELECT, INSERT, UPDATE, DELETE ON agent TO manyforge_app;

ALTER TABLE agent ENABLE ROW LEVEL SECURITY;
CREATE POLICY agent_rls ON agent FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
```

- [ ] **Step 2: Write `migrations/0026_agent.down.sql`**

```sql
-- Reverse 0026_agent.
DROP POLICY IF EXISTS agent_rls ON agent;

ALTER TABLE agent DISABLE ROW LEVEL SECURITY;

REVOKE SELECT, INSERT, UPDATE, DELETE ON agent FROM manyforge_app;

DROP TRIGGER IF EXISTS agent_troot_immutable ON agent;

DROP TABLE IF EXISTS agent;
```

- [ ] **Step 3: Append the bare `agent` table to `db/schema.sql`**

Append AFTER the `ai_provider_credential` block (around `db/schema.sql:307`). Bare def only — NO `DEFAULT`, NO triggers/RLS/grants (match the credential entry's style); KEEP the CHECK + UNIQUE + FK so sqlc's parse is faithful:

```sql

CREATE TABLE agent (
    id                   uuid PRIMARY KEY,
    business_id          uuid NOT NULL,
    tenant_root_id       uuid NOT NULL,
    principal_id         uuid NOT NULL,
    name                 text NOT NULL,
    provider             ai_provider NOT NULL,
    model                text NOT NULL,
    system_prompt        text NOT NULL,
    allowed_tools        text[] NOT NULL,
    autonomy_mode        smallint NOT NULL,
    enabled              boolean NOT NULL,
    monthly_budget_cents integer NOT NULL,
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    UNIQUE (business_id, name),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (principal_id) REFERENCES principal (id)
);
```

- [ ] **Step 4: Verify the migration applies and reverses cleanly**

Run (needs a dev DB / `MANYFORGE_DATABASE_URL`; if unavailable in this env, the testcontainers int-test in Task 6 exercises the up migration — note that and proceed):
```bash
make migrate                                   # applies 0026 (and 0027 once it exists)
psql "$MANYFORGE_DATABASE_URL" -c '\d agent'   # table present with RLS
migrate -path migrations -database "$MANYFORGE_DATABASE_URL" down 1   # reverse 0026
```
Expected: `agent` created then dropped without error. (Do NOT leave the DB rolled back — re-apply with `make migrate`.)

- [ ] **Step 5: Confirm the schema still builds for sqlc**

Run: `make generate` (it must parse `db/schema.sql` without error even before queries exist).
Expected: no error (the new table parses).

- [ ] **Step 6: Commit**

```bash
git add migrations/0026_agent.up.sql migrations/0026_agent.down.sql db/schema.sql
git commit -m "feat(db): agent table + RLS, migration 0026 (US2)"
```

---

## Task 2: Migration 0027 — `agents.configure` permission

**Files:**
- Create: `migrations/0027_agent_permissions.up.sql`, `migrations/0027_agent_permissions.down.sql`

- [ ] **Step 1: Write `migrations/0027_agent_permissions.up.sql`** (mirror `0015_support_permissions.up.sql`)

```sql
-- 0027: agent-runtime permission catalog (Spec 003). agents.configure gates agent
-- definition CRUD (and, when exposed, provider-credential CRUD — design §3.4).
-- Granted to the owner + admin presets (configuring agents is an administrative
-- action). owner is is_locked / all-permissions in the resolver but is seeded here
-- for parity with the other catalog migrations. Key/module are authoritative and
-- shared verbatim with the OpenAPI contract — do not rename.

-- security: system catalog, no tenant scoping
INSERT INTO permission (key, module, description) VALUES
    ('agents.configure', 'agents', 'Create, update, and delete agent definitions and provider credentials');

-- owner + admin ⇒ agents.configure.
INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key = 'agents.configure'
    WHERE r.tenant_root_id IS NULL AND r.key IN ('owner', 'admin');
```

- [ ] **Step 2: Write `migrations/0027_agent_permissions.down.sql`**

```sql
-- Reverse 0027_agent_permissions.
DELETE FROM role_permission WHERE permission_key = 'agents.configure';
DELETE FROM permission WHERE key = 'agents.configure';
```

- [ ] **Step 3: Verify apply + reverse**

Run: `make migrate` then confirm, then reverse-and-reapply:
```bash
psql "$MANYFORGE_DATABASE_URL" -c "SELECT key FROM permission WHERE key='agents.configure';"   # 1 row
migrate -path migrations -database "$MANYFORGE_DATABASE_URL" down 1   # reverse 0027
make migrate                                                          # reapply
```
Expected: row present after apply, gone after down, present again after reapply. (No schema.sql change — schema.sql carries no seed INSERTs.)

- [ ] **Step 4: Commit**

```bash
git add migrations/0027_agent_permissions.up.sql migrations/0027_agent_permissions.down.sql
git commit -m "feat(rbac): agents.configure permission catalog + presets, migration 0027 (US2)"
```

---

## Task 3: sqlc queries — `db/query/agent.sql` + generate

**Files:**
- Create: `db/query/agent.sql`
- Regenerate: `internal/platform/db/dbgen/*` (via `make generate`)

- [ ] **Step 1: Write `db/query/agent.sql`**

```sql
-- Agent runtime (spec 003 US2) — agent definition queries. Every query runs inside
-- the caller's RLS principal context (db.WithPrincipal) AND pushes the
-- (business_id, …) ownership predicate into SQL (dual enforcement, mirroring ai.sql).
-- tenant_root_id is derived from the business row on insert.

-- CreateAgentPrincipal creates the kind='agent' principal for a new agent, homed at
-- and tenant-scoped to the business. INSERT…SELECT FROM business gates on RLS
-- visibility: an invisible business yields no row → ErrNoRows → 404 (no oracle).
-- principal is not RLS-scoped, so the gate lives in the business SELECT.
-- name: CreateAgentPrincipal :one
INSERT INTO principal (id, kind, home_business_id, tenant_root_id, created_at)
SELECT sqlc.arg('id')::uuid, 'agent', b.id, b.tenant_root_id, now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING id;

-- CreateAgent inserts an agent definition. tenant_root_id is derived from the
-- business row (RLS-gated). Duplicate (business_id, name) → 23505 → 409.
-- name: CreateAgent :one
INSERT INTO agent (
    id, business_id, tenant_root_id, principal_id, name, provider, model,
    system_prompt, allowed_tools, autonomy_mode, enabled, monthly_budget_cents,
    created_at, updated_at)
SELECT
    sqlc.arg('id')::uuid,
    b.id,
    b.tenant_root_id,
    sqlc.arg('principal_id')::uuid,
    sqlc.arg('name'),
    sqlc.arg('provider')::ai_provider,
    sqlc.arg('model'),
    sqlc.arg('system_prompt'),
    sqlc.arg('allowed_tools')::text[],
    sqlc.arg('autonomy_mode')::smallint,
    sqlc.arg('enabled'),
    sqlc.arg('monthly_budget_cents')::integer,
    now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;

-- GetAgent loads an agent by (id, business_id) — the ownership predicate. RLS
-- scopes rows to the caller's authorized businesses; the explicit business_id is
-- defense in depth. pgx.ErrNoRows => ErrNotFound.
-- name: GetAgent :one
SELECT * FROM agent
WHERE id = $1 AND business_id = $2;

-- ListAgents lists all agents for a business, ordered by name for a stable result.
-- name: ListAgents :many
SELECT * FROM agent
WHERE business_id = $1
ORDER BY name;

-- UpdateAgent partially updates an agent (PATCH): COALESCE(narg, col) preserves any
-- field the caller omitted (narg NULL = absent). provider is immutable (not settable
-- here). No match → ErrNoRows → 404.
-- name: UpdateAgent :one
UPDATE agent SET
    name                 = COALESCE(sqlc.narg('name'), name),
    model                = COALESCE(sqlc.narg('model'), model),
    system_prompt        = COALESCE(sqlc.narg('system_prompt'), system_prompt),
    allowed_tools        = COALESCE(sqlc.narg('allowed_tools')::text[], allowed_tools),
    autonomy_mode        = COALESCE(sqlc.narg('autonomy_mode')::smallint, autonomy_mode),
    enabled              = COALESCE(sqlc.narg('enabled'), enabled),
    monthly_budget_cents = COALESCE(sqlc.narg('monthly_budget_cents')::integer, monthly_budget_cents),
    updated_at           = now()
WHERE id = sqlc.arg('id')::uuid AND business_id = sqlc.arg('business_id')::uuid
RETURNING *;

-- DeleteAgent atomically deletes the agent and its kind='agent' principal. The agent
-- row is deleted first (it FKs the principal), then the principal. rows-affected (the
-- principal delete) = 0 when the agent doesn't exist / isn't visible → 404 (no oracle).
-- name: DeleteAgent :execrows
WITH del AS (
    DELETE FROM agent WHERE id = $1 AND business_id = $2 RETURNING principal_id
)
DELETE FROM principal WHERE id IN (SELECT principal_id FROM del) AND kind = 'agent';
```

- [ ] **Step 2: Generate**

Run: `make generate`
Expected: regenerates `internal/platform/db/dbgen/agent.sql.go` with `CreateAgentPrincipal`/`CreateAgent`/`GetAgent`/`ListAgents`/`UpdateAgent`/`DeleteAgent` + a `dbgen.Agent` row model + params structs. `dbgen/models.go` gains `Agent`.

- [ ] **Step 3: Confirm it builds**

Run: `go build ./internal/platform/db/...`
Expected: clean.

- [ ] **Step 4: Inspect the generated params (sanity, do not edit)**

Run: `grep -nE "type (Create|Update)AgentParams|AllowedTools|AutonomyMode|MonthlyBudgetCents" internal/platform/db/dbgen/agent.sql.go | head`
Expected: `AllowedTools []string`, `AutonomyMode int16`, `MonthlyBudgetCents int32`; Update params use pointer types (`*string`, `*int16`, `*bool`, `*int32`) for the narg fields. (If sqlc named a bare `$1`/`$2` as `Column1` etc. in Get/List/Delete, note the exact field names for Task 5.)

- [ ] **Step 5: Commit**

```bash
git add db/query/agent.sql internal/platform/db/dbgen/
git commit -m "feat(db): agent sqlc queries + create-agent-principal (US2)"
```

---

## Task 4: `AgentService` — types, validation, and `Create` (+ principal)

**Files:**
- Create: `internal/agents/agent.go`
- Test: `internal/agents/agent_test.go`

- [ ] **Step 1: Write the failing unit test** (pure validation — `DB: nil`)

```go
package agents

import (
	"errors"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestValidateCreateAgent(t *testing.T) {
	base := CreateAgentInput{Name: "Triage Bot", Provider: "anthropic", Model: "claude-sonnet-4-5", AutonomyMode: 1, MonthlyBudgetCents: 0}
	if err := validateCreateAgent(base); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*CreateAgentInput)
	}{
		{"empty name", func(in *CreateAgentInput) { in.Name = "" }},
		{"unknown provider", func(in *CreateAgentInput) { in.Provider = "bedrock" }},
		{"empty model", func(in *CreateAgentInput) { in.Model = "" }},
		{"mode 0", func(in *CreateAgentInput) { in.AutonomyMode = 0 }},
		{"mode 4", func(in *CreateAgentInput) { in.AutonomyMode = 4 }},
		{"negative budget", func(in *CreateAgentInput) { in.MonthlyBudgetCents = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mut(&in)
			if err := validateCreateAgent(in); !errors.Is(err, errs.ErrValidation) {
				t.Fatalf("want ErrValidation, got %v", err)
			}
		})
	}
}

func TestValidateUpdateAgent(t *testing.T) {
	ptr := func(s string) *string { return &s }
	mode := func(i int) *int { return &i }
	if err := validateUpdateAgent(UpdateAgentInput{}); err != nil {
		t.Fatalf("empty patch should be valid (no-op): %v", err)
	}
	bad := []UpdateAgentInput{
		{Name: ptr("")},
		{Model: ptr("")},
		{AutonomyMode: mode(0)},
		{AutonomyMode: mode(9)},
		{MonthlyBudgetCents: func(i int) *int { return &i }(-5)},
	}
	for i, in := range bad {
		if err := validateUpdateAgent(in); !errors.Is(err, errs.ErrValidation) {
			t.Fatalf("case %d: want ErrValidation, got %v", i, err)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agents/ -run 'TestValidateCreateAgent|TestValidateUpdateAgent' -v`
Expected: FAIL — `undefined: CreateAgentInput` / `validateCreateAgent`.

- [ ] **Step 3: Write `internal/agents/agent.go`** (types + validation + Create + helpers)

```go
package agents

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// agentDB is the minimal DB surface AgentService needs — satisfied by the real
// *db.DB. An interface so unit tests can omit it (validation runs with DB nil).
type agentDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
}

// AgentService manages business-bound agent definitions over the RLS DB. Each
// Create also mints the agent's kind='agent' principal (its acting identity).
type AgentService struct {
	DB agentDB
}

// Agent is an agent definition as returned to callers.
type Agent struct {
	ID                 uuid.UUID
	BusinessID         uuid.UUID
	PrincipalID        uuid.UUID
	Name               string
	Provider           string
	Model              string
	SystemPrompt       string
	AllowedTools       []string
	AutonomyMode       int
	Enabled            bool
	MonthlyBudgetCents int
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// CreateAgentInput is the caller-supplied agent to create.
type CreateAgentInput struct {
	Name               string
	Provider           string
	Model              string
	SystemPrompt       string
	AllowedTools       []string
	AutonomyMode       int
	Enabled            bool
	MonthlyBudgetCents int
}

// UpdateAgentInput is a partial (PATCH) update — nil fields are left unchanged.
// Provider is intentionally absent: it is immutable after creation.
type UpdateAgentInput struct {
	Name               *string
	Model              *string
	SystemPrompt       *string
	AllowedTools       *[]string
	AutonomyMode       *int
	Enabled            *bool
	MonthlyBudgetCents *int
}

func validateCreateAgent(in CreateAgentInput) error {
	if in.Name == "" {
		return fmt.Errorf("agents: name required: %w", errs.ErrValidation)
	}
	if !knownProviders[in.Provider] {
		return fmt.Errorf("agents: unknown provider %q: %w", in.Provider, errs.ErrValidation)
	}
	if in.Model == "" {
		return fmt.Errorf("agents: model required: %w", errs.ErrValidation)
	}
	if in.AutonomyMode < 1 || in.AutonomyMode > 3 {
		return fmt.Errorf("agents: autonomy_mode must be 1, 2, or 3: %w", errs.ErrValidation)
	}
	if in.MonthlyBudgetCents < 0 {
		return fmt.Errorf("agents: monthly_budget_cents must be >= 0: %w", errs.ErrValidation)
	}
	return nil
}

func validateUpdateAgent(in UpdateAgentInput) error {
	if in.Name != nil && *in.Name == "" {
		return fmt.Errorf("agents: name cannot be empty: %w", errs.ErrValidation)
	}
	if in.Model != nil && *in.Model == "" {
		return fmt.Errorf("agents: model cannot be empty: %w", errs.ErrValidation)
	}
	if in.AutonomyMode != nil && (*in.AutonomyMode < 1 || *in.AutonomyMode > 3) {
		return fmt.Errorf("agents: autonomy_mode must be 1, 2, or 3: %w", errs.ErrValidation)
	}
	if in.MonthlyBudgetCents != nil && *in.MonthlyBudgetCents < 0 {
		return fmt.Errorf("agents: monthly_budget_cents must be >= 0: %w", errs.ErrValidation)
	}
	return nil
}

// toAgent maps a dbgen row into the domain Agent (narrowing int16/int32 → int).
func toAgent(r dbgen.Agent) Agent {
	tools := r.AllowedTools
	if tools == nil {
		tools = []string{}
	}
	return Agent{
		ID: r.ID, BusinessID: r.BusinessID, PrincipalID: r.PrincipalID,
		Name: r.Name, Provider: string(r.Provider), Model: r.Model,
		SystemPrompt: r.SystemPrompt, AllowedTools: tools,
		AutonomyMode: int(r.AutonomyMode), Enabled: r.Enabled,
		MonthlyBudgetCents: int(r.MonthlyBudgetCents),
		CreatedAt:          r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// Create mints the agent's kind='agent' principal and inserts the agent row in one
// RLS transaction. An invisible/foreign business → ErrNoRows (from the principal
// insert's business gate) → ErrNotFound (no oracle). Duplicate (business, name) → 409.
func (s *AgentService) Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateAgentInput) (Agent, error) {
	if err := validateCreateAgent(in); err != nil {
		return Agent{}, err
	}
	tools := in.AllowedTools
	if tools == nil {
		tools = []string{}
	}
	agentID := uuid.New()
	agentPrincipalID := uuid.New()
	var row dbgen.Agent
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		// (1) Create the agent's principal — gated on business visibility.
		if _, perr := q.CreateAgentPrincipal(ctx, dbgen.CreateAgentPrincipalParams{
			ID: agentPrincipalID, BusinessID: businessID,
		}); perr != nil {
			return perr // pgx.ErrNoRows when the business is invisible
		}
		// (2) Insert the agent row referencing that principal.
		r, aerr := q.CreateAgent(ctx, dbgen.CreateAgentParams{
			ID:                 agentID,
			PrincipalID:        agentPrincipalID,
			Name:               in.Name,
			Provider:           dbgen.AiProvider(in.Provider),
			Model:              in.Model,
			SystemPrompt:       in.SystemPrompt,
			AllowedTools:       tools,
			AutonomyMode:       int16(in.AutonomyMode),
			Enabled:            in.Enabled,
			MonthlyBudgetCents: int32(in.MonthlyBudgetCents),
			BusinessID:         businessID,
		})
		row = r
		return aerr
	})
	if err != nil {
		return Agent{}, mapAgentErr(err)
	}
	return toAgent(row), nil
}

// mapAgentErr converts a query/closure error into a stable service-layer sentinel.
// pgx.ErrNoRows → ErrNotFound (no oracle); 23505 (duplicate (business, name)) →
// ErrConflict; typed sentinels pass through; everything else wraps for a 500.
func mapAgentErr(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("agents: not found: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return fmt.Errorf("agents: duplicate agent: %w", errs.ErrConflict)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound),
		errors.Is(err, errs.ErrConflict), errors.Is(err, errs.ErrRateLimited):
		return err
	default:
		return fmt.Errorf("agents: query: %w", err)
	}
}
```

> If Task 3 Step 4 showed `CreateAgentParams`/`CreateAgentPrincipalParams` field names differing from the above (e.g. sqlc pluralization), adjust the struct-literal field names to match the generated code — `go build` is the source of truth, not this snippet.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agents/ -run 'TestValidateCreateAgent|TestValidateUpdateAgent' -v`
Expected: PASS.

- [ ] **Step 5: Build + lint**

Run: `go build ./internal/agents/ && golangci-lint run ./internal/agents/...`
Expected: clean, `0 issues.` (Create/Update full surface lands in Task 5; this compiles standalone — `validateUpdateAgent`/`toAgent` are used by Task 5 but ALSO by this file's tests indirectly; if `unused` flags `validateUpdateAgent` or `toAgent` here, that's expected to clear in Task 5 — to keep THIS commit lint-clean, this task's test already references `validateUpdateAgent`, and `toAgent` is used by `Create`. So both are used. Verify `0 issues.`)

- [ ] **Step 6: Commit**

```bash
git add internal/agents/agent.go internal/agents/agent_test.go
git commit -m "feat(agents): AgentService Create (+ agent principal) + validation (US2)"
```

---

## Task 5: `AgentService` — `Get`, `List`, `Update`, `Delete`

**Files:**
- Modify: `internal/agents/agent.go`

- [ ] **Step 1: Add the read/update/delete methods** (append to `agent.go`)

```go
// Get loads one agent by (id, business_id). RLS + the explicit business_id predicate
// make a foreign/unknown id indistinguishable (no oracle). pgx.ErrNoRows → 404.
func (s *AgentService) Get(ctx context.Context, principalID, businessID, agentID uuid.UUID) (Agent, error) {
	var row dbgen.Agent
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).GetAgent(ctx, dbgen.GetAgentParams{ID: agentID, BusinessID: businessID})
		row = r
		return qerr
	})
	if err != nil {
		return Agent{}, mapAgentErr(err)
	}
	return toAgent(row), nil
}

// List returns all agents for a business, ordered by name.
func (s *AgentService) List(ctx context.Context, principalID, businessID uuid.UUID) ([]Agent, error) {
	var rows []dbgen.Agent
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).ListAgents(ctx, businessID)
		rows = r
		return qerr
	})
	if err != nil {
		return nil, mapAgentErr(err)
	}
	out := make([]Agent, 0, len(rows))
	for _, r := range rows {
		out = append(out, toAgent(r))
	}
	return out, nil
}

// Update applies a partial change. Omitted (nil) fields are preserved via COALESCE
// in SQL. No matching (id, business_id) → ErrNoRows → 404 (no oracle).
func (s *AgentService) Update(ctx context.Context, principalID, businessID, agentID uuid.UUID, in UpdateAgentInput) (Agent, error) {
	if err := validateUpdateAgent(in); err != nil {
		return Agent{}, err
	}
	params := dbgen.UpdateAgentParams{ID: agentID, BusinessID: businessID}
	params.Name = in.Name
	params.Model = in.Model
	params.SystemPrompt = in.SystemPrompt
	if in.AllowedTools != nil {
		params.AllowedTools = *in.AllowedTools
	}
	if in.AutonomyMode != nil {
		m := int16(*in.AutonomyMode)
		params.AutonomyMode = &m
	}
	params.Enabled = in.Enabled
	if in.MonthlyBudgetCents != nil {
		c := int32(*in.MonthlyBudgetCents)
		params.MonthlyBudgetCents = &c
	}
	var row dbgen.Agent
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).UpdateAgent(ctx, params)
		row = r
		return qerr
	})
	if err != nil {
		return Agent{}, mapAgentErr(err)
	}
	return toAgent(row), nil
}

// Delete removes an agent and its agent principal atomically. rows-affected 0 (the
// agent didn't exist / wasn't visible) → ErrNotFound (no oracle).
func (s *AgentService) Delete(ctx context.Context, principalID, businessID, agentID uuid.UUID) error {
	var affected int64
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		n, qerr := dbgen.New(tx).DeleteAgent(ctx, dbgen.DeleteAgentParams{ID: agentID, BusinessID: businessID})
		affected = n
		return qerr
	})
	if err != nil {
		return mapAgentErr(err)
	}
	if affected == 0 {
		return fmt.Errorf("agents: not found: %w", errs.ErrNotFound)
	}
	return nil
}
```

> **Generated-name caveat:** `GetAgentParams`/`DeleteAgentParams` field names follow sqlc's naming of `$1`/`$2`. If Task 3 Step 4 showed bare positional args named `ID`/`BusinessID`, the above is correct. If sqlc emitted a 2-arg method signature instead of a Params struct (it does this for `:one`/`:many`/`:execrows` with ≤? args — verify), call it positionally instead, e.g. `GetAgent(ctx, agentID, businessID)` and `DeleteAgent(ctx, agentID, businessID)`. Match the generated signature exactly; `go build` decides.

- [ ] **Step 2: Build + lint + the existing unit tests**

Run: `go build ./internal/agents/ && go test ./internal/agents/ && golangci-lint run ./internal/agents/...`
Expected: build clean, unit tests pass, `0 issues.`

- [ ] **Step 3: Commit**

```bash
git add internal/agents/agent.go
git commit -m "feat(agents): AgentService Get/List/Update/Delete over RLS (US2)"
```

---

## Task 6: Integration test — CRUD round-trip + cross-tenant no-oracle isolation

**Files:**
- Create: `internal/agents/agent_integration_test.go`

This mirrors `internal/agents/credential_integration_test.go` and reuses `seedAgentTenant` (from `internal/agents/testsupport_integration_test.go`). The caller principal is the seed's RLS-authorized principal (it has a membership → `authorized_businesses` includes its home business), which is all the *service* layer needs (permission gating is the HTTP layer's job).

- [ ] **Step 1: Write the failing integration test**

```go
//go:build integration

package agents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestAgentCRUDRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	seed := seedAgentTenant(ctx, t, tdb)
	svc := &AgentService{DB: tdb.App}

	// Create
	created, err := svc.Create(ctx, seed.principalID, seed.businessID, CreateAgentInput{
		Name: "Triage Bot", Provider: "anthropic", Model: "claude-sonnet-4-5",
		SystemPrompt: "Be helpful.", AllowedTools: []string{"get_ticket", "set_priority"},
		AutonomyMode: 1, Enabled: true, MonthlyBudgetCents: 5000,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == uuid.Nil || created.PrincipalID == uuid.Nil {
		t.Fatalf("Create returned empty ids: %+v", created)
	}
	if created.Provider != "anthropic" || created.AutonomyMode != 1 || len(created.AllowedTools) != 2 {
		t.Fatalf("Create round-trip mismatch: %+v", created)
	}

	// The created principal is a kind='agent' principal homed at the business.
	var kind string
	var home uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		`SELECT kind, home_business_id FROM principal WHERE id = $1`, created.PrincipalID,
	).Scan(&kind, &home); err != nil {
		t.Fatalf("read agent principal: %v", err)
	}
	if kind != "agent" || home != seed.businessID {
		t.Fatalf("agent principal kind=%q home=%v, want agent/%v", kind, home, seed.businessID)
	}

	// Get
	got, err := svc.Get(ctx, seed.principalID, seed.businessID, created.ID)
	if err != nil || got.Name != "Triage Bot" {
		t.Fatalf("Get: %+v err=%v", got, err)
	}

	// List
	list, err := svc.List(ctx, seed.principalID, seed.businessID)
	if err != nil || len(list) != 1 {
		t.Fatalf("List: %d items err=%v", len(list), err)
	}

	// Update (PATCH semantics: only model + enabled; name/tools preserved)
	model := "claude-opus-4-1"
	enabled := false
	upd, err := svc.Update(ctx, seed.principalID, seed.businessID, created.ID, UpdateAgentInput{
		Model: &model, Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd.Model != "claude-opus-4-1" || upd.Enabled || upd.Name != "Triage Bot" || len(upd.AllowedTools) != 2 {
		t.Fatalf("Update did not apply PATCH semantics: %+v", upd)
	}

	// Duplicate name → conflict
	if _, err := svc.Create(ctx, seed.principalID, seed.businessID, CreateAgentInput{
		Name: "Triage Bot", Provider: "openai", Model: "gpt-4o", AutonomyMode: 1,
	}); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("duplicate name: want ErrConflict, got %v", err)
	}

	// Delete (removes agent + its principal)
	if err := svc.Delete(ctx, seed.principalID, seed.businessID, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(ctx, seed.principalID, seed.businessID, created.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("Get after delete: want ErrNotFound, got %v", err)
	}
	var principalCount int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM principal WHERE id = $1`, created.PrincipalID,
	).Scan(&principalCount); err != nil {
		t.Fatalf("count principal: %v", err)
	}
	if principalCount != 0 {
		t.Fatalf("agent principal not deleted with agent (count=%d)", principalCount)
	}
}

func TestAgentCrossTenantNoOracle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	a := seedAgentTenant(ctx, t, tdb)
	b := seedAgentTenant(ctx, t, tdb)
	svc := &AgentService{DB: tdb.App}

	created, err := svc.Create(ctx, a.principalID, a.businessID, CreateAgentInput{
		Name: "A Bot", Provider: "anthropic", Model: "claude-sonnet-4-5", AutonomyMode: 1,
	})
	if err != nil {
		t.Fatalf("seed tenant A agent: %v", err)
	}

	// Tenant B resolving tenant A's real agent id + A's real business id → not-found
	// (RLS excludes A's rows from B's principal). Same shape as an unknown id.
	if _, err := svc.Get(ctx, b.principalID, a.businessID, created.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Get: want ErrNotFound (no oracle), got %v", err)
	}
	if err := svc.Delete(ctx, b.principalID, a.businessID, created.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Delete: want ErrNotFound (no oracle), got %v", err)
	}
	// Tenant B cannot CREATE an agent in tenant A's business (business invisible) →
	// the principal-insert business gate yields ErrNoRows → ErrNotFound.
	if _, err := svc.Create(ctx, b.principalID, a.businessID, CreateAgentInput{
		Name: "Intruder", Provider: "anthropic", Model: "claude-sonnet-4-5", AutonomyMode: 1,
	}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Create: want ErrNotFound (no oracle), got %v", err)
	}
	// Tenant A's agent is untouched.
	list, err := svc.List(ctx, a.principalID, a.businessID)
	if err != nil || len(list) != 1 {
		t.Fatalf("tenant A list after intrusion attempts: %d err=%v", len(list), err)
	}
}
```

- [ ] **Step 2: Run it (testcontainers; Docker required)**

Run: `go test -tags integration ./internal/agents/ -run 'TestAgentCRUDRoundTrip|TestAgentCrossTenantNoOracle' -v`
Expected: PASS. (If `seedAgentTenant`'s return field names differ — confirm via `grep -n "func seedAgentTenant\|type agentSeed" internal/agents/testsupport_integration_test.go` — adjust `seed.principalID`/`seed.businessID` accordingly.)

- [ ] **Step 3: Commit**

```bash
git add internal/agents/agent_integration_test.go
git commit -m "test(agents): agent CRUD round-trip + cross-tenant no-oracle isolation (US2)"
```

---

## Task 7: HTTP handler + DTOs

**Files:**
- Create: `internal/agents/agent_handler.go`
- Test: `internal/agents/agent_handler_test.go`

- [ ] **Step 1: Write the failing handler test** (httptest + a fake service via a small interface)

First, the handler depends on an interface so the test can inject a fake. Add this to the test:

```go
package agents

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// fakeAgentSvc implements agentCRUD for handler tests (no DB).
type fakeAgentSvc struct {
	created   Agent
	createErr error
	got       Agent
	getErr    error
}

func (f *fakeAgentSvc) Create(context.Context, uuid.UUID, uuid.UUID, CreateAgentInput) (Agent, error) {
	return f.created, f.createErr
}
func (f *fakeAgentSvc) Get(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (Agent, error) {
	return f.got, f.getErr
}
func (f *fakeAgentSvc) List(context.Context, uuid.UUID, uuid.UUID) ([]Agent, error) {
	return []Agent{f.got}, nil
}
func (f *fakeAgentSvc) Update(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, UpdateAgentInput) (Agent, error) {
	return f.got, nil
}
func (f *fakeAgentSvc) Delete(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error { return nil }

// newAgentTestRing builds an in-memory Ed25519 key ring (no DB / no network) — the
// codebase's standard way to authenticate handler tests (see
// internal/ticketing/oracle_integration_test.go and internal/account/http_test.go).
func newAgentTestRing(t *testing.T) *auth.KeyRing {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	ring, err := auth.NewKeyRing("manyforge", "manyforge-api", "k1", priv, map[string]ed25519.PublicKey{"k1": pub})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	return ring
}

func mintBearer(t *testing.T, ring *auth.KeyRing, pid uuid.UUID) string {
	t.Helper()
	tok, err := ring.Sign(pid, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}

// serveAgent mounts the handler behind the real auth chain (AuthToPrincipal +
// RequireAuth) and serves one request, returning the recorder.
func serveAgent(h *Handler, ring *auth.KeyRing, method, target, bearer string, body io.Reader) *httptest.ResponseRecorder {
	mux := httpx.NewRouter(ring)
	mux.Group(func(pr chi.Router) {
		pr.Use(httpx.RequireAuth)
		h.ProtectedRoutes(pr)
	})
	req := httptest.NewRequest(method, target, body)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestCreateAgentHandler_Created(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	svc := &fakeAgentSvc{created: Agent{
		ID: uuid.New(), BusinessID: bid, PrincipalID: uuid.New(), Name: "Bot",
		Provider: "anthropic", Model: "claude-sonnet-4-5", AllowedTools: []string{},
		AutonomyMode: 1, Enabled: true,
	}}
	h := NewHandler(svc)
	body, _ := json.Marshal(map[string]any{"name": "Bot", "provider": "anthropic", "model": "claude-sonnet-4-5", "autonomy_mode": 1})
	rec := serveAgent(h, ring, http.MethodPost, "/businesses/"+bid.String()+"/agents", mintBearer(t, ring, uuid.New()), bytes.NewReader(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp agentResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp.Name != "Bot" || resp.Provider != "anthropic" || resp.AllowedTools == nil {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestCreateAgentHandler_Unauthenticated(t *testing.T) {
	ring := newAgentTestRing(t)
	h := NewHandler(&fakeAgentSvc{})
	rec := serveAgent(h, ring, http.MethodPost, "/businesses/"+uuid.New().String()+"/agents", "", bytes.NewReader([]byte(`{}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no bearer)", rec.Code)
	}
}

func TestCreateAgentHandler_BadJSON(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	h := NewHandler(&fakeAgentSvc{})
	rec := serveAgent(h, ring, http.MethodPost, "/businesses/"+bid.String()+"/agents", mintBearer(t, ring, uuid.New()), bytes.NewReader([]byte("not json")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestGetAgentHandler_NotFound(t *testing.T) {
	ring := newAgentTestRing(t)
	bid := uuid.New()
	h := NewHandler(&fakeAgentSvc{getErr: errs.ErrNotFound})
	rec := serveAgent(h, ring, http.MethodGet, "/businesses/"+bid.String()+"/agents/"+uuid.New().String(), mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestGetAgentHandler_BadBusinessID(t *testing.T) {
	ring := newAgentTestRing(t)
	h := NewHandler(&fakeAgentSvc{})
	rec := serveAgent(h, ring, http.MethodGet, "/businesses/not-a-uuid/agents/"+uuid.New().String(), mintBearer(t, ring, uuid.New()), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no oracle on malformed id)", rec.Code)
	}
}
```

> **Verify these helper signatures against the codebase** before relying on them (they mirror `internal/ticketing/oracle_integration_test.go` `mintBearer`/`newTicketReadRouter` and `internal/account/http_test.go`): `auth.NewKeyRing(issuer, audience, activeKID string, signing ed25519.PrivateKey, verify map[string]ed25519.PublicKey) (*auth.KeyRing, error)`; `(*auth.KeyRing).Sign(principalID uuid.UUID, ttl time.Duration, now time.Time) (string, error)`; `httpx.NewRouter(ring) chi.Router` whose middleware chain already includes `AuthToPrincipal`. There is intentionally **no exported principal-context setter** — the context key is unexported, so authenticate via a minted Bearer token as shown. If `NewRouter` returns a concrete `*chi.Mux`, `.Group` still works (it implements `chi.Router`). This test is UNTAGGED (no DB/Docker — just in-memory crypto + httptest), so it runs in `make test`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agents/ -run 'AgentHandler' -v`
Expected: FAIL — `undefined: Handler` / `NewHandler` / `agentResp`.

- [ ] **Step 3: Write `internal/agents/agent_handler.go`** (mirror `internal/authz/handler.go`)

```go
package agents

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// agentCRUD is the subset of AgentService the handler needs (an interface so
// handler tests can inject a fake). *AgentService satisfies it.
type agentCRUD interface {
	Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateAgentInput) (Agent, error)
	Get(ctx context.Context, principalID, businessID, agentID uuid.UUID) (Agent, error)
	List(ctx context.Context, principalID, businessID uuid.UUID) ([]Agent, error)
	Update(ctx context.Context, principalID, businessID, agentID uuid.UUID, in UpdateAgentInput) (Agent, error)
	Delete(ctx context.Context, principalID, businessID, agentID uuid.UUID) error
}

// Handler exposes agent-definition CRUD over HTTP. Mounted behind the
// agents.configure RequirePermission gate (so a lacking perm / invisible business
// is a no-oracle 404).
type Handler struct{ svc agentCRUD }

// NewHandler builds an agents HTTP handler.
func NewHandler(svc agentCRUD) *Handler { return &Handler{svc: svc} }

// ProtectedRoutes mounts authenticated agent endpoints under a business.
func (h *Handler) ProtectedRoutes(r chi.Router) {
	r.Route("/businesses/{id}/agents", func(r chi.Router) {
		r.Get("/", h.listAgents)
		r.Post("/", h.createAgent)
		r.Get("/{agentID}", h.getAgent)
		r.Patch("/{agentID}", h.updateAgent)
		r.Delete("/{agentID}", h.deleteAgent)
	})
}

func agentBusinessID(r *http.Request) (uuid.UUID, error) { return uuid.Parse(chi.URLParam(r, "id")) }
func agentPathID(r *http.Request) (uuid.UUID, error)     { return uuid.Parse(chi.URLParam(r, "agentID")) }

// agentResp is the OpenAPI Agent response shape.
type agentResp struct {
	ID                 string    `json:"id"`
	BusinessID         string    `json:"business_id"`
	PrincipalID        string    `json:"principal_id"`
	Name               string    `json:"name"`
	Provider           string    `json:"provider"`
	Model              string    `json:"model"`
	SystemPrompt       string    `json:"system_prompt"`
	AllowedTools       []string  `json:"allowed_tools"`
	AutonomyMode       int       `json:"autonomy_mode"`
	Enabled            bool      `json:"enabled"`
	MonthlyBudgetCents int       `json:"monthly_budget_cents"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func toAgentResp(a Agent) agentResp {
	tools := a.AllowedTools
	if tools == nil {
		tools = []string{}
	}
	return agentResp{
		ID: a.ID.String(), BusinessID: a.BusinessID.String(), PrincipalID: a.PrincipalID.String(),
		Name: a.Name, Provider: a.Provider, Model: a.Model, SystemPrompt: a.SystemPrompt,
		AllowedTools: tools, AutonomyMode: a.AutonomyMode, Enabled: a.Enabled,
		MonthlyBudgetCents: a.MonthlyBudgetCents, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
}

func (h *Handler) listAgents(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := agentBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	agents, err := h.svc.List(r.Context(), pid, bid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]agentResp, 0, len(agents))
	for _, a := range agents {
		out = append(out, toAgentResp(a))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *Handler) createAgent(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := agentBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	// autonomy_mode and enabled default (1 / true) when omitted, matching the DB
	// defaults — pointer fields distinguish "omitted" from an explicit value.
	var in struct {
		Name               string   `json:"name"`
		Provider           string   `json:"provider"`
		Model              string   `json:"model"`
		SystemPrompt       string   `json:"system_prompt"`
		AllowedTools       []string `json:"allowed_tools"`
		AutonomyMode       *int     `json:"autonomy_mode"`
		Enabled            *bool    `json:"enabled"`
		MonthlyBudgetCents int      `json:"monthly_budget_cents"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	mode := 1
	if in.AutonomyMode != nil {
		mode = *in.AutonomyMode
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	created, err := h.svc.Create(r.Context(), pid, bid, CreateAgentInput{
		Name: in.Name, Provider: in.Provider, Model: in.Model, SystemPrompt: in.SystemPrompt,
		AllowedTools: in.AllowedTools, AutonomyMode: mode, Enabled: enabled,
		MonthlyBudgetCents: in.MonthlyBudgetCents,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toAgentResp(created))
}

func (h *Handler) getAgent(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := agentBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	aid, err := agentPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	a, err := h.svc.Get(r.Context(), pid, bid, aid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toAgentResp(a))
}

func (h *Handler) updateAgent(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := agentBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	aid, err := agentPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	// Pointer fields distinguish "absent" from "set" for PATCH semantics.
	var in struct {
		Name               *string   `json:"name"`
		Model              *string   `json:"model"`
		SystemPrompt       *string   `json:"system_prompt"`
		AllowedTools       *[]string `json:"allowed_tools"`
		AutonomyMode       *int      `json:"autonomy_mode"`
		Enabled            *bool     `json:"enabled"`
		MonthlyBudgetCents *int      `json:"monthly_budget_cents"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	a, err := h.svc.Update(r.Context(), pid, bid, aid, UpdateAgentInput{
		Name: in.Name, Model: in.Model, SystemPrompt: in.SystemPrompt,
		AllowedTools: in.AllowedTools, AutonomyMode: in.AutonomyMode,
		Enabled: in.Enabled, MonthlyBudgetCents: in.MonthlyBudgetCents,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toAgentResp(a))
}

func (h *Handler) deleteAgent(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := agentBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	aid, err := agentPathID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	if err := h.svc.Delete(r.Context(), pid, bid, aid); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

> `*AgentService` must satisfy `agentCRUD`. Its methods (Tasks 4–5) match the interface exactly. The handler depends on the interface, not the concrete type, so the fake works.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agents/ -run 'AgentHandler' -v`
Expected: PASS (5 tests: Created, Unauthenticated, BadJSON, NotFound, BadBusinessID). If a helper signature differs from the verify-note above, match the real `auth`/`httpx` API — `go build` decides.

- [ ] **Step 5: Build + lint**

Run: `go build ./internal/agents/ && go test ./internal/agents/ && golangci-lint run ./internal/agents/...`
Expected: clean, `0 issues.`

- [ ] **Step 6: Commit**

```bash
git add internal/agents/agent_handler.go internal/agents/agent_handler_test.go
git commit -m "feat(agents): agent CRUD HTTP handler + DTOs (US2)"
```

---

## Task 8: Wire the handler + permission gate into `main.go`

**Files:**
- Modify: `cmd/manyforge/main.go`

- [ ] **Step 1: Construct the AgentService + handler** (near the other handler construction, after `ticketH := ticketing.NewHandler(...)` ~`main.go:119`)

```go
	agentSvc := &agents.AgentService{DB: database}
	agentH := agents.NewHandler(agentSvc)
```

Add the import `"github.com/manyforge/manyforge/internal/agents"` to the import block (it is not yet imported — confirm with `grep -n 'internal/agents' cmd/manyforge/main.go`).

- [ ] **Step 2: Add the handler + middleware fields to `apiHandlers`** (the struct at `main.go:364`)

```go
	agents        *agents.Handler
	// agentsConfigure gates the US2 agent-definition CRUD slice on the
	// agents.configure permission, same RLS-bound 404-on-lacking-perm shape as the
	// other groups.
	agentsConfigure func(http.Handler) http.Handler
```

- [ ] **Step 3: Populate them in the `mountAPIRoutes(mux, apiHandlers{...})` literal** (~`main.go:286`)

```go
		agents:          agentH,
		agentsConfigure: httpx.RequirePermission(database, permResolve, "agents.configure", businessIDFromPath),
```

- [ ] **Step 4: Mount the gated group inside `mountAPIRoutes`** (inside the `pr.Group(func(pr chi.Router){...})` authenticated block, after the inbox-management group ~`main.go:463`)

```go
			// US2 agent-definition slice: CRUD agents under a business, gated on
			// agents.configure (migration-0027 catalog). Same RLS-bound
			// 404-on-lacking-perm semantics as the other groups.
			pr.Group(func(ag chi.Router) {
				ag.Use(h.agentsConfigure)
				h.agents.ProtectedRoutes(ag)
			})
```

- [ ] **Step 5: Build + vet + lint**

Run: `go build ./... && go vet ./... && golangci-lint run ./cmd/... ./internal/agents/...`
Expected: clean, `0 issues.`

- [ ] **Step 6: Confirm the routes are registered** (untagged drift test still green; the new routes will be "undocumented" until Task 9/10 — that's caught by drift_003, not the 001/002 tests, because the agent paths aren't on the 001/002 surface)

Run: `go test ./cmd/manyforge/ 2>&1 | tail -5`
Expected: PASS (the untagged `TestOpenAPIDrift` only governs the 001 surface; agent routes under `/businesses/{}/agents` are not 001/002 ops, so they don't trip it).

- [ ] **Step 7: Commit**

```bash
git add cmd/manyforge/main.go
git commit -m "feat(agents): wire agent CRUD routes behind agents.configure gate (US2)"
```

---

## Task 9: OpenAPI contract — `specs/003-agent-runtime/contracts/openapi.yaml`

**Files:**
- Create: `specs/003-agent-runtime/contracts/openapi.yaml`

- [ ] **Step 1: Write the contract** (mirror the 002 style: `openapi: 3.1.0`, bearer security, `/businesses/{id}/...` paths)

```yaml
openapi: 3.1.0
info:
  title: manyforge Agent Runtime API
  version: 0.1.0
  description: >
    Spec 003 — Agent Runtime & AI Gateway. US2 surface: business-bound agent
    definition CRUD, gated by the agents.configure permission. Authorization and
    existence are indistinguishable — a lacking permission or an invisible business
    returns the same 404 as an unknown id (no existence oracle).
servers:
  - url: /api/v1
security:
  - bearerAuth: []
paths:
  /businesses/{id}/agents:
    parameters:
      - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
    get:
      operationId: listAgents
      summary: List agent definitions for a business
      responses:
        "200":
          description: Agent list
          content:
            application/json:
              schema: { $ref: "#/components/schemas/AgentList" }
        "404": { $ref: "#/components/responses/NotFound" }
    post:
      operationId: createAgent
      summary: Create an agent definition (creates its agent principal)
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: "#/components/schemas/CreateAgentRequest" }
      responses:
        "201":
          description: Created agent
          content:
            application/json:
              schema: { $ref: "#/components/schemas/Agent" }
        "400": { $ref: "#/components/responses/ValidationError" }
        "404": { $ref: "#/components/responses/NotFound" }
        "409": { $ref: "#/components/responses/Conflict" }
  /businesses/{id}/agents/{agentID}:
    parameters:
      - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      - { name: agentID, in: path, required: true, schema: { type: string, format: uuid } }
    get:
      operationId: getAgent
      summary: Get one agent definition
      responses:
        "200":
          description: Agent
          content:
            application/json:
              schema: { $ref: "#/components/schemas/Agent" }
        "404": { $ref: "#/components/responses/NotFound" }
    patch:
      operationId: updateAgent
      summary: Partially update an agent definition
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: "#/components/schemas/UpdateAgentRequest" }
      responses:
        "200":
          description: Updated agent
          content:
            application/json:
              schema: { $ref: "#/components/schemas/Agent" }
        "400": { $ref: "#/components/responses/ValidationError" }
        "404": { $ref: "#/components/responses/NotFound" }
        "409": { $ref: "#/components/responses/Conflict" }
    delete:
      operationId: deleteAgent
      summary: Delete an agent definition and its agent principal
      responses:
        "204": { description: Deleted }
        "404": { $ref: "#/components/responses/NotFound" }
components:
  securitySchemes:
    bearerAuth:
      type: http
      scheme: bearer
      bearerFormat: JWT
  responses:
    NotFound:
      description: Not found (also returned for lacking permission / invisible business — no oracle)
      content:
        application/json:
          schema: { $ref: "#/components/schemas/Error" }
    ValidationError:
      description: Invalid request body
      content:
        application/json:
          schema: { $ref: "#/components/schemas/Error" }
    Conflict:
      description: Conflict (duplicate agent name in the business)
      content:
        application/json:
          schema: { $ref: "#/components/schemas/Error" }
  schemas:
    Error:
      type: object
      required: [code, message]
      properties:
        code: { type: string }
        message: { type: string }
    Agent:
      type: object
      required: [id, business_id, principal_id, name, provider, model, system_prompt, allowed_tools, autonomy_mode, enabled, monthly_budget_cents, created_at, updated_at]
      properties:
        id: { type: string, format: uuid }
        business_id: { type: string, format: uuid }
        principal_id: { type: string, format: uuid }
        name: { type: string }
        provider: { type: string, enum: [anthropic, openai, ollama, vllm] }
        model: { type: string }
        system_prompt: { type: string }
        allowed_tools: { type: array, items: { type: string } }
        autonomy_mode: { type: integer, enum: [1, 2, 3] }
        enabled: { type: boolean }
        monthly_budget_cents: { type: integer, minimum: 0 }
        created_at: { type: string, format: date-time }
        updated_at: { type: string, format: date-time }
    AgentList:
      type: object
      required: [items]
      properties:
        items:
          type: array
          items: { $ref: "#/components/schemas/Agent" }
    CreateAgentRequest:
      type: object
      required: [name, provider, model]
      properties:
        name: { type: string, minLength: 1 }
        provider: { type: string, enum: [anthropic, openai, ollama, vllm] }
        model: { type: string, minLength: 1 }
        system_prompt: { type: string }
        allowed_tools: { type: array, items: { type: string } }
        autonomy_mode: { type: integer, enum: [1, 2, 3], default: 1 }
        enabled: { type: boolean, default: true }
        monthly_budget_cents: { type: integer, minimum: 0, default: 0 }
    UpdateAgentRequest:
      type: object
      description: Partial update — omit a field to leave it unchanged. provider is immutable.
      properties:
        name: { type: string, minLength: 1 }
        model: { type: string, minLength: 1 }
        system_prompt: { type: string }
        allowed_tools: { type: array, items: { type: string } }
        autonomy_mode: { type: integer, enum: [1, 2, 3] }
        enabled: { type: boolean }
        monthly_budget_cents: { type: integer, minimum: 0 }
```

- [ ] **Step 2: Validate it parses as YAML**

Run: `go run gopkg.in/yaml.v3 2>/dev/null; python3 -c "import yaml,sys; yaml.safe_load(open('specs/003-agent-runtime/contracts/openapi.yaml')); print('ok')"`
Expected: `ok` (any YAML validator works; the drift test in Task 10 also parses it).

- [ ] **Step 3: Commit**

```bash
git add specs/003-agent-runtime/contracts/openapi.yaml
git commit -m "docs(agents): OpenAPI contract for agent CRUD (Spec 003 US2)"
```

---

## Task 10: Contract-drift test — `drift_003_test.go`

**Files:**
- Create: `cmd/manyforge/drift_003_test.go`

This mirrors `drift_002_test.go` and reuses the shared helpers `apiRoutes`, `specRoutesFrom`, `specPath`, `normalizePath` (defined untagged in `drift_test.go`, visible under the `contract` tag).

- [ ] **Step 1: Write the test**

```go
//go:build contract

package main

import (
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// inScope003Ops is the COMPLETE set of spec-003 operations served by the router so
// far (US2 agent-definition CRUD). Each entry is asserted both ways by
// TestOpenAPIDrift003 — present in the router AND documented in the 003 contract.
var inScope003Ops = []string{
	"GET /businesses/{}/agents",
	"POST /businesses/{}/agents",
	"GET /businesses/{}/agents/{}",
	"PATCH /businesses/{}/agents/{}",
	"DELETE /businesses/{}/agents/{}",
}

// is003Op reports whether a normalized "METHOD /path" belongs to the 003 surface
// (the business-nested /agents routes), as opposed to the 001/002 routes that share
// the /businesses prefix.
func is003Op(op string) bool {
	return strings.Contains(op, "/agents")
}

func spec003Routes(t *testing.T) map[string]bool {
	t.Helper()
	return specRoutesFrom(t, specPath("specs", "003-agent-runtime", "contracts", "openapi.yaml"))
}

// TestOpenAPIDrift003 pins the spec-003 agent-runtime contract against the FULL
// production router (built via mountAPIRoutes, the same seam main uses):
//  1. Presence: every in-scope 003 operation is REGISTERED.
//  2. No drift: every registered route on the 003 (/agents) surface is documented.
func TestOpenAPIDrift003(t *testing.T) {
	routes := apiRoutes(t)
	spec003 := spec003Routes(t)

	var missing []string
	for _, op := range inScope003Ops {
		if !spec003[op] {
			t.Errorf("test bug: in-scope op %q is not declared in the 003 openapi.yaml", op)
		}
		if !routes[op] {
			missing = append(missing, op)
		}
	}
	sort.Strings(missing)
	for _, op := range missing {
		t.Errorf("003 drift: %q is in-scope (US2) and in openapi.yaml but not served by the router", op)
	}

	var undocumented []string
	for op := range routes {
		if !is003Op(op) {
			continue // 001/002 route; covered by the other drift tests.
		}
		if !spec003[op] {
			undocumented = append(undocumented, op)
		}
	}
	sort.Strings(undocumented)
	for _, op := range undocumented {
		t.Errorf("003 drift: %q is served by the router but not in 003 openapi.yaml", op)
	}
}

// TestAgentEndpointContract pins the response-code shape for the US2 agent endpoints
// in the 003 contract — a pure spec-file assertion (no DB, no router).
func TestAgentEndpointContract(t *testing.T) {
	raw, err := os.ReadFile(specPath("specs", "003-agent-runtime", "contracts", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read 003 openapi: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse 003 openapi: %v", err)
	}
	codesFor := func(path, verb string) map[string]yaml.Node {
		node, ok := doc.Paths[path][verb]
		if !ok {
			t.Fatalf("003 openapi: missing %s %s", strings.ToUpper(verb), path)
		}
		var op struct {
			Responses map[string]yaml.Node `yaml:"responses"`
		}
		if err := node.Decode(&op); err != nil {
			t.Fatalf("decode %s %s: %v", verb, path, err)
		}
		return op.Responses
	}
	want := map[string]map[string][]string{
		"/businesses/{id}/agents": {
			"get":  {"200", "404"},
			"post": {"201", "400", "404", "409"},
		},
		"/businesses/{id}/agents/{agentID}": {
			"get":    {"200", "404"},
			"patch":  {"200", "400", "404", "409"},
			"delete": {"204", "404"},
		},
	}
	for path, verbs := range want {
		for verb, codes := range verbs {
			got := codesFor(path, verb)
			for _, code := range codes {
				if _, ok := got[code]; !ok {
					t.Errorf("003 openapi: %s %s must document response %s", strings.ToUpper(verb), path, code)
				}
			}
		}
	}
}
```

- [ ] **Step 2: Run the contract test**

Run: `make contract-test 2>&1 | tail -20`
Expected: PASS — `TestOpenAPIDrift003` (routes ↔ contract match) and `TestAgentEndpointContract` (response codes) both green, alongside the existing 001/002 drift tests.

> If `TestOpenAPIDrift003` reports the agent ops "served but not documented" or vice-versa, the mismatch is a path-string difference — confirm the router path (`/businesses/{id}/agents`, Task 7) and the contract path string match after `{param}`→`{}` normalization. They are identical by construction; if not, fix whichever drifted.

- [ ] **Step 3: Commit**

```bash
git add cmd/manyforge/drift_003_test.go
git commit -m "test(agents): OpenAPI drift + response-code pins for agent CRUD (Spec 003 US2)"
```

---

## Task 11: Security-regression pin + full gate

**Files:**
- Create: `internal/security_regression/agent_definition_ownership_pin_test.go`

**Why source-level, not behavioral:** behavioral cross-tenant isolation is already covered rigorously by `internal/agents`'s `TestAgentCrossTenantNoOracle` (Task 6, integration). `security_regression` is a *different* package and cannot reach `internal/agents`'s unexported `seedAgentTenant`; its own `seedAgentTenant` (in `agent_containment_test.go`) doesn't grant the agent principal a membership, so a caller seeded there can't pass RLS. Rather than duplicate a fragile DB seed, this pin is an **untagged source-level guard** (the CLAUDE.md pattern: `strings.Contains` the SQL fragment so a refactor that drops the ownership predicate / RLS policy fails CI loudly). It runs in BOTH `make test` and `make sec-test`, needs no Docker, and reuses the package's untagged `mustRead`.

- [ ] **Step 1: Write the source-level ownership/RLS pin**

```go
// Finding: Spec 003 US2 — agent definition queries enforce the (business_id)
// ownership predicate in SQL (dual enforcement with RLS), and the agent table has
// the business-scoped RLS policy + grant. A refactor that drops either fails CI.
// Behavioral cross-tenant isolation is covered by internal/agents'
// TestAgentCrossTenantNoOracle (integration). See manyforge-6r2.
package security_regression

import (
	"strings"
	"testing"
)

func TestAgentQueriesScopeByBusiness(t *testing.T) {
	sql := mustRead(t, "../../db/query/agent.sql")
	// Get + Delete carry the (id, business_id) ownership predicate ($2).
	if !strings.Contains(sql, "business_id = $2") {
		t.Errorf("agent.sql: Get/Delete must scope by business_id ($2 ownership predicate)")
	}
	// Update carries the business_id ownership predicate.
	if !strings.Contains(sql, "business_id = sqlc.arg('business_id')") {
		t.Errorf("agent.sql: UpdateAgent must scope by business_id")
	}
	// List is business-scoped.
	if !strings.Contains(sql, "WHERE business_id = $1") {
		t.Errorf("agent.sql: ListAgents must scope by business_id")
	}
}

func TestAgentTableHasBusinessScopedRLS(t *testing.T) {
	mig := mustRead(t, "../../migrations/0026_agent.up.sql")
	for _, frag := range []string{
		"ENABLE ROW LEVEL SECURITY",
		"authorized_businesses(current_principal())",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON agent TO manyforge_app",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0026_agent.up.sql: missing RLS fragment %q", frag)
		}
	}
}
```

> `mustRead(t, path) string` is the package's existing untagged helper (`escalation_pin_test.go`) — do NOT redefine it, do NOT add an `os` import. Paths are relative to the package dir (`internal/security_regression/`): `../../db/query/agent.sql` and `../../migrations/0026_agent.up.sql` resolve to the repo root. (If a fragment string differs from what Tasks 1/3 actually wrote — e.g. spacing — align the assertion to the real file; the pin must reflect the shipped SQL.)

- [ ] **Step 2: Run the security pin**

Run: `go test ./internal/security_regression/ -run 'TestAgentQueriesScopeByBusiness|TestAgentTableHasBusinessScopedRLS' -v`
Expected: PASS (untagged — also runs under `make sec-test` via the integration tag).

- [ ] **Step 3: Run the FULL gate**

```bash
make test
make contract-test
make lint          # golangci-lint MUST be 0 issues
make sec-test      # includes the new agent isolation pin
make int-test      # testcontainers, Docker, -p 1, ~6 min — includes internal/agents integration
go build ./...
```
Expected: ALL GREEN. If Docker is unavailable here, run `make test && make contract-test && make lint && go build ./...` and explicitly note that int-test/sec-test were deferred — but per CLAUDE.md "no pre-existing failures," they MUST run before the work is called done.

- [ ] **Step 4: Commit + close**

```bash
git add internal/security_regression/agent_definition_ownership_pin_test.go
git commit -m "test(sec): pin agent ownership predicate + RLS policy in SQL (US2 merge gate)"
bd close manyforge-6r2
```

---

## Self-review (run after execution)

**Spec coverage (design §5 US2 + §3.2/§3.4):**
- ✅ Business-bound `agent` table with all §3.2 columns (+ `name`) — Task 1.
- ✅ Bound to a `kind='agent'` principal created at agent-create time — Task 3 (`CreateAgentPrincipal`) + Task 4.
- ✅ `AgentService` CRUD over RLS with the ownership predicate in SQL — Tasks 4–5.
- ✅ `agents.configure` permission + presets — Task 2; enforced via `RequirePermission` — Task 8.
- ✅ Ownership predicates + no-oracle 404s (foreign/unknown id indistinguishable; cross-tenant returns not-found) — Tasks 5, 6, 11; HTTP 404 mapping — Task 7.
- ✅ Full HTTP + OpenAPI + drift (the locked scope decision) — Tasks 7–10.
- ⏭️ NOT in scope (correctly deferred): the agent principal's membership + role (US3 — the agent doesn't act yet), the run loop/gate/queue (US3/US4), credential HTTP (still service-only).

**Type consistency:** `CreateAgentInput`/`UpdateAgentInput`/`Agent` are used identically across service (Tasks 4–5), handler (Task 7), and tests; `agentCRUD` interface methods match `AgentService`'s signatures exactly; `agentResp` JSON field names match the OpenAPI `Agent` schema property names (snake_case) one-for-one; the `inScope003Ops` paths match the handler's `chi` routes after `{}` normalization and the OpenAPI path strings.

**Placeholder scan:** the only conditional guidance is where generated sqlc names or a test seam (`ContextWithPrincipal`, `seedAgentTenant` field names, the `security_regression` seed) must be verified against the actual code — each has an explicit `grep`/`go build`-decides instruction, not a TBD.

**Risk notes for the implementer:** (1) sqlc may emit positional method signatures rather than Params structs for the simple Get/List/Delete queries — match the generated code, `go build` is truth. (2) The `httpx` principal-injection test seam name must be confirmed (Task 7 Step 1). (3) The `security_regression` cross-package seed must be inlined (Task 11 Step 1 note).

---

## Execution handoff

**Plan complete and saved to `docs/superpowers/plans/2026-06-03-us2-agent-definitions.md`.**

Per the chosen flow, execution proceeds via **superpowers:subagent-driven-development** — fresh subagent per unit, spec review then code-quality review between units. Suggested implementer units: **A** = DB layer (Tasks 1–3), **B** = service (Tasks 4–5), **C** = integration test (Task 6), **D** = HTTP handler (Task 7), **E** = main wiring + OpenAPI + drift (Tasks 8–10), **F** = security pin + full gate (Task 11). The gate (`make test`/`contract-test`/`lint` + `int-test`/`sec-test`) must be GREEN before `bd close manyforge-6r2` and the session-end push.
