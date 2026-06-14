# MCP Per-Tool Reclassification + Admin UI — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a business admin reclassify specific MCP tools to Read/Reversible (so they auto-execute mode-dependently instead of always queuing for approval), backed by a per-business policy store + gate integration, and ship the MCP admin UI (per-tool policy editor + server management).

**Architecture:** A new `mcp_tool_policy` table (PK `(mcp_server_id, tool_name)`, `effect smallint CHECK IN (0,1)`, FK to `mcp_server` `ON DELETE CASCADE`, RLS like `mcp_server`) stores promotions; `External` = absence of a row (fail-closed default). At run-start discovery, `MCPHost` consults the policy per server and sets each discovered tool's `Effect` (default `EffectExternal`); the unchanged `gate(effect, mode)` then auto-execs or queues. A policy/discovery HTTP API (gated by `agents.configure`) + an Angular admin area (server CRUD UI wiring the existing API + a per-tool effect editor) complete it.

**Tech Stack:** Go, PostgreSQL (RLS + composite FK cascade), sqlc **v1.27.0** (`/opt/homebrew/bin/sqlc generate` ONLY), pgx/v5, chi; Angular 21 (standalone components, signals), Playwright e2e. Integration tests `//go:build integration` against ephemeral Postgres (`internal/platform/db/testdb`).

**Issue:** manyforge-k0d (epic manyforge-deo — Spec 003 US6 follow-up). Spec: `docs/superpowers/specs/2026-06-13-mcp-tool-reclassification-design.md`.

---

## Spec reconciliations (decisions locked before coding)

1. **`EffectClass` values** (`internal/agents/tools.go`): `EffectRead=0, EffectReversible=1, EffectExternal=2, EffectIrreversible=3`. Policy stores only `0|1`.
2. **Gate insertion point is discovery only.** `gate(effect, mode)` takes only `(effect, mode)`; the policy changes the *effect an MCP tool reports* at `mcp_host.go discoverServerTools`. `InvokeMCPTool` (approved-execution path) is unchanged — a Reversible-reclassified tool auto-execs inline and never reaches it; an External tool queues→approves→`InvokeMCPTool` as today. **No TOCTOU**: policy applies at the next run's discovery; an in-flight run keeps the effect captured at its discovery.
3. **The existing security pin** `TestPin_MCPToolsDefaultExternal` (`internal/security_regression/mcp_us6_pins_test.go`) greps the literal `Effect:       EffectExternal`. We restructure discovery to `effect := EffectExternal` (default) + explicit override, and **reframe the pin** to assert the default + `mcp.invoke` + that overrides only come from the policy table.
4. **Run-path policy read needs no DEFINER.** Discovery runs under the agent principal via `WithPrincipal` (`runner.go:209` → `ListEnabledForAgent`); an RLS-scoped `SELECT … FROM mcp_tool_policy WHERE mcp_server_id=$1` under that principal works once the table's RLS mirrors `mcp_server_rls`.
5. **No `id` column on the policy** — composite PK `(mcp_server_id, tool_name)`. Audit rows use `mcp_server_id` as `target_id` (uuid) and carry `tool_name` in the value JSON.
6. **`db/schema.sql` is a sqlc mirror** — table + indexes only (no DEFAULT/RLS/trigger/CHECK). The `effect` CHECK lives only in the migration; pins that assert it read the **migration**.
7. **Frontend gaps (route around, do not "fix"):** (a) only `authGuard` (token presence) exists — **no admin route guard**; authorization is enforced server-side (`agents.configure` → 403, surfaced as "You don't have access"). (b) `proxy.conf.json` is NOT wired into `angular.json` — dev/e2e commands must pass `--proxy-config proxy.conf.json --port 4300`. (c) **No `lint` npm script** — frontend verify = `build` + `test` + `e2e`.

---

## File structure

**New (backend):**
- `migrations/0053_mcp_tool_policy.up.sql` / `.down.sql`
- `db/query/mcp_tool_policy.sql` — policy CRUD + run-path list + a `GetEnabledMCPServerByID`
- `internal/agents/mcp_tool_policy.go` — `MCPToolPolicyService` (CRUD + audit) and the run-path resolver
- `internal/agents/mcp_tool_policy_handler.go` — discovery endpoint + policy CRUD handlers
- `internal/agents/mcp_tool_policy_test.go` — service unit/mapping tests
- `internal/agents/mcp_tool_policy_integration_test.go` — RLS + cascade + gate-integration cases
- `internal/security_regression/mcp_tool_policy_pins_test.go` — new pins

**Modified (backend):**
- `db/schema.sql` — mirror `mcp_tool_policy`
- `internal/agents/mcp_host.go` — `Policies` field + per-server consult at discovery
- `internal/agents/mcp_server.go` — `ResolveEnabledByID` (for the discovery endpoint); add it to the `mcpServerResolver` interface
- `internal/agents/mcp_server_handler.go` — nest the policy routes under `/{serverID}`
- `internal/platform/db/dbgen/*` — regenerated (do not hand-edit)
- `cmd/manyforge/main.go` — construct the policy service/handler, wire into `MCPHost` + the route group
- `internal/security_regression/mcp_us6_pins_test.go` — reframe `TestPin_MCPToolsDefaultExternal`
- `specs/003-agent-runtime/contracts/openapi.yaml` — new paths + schemas

**New (frontend):**
- `web/src/app/core/mcp.service.ts`
- `web/src/app/pages/mcp/server-list.ts`, `server-form.ts`, `server-tools.ts`
- `web/e2e/mcp.spec.ts`

**Modified (frontend):**
- `web/src/app/app.routes.ts` — `mcp` + `mcp/:businessId/:serverId` routes
- `web/src/app/ui/nav.ts` — `MCP` nav item

---

## Conventions for every task

- **Go:** prefix with `export PATH="$HOME/go/bin:$PATH"`. Gates: `go build ./...` · `make test` · `make sec-test` · `make lint` (all exit 0). Integration: `go test -tags integration ./internal/agents/... -run <Name>` (Docker up; ~80s — use `-run`).
- **sqlc (CRITICAL):** regenerate ONLY with `/opt/homebrew/bin/sqlc generate` (the pinned v1.27.0). NEVER `make generate` (bare `sqlc` on PATH = v1.31.1, churns everything). After generating, `git status -s internal/platform/db/dbgen/` must show only the new query file's generated output + `models.go`/`querier.go`/`db.go`.
- **Migrate:** `set -a; . ./.air.env; set +a; make migrate` (needs `MANYFORGE_DATABASE_URL`, run as the `manyforge` superuser/owner role). Latest migration = **0052**; this adds **0053**.
- **Frontend:** `cd web`. Dev: `npm start -- --port 4300 --proxy-config proxy.conf.json`. Build: `npm run build`. Unit: `npm test -- --run <file>` (Vitest). e2e: `npm run e2e` (dev server must be on :4300). No `lint` script.
- **gopls inline diagnostics are STALE** for agents/dbgen, esp. after a sqlc regen — trust `go build`/`go test`.
- **Never `git add -A`** (bd hook stages the journal; sweeps untracked `CLAUDE.md`). Commit explicit paths. **No `Co-Authored-By`** trailer.

---

## Task 1: Migration 0053 — `mcp_tool_policy`

**Files:**
- Create: `migrations/0053_mcp_tool_policy.up.sql`, `migrations/0053_mcp_tool_policy.down.sql`
- Modify: `db/schema.sql`

- [ ] **Step 1: Write the up migration**

`migrations/0053_mcp_tool_policy.up.sql`:
```sql
-- 0053: per-business per-tool MCP effect policy (manyforge-k0d, Spec 003 US6 follow-up).
-- Lets an admin reclassify a specific MCP tool to Read(0)/Reversible(1) so it auto-executes
-- mode-dependently instead of always queuing. effect IN (0,1) STRUCTURALLY forbids storing
-- External(2)/Irreversible(3): External = absence of a row (the fail-closed default). Keyed by
-- the stable mcp_server.id (not the mutable name) with ON DELETE CASCADE so deleting a server
-- removes its tool policies. RLS mirrors mcp_server_rls (0036).
CREATE TABLE mcp_tool_policy (
    mcp_server_id  uuid     NOT NULL,
    business_id    uuid     NOT NULL,
    tenant_root_id uuid     NOT NULL,
    tool_name      text     NOT NULL,
    effect         smallint NOT NULL CHECK (effect IN (0, 1)),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (mcp_server_id, tool_name),
    FOREIGN KEY (mcp_server_id, tenant_root_id) REFERENCES mcp_server (id, tenant_root_id) ON DELETE CASCADE,
    FOREIGN KEY (business_id, tenant_root_id)   REFERENCES business (id, tenant_root_id)
);
CREATE INDEX mcp_tool_policy_business_idx ON mcp_tool_policy (business_id, tenant_root_id);
CREATE TRIGGER mcp_tool_policy_troot_immutable
    BEFORE UPDATE ON mcp_tool_policy
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();
GRANT SELECT, INSERT, UPDATE, DELETE ON mcp_tool_policy TO manyforge_app;
ALTER TABLE mcp_tool_policy ENABLE ROW LEVEL SECURITY;
CREATE POLICY mcp_tool_policy_rls ON mcp_tool_policy FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);
```

- [ ] **Step 2: Write the down migration**

`migrations/0053_mcp_tool_policy.down.sql`:
```sql
DROP TABLE IF EXISTS mcp_tool_policy;
```

- [ ] **Step 3: Mirror in `db/schema.sql`**

After the `mcp_server` block in `db/schema.sql` (sqlc-mirror style — no DEFAULT/RLS/trigger/CHECK):
```sql
CREATE TABLE mcp_tool_policy (
    mcp_server_id  uuid     NOT NULL,
    business_id    uuid     NOT NULL,
    tenant_root_id uuid     NOT NULL,
    tool_name      text     NOT NULL,
    effect         smallint NOT NULL,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    PRIMARY KEY (mcp_server_id, tool_name),
    FOREIGN KEY (mcp_server_id, tenant_root_id) REFERENCES mcp_server (id, tenant_root_id) ON DELETE CASCADE,
    FOREIGN KEY (business_id, tenant_root_id)   REFERENCES business (id, tenant_root_id)
);
CREATE INDEX mcp_tool_policy_business_idx ON mcp_tool_policy (business_id, tenant_root_id);
```

- [ ] **Step 4: Apply + verify**

```bash
export PATH="$HOME/go/bin:$PATH"
set -a; . ./.air.env; set +a; make migrate
psql "$MANYFORGE_DATABASE_URL" -c "\d mcp_tool_policy"
psql "$MANYFORGE_DATABASE_URL" -c "SELECT conname FROM pg_constraint WHERE conrelid='mcp_tool_policy'::regclass;"
```
Expected: table exists with the `effect` CHECK, the composite cascade FK, and the RLS policy. Confirm `make migrate` reached 0053.

- [ ] **Step 5: Commit**

```bash
git add migrations/0053_mcp_tool_policy.up.sql migrations/0053_mcp_tool_policy.down.sql db/schema.sql
git commit -m "feat(agents): migration for MCP per-tool effect policy (manyforge-k0d)"
```

---

## Task 2: sqlc queries

**Files:**
- Create: `db/query/mcp_tool_policy.sql`
- Modify: `db/query/mcp.sql` (add `GetEnabledMCPServerByID`)
- Regenerate: `internal/platform/db/dbgen/*`

- [ ] **Step 1: Write the policy queries**

`db/query/mcp_tool_policy.sql`:
```sql
-- MCP per-tool effect policy (manyforge-k0d). Every query runs in the caller's RLS principal
-- context AND pushes the (business_id, …) ownership predicate into SQL (dual enforcement).

-- name: UpsertMCPToolPolicy :one
-- Derives (business_id, tenant_root_id) from the RLS-visible mcp_server row, so an invisible or
-- foreign server yields no row → pgx.ErrNoRows → 404 (no oracle). Upsert on (mcp_server_id, tool_name).
INSERT INTO mcp_tool_policy (mcp_server_id, business_id, tenant_root_id, tool_name, effect, created_at, updated_at)
SELECT m.id, m.business_id, m.tenant_root_id, sqlc.arg('tool_name')::text, sqlc.arg('effect')::smallint, now(), now()
FROM mcp_server m
WHERE m.id = sqlc.arg('mcp_server_id')::uuid AND m.business_id = sqlc.arg('business_id')::uuid
ON CONFLICT (mcp_server_id, tool_name) DO UPDATE SET effect = excluded.effect, updated_at = now()
RETURNING *;

-- name: GetMCPToolPolicy :one
SELECT * FROM mcp_tool_policy
WHERE mcp_server_id = sqlc.arg('mcp_server_id')::uuid AND tool_name = sqlc.arg('tool_name')::text
  AND business_id = sqlc.arg('business_id')::uuid;

-- name: ListMCPToolPolicies :many
SELECT * FROM mcp_tool_policy
WHERE mcp_server_id = sqlc.arg('mcp_server_id')::uuid AND business_id = sqlc.arg('business_id')::uuid
ORDER BY tool_name;

-- name: DeleteMCPToolPolicy :execrows
DELETE FROM mcp_tool_policy
WHERE mcp_server_id = sqlc.arg('mcp_server_id')::uuid AND tool_name = sqlc.arg('tool_name')::text
  AND business_id = sqlc.arg('business_id')::uuid;

-- name: ListToolPoliciesByServer :many
-- Run-path: the discovery loop reads this under the AGENT principal (RLS scopes it to the
-- agent's business) to classify discovered tools.
SELECT tool_name, effect FROM mcp_tool_policy WHERE mcp_server_id = sqlc.arg('mcp_server_id')::uuid;
```

- [ ] **Step 2: Add `GetEnabledMCPServerByID` to `db/query/mcp.sql`**

Append (template for resolving a single enabled server by id — the discovery endpoint needs this to connect):
```sql
-- name: GetEnabledMCPServerByID :one
-- Resolve one enabled server by id under RLS (+ explicit business_id). Used by the tool-discovery
-- endpoint to connect. pgx.ErrNoRows → 404.
SELECT * FROM mcp_server
WHERE id = sqlc.arg('id')::uuid AND business_id = sqlc.arg('business_id')::uuid AND enabled;
```

- [ ] **Step 3: Regenerate dbgen**

```bash
/opt/homebrew/bin/sqlc generate
git status -s internal/platform/db/dbgen/
export PATH="$HOME/go/bin:$PATH"; go build ./internal/platform/db/...
```
Expected: new `mcp_tool_policy.sql.go` + updated `mcp.sql.go`/`models.go`/`querier.go`. No unrelated churn (else `git checkout internal/platform/db/dbgen/` and re-run with `/opt/homebrew/bin/sqlc`).

- [ ] **Step 4: Commit**

```bash
git add db/query/mcp_tool_policy.sql db/query/mcp.sql internal/platform/db/dbgen/
git commit -m "feat(agents): sqlc queries for MCP tool policy (manyforge-k0d)"
```

---

## Task 3: `MCPToolPolicyService` (CRUD + audit + run-path resolver)

**Files:**
- Create: `internal/agents/mcp_tool_policy.go`
- Create: `internal/agents/mcp_tool_policy_test.go`

- [ ] **Step 1: Write the failing unit test (effect string↔class mapping + validation)**

`internal/agents/mcp_tool_policy_test.go`:
```go
package agents

import "testing"

func TestEffectFromString(t *testing.T) {
	cases := map[string]struct {
		eff EffectClass
		ok  bool
	}{
		"read":       {EffectRead, true},
		"reversible": {EffectReversible, true},
		"external":   {0, false}, // not assignable
		"":           {0, false},
		"delete":     {0, false},
	}
	for in, want := range cases {
		got, err := effectFromString(in)
		if want.ok && (err != nil || got != want.eff) {
			t.Errorf("effectFromString(%q) = (%d,%v), want (%d,nil)", in, got, err, want.eff)
		}
		if !want.ok && err == nil {
			t.Errorf("effectFromString(%q) = nil err, want validation error", in)
		}
	}
}

func TestEffectToString(t *testing.T) {
	if effectToString(EffectRead) != "read" || effectToString(EffectReversible) != "reversible" {
		t.Fatal("effectToString mapping wrong")
	}
	if effectToString(EffectExternal) != "external" {
		t.Fatal("EffectExternal must stringify to external (the default)")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
export PATH="$HOME/go/bin:$PATH"
go test ./internal/agents/ -run 'TestEffect' -v
```
Expected: FAIL — `effectFromString`/`effectToString` undefined.

- [ ] **Step 3: Write the service**

`internal/agents/mcp_tool_policy.go`:
```go
// MCP per-tool effect policy (manyforge-k0d). An admin promotes specific MCP tools to
// Read/Reversible so they auto-execute mode-dependently; External (the fail-closed default) is
// the absence of a row. Mirrors MCPServerService: every method runs in the caller's RLS
// principal context AND pushes the ownership predicate into SQL.
package agents

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// MCPToolPolicy is one persisted reclassification returned to admin callers.
type MCPToolPolicy struct {
	ServerID uuid.UUID
	ToolName string
	Effect   string // "read" | "reversible"
}

type mcpPolicyDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
}

// MCPToolPolicyService is the admin CRUD over mcp_tool_policy AND the run-path resolver the
// MCPHost consults at discovery.
type MCPToolPolicyService struct{ DB mcpPolicyDB }

// effectFromString maps the API effect token to the assignable EffectClass. Only Read/Reversible
// are assignable (the table CHECK enforces this too); anything else is a validation error.
func effectFromString(s string) (EffectClass, error) {
	switch s {
	case "read":
		return EffectRead, nil
	case "reversible":
		return EffectReversible, nil
	default:
		return 0, fmt.Errorf("agents: effect %q not assignable (want read|reversible): %w", s, errs.ErrValidation)
	}
}

func effectToString(e EffectClass) string {
	switch e {
	case EffectRead:
		return "read"
	case EffectReversible:
		return "reversible"
	default:
		return "external"
	}
}

// Upsert sets (or replaces) the policy for one (server, tool). Audits old→new in the same tx.
func (s *MCPToolPolicyService) Upsert(ctx context.Context, principalID, businessID, serverID uuid.UUID, toolName, effectStr string) (MCPToolPolicy, error) {
	if toolName == "" {
		return MCPToolPolicy{}, fmt.Errorf("agents: tool_name required: %w", errs.ErrValidation)
	}
	eff, err := effectFromString(effectStr)
	if err != nil {
		return MCPToolPolicy{}, err
	}
	var out MCPToolPolicy
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		// Old value for the audit (best-effort: no row => "external").
		oldStr := "external"
		if prev, gerr := q.GetMCPToolPolicy(ctx, dbgen.GetMCPToolPolicyParams{
			McpServerID: serverID, ToolName: toolName, BusinessID: businessID,
		}); gerr == nil {
			oldStr = effectToString(EffectClass(prev.Effect))
		} else if !errors.Is(gerr, pgx.ErrNoRows) {
			return gerr
		}
		row, ierr := q.UpsertMCPToolPolicy(ctx, dbgen.UpsertMCPToolPolicyParams{
			McpServerID: serverID, BusinessID: businessID, ToolName: toolName, Effect: int16(eff),
		})
		if ierr != nil {
			return ierr // ErrNoRows when the server is invisible/foreign → mapped to 404 below
		}
		out = MCPToolPolicy{ServerID: serverID, ToolName: toolName, Effect: effectToString(eff)}
		tt := "mcp_tool_policy"
		return audit.Write(ctx, tx, audit.Entry{
			BusinessID: &businessID, TenantRootID: &row.TenantRootID, ActorPrincipalID: &principalID,
			Action: "mcp.tool_policy.set", TargetType: &tt, TargetID: &serverID,
			OldValue: map[string]any{"tool": toolName, "effect": oldStr},
			NewValue: map[string]any{"tool": toolName, "effect": effectToString(eff)},
		})
	})
	if err != nil {
		return MCPToolPolicy{}, mapMCPErr(err)
	}
	return out, nil
}

// List returns the persisted policies for one server (admin view).
func (s *MCPToolPolicyService) List(ctx context.Context, principalID, businessID, serverID uuid.UUID) ([]MCPToolPolicy, error) {
	var out []MCPToolPolicy
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		rows, qerr := dbgen.New(tx).ListMCPToolPolicies(ctx, dbgen.ListMCPToolPoliciesParams{
			McpServerID: serverID, BusinessID: businessID,
		})
		if qerr != nil {
			return qerr
		}
		for _, r := range rows {
			out = append(out, MCPToolPolicy{ServerID: r.McpServerID, ToolName: r.ToolName, Effect: effectToString(EffectClass(r.Effect))})
		}
		return nil
	})
	if err != nil {
		return nil, mapMCPErr(err)
	}
	return out, nil
}

// Delete clears a policy (revert to External default). Audits the removal in the same tx.
func (s *MCPToolPolicyService) Delete(ctx context.Context, principalID, businessID, serverID uuid.UUID, toolName string) error {
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		prev, gerr := q.GetMCPToolPolicy(ctx, dbgen.GetMCPToolPolicyParams{
			McpServerID: serverID, ToolName: toolName, BusinessID: businessID,
		})
		if errors.Is(gerr, pgx.ErrNoRows) {
			return errs.ErrNotFound
		}
		if gerr != nil {
			return gerr
		}
		n, derr := q.DeleteMCPToolPolicy(ctx, dbgen.DeleteMCPToolPolicyParams{
			McpServerID: serverID, ToolName: toolName, BusinessID: businessID,
		})
		if derr != nil {
			return derr
		}
		if n == 0 {
			return errs.ErrNotFound
		}
		tt := "mcp_tool_policy"
		return audit.Write(ctx, tx, audit.Entry{
			BusinessID: &businessID, TenantRootID: &prev.TenantRootID, ActorPrincipalID: &principalID,
			Action: "mcp.tool_policy.cleared", TargetType: &tt, TargetID: &serverID,
			OldValue: map[string]any{"tool": toolName, "effect": effectToString(EffectClass(prev.Effect))},
		})
	})
	if err != nil {
		return mapMCPErr(err)
	}
	return nil
}

// ListToolPoliciesByServer is the run-path resolver (MCPHost.Policies). Returns tool_name →
// EffectClass for one server, read under the AGENT principal (RLS-scoped to its business).
func (s *MCPToolPolicyService) ListToolPoliciesByServer(ctx context.Context, principalID, businessID, serverID uuid.UUID) (map[string]EffectClass, error) {
	out := map[string]EffectClass{}
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		rows, qerr := dbgen.New(tx).ListToolPoliciesByServer(ctx, serverID)
		if qerr != nil {
			return qerr
		}
		for _, r := range rows {
			out[r.ToolName] = EffectClass(r.Effect)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("agents: list tool policies: %w", err)
	}
	return out, nil
}

var _ = db.PGUUID // keep the db import if unused after edits; remove if go vet flags it
```

> Note: `mapMCPErr` already exists in `internal/agents/mcp_server.go` (maps `pgx.ErrNoRows → ErrNotFound`, `23505 → ErrConflict`, preserves typed sentinels). Reuse it. Confirm `audit.Entry` fields (`BusinessID *uuid.UUID`, `TenantRootID *uuid.UUID`, `ActorPrincipalID *uuid.UUID`, `Action string`, `TargetType *string`, `TargetID *uuid.UUID`, `OldValue/NewValue any`) against `internal/platform/audit/audit.go`. Remove the `db.PGUUID` keep-alive line if the `db` import is otherwise unused (gofmt/vet will tell you).

- [ ] **Step 4: Run unit tests**

```bash
export PATH="$HOME/go/bin:$PATH"
go test ./internal/agents/ -run 'TestEffect' -v && go build ./internal/agents/...
```
Expected: PASS + build clean. (Behavioral CRUD is covered by the integration test in Task 4's file; commit there.)

---

## Task 4: Gate integration — consult the policy at discovery + reframe the pin

**Files:**
- Modify: `internal/agents/mcp_host.go`
- Modify: `internal/security_regression/mcp_us6_pins_test.go`
- Create: `internal/agents/mcp_tool_policy_integration_test.go`
- Create: `internal/security_regression/mcp_tool_policy_pins_test.go`

- [ ] **Step 1: Add the policy resolver to `MCPHost` and consult it at discovery**

In `internal/agents/mcp_host.go`, add the resolver interface + field, and thread a per-server policy map into `discoverServerTools`.

Add near the `mcpServerResolver` interface:
```go
// toolPolicyResolver lets discovery reclassify specific MCP tools (manyforge-k0d). A nil
// resolver (or a lookup error) leaves every tool at the fail-closed EffectExternal default.
type toolPolicyResolver interface {
	ListToolPoliciesByServer(ctx context.Context, principalID, businessID, serverID uuid.UUID) (map[string]EffectClass, error)
}
```

Add to the `MCPHost` struct (after `Servers`):
```go
type MCPHost struct {
	Servers  mcpServerResolver
	Policies toolPolicyResolver // manyforge-k0d: per-tool effect overrides; nil = all External
	Connect  mcp.ClientFactory
	Logger   *slog.Logger
}
```

In `DiscoverTools`, fetch the policy map per server and pass it down — change the loop body:
```go
	for _, server := range servers {
		policyMap := h.policiesFor(ctx, principalID, businessID, server.ID)
		discovered, err := h.discoverServerTools(ctx, server, policyMap)
		if err != nil {
			h.Logger.WarnContext(ctx, "agent.mcp.discovery_failed",
				"server_id", server.ID, "server_name", server.Name, "err", err)
			failures = append(failures, DiscoveryFailure{ServerID: server.ID, ServerName: server.Name, Err: err.Error()})
			continue
		}
		tools = append(tools, discovered...)
	}
```

Add the helper:
```go
// policiesFor best-effort loads the per-tool effect overrides for one server. Any error (or a
// nil resolver) returns nil, so discovery proceeds with every tool at the External default —
// the policy can only RELAX from External, never make discovery fail-open.
func (h *MCPHost) policiesFor(ctx context.Context, principalID, businessID, serverID uuid.UUID) map[string]EffectClass {
	if h.Policies == nil {
		return nil
	}
	pm, err := h.Policies.ListToolPoliciesByServer(ctx, principalID, businessID, serverID)
	if err != nil {
		h.Logger.WarnContext(ctx, "agent.mcp.policy_lookup_failed", "server_id", serverID, "err", err)
		return nil
	}
	return pm
}
```

Change `discoverServerTools` signature to accept the map, and set `Effect` from it (default stays `EffectExternal`):
```go
func (h *MCPHost) discoverServerTools(ctx context.Context, server ResolvedMCPServer, policyMap map[string]EffectClass) ([]Tool, error) {
	client := h.Connect(server.URL, server.AuthHeader)
	if err := client.Initialize(ctx); err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	defs, err := client.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	tools := make([]Tool, 0, len(defs))
	for _, def := range defs {
		capturedClient := client
		capturedDef := def
		capturedServer := server

		schemaJSON := ""
		if len(capturedDef.InputSchema) > 0 {
			schemaJSON = string(capturedDef.InputSchema)
		}
		// manyforge-k0d: External is the fail-closed default; an explicit per-business policy may
		// promote this tool to Read/Reversible (the only values the policy table can store).
		effect := EffectExternal
		if pe, ok := policyMap[capturedDef.Name]; ok {
			effect = pe
		}
		tools = append(tools, Tool{
			Name:         "mcp:" + capturedServer.Name + ":" + capturedDef.Name,
			Description:  capturedDef.Description,
			SchemaJSON:   schemaJSON,
			Effect:       effect,
			RequiredPerm: "mcp.invoke",
			Invoke: func(ctx context.Context, pid, bid uuid.UUID, args json.RawMessage) (string, error) {
				idemHint := ""
				if k, ok := approvalKeyFrom(ctx); ok {
					idemHint = k.String()
				}
				res, err := capturedClient.CallTool(ctx, capturedDef.Name, args, idemHint)
				if err != nil {
					return "", fmt.Errorf("mcp tool %s/%s: %w", capturedServer.Name, capturedDef.Name, err)
				}
				if res.IsError {
					return "", fmt.Errorf("mcp tool %s/%s returned error: %s", capturedServer.Name, capturedDef.Name, res.Content)
				}
				return res.Content, nil
			},
		})
	}
	return tools, nil
}
```

- [ ] **Step 2: Reframe the existing pin**

In `internal/security_regression/mcp_us6_pins_test.go`, replace `TestPin_MCPToolsDefaultExternal`'s body so it pins the *default* (now a variable initialization) + `mcp.invoke`, not the old struct-literal value:
```go
// TestPin_MCPToolsDefaultExternal pins the fail-closed default: a discovered MCP tool starts at
// EffectExternal and requires mcp.invoke. manyforge-k0d lets an explicit per-business policy
// promote a tool to Read/Reversible, so the assertion is now "default is External (the var init),
// override is explicit" — never "every MCP tool is unconditionally External".
func TestPin_MCPToolsDefaultExternal(t *testing.T) {
	host := mustRead(t, "../agents/mcp_host.go")
	for _, frag := range []string{
		`effect := EffectExternal`,        // the fail-closed default
		`RequiredPerm: "mcp.invoke"`,      // stays RBAC-gated
		`Effect:       effect`,            // the tool takes the (defaulted/overridden) effect
	} {
		if !strings.Contains(host, frag) {
			t.Errorf("mcp_host.go: missing fragment %q — MCP tools must default to External and require mcp.invoke", frag)
		}
	}
}
```

- [ ] **Step 3: Add new pins**

`internal/security_regression/mcp_tool_policy_pins_test.go`:
```go
// Source-level pins for manyforge-k0d (MCP per-tool reclassification). No build tag → run in
// `make test` + `make sec-test`. They make a refactor that drops a k0d guardrail fail loudly.
package security_regression

import (
	"strings"
	"testing"
)

// TestPin_ToolPolicyPromotionsOnly pins the schema guardrail: effect IN (0,1) means a policy can
// ONLY promote to Read/Reversible — External(2)/Irreversible(3) are structurally unstorable, so
// an admin can never fabricate a more-permissive-than-intended class. External = absence of a row.
func TestPin_ToolPolicyPromotionsOnly(t *testing.T) {
	mig := mustRead(t, "../../migrations/0053_mcp_tool_policy.up.sql")
	if !strings.Contains(mig, "CHECK (effect IN (0, 1))") {
		t.Error("0053: mcp_tool_policy.effect must be CHECK (effect IN (0, 1)) — promotions only, no External/Irreversible")
	}
}

// TestPin_ToolPolicyTenantScopedAndCascade pins RLS isolation + the FK cascade lifecycle.
func TestPin_ToolPolicyTenantScopedAndCascade(t *testing.T) {
	mig := mustRead(t, "../../migrations/0053_mcp_tool_policy.up.sql")
	for _, frag := range []string{
		"ENABLE ROW LEVEL SECURITY",
		"authorized_businesses(current_principal())",
		"REFERENCES mcp_server (id, tenant_root_id) ON DELETE CASCADE",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0053: missing fragment %q (tenant isolation / cascade lifecycle)", frag)
		}
	}
}

// TestPin_ToolPolicyDiscoveryDefaultsClosed pins that discovery still defaults to External and
// only promotes from the policy map (the override is layered, not a replacement of the default).
func TestPin_ToolPolicyDiscoveryDefaultsClosed(t *testing.T) {
	host := mustRead(t, "../agents/mcp_host.go")
	for _, frag := range []string{
		"effect := EffectExternal",
		"policyMap[capturedDef.Name]",
	} {
		if !strings.Contains(host, frag) {
			t.Errorf("mcp_host.go: missing fragment %q — discovery must default External and override from the policy map", frag)
		}
	}
}
```

- [ ] **Step 4: Write the gate-integration + RLS + cascade integration tests**

`internal/agents/mcp_tool_policy_integration_test.go`:
```go
//go:build integration

package agents

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/mcp"
)

// fakeResolverByID returns a single ResolvedMCPServer for discovery (no real network).
type fixedServerResolver struct{ server ResolvedMCPServer }

func (f fixedServerResolver) ListEnabledForAgent(_ context.Context, _, _ uuid.UUID, _ []uuid.UUID) ([]ResolvedMCPServer, error) {
	return []ResolvedMCPServer{f.server}, nil
}
func (f fixedServerResolver) ResolveEnabledByName(_ context.Context, _, _ uuid.UUID, _ string) (ResolvedMCPServer, error) {
	return f.server, nil
}
func (f fixedServerResolver) ResolveEnabledByID(_ context.Context, _, _, _ uuid.UUID) (ResolvedMCPServer, error) {
	return f.server, nil
}

// seedMCPServer inserts an mcp_server row via Super and returns its id.
func seedMCPServer(ctx context.Context, t *testing.T, tdb *testdb.TestDB, s runSeed, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO mcp_server (id,business_id,tenant_root_id,name,url,enabled,created_at,updated_at)
		 VALUES ($1,$2,$2,$3,'https://mcp.example.test',true,now(),now())`,
		id, s.businessID, name); err != nil {
		t.Fatalf("seed mcp_server: %v", err)
	}
	return id
}

// Discovery applies a Reversible policy to a tool while leaving an unclassified tool External.
func TestToolPolicy_DiscoveryAppliesEffect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	s := seedRunTenant(ctx, t, tdb)
	serverID := seedMCPServer(ctx, t, tdb, "acme")

	// Promote "safe_read" → Reversible via the real service (under the owner principal).
	policySvc := &MCPToolPolicyService{DB: tdb.App}
	if _, err := policySvc.Upsert(ctx, s.ownerID, s.businessID, serverID, "safe_read", "reversible"); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}

	// Discover two tools from a mock MCP client: "safe_read" (policied) + "do_thing" (unclassified).
	mockClient := mcp.NewMockClient(
		[]mcp.ToolDef{{Name: "safe_read", Description: "r"}, {Name: "do_thing", Description: "d"}},
		nil,
	)
	host := &MCPHost{
		Servers:  fixedServerResolver{server: ResolvedMCPServer{ID: serverID, Name: "acme", URL: "https://x", AuthHeader: ""}},
		Policies: policySvc,
		Connect:  func(_, _ string) mcp.ClientLike { return mockClient },
		Logger:   slog.Default(),
	}
	tools, _, err := host.DiscoverTools(ctx, s.ownerID, s.businessID, []uuid.UUID{serverID})
	if err != nil {
		t.Fatalf("DiscoverTools: %v", err)
	}
	got := map[string]EffectClass{}
	for _, tl := range tools {
		got[tl.Name] = tl.Effect
	}
	if got["mcp:acme:safe_read"] != EffectReversible {
		t.Errorf("safe_read effect = %d, want Reversible(%d)", got["mcp:acme:safe_read"], EffectReversible)
	}
	if got["mcp:acme:do_thing"] != EffectExternal {
		t.Errorf("do_thing effect = %d, want External(%d) (unclassified default)", got["mcp:acme:do_thing"], EffectExternal)
	}
}

// Deleting the mcp_server cascades its tool policies away.
func TestToolPolicy_CascadeOnServerDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	s := seedRunTenant(ctx, t, tdb)
	serverID := seedMCPServer(ctx, t, tdb, "acme")
	policySvc := &MCPToolPolicyService{DB: tdb.App}
	if _, err := policySvc.Upsert(ctx, s.ownerID, s.businessID, serverID, "t", "read"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := tdb.Super.Exec(ctx, `DELETE FROM mcp_server WHERE id=$1`, serverID); err != nil {
		t.Fatalf("delete server: %v", err)
	}
	var n int
	if err := tdb.Super.QueryRow(ctx, `SELECT count(*) FROM mcp_tool_policy WHERE mcp_server_id=$1`, serverID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("policies after server delete = %d, want 0 (cascade)", n)
	}
}

// A foreign/unknown server id is a no-oracle 404 (errs.ErrNotFound) on upsert.
func TestToolPolicy_ForeignServerIsNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	s := seedRunTenant(ctx, t, tdb)
	policySvc := &MCPToolPolicyService{DB: tdb.App}
	_, err = policySvc.Upsert(ctx, s.ownerID, s.businessID, uuid.New() /* nonexistent */, "t", "read")
	if err == nil {
		t.Fatal("upsert against unknown server: want ErrNotFound, got nil")
	}
}
```

> **Verified anchors (already confirmed against the code):** `mcp.NewMockClient(listResult []mcp.ToolDef, callResults map[string][]mcp.Result)` — pass `nil` for the second arg; `mcp.ToolDef{Name, Description, InputSchema}` (`internal/platform/mcp/schema.go`). The `testdb` setup uses the real `run_integration_test.go` pattern: `ctx, cancel := context.WithTimeout(...)` + `testdb.Start(ctx)` + `t.Cleanup(func(){ tdb.Close(context.Background()) })`, logger `slog.Default()` (matching `mcp_integration_test.go`). `seedRunTenant`/`runSeed`/`tdb.Super`/`tdb.App` are shared helpers in `run_integration_test.go`.

- [ ] **Step 5: Run + verify**

```bash
export PATH="$HOME/go/bin:$PATH"
go build ./... && go test ./internal/security_regression/ -run 'TestPin_(MCPToolsDefaultExternal|ToolPolicy)' -v
go test -tags integration ./internal/agents/ -run 'TestToolPolicy' -v
```
Expected: pins PASS (reframed + new); integration cases PASS.

- [ ] **Step 6: Commit (service + gate + pins)**

```bash
git add internal/agents/mcp_tool_policy.go internal/agents/mcp_tool_policy_test.go \
        internal/agents/mcp_host.go internal/agents/mcp_tool_policy_integration_test.go \
        internal/security_regression/mcp_us6_pins_test.go internal/security_regression/mcp_tool_policy_pins_test.go
git commit -m "feat(agents): MCP tool-policy store + gate integration + pins (manyforge-k0d)"
```

---

## Task 5: HTTP API — discovery endpoint + policy CRUD + wiring

**Files:**
- Modify: `internal/agents/mcp_server.go` (add `ResolveEnabledByID` + to the `mcpServerResolver` interface)
- Modify: `internal/agents/mcp_host.go` (add `DiscoverServerToolDefs`)
- Create: `internal/agents/mcp_tool_policy_handler.go`
- Modify: `internal/agents/mcp_server_handler.go` (nest policy routes under `/{serverID}`)
- Modify: `cmd/manyforge/main.go` (construct + wire)
- Modify: `specs/003-agent-runtime/contracts/openapi.yaml`

- [ ] **Step 1: Add `ResolveEnabledByID` to `MCPServerService`**

In `internal/agents/mcp_server.go`, add (mirror `ResolveEnabledByName`, but by id, using the new `GetEnabledMCPServerByID` query; unseal the auth header):
```go
// ResolveEnabledByID fetches a single enabled server by id under RLS and unseals its auth header.
// Used by the tool-discovery endpoint. pgx.ErrNoRows → ErrNotFound (no oracle).
func (s *MCPServerService) ResolveEnabledByID(ctx context.Context, principalID, businessID, serverID uuid.UUID) (ResolvedMCPServer, error) {
	var out ResolvedMCPServer
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		row, qerr := dbgen.New(tx).GetEnabledMCPServerByID(ctx, dbgen.GetEnabledMCPServerByIDParams{
			ID: serverID, BusinessID: businessID,
		})
		if qerr != nil {
			return qerr
		}
		header, herr := s.resolveAuthHeader(row.SealedAuthRef)
		if herr != nil {
			return herr
		}
		out = ResolvedMCPServer{ID: row.ID, Name: row.Name, URL: row.Url, AuthHeader: header}
		return nil
	})
	if err != nil {
		return ResolvedMCPServer{}, mapMCPErr(err)
	}
	return out, nil
}
```
Add `ResolveEnabledByID(ctx context.Context, principalID, businessID, serverID uuid.UUID) (ResolvedMCPServer, error)` to the `mcpServerResolver` interface in `mcp_host.go` (so `*MCPServerService` still satisfies it and the discovery method can call it).

- [ ] **Step 2: Add `DiscoverServerToolDefs` to `MCPHost`**

In `internal/agents/mcp_host.go`:
```go
// DiscoveredToolDef is one tool advertised by a live server (for the admin discovery endpoint).
type DiscoveredToolDef struct {
	Name        string
	Description string
}

// DiscoverServerToolDefs best-effort lists one server's tools for the admin UI. A missing/foreign
// server is an error (→ 404). A reachable-but-failing server returns (nil, false, nil) so the UI
// can still edit policies by typed name. Does NOT consult the policy (the handler annotates).
func (h *MCPHost) DiscoverServerToolDefs(ctx context.Context, principalID, businessID, serverID uuid.UUID) ([]DiscoveredToolDef, bool, error) {
	server, err := h.Servers.ResolveEnabledByID(ctx, principalID, businessID, serverID)
	if err != nil {
		return nil, false, err // ErrNotFound → 404
	}
	client := h.Connect(server.URL, server.AuthHeader)
	if err := client.Initialize(ctx); err != nil {
		h.Logger.WarnContext(ctx, "agent.mcp.discover_unreachable", "server_id", serverID, "err", err)
		return nil, false, nil
	}
	defs, err := client.ListTools(ctx)
	if err != nil {
		h.Logger.WarnContext(ctx, "agent.mcp.discover_listfail", "server_id", serverID, "err", err)
		return nil, false, nil
	}
	out := make([]DiscoveredToolDef, 0, len(defs))
	for _, d := range defs {
		out = append(out, DiscoveredToolDef{Name: d.Name, Description: d.Description})
	}
	return out, true, nil
}
```

- [ ] **Step 3: Write the policy handler**

`internal/agents/mcp_tool_policy_handler.go`:
```go
package agents

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// mcpToolPolicyCRUD is the handler's view of the policy service (fakeable).
type mcpToolPolicyCRUD interface {
	List(ctx context.Context, principalID, businessID, serverID uuid.UUID) ([]MCPToolPolicy, error)
	Upsert(ctx context.Context, principalID, businessID, serverID uuid.UUID, toolName, effect string) (MCPToolPolicy, error)
	Delete(ctx context.Context, principalID, businessID, serverID uuid.UUID, toolName string) error
}

// toolDiscoverer is the handler's view of the MCP host (fakeable).
type toolDiscoverer interface {
	DiscoverServerToolDefs(ctx context.Context, principalID, businessID, serverID uuid.UUID) ([]DiscoveredToolDef, bool, error)
}

// MCPToolPolicyHandler serves the per-tool policy + discovery endpoints. Mounted nested under
// /businesses/{id}/mcp_servers/{serverID} (so it shares the agents.configure gate).
type MCPToolPolicyHandler struct {
	policies   mcpToolPolicyCRUD
	discoverer toolDiscoverer
}

func NewMCPToolPolicyHandler(p mcpToolPolicyCRUD, d toolDiscoverer) *MCPToolPolicyHandler {
	return &MCPToolPolicyHandler{policies: p, discoverer: d}
}

// Mount registers the nested routes on a router already scoped to /{serverID}.
func (h *MCPToolPolicyHandler) Mount(r chi.Router) {
	r.Get("/tools", h.listTools)
	r.Get("/tool_policies", h.listPolicies)
	r.Put("/tool_policies/{toolName}", h.putPolicy)
	r.Delete("/tool_policies/{toolName}", h.deletePolicy)
}

type toolDefResp struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Effect      string `json:"effect"` // read | reversible | external
}
type discoverResp struct {
	Reachable bool          `json:"reachable"`
	Tools     []toolDefResp `json:"tools"`
}
type policyResp struct {
	ToolName string `json:"tool_name"`
	Effect   string `json:"effect"`
}

func (h *MCPToolPolicyHandler) listTools(w http.ResponseWriter, r *http.Request) {
	pid, bid, sid, ok := h.ids(w, r)
	if !ok {
		return
	}
	policies, err := h.policies.List(r.Context(), pid, bid, sid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	byName := map[string]string{}
	for _, p := range policies {
		byName[p.ToolName] = p.Effect
	}
	defs, reachable, err := h.discoverer.DiscoverServerToolDefs(r.Context(), pid, bid, sid)
	if err != nil {
		httpx.WriteError(w, r, err) // ErrNotFound → 404 for a foreign/unknown server
		return
	}
	resp := discoverResp{Reachable: reachable, Tools: []toolDefResp{}}
	for _, d := range defs {
		eff := byName[d.Name]
		if eff == "" {
			eff = "external"
		}
		resp.Tools = append(resp.Tools, toolDefResp{Name: d.Name, Description: d.Description, Effect: eff})
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (h *MCPToolPolicyHandler) listPolicies(w http.ResponseWriter, r *http.Request) {
	pid, bid, sid, ok := h.ids(w, r)
	if !ok {
		return
	}
	policies, err := h.policies.List(r.Context(), pid, bid, sid)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := []policyResp{}
	for _, p := range policies {
		out = append(out, policyResp{ToolName: p.ToolName, Effect: p.Effect})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *MCPToolPolicyHandler) putPolicy(w http.ResponseWriter, r *http.Request) {
	pid, bid, sid, ok := h.ids(w, r)
	if !ok {
		return
	}
	toolName := chi.URLParam(r, "toolName")
	var in struct {
		Effect string `json:"effect"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	p, err := h.policies.Upsert(r.Context(), pid, bid, sid, toolName, in.Effect)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, policyResp{ToolName: p.ToolName, Effect: p.Effect})
}

func (h *MCPToolPolicyHandler) deletePolicy(w http.ResponseWriter, r *http.Request) {
	pid, bid, sid, ok := h.ids(w, r)
	if !ok {
		return
	}
	if err := h.policies.Delete(r.Context(), pid, bid, sid, chi.URLParam(r, "toolName")); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ids extracts the principal + business + server ids; any failure is a no-oracle 404.
func (h *MCPToolPolicyHandler) ids(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, uuid.UUID, bool) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	bid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	sid, err := uuid.Parse(chi.URLParam(r, "serverID"))
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	return pid, bid, sid, true
}
```
> Confirm `httpx.PrincipalFromContext`, `httpx.DecodeJSON`, `httpx.WriteJSON`, `httpx.WriteError`, and the chi import path (`github.com/go-chi/chi/v5`) against `mcp_server_handler.go`. Add a missing `context` import.

- [ ] **Step 4: Nest the policy routes under `/{serverID}` in `MCPServerHandler`**

In `internal/agents/mcp_server_handler.go`, give the handler an optional policy field and restructure `ProtectedRoutes` to nest `{serverID}` (semantically identical routing, no chi conflict):
```go
type MCPServerHandler struct {
	svc    mcpServerCRUD
	policy *MCPToolPolicyHandler // manyforge-k0d: optional nested per-tool policy routes
}

func NewMCPServerHandler(svc mcpServerCRUD, policy *MCPToolPolicyHandler) *MCPServerHandler {
	return &MCPServerHandler{svc: svc, policy: policy}
}

func (h *MCPServerHandler) ProtectedRoutes(r chi.Router) {
	r.Route("/businesses/{id}/mcp_servers", func(r chi.Router) {
		r.Get("/", h.listMCPServers)
		r.Post("/", h.createMCPServer)
		r.Route("/{serverID}", func(r chi.Router) {
			r.Get("/", h.getMCPServer)
			r.Patch("/", h.updateMCPServer)
			r.Delete("/", h.deleteMCPServer)
			if h.policy != nil {
				h.policy.Mount(r)
			}
		})
	})
}
```
> The handlers `getMCPServer`/`updateMCPServer`/`deleteMCPServer` already parse `{serverID}` via `mcpPathID` — unchanged. Update the existing `NewMCPServerHandler(svc)` call in `main.go` to the 2-arg form (Step 5).

- [ ] **Step 5: Wire in `main.go`**

In `cmd/manyforge/main.go`, where `mcpServerSvc`/`mcpH`/`mcpHost` are constructed (around lines 254-272):
```go
	mcpServerSvc := &agents.MCPServerService{DB: database, Sealer: mcpSealer}
	mcpPolicySvc := &agents.MCPToolPolicyService{DB: database}
	// ... mcpHTTP / mcpConnect as today ...
	mcpHost := &agents.MCPHost{Servers: mcpServerSvc, Policies: mcpPolicySvc, Connect: mcpConnect, Logger: logger}
	mcpPolicyH := agents.NewMCPToolPolicyHandler(mcpPolicySvc, mcpHost)
	mcpH := agents.NewMCPServerHandler(mcpServerSvc, mcpPolicyH)
	agentEngine.MCP = mcpHost
	approvalExec.MCP = mcpHost
	agentSvc.MCPServers = mcpServerSvc
```
The existing route-mount block (lines 798-804) is unchanged — `h.mcp.ProtectedRoutes(mc)` now also mounts the nested policy routes under the same `h.mcpConfigure` (`agents.configure`) gate.

- [ ] **Step 6: OpenAPI**

In `specs/003-agent-runtime/contracts/openapi.yaml`, add the four paths under the MCP servers section:
```yaml
  /businesses/{id}/mcp_servers/{serverID}/tools:
    get:
      summary: Best-effort live discovery of a server's tools, annotated with their effect policy
      responses:
        '200':
          description: OK
          content:
            application/json:
              schema:
                type: object
                properties:
                  reachable: { type: boolean }
                  tools:
                    type: array
                    items:
                      type: object
                      properties:
                        name: { type: string }
                        description: { type: string }
                        effect: { type: string, enum: [read, reversible, external] }
        '404': { description: Not found }
  /businesses/{id}/mcp_servers/{serverID}/tool_policies/{toolName}:
    put:
      summary: Set a tool's effect policy (promote to read/reversible)
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [effect]
              properties:
                effect: { type: string, enum: [read, reversible] }
      responses:
        '200': { description: OK }
        '400': { description: Validation error }
        '404': { description: Not found }
    delete:
      summary: Clear a tool's effect policy (revert to external default)
      responses:
        '204': { description: Cleared }
        '404': { description: Not found }
```
(Also add a `GET …/tool_policies` list path mirroring the `tools` shape if you want it documented.)

- [ ] **Step 7: Build + gates + commit**

```bash
export PATH="$HOME/go/bin:$PATH"
go build ./... && make test 2>&1 | tail -5 && make sec-test 2>&1 | tail -8
git add internal/agents/mcp_server.go internal/agents/mcp_host.go \
        internal/agents/mcp_tool_policy_handler.go internal/agents/mcp_server_handler.go \
        cmd/manyforge/main.go specs/003-agent-runtime/contracts/openapi.yaml
git commit -m "feat(agents): MCP tool-policy + discovery HTTP API (manyforge-k0d)"
```
Expected: build + gates green.

---

## Task 6: Frontend — `mcp.service.ts`

**Files:**
- Create: `web/src/app/core/mcp.service.ts`

- [ ] **Step 1: Write the service (mirrors `connectors.service.ts`)**

`web/src/app/core/mcp.service.ts`:
```ts
import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';

export interface MCPServer {
  id: string;
  business_id: string;
  name: string;
  url: string;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface CreateMCPServerBody {
  name: string;
  url: string;
  auth_token?: string; // write-only; never returned
}

export interface UpdateMCPServerBody {
  name?: string;
  url?: string;
  enabled?: boolean;
  auth_token?: string; // omit to keep current; "" to clear
}

export type ToolEffect = 'read' | 'reversible' | 'external';

export interface DiscoveredTool {
  name: string;
  description: string;
  effect: ToolEffect;
}

export interface DiscoverToolsResp {
  reachable: boolean;
  tools: DiscoveredTool[];
}

@Injectable({ providedIn: 'root' })
export class McpService {
  private http = inject(HttpClient);

  private base(businessId: string): string {
    return `/api/v1/businesses/${businessId}/mcp_servers`;
  }

  list(businessId: string): Observable<{ items: MCPServer[] }> {
    return this.http.get<{ items: MCPServer[] }>(this.base(businessId));
  }
  create(businessId: string, body: CreateMCPServerBody): Observable<MCPServer> {
    return this.http.post<MCPServer>(this.base(businessId), body);
  }
  update(businessId: string, id: string, body: UpdateMCPServerBody): Observable<MCPServer> {
    return this.http.patch<MCPServer>(`${this.base(businessId)}/${id}`, body);
  }
  remove(businessId: string, id: string): Observable<void> {
    return this.http.delete<void>(`${this.base(businessId)}/${id}`);
  }
  discoverTools(businessId: string, serverId: string): Observable<DiscoverToolsResp> {
    return this.http.get<DiscoverToolsResp>(`${this.base(businessId)}/${serverId}/tools`);
  }
  setPolicy(businessId: string, serverId: string, toolName: string, effect: 'read' | 'reversible'): Observable<{ tool_name: string; effect: string }> {
    return this.http.put<{ tool_name: string; effect: string }>(
      `${this.base(businessId)}/${serverId}/tool_policies/${encodeURIComponent(toolName)}`,
      { effect },
    );
  }
  clearPolicy(businessId: string, serverId: string, toolName: string): Observable<void> {
    return this.http.delete<void>(`${this.base(businessId)}/${serverId}/tool_policies/${encodeURIComponent(toolName)}`);
  }
}
```
> The list response shape: `mcp_server_handler.go listMCPServers` returns `{ items: [...] }`? Confirm — if it returns a bare array, change `list()` to `Observable<MCPServer[]>` and adjust pages. (The connectors handler returns `{ items }`; verify the MCP one matches.)

- [ ] **Step 2: Build check**

```bash
cd web && npm run build 2>&1 | tail -5
```
Expected: build PASS (service compiles). Commit with the pages in Task 9.

---

## Task 7: Frontend — server list + form pages

**Files:**
- Create: `web/src/app/pages/mcp/server-list.ts`
- Create: `web/src/app/pages/mcp/server-form.ts`

- [ ] **Step 1: Write the server-form (write-only auth token)**

`web/src/app/pages/mcp/server-form.ts` (mirrors `connector-form.ts`; auth token is `type=password`, never prefilled, omitted when blank):
```ts
import { HttpErrorResponse } from '@angular/common/http';
import { Component, EventEmitter, Input, OnInit, Output, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { McpService, MCPServer } from '../../core/mcp.service';

@Component({
  selector: 'app-mcp-server-form',
  imports: [FormsModule],
  template: `
    <form class="mf-add-form" data-testid="mcp-server-form" (ngSubmit)="submit()">
      <div class="mf-field" style="flex:1 1 180px">
        <label for="mcp-name">Name</label>
        <input id="mcp-name" type="text" class="mf-input" data-testid="mcp-name"
               [(ngModel)]="name" name="name" autocomplete="off" [disabled]="submitting()" />
        <span style="color:var(--mf-text-faint);font-size:var(--mf-fs-xs)">No colons (used in tool ids).</span>
      </div>
      <div class="mf-field" style="flex:1 1 260px">
        <label for="mcp-url">URL</label>
        <input id="mcp-url" type="url" class="mf-input" data-testid="mcp-url"
               placeholder="https://mcp.example.com" [(ngModel)]="url" name="url" autocomplete="off" [disabled]="submitting()" />
      </div>
      <div class="mf-field" style="flex:1 1 200px">
        <label for="mcp-token">Auth token{{ mode === 'edit' ? ' (leave blank to keep)' : '' }}</label>
        <input id="mcp-token" type="password" class="mf-input" data-testid="mcp-token"
               placeholder="••••••••" [(ngModel)]="authToken" name="auth_token" autocomplete="off" [disabled]="submitting()" />
        <span style="color:var(--mf-text-faint);font-size:var(--mf-fs-xs)">Never shown again.</span>
      </div>
      <div style="display:flex;gap:8px;align-items:flex-end">
        <button type="submit" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="mcp-form-submit"
                [disabled]="submitting() || !valid()">{{ submitting() ? 'Saving…' : (mode === 'create' ? 'Add server' : 'Save') }}</button>
        <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="mcp-form-cancel"
                (click)="cancelled.emit()" [disabled]="submitting()">Cancel</button>
      </div>
      @if (error()) { <p class="mf-err" data-testid="mcp-form-error" style="flex:1 1 100%">{{ error() }}</p> }
    </form>
  `,
})
export class McpServerFormComponent implements OnInit {
  @Input() businessId = '';
  @Input() mode: 'create' | 'edit' = 'create';
  @Input() server: MCPServer | null = null;
  @Output() saved = new EventEmitter<MCPServer>();
  @Output() cancelled = new EventEmitter<void>();

  private api = inject(McpService);
  name = '';
  url = '';
  authToken = '';
  submitting = signal(false);
  error = signal('');

  ngOnInit(): void {
    if (this.mode === 'edit' && this.server) {
      this.name = this.server.name;
      this.url = this.server.url;
      // authToken intentionally NOT prefilled (write-only).
    }
  }

  valid(): boolean {
    if (this.mode === 'create') return !!this.name.trim() && !!this.url.trim() && !this.name.includes(':');
    return !!this.name.trim() && !!this.url.trim() && !this.name.includes(':');
  }

  submit(): void {
    if (this.submitting() || !this.valid()) return;
    this.submitting.set(true);
    this.error.set('');
    const obs =
      this.mode === 'create'
        ? this.api.create(this.businessId, { name: this.name.trim(), url: this.url.trim(), auth_token: this.authToken || undefined })
        : this.api.update(this.businessId, this.server!.id, {
            name: this.name.trim(),
            url: this.url.trim(),
            auth_token: this.authToken || undefined, // omitted when blank → keep current
          });
    obs.subscribe({
      next: (s) => { this.submitting.set(false); this.authToken = ''; this.saved.emit(s); },
      error: (e: HttpErrorResponse) => { this.submitting.set(false); this.error.set(this.describe(e)); },
    });
  }

  private describe(e: HttpErrorResponse): string {
    if (e.status === 400) {
      const msg = (e.error as { message?: string } | null)?.message;
      return msg ? `Rejected: ${msg}` : 'Rejected. Check the values.';
    }
    if (e.status === 409) return 'A server with that name already exists.';
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return 'Could not save. Please try again.';
  }
}
```

- [ ] **Step 2: Write the server-list page**

`web/src/app/pages/mcp/server-list.ts` (mirrors `connectors/list.ts`: business `<select>` via `CurrentBusinessService`, `@for` rows, inline form, optimistic updates, toasts; each server row links to its tools page):
```ts
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { HttpErrorResponse } from '@angular/common/http';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { McpService, MCPServer } from '../../core/mcp.service';
import { Business } from '../../core/tree';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { ToastService } from '../../ui/toast/toast.service';
import { McpServerFormComponent } from './server-form';

@Component({
  selector: 'app-mcp-server-list',
  imports: [FormsModule, RouterLink, PageHeader, EmptyState, McpServerFormComponent],
  template: `
    <div class="mf-card" data-testid="mcp-page">
      <mf-page-header title="MCP servers" [subtitle]="items().length + ' configured'"></mf-page-header>
      <div class="mf-filters">
        <div class="mf-field" style="flex:1 1 220px">
          <label for="mcp-biz">Business</label>
          <select id="mcp-biz" class="mf-select" data-testid="mcp-business-select"
                  [ngModel]="businessId()" (ngModelChange)="selectBusiness($event)">
            <option value="" disabled>Choose a business…</option>
            @for (b of businesses(); track b.id) {
              <option [value]="b.id">{{ b.is_tenant_root ? b.name + ' (master)' : b.name }}</option>
            }
          </select>
        </div>
        <div style="display:flex;align-items:flex-end">
          <button class="mf-btn mf-btn-primary mf-btn-sm" data-testid="mcp-add-toggle"
                  (click)="showAdd.set(!showAdd())" [disabled]="!businessId()">{{ showAdd() ? 'Close' : 'Add server' }}</button>
        </div>
      </div>
      @if (showAdd() && businessId()) {
        <app-mcp-server-form mode="create" [businessId]="businessId()" (saved)="onCreated()" (cancelled)="showAdd.set(false)" />
      }
      <div class="mf-table" data-testid="mcp-list">
        <div class="mf-tr mf-th"><span style="flex:1">Name</span><span style="width:90px">Enabled</span><span style="width:260px"></span></div>
        @for (s of items(); track s.id) {
          <div class="mf-tr" data-testid="mcp-row" [attr.data-server-id]="s.id">
            <span style="flex:1" data-testid="mcp-name-cell">{{ s.name }}<span style="display:block;color:var(--mf-text-faint);font-size:var(--mf-fs-xs)">{{ s.url }}</span></span>
            <span style="width:90px">{{ s.enabled ? 'Yes' : 'No' }}</span>
            <span style="width:260px;display:flex;gap:6px;justify-content:flex-end;flex-wrap:wrap">
              <a class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="mcp-tools"
                 [routerLink]="['/mcp', businessId(), s.id]">Tools</a>
              <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="mcp-edit" (click)="editId.set(editId() === s.id ? '' : s.id)">Edit</button>
              <button class="mf-btn mf-btn-danger mf-btn-sm" data-testid="mcp-delete" (click)="remove(s)">Delete</button>
            </span>
            @if (editId() === s.id) {
              <div style="flex:1 1 100%"><app-mcp-server-form mode="edit" [businessId]="businessId()" [server]="s" (saved)="onEdited()" (cancelled)="editId.set('')" /></div>
            }
          </div>
        }
        @if (!items().length) { <mf-empty-state title="No MCP servers" data-testid="mcp-empty">Add an MCP server to expose its tools to agents.</mf-empty-state> }
      </div>
      @if (error()) { <p class="mf-err" data-testid="mcp-error">{{ error() }}</p> }
    </div>
  `,
})
export class McpServerListComponent implements OnInit {
  private bizApi = inject(BusinessService);
  private api = inject(McpService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  items = signal<MCPServer[]>([]);
  error = signal('');
  showAdd = signal(false);
  editId = signal<string>('');

  ngOnInit(): void {
    this.bizApi.list().subscribe({
      next: (r) => {
        const items = r.items ?? [];
        this.businesses.set(items);
        const id = this.current.businessId() ?? items[0]?.id;
        if (id) { this.businessId.set(id); this.current.set(id); this.reload(); }
      },
      error: () => this.error.set('Could not load businesses'),
    });
  }

  selectBusiness(id: string): void { this.businessId.set(id); this.current.set(id); this.editId.set(''); this.reload(); }

  reload(): void {
    if (!this.businessId()) return;
    const biz = this.businessId();
    this.api.list(biz).subscribe({
      next: (r) => { if (this.businessId() === biz) { this.items.set(r.items ?? []); this.error.set(''); } },
      error: () => { if (this.businessId() === biz) { this.items.set([]); this.error.set('Could not load servers'); } },
    });
  }

  onCreated(): void { this.showAdd.set(false); this.toast.success('Server added'); this.reload(); }
  onEdited(): void { this.editId.set(''); this.toast.success('Server updated'); this.reload(); }

  remove(s: MCPServer): void {
    this.api.remove(this.businessId(), s.id).subscribe({
      next: () => { this.items.update((xs) => xs.filter((x) => x.id !== s.id)); this.toast.success('Server deleted'); },
      error: (e: HttpErrorResponse) => this.toast.error(e.status === 404 ? 'Not found' : 'Delete failed'),
    });
  }
}
```

- [ ] **Step 3: Build**

```bash
cd web && npm run build 2>&1 | tail -5
```
Expected: PASS. Commit with Task 9.

---

## Task 8: Frontend — server tools (effect editor)

**Files:**
- Create: `web/src/app/pages/mcp/server-tools.ts`

- [ ] **Step 1: Write the tools page**

`web/src/app/pages/mcp/server-tools.ts` (route params `:businessId/:serverId`; best-effort discovery; per-tool effect selector; unreachable banner):
```ts
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute } from '@angular/router';
import { HttpErrorResponse } from '@angular/common/http';
import { DiscoveredTool, McpService, ToolEffect } from '../../core/mcp.service';
import { PageHeader } from '../../ui/page-header/page-header';
import { ToastService } from '../../ui/toast/toast.service';

@Component({
  selector: 'app-mcp-server-tools',
  imports: [FormsModule, PageHeader],
  template: `
    <div class="mf-card" data-testid="mcp-tools-page">
      <mf-page-header title="Tool policies" [subtitle]="serverId()"></mf-page-header>
      @if (!reachable()) {
        <p class="mf-err" data-testid="mcp-unreachable">Server unreachable — showing saved policies only. New tools can't be discovered until it's reachable.</p>
      }
      <div class="mf-table" data-testid="mcp-tools-list">
        <div class="mf-tr mf-th"><span style="flex:1">Tool</span><span style="width:200px">Effect</span></div>
        @for (t of tools(); track t.name) {
          <div class="mf-tr" data-testid="mcp-tool-row" [attr.data-tool-name]="t.name">
            <span style="flex:1">{{ t.name }}<span style="display:block;color:var(--mf-text-faint);font-size:var(--mf-fs-xs)">{{ t.description }}</span></span>
            <span style="width:200px">
              <select class="mf-select" data-testid="mcp-tool-effect" [ngModel]="t.effect" (ngModelChange)="setEffect(t, $event)">
                <option value="external">External (default — queues)</option>
                <option value="reversible">Reversible (auto in Assist)</option>
                <option value="read">Safe / read (always auto)</option>
              </select>
            </span>
          </div>
        }
        @if (!tools().length) { <p data-testid="mcp-tools-empty" style="color:var(--mf-text-muted)">No tools.</p> }
      </div>
      @if (error()) { <p class="mf-err" data-testid="mcp-tools-error">{{ error() }}</p> }
    </div>
  `,
})
export class McpServerToolsComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private api = inject(McpService);
  private toast = inject(ToastService);

  businessId = signal('');
  serverId = signal('');
  tools = signal<DiscoveredTool[]>([]);
  reachable = signal(true);
  error = signal('');

  ngOnInit(): void {
    this.businessId.set(this.route.snapshot.paramMap.get('businessId') ?? '');
    this.serverId.set(this.route.snapshot.paramMap.get('serverId') ?? '');
    this.load();
  }

  load(): void {
    this.api.discoverTools(this.businessId(), this.serverId()).subscribe({
      next: (r) => { this.reachable.set(r.reachable); this.tools.set(r.tools ?? []); this.error.set(''); },
      error: (e: HttpErrorResponse) => this.error.set(e.status === 404 ? 'Server not found' : 'Could not load tools'),
    });
  }

  setEffect(t: DiscoveredTool, effect: ToolEffect): void {
    const prev = t.effect;
    if (effect === 'external') {
      this.api.clearPolicy(this.businessId(), this.serverId(), t.name).subscribe({
        next: () => { this.apply(t, 'external'); this.toast.success('Reverted to default'); },
        error: (e: HttpErrorResponse) => this.fail(t, prev, e),
      });
    } else {
      this.api.setPolicy(this.businessId(), this.serverId(), t.name, effect).subscribe({
        next: () => { this.apply(t, effect); this.toast.success('Policy saved'); },
        error: (e: HttpErrorResponse) => this.fail(t, prev, e),
      });
    }
  }

  private apply(t: DiscoveredTool, effect: ToolEffect): void {
    this.tools.update((xs) => xs.map((x) => (x.name === t.name ? { ...x, effect } : x)));
  }
  private fail(t: DiscoveredTool, prev: ToolEffect, e: HttpErrorResponse): void {
    this.apply(t, prev); // revert the optimistic select
    this.toast.error(e.status === 403 || e.status === 404 ? "You don't have access" : 'Could not save policy');
  }
}
```

- [ ] **Step 2: Build**

```bash
cd web && npm run build 2>&1 | tail -5
```
Expected: PASS. Commit with Task 9.

---

## Task 9: Frontend — routes + nav

**Files:**
- Modify: `web/src/app/app.routes.ts`
- Modify: `web/src/app/ui/nav.ts`

- [ ] **Step 1: Register routes**

In `web/src/app/app.routes.ts`, add (after the `connectors` route), using `authGuard` (the only guard; authorization is server-side):
```ts
  {
    path: 'mcp',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/mcp/server-list').then((m) => m.McpServerListComponent),
  },
  {
    path: 'mcp/:businessId/:serverId',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/mcp/server-tools').then((m) => m.McpServerToolsComponent),
  },
```

- [ ] **Step 2: Add nav item**

In `web/src/app/ui/nav.ts`, add to the `NAV_ITEMS` array:
```ts
  { label: 'MCP', route: '/mcp', testid: 'nav-mcp' },
```
> Confirm the `NAV_ITEMS` item shape (label/route/testid) against the existing entries; match it exactly.

- [ ] **Step 3: Build + unit + commit (Tasks 6-9 together)**

```bash
cd web && npm run build 2>&1 | tail -5 && npm test -- --run 2>&1 | tail -10
cd /Users/jigglypuff/dev/manyforge
git add web/src/app/core/mcp.service.ts web/src/app/pages/mcp/ web/src/app/app.routes.ts web/src/app/ui/nav.ts
git commit -m "feat(web): MCP admin UI — server CRUD + per-tool policy editor (manyforge-k0d)"
```
Expected: build PASS; unit tests PASS.

---

## Task 10: Playwright e2e

**Files:**
- Create: `web/e2e/mcp.spec.ts`

- [ ] **Step 1: Write the e2e spec (mirrors `web/e2e/connectors.spec.ts`)**

`web/e2e/mcp.spec.ts` — auth via `localStorage` seed + `page.route` mocks (no backend); covers server CRUD round-trip, tool reclassification round-trip, and the unreachable-server banner:
```ts
import { test, expect, Page } from '@playwright/test';

const profile = { id: 'u1', email: 'admin@x.test', name: 'Admin' };
const biz = { items: [{ id: 'b1', name: 'Acme', is_tenant_root: true }] };

async function auth(page: Page) {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
}

test('MCP server list renders + create', async ({ page }) => {
  await auth(page);
  let created = false;
  await page.route('**/api/v1/businesses/b1/mcp_servers', (r) => {
    if (r.request().method() === 'POST') {
      created = true;
      return r.fulfill({ json: { id: 's1', business_id: 'b1', name: 'acme', url: 'https://m', enabled: true, created_at: '', updated_at: '' } });
    }
    return r.fulfill({ json: { items: created ? [{ id: 's1', business_id: 'b1', name: 'acme', url: 'https://m', enabled: true, created_at: '', updated_at: '' }] : [] } });
  });
  await page.goto('/mcp');
  await expect(page.getByTestId('mcp-page')).toBeVisible();
  await page.getByTestId('mcp-add-toggle').click();
  await page.getByTestId('mcp-name').fill('acme');
  await page.getByTestId('mcp-url').fill('https://m');
  await page.getByTestId('mcp-form-submit').click();
  await expect(page.getByTestId('mcp-row')).toHaveCount(1);
});

test('tool reclassification round-trip', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/mcp_servers/s1/tools', (r) =>
    r.fulfill({ json: { reachable: true, tools: [{ name: 'get_thing', description: 'reads', effect: 'external' }] } }),
  );
  let putBody: unknown = null;
  await page.route('**/api/v1/businesses/b1/mcp_servers/s1/tool_policies/get_thing', (r) => {
    putBody = r.request().postDataJSON();
    return r.fulfill({ json: { tool_name: 'get_thing', effect: 'reversible' } });
  });
  await page.goto('/mcp/b1/s1');
  await expect(page.getByTestId('mcp-tool-row')).toHaveCount(1);
  await page.getByTestId('mcp-tool-effect').selectOption('reversible');
  await expect(page.getByTestId('toast')).toContainText('Policy saved');
  expect(putBody).toEqual({ effect: 'reversible' });
});

test('unreachable server shows banner', async ({ page }) => {
  await auth(page);
  await page.route('**/api/v1/businesses/b1/mcp_servers/s1/tools', (r) =>
    r.fulfill({ json: { reachable: false, tools: [] } }),
  );
  await page.goto('/mcp/b1/s1');
  await expect(page.getByTestId('mcp-unreachable')).toBeVisible();
});
```
> Confirm the toast testid (`toast`) against `connectors.spec.ts` / the toast component. Adjust selectors if the real component differs.

- [ ] **Step 2: Run e2e (dev server must be up on :4300)**

```bash
cd web
# In one terminal: npm start -- --port 4300 --proxy-config proxy.conf.json
npm run e2e -- e2e/mcp.spec.ts
```
Expected: 3 specs PASS.

- [ ] **Step 3: Commit**

```bash
cd /Users/jigglypuff/dev/manyforge
git add web/e2e/mcp.spec.ts
git commit -m "test(web): e2e for MCP admin + tool reclassification (manyforge-k0d)"
```

---

## Task 11: Full gates + close

**Files:** none (verification + bookkeeping)

- [ ] **Step 1: Run every gate green**

```bash
export PATH="$HOME/go/bin:$PATH"
go build ./... && make test && make sec-test && make lint
go test -tags integration ./internal/agents/... 2>&1 | tail -20
cd web && npm run build && npm test -- --run && cd ..
# e2e: start dev server on :4300, then: cd web && npm run e2e
```
Expected: all green. No "pre-existing failure" exceptions.

- [ ] **Step 2: Push + close**

```bash
git pull --rebase    # harmless bd-journal-dirty error if origin isn't ahead; verify next line
git log --oneline origin/master..HEAD    # the ~8 k0d commits
git push
git status           # up to date with origin
bd close manyforge-k0d
git commit -m "chore(bd): close manyforge-k0d (MCP per-tool reclassification + admin UI)" .beads/issues.jsonl 2>/dev/null || true
git push
```

- [ ] **Step 3: Update `HANDOFF.md`** — k0d done; note remaining Spec-003 P4 tail.

---

## Self-review notes (author checklist — applied)

- **Spec coverage:** policy table + `effect IN (0,1)` guardrail (Task 1); sqlc (Task 2); service + audit + run-path resolver (Task 3); gate integration at discovery + pin reframe + new pins + gate-integration/cascade/RLS integration tests (Task 4); discovery endpoint + policy CRUD API + `agents.configure` gate + OpenAPI (Task 5); `mcp.service.ts` (Task 6); server CRUD UI with write-only auth (Task 7); per-tool effect editor + unreachable state (Task 8); routes/nav (Task 9); Playwright e2e (Task 10). Build order from the spec is preserved.
- **Type consistency:** effect smallint `0|1` ↔ `EffectClass` ↔ API strings `read|reversible|external` via `effectFromString`/`effectToString`; `MCPHost.Policies toolPolicyResolver`; `MCPToolPolicyService.ListToolPoliciesByServer` returns `map[string]EffectClass`; handler/service `Upsert(... effect string)`; `NewMCPServerHandler(svc, policy)` 2-arg form used consistently in the route nest + main.go.
- **No placeholders:** every step has literal code/SQL/commands. Items marked "Confirm …/Verify before running …" are genuine pre-flight checks against existing symbols (mock client shape, testdb setup helper, nav-item shape, list-response envelope), not deferred work — each names the exact file to confirm against.
- **Known scope note:** the policy applies at discovery time, so reclassifying a tool affects the *next* agent run, not in-flight runs (documented in reconciliation #2 — no TOCTOU security gap).
```