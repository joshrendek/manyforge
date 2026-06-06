# US7 — Accounting & Reporting Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Surface per-tenant token/cost aggregates and a per-run breakdown (business summary + per-agent table + paginated run list, over preset/custom windows) via API and an Angular page, and move model pricing into a system-catalog table backing the `ai.Registry`.

**Architecture:** Reports `SUM` the `cost_cents` already frozen into each `agent_run` row at execution time (no run-loop changes, no recompute). A new `model_pricing` system-catalog table (no RLS, `GRANT SELECT`, like `permission`) is loaded once at startup into the existing in-memory `ai.Registry` by a loader in the `agents` package (keeps `internal/platform/ai` DB-free). The accounting **summary** is a new `AccountingStore` + `AccountingHandler` at `/businesses/{id}/accounting`; the **runs-list** drill-down extends the existing run feature (`AgentRunStore.ListRuns` + a new `GET` on the runs subtree). A shared pure `ResolveWindow` maps window params to `[from,to)` UTC bounds. RLS scopes every read automatically under `WithPrincipal`.

**Tech Stack:** Go (chi, pgx, sqlc/dbgen, testcontainers), PostgreSQL (RLS), Angular 21 (signals, HttpClient, Vitest), Playwright.

**Spec:** `docs/superpowers/specs/2026-06-05-us7-accounting-design.md` · **bd:** `manyforge-deo.3`

**Conventions (from prior US work — do not relearn):**
- `export PATH="$PATH:$HOME/go/bin"` before `make lint` (golangci-lint lives there; otherwise lint is vet-only — a false pass). `.golangci.yml` does NOT run gofmt — run `gofmt -w` yourself on every touched `.go` file.
- `sqlc` reads `db/schema.sql`, NOT migrations. Mirror every new table/column there (NOT NULL + PK matter; strip `DEFAULT` and `GRANT`/RLS/triggers — those stay migration-only). Run `make generate` after editing `db/query/*.sql` or `db/schema.sql`; never hand-edit `internal/platform/db/dbgen/`.
- gopls lies about fresh `dbgen.X` refs — trust `go build`/`go test`/`make generate`, not IDE diagnostics.
- Throwaway migration-test Postgres MUST use a free port — `55432` is the dev DB. Use `-p 55433:5432 postgres:16`.
- Commits: NO `Co-Authored-By` trailer (project rule). The bd hook re-exports `.beads/issues.jsonl` each commit (cosmetic) — stage it with your changes.
- Latest migration is `0037`; this plan adds `0038`.

---

## File Structure

**Backend — pricing:**
- Create `migrations/0038_model_pricing.up.sql`, `migrations/0038_model_pricing.down.sql` — system-catalog table + seed.
- Modify `db/schema.sql` — mirror `model_pricing`.
- Create `db/query/model_pricing.sql` — `ListModelPricing`.
- Create `internal/agents/model_pricing.go` — `LoadModelRegistry(ctx, db) (*ai.Registry, error)`.
- Create `internal/agents/model_pricing_test.go` — mapping unit test.

**Backend — accounting summary:**
- Create `internal/agents/accounting_window.go` — `ResolveWindow` (pure, shared).
- Create `internal/agents/accounting_window_test.go` — table-driven.
- Create `db/query/accounting.sql` — `AccountingSummaryByAgent`.
- Create `internal/agents/accounting.go` — `AccountingStore`, `Summary`, types.
- Create `internal/agents/accounting_handler.go` — `AccountingHandler`.
- Create `internal/agents/accounting_integration_test.go` — testcontainers.

**Backend — runs-list drill-down (extends existing run feature):**
- Modify `internal/agents/agent_run.go` — add `CreatedAt` to `AgentRun`; add `ListRuns`.
- Create `internal/agents/run_cursor.go` — keyset cursor encode/decode.
- Modify `db/query/agent_run.sql` — add `ListAgentRuns`.
- Modify `internal/agents/agent_run_handler.go` — add `listRuns` + route + `runListItem`.
- Modify `internal/agents/run_service.go` — add `ListRuns` to `runOps` impl.

**Backend — wiring + contract:**
- Modify `cmd/manyforge/main.go` — DB-backed registry; construct + mount `AccountingHandler`.
- Modify `specs/003-agent-runtime/contracts/openapi.yaml` — new paths + schemas.

**Frontend:**
- Create `web/src/app/core/accounting.service.ts` + interfaces.
- Create `web/src/app/core/accounting.service.spec.ts`.
- Create `web/src/app/pages/accounting/summary.ts`.
- Create `web/src/app/pages/accounting/agent-runs.ts`.
- Modify `web/src/app/app.routes.ts` — two routes.
- Modify the dashboard component — "Accounting" nav link.
- Create `web/e2e/accounting.spec.ts` — Playwright regression.

**Security:**
- Create `internal/security_regression/accounting_us7_pins_test.go`.

---

## Task 1: `model_pricing` system-catalog migration + seed

**Files:**
- Create: `migrations/0038_model_pricing.up.sql`, `migrations/0038_model_pricing.down.sql`
- Modify: `db/schema.sql`

System catalog: NO `business_id`, NO RLS — mirrors `permission` (migration `0003_rbac.up.sql`). App role gets `SELECT` only (no runtime write surface). Columns mirror the `ai.Model` struct so the registry loads complete models.

- [ ] **Step 1: Write the up migration**

Create `migrations/0038_model_pricing.up.sql`:

```sql
-- 0038: model_pricing — system catalog for model metadata + pricing (Spec 003 US7).
-- Single source of truth for the ai.Registry (replaces the hardcoded seed.go list in
-- prod; seed.go stays the test fixture). Pricing is integer cents per MILLION tokens.
-- security: system catalog, no tenant scoping (like permission in 0003) — no RLS,
-- SELECT-only grant; writes happen via migration, never from the app.
CREATE TABLE model_pricing (
    model_id              text PRIMARY KEY,
    provider              text NOT NULL,
    display_name          text NOT NULL,
    context_window        integer NOT NULL,
    input_cents_per_mtok  bigint NOT NULL,
    output_cents_per_mtok bigint NOT NULL,
    supports_tools        boolean NOT NULL DEFAULT true,
    enabled               boolean NOT NULL DEFAULT true,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);
GRANT SELECT ON model_pricing TO manyforge_app;

-- Seed mirrors internal/platform/ai/seed.go RegisterDefaults (kept in sync; pinned by
-- TestPin_ModelPricingSeedMatchesDefaults in internal/security_regression).
INSERT INTO model_pricing
    (model_id, provider, display_name, context_window, input_cents_per_mtok, output_cents_per_mtok, supports_tools)
VALUES
    ('claude-sonnet-4-5', 'anthropic', 'Claude Sonnet 4.5', 200000, 300, 1500, true),
    ('claude-opus-4-1',   'anthropic', 'Claude Opus 4.1',   200000, 1500, 7500, true),
    ('claude-haiku-4-5',  'anthropic', 'Claude Haiku 4.5',  200000, 100, 500, true),
    ('gpt-4o',            'openai',    'GPT-4o',             128000, 250, 1000, true),
    ('gpt-4o-mini',       'openai',    'GPT-4o mini',        128000, 15, 60, true);
```

- [ ] **Step 2: Write the down migration**

Create `migrations/0038_model_pricing.down.sql`:

```sql
-- Reverse 0038_model_pricing.
REVOKE SELECT ON model_pricing FROM manyforge_app;
DROP TABLE IF EXISTS model_pricing;
```

- [ ] **Step 3: Mirror into `db/schema.sql`**

Add this table to `db/schema.sql` (find the section with the other catalog/agent tables; placement is not load-bearing). NO defaults, NO grant (schema.sql is the sqlc reference snapshot):

```sql
CREATE TABLE model_pricing (
    model_id              text PRIMARY KEY,
    provider              text NOT NULL,
    display_name          text NOT NULL,
    context_window        integer NOT NULL,
    input_cents_per_mtok  bigint NOT NULL,
    output_cents_per_mtok bigint NOT NULL,
    supports_tools        boolean NOT NULL,
    enabled               boolean NOT NULL,
    created_at            timestamptz NOT NULL,
    updated_at            timestamptz NOT NULL
);
```

- [ ] **Step 4: Verify the migration applies cleanly on a throwaway Postgres**

Run (note port 55433 — NOT the dev DB on 55432):

```bash
docker run -d --rm --name mf-mig-test -p 55433:5432 -e POSTGRES_PASSWORD=pw -e POSTGRES_USER=mf -e POSTGRES_DB=mf postgres:16
sleep 3
# create the app role the GRANT targets, then apply all migrations up to 0038
PGPASSWORD=pw psql -h localhost -p 55433 -U mf -d mf -c "CREATE ROLE manyforge_app;" 2>/dev/null
export MANYFORGE_DATABASE_URL="postgres://mf:pw@localhost:55433/mf?sslmode=disable"
migrate -path migrations -database "$MANYFORGE_DATABASE_URL" up
PGPASSWORD=pw psql -h localhost -p 55433 -U mf -d mf -c "SELECT model_id, input_cents_per_mtok FROM model_pricing ORDER BY model_id;"
migrate -path migrations -database "$MANYFORGE_DATABASE_URL" down -all
docker stop mf-mig-test
```

Expected: 5 rows printed (`claude-haiku-4-5 100`, `claude-opus-4-1 1500`, `claude-sonnet-4-5 300`, `gpt-4o 250`, `gpt-4o-mini 15`); `down -all` succeeds with no error.

- [ ] **Step 5: Commit**

```bash
gofmt -l . >/dev/null  # (no go files changed yet; skip if it errors)
git add migrations/0038_model_pricing.up.sql migrations/0038_model_pricing.down.sql db/schema.sql .beads/issues.jsonl
git commit -m "feat(agents): model_pricing system-catalog table + seed (US7, manyforge-deo.3)"
```

---

## Task 2: DB-backed model registry loader

**Files:**
- Create: `db/query/model_pricing.sql`
- Create: `internal/agents/model_pricing.go`
- Create: `internal/agents/model_pricing_test.go`

Keep `internal/platform/ai` DB-free: the loader lives in `agents` (already imports `ai` and `dbgen`). It reads via `WithTx` (no principal — `model_pricing` has no RLS).

- [ ] **Step 1: Write the query**

Create `db/query/model_pricing.sql`:

```sql
-- name: ListModelPricing :many
SELECT model_id, provider, context_window, input_cents_per_mtok, output_cents_per_mtok, supports_tools
FROM model_pricing
WHERE enabled = true
ORDER BY model_id;
```

- [ ] **Step 2: Generate dbgen**

Run: `make generate`
Expected: `internal/platform/db/dbgen/model_pricing.sql.go` appears with `ListModelPricing(ctx) ([]ListModelPricingRow, error)` and a `ListModelPricingRow` struct. Then `go build ./...` succeeds.

- [ ] **Step 3: Write the failing test (pure row→Model mapping)**

Create `internal/agents/model_pricing_test.go`:

```go
package agents

import (
	"testing"

	"github.com/google/uuid"
	"manyforge/internal/platform/ai"
	"manyforge/internal/platform/db/dbgen"
)

func TestModelRowToAIModel(t *testing.T) {
	row := dbgen.ListModelPricingRow{
		ModelID: "claude-sonnet-4-5", Provider: "anthropic", ContextWindow: 200000,
		InputCentsPerMtok: 300, OutputCentsPerMtok: 1500, SupportsTools: true,
	}
	got := modelRowToAIModel(row)
	want := ai.Model{
		ID: "claude-sonnet-4-5", Provider: "anthropic", ContextWindow: 200000,
		InputCentsPerMTok: 300, OutputCentsPerMTok: 1500, SupportsTools: true,
	}
	if got != want {
		t.Fatalf("modelRowToAIModel = %+v, want %+v", got, want)
	}
	// Cost math must match the registry's (1M-token call at 300/1500 = 300 + 1500).
	if c := got.CostCents(ai.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}); c != 1800 {
		t.Fatalf("CostCents = %d, want 1800", c)
	}
	_ = uuid.Nil // imported for parity with sibling tests; remove if unused
}
```

(Adjust the `dbgen.ListModelPricingRow` field names if `make generate` produced different casing — read the generated struct and match exactly.)

- [ ] **Step 4: Run the test to verify it fails**

Run: `go test ./internal/agents/ -run TestModelRowToAIModel -v`
Expected: FAIL — `undefined: modelRowToAIModel`.

- [ ] **Step 5: Write the loader**

Create `internal/agents/model_pricing.go`:

```go
package agents

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"manyforge/internal/platform/ai"
	"manyforge/internal/platform/db/dbgen"
)

// modelPricingDB is the subset of db.DB the loader needs. model_pricing is a
// system catalog (no RLS), so a plain transaction without a principal reads it.
type modelPricingDB interface {
	WithTx(ctx context.Context, fn func(pgx.Tx) error) error
}

func modelRowToAIModel(r dbgen.ListModelPricingRow) ai.Model {
	return ai.Model{
		ID:                 r.ModelID,
		Provider:           r.Provider,
		ContextWindow:      int(r.ContextWindow),
		InputCentsPerMTok:  r.InputCentsPerMtok,
		OutputCentsPerMTok: r.OutputCentsPerMtok,
		SupportsTools:      r.SupportsTools,
	}
}

// LoadModelRegistry builds an ai.Registry from the model_pricing catalog. It is the
// prod source of truth (ai.RegisterDefaults stays the test fixture). An empty catalog
// is an error — a misconfigured deploy should fail loudly, not run with zero models.
func LoadModelRegistry(ctx context.Context, database modelPricingDB) (*ai.Registry, error) {
	reg := ai.NewRegistry()
	err := database.WithTx(ctx, func(tx pgx.Tx) error {
		rows, e := dbgen.New(tx).ListModelPricing(ctx)
		if e != nil {
			return e
		}
		for _, row := range rows {
			reg.Register(modelRowToAIModel(row))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("agents: load model registry: %w", err)
	}
	if _, ok := reg.Lookup("claude-sonnet-4-5"); !ok {
		return nil, fmt.Errorf("agents: model_pricing catalog is empty or unseeded")
	}
	return reg, nil
}
```

Remove the `uuid` import line from the test if it is unused after writing it.

- [ ] **Step 6: Run the test to verify it passes**

Run: `go test ./internal/agents/ -run TestModelRowToAIModel -v && go build ./...`
Expected: PASS; build clean.

- [ ] **Step 7: Wire main.go to build the registry from the DB**

In `cmd/manyforge/main.go`, replace the registry seed (lines ~155-156):

```go
	aiReg := ai.NewRegistry()
	ai.RegisterDefaults(aiReg)
```

with:

```go
	aiReg, err := agents.LoadModelRegistry(ctx, database)
	if err != nil {
		return fmt.Errorf("load model registry: %w", err)
	}
```

(Use whatever the surrounding `ctx`/error-return convention is — match the adjacent startup calls. If `err` is already declared in scope, use `=`; if not, `:=`. If startup uses `log.Fatal` instead of returning errors, mirror that.)

- [ ] **Step 8: Verify build + existing tests still pass**

Run: `go build ./... && go test ./internal/agents/ ./internal/platform/ai/ 2>&1 | tail -20`
Expected: build clean; existing agent + ai tests PASS (the `Cost` closure still calls `aiReg.Lookup` exactly as before).

- [ ] **Step 9: gofmt + commit**

```bash
gofmt -w internal/agents/model_pricing.go internal/agents/model_pricing_test.go cmd/manyforge/main.go
git add db/query/model_pricing.sql internal/platform/db/dbgen/ internal/agents/model_pricing.go internal/agents/model_pricing_test.go cmd/manyforge/main.go .beads/issues.jsonl
git commit -m "feat(agents): DB-backed ai.Registry from model_pricing (US7)"
```

---

## Task 3: Window resolver (pure, shared)

**Files:**
- Create: `internal/agents/accounting_window.go`
- Create: `internal/agents/accounting_window_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/agents/accounting_window_test.go`:

```go
package agents

import (
	"errors"
	"testing"
	"time"

	"manyforge/internal/platform/errs"
)

func TestResolveWindow(t *testing.T) {
	now := time.Date(2026, 6, 5, 14, 30, 0, 0, time.UTC)
	monthStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	prevMonthStart := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name, win, from, to string
		wantFrom, wantTo    time.Time
		wantErr             bool
	}{
		{name: "default empty", win: "", wantFrom: monthStart, wantTo: now},
		{name: "this_month", win: "this_month", wantFrom: monthStart, wantTo: now},
		{name: "last_month", win: "last_month", wantFrom: prevMonthStart, wantTo: monthStart},
		{name: "last_30_days", win: "last_30_days", wantFrom: now.Add(-30 * 24 * time.Hour), wantTo: now},
		{name: "custom rfc3339", win: "custom", from: "2026-03-01T00:00:00Z", to: "2026-03-31T00:00:00Z",
			wantFrom: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), wantTo: time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)},
		{name: "custom date-only", win: "custom", from: "2026-03-01", to: "2026-03-31",
			wantFrom: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), wantTo: time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)},
		{name: "custom from>to", win: "custom", from: "2026-03-31", to: "2026-03-01", wantErr: true},
		{name: "custom over cap", win: "custom", from: "2024-01-01", to: "2026-01-02", wantErr: true},
		{name: "custom unparseable", win: "custom", from: "nope", to: "2026-03-01", wantErr: true},
		{name: "unknown window", win: "yesterday", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, err := ResolveWindow(c.win, c.from, c.to, now)
			if c.wantErr {
				if !errors.Is(err, errs.ErrValidation) {
					t.Fatalf("want ErrValidation, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !w.From.Equal(c.wantFrom) || !w.To.Equal(c.wantTo) {
				t.Fatalf("got [%s,%s), want [%s,%s)", w.From, w.To, c.wantFrom, c.wantTo)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agents/ -run TestResolveWindow -v`
Expected: FAIL — `undefined: ResolveWindow`.

- [ ] **Step 3: Write the resolver**

Create `internal/agents/accounting_window.go`:

```go
package agents

import (
	"fmt"
	"time"

	"manyforge/internal/platform/errs"
)

// Window is a half-open [From, To) time range in UTC. Reports filter agent_run by
// created_at within this range.
type Window struct {
	From time.Time
	To   time.Time
}

// maxWindowSpan caps a custom range so a report can't trigger an unbounded scan
// (defense in depth alongside the row LIMIT on the runs list).
const maxWindowSpan = 366 * 24 * time.Hour

// ResolveWindow maps a window name (+ optional custom from/to) to an explicit
// [From, To) in UTC. `now` is injected for testability. Presets:
//   "" / "this_month" -> [first-of-month, now]
//   "last_month"      -> [first-of-prev-month, first-of-month]
//   "last_30_days"    -> [now-30d, now]
//   "custom"          -> [from, to], parsed as RFC3339 or YYYY-MM-DD
func ResolveWindow(name, from, to string, now time.Time) (Window, error) {
	n := now.UTC()
	monthStart := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
	switch name {
	case "", "this_month":
		return Window{From: monthStart, To: n}, nil
	case "last_month":
		return Window{From: monthStart.AddDate(0, -1, 0), To: monthStart}, nil
	case "last_30_days":
		return Window{From: n.Add(-30 * 24 * time.Hour), To: n}, nil
	case "custom":
		f, err := parseWindowTime(from)
		if err != nil {
			return Window{}, fmt.Errorf("accounting: bad 'from': %w", errs.ErrValidation)
		}
		t, err := parseWindowTime(to)
		if err != nil {
			return Window{}, fmt.Errorf("accounting: bad 'to': %w", errs.ErrValidation)
		}
		if t.Before(f) {
			return Window{}, fmt.Errorf("accounting: 'from' must be <= 'to': %w", errs.ErrValidation)
		}
		if t.Sub(f) > maxWindowSpan {
			return Window{}, fmt.Errorf("accounting: window exceeds %d days: %w", int(maxWindowSpan.Hours()/24), errs.ErrValidation)
		}
		return Window{From: f, To: t}, nil
	default:
		return Window{}, fmt.Errorf("accounting: unknown window %q: %w", name, errs.ErrValidation)
	}
}

func parseWindowTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/agents/ -run TestResolveWindow -v`
Expected: PASS (all subtests).

- [ ] **Step 5: gofmt + commit**

```bash
gofmt -w internal/agents/accounting_window.go internal/agents/accounting_window_test.go
git add internal/agents/accounting_window.go internal/agents/accounting_window_test.go .beads/issues.jsonl
git commit -m "feat(agents): accounting window resolver (US7)"
```

---

## Task 4: Accounting summary store + query

**Files:**
- Create: `db/query/accounting.sql`
- Create: `internal/agents/accounting.go`
- Create: `internal/agents/accounting_integration_test.go`

- [ ] **Step 1: Write the summary query**

Create `db/query/accounting.sql`:

```sql
-- name: AccountingSummaryByAgent :many
-- Per-agent usage rollup for a business over [from_ts, to_ts). LEFT JOIN so agents
-- with zero runs in the window still appear (with zeros). RLS on agent + agent_run
-- (under WithPrincipal) scopes to the caller's businesses; the business_id arg narrows.
SELECT
    a.id AS agent_id,
    a.name,
    a.monthly_budget_cents,
    COUNT(r.id) AS run_count,
    COALESCE(SUM(r.tokens_in), 0)::bigint  AS tokens_in,
    COALESCE(SUM(r.tokens_out), 0)::bigint AS tokens_out,
    COALESCE(SUM(r.cost_cents), 0)::bigint AS cost_cents
FROM agent a
LEFT JOIN agent_run r
    ON r.agent_id = a.id
    AND r.business_id = a.business_id
    AND r.created_at >= sqlc.arg('from_ts')::timestamptz
    AND r.created_at <  sqlc.arg('to_ts')::timestamptz
WHERE a.business_id = sqlc.arg('business_id')::uuid
GROUP BY a.id, a.name, a.monthly_budget_cents
ORDER BY cost_cents DESC, a.name;
```

- [ ] **Step 2: Generate dbgen**

Run: `make generate && go build ./...`
Expected: `AccountingSummaryByAgent` + its params/row structs generated; build clean. Read the generated `AccountingSummaryByAgentRow` and `...Params` field names — match them in Step 3.

- [ ] **Step 3: Write the store + types**

Create `internal/agents/accounting.go`:

```go
package agents

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"manyforge/internal/platform/db/dbgen"
)

// AgentUsage is one agent's rollup within a window.
type AgentUsage struct {
	AgentID            uuid.UUID
	Name               string
	MonthlyBudgetCents int
	RunCount           int64
	TokensIn           int64
	TokensOut          int64
	CostCents          int64
}

// Summary is a business-wide rollup for a window: totals (summed from the rows) plus
// the per-agent breakdown.
type Summary struct {
	Window     Window
	TotalCost  int64
	TotalIn    int64
	TotalOut   int64
	TotalRuns  int64
	Agents     []AgentUsage
}

type accountingDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
}

// AccountingStore reads usage aggregates. Read-only; separate from AgentRunStore
// (run lifecycle).
type AccountingStore struct{ DB accountingDB }

// SummaryForWindow returns the per-agent rollup for a business over the window,
// with business totals summed from the rows (one round-trip).
func (s *AccountingStore) SummaryForWindow(ctx context.Context, principalID, businessID uuid.UUID, w Window) (Summary, error) {
	out := Summary{Window: w}
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		rows, e := dbgen.New(tx).AccountingSummaryByAgent(ctx, dbgen.AccountingSummaryByAgentParams{
			BusinessID: businessID,
			FromTs:     w.From,
			ToTs:       w.To,
		})
		if e != nil {
			return e
		}
		for _, r := range rows {
			out.Agents = append(out.Agents, AgentUsage{
				AgentID:            r.AgentID,
				Name:               r.Name,
				MonthlyBudgetCents: int(r.MonthlyBudgetCents),
				RunCount:           r.RunCount,
				TokensIn:           r.TokensIn,
				TokensOut:          r.TokensOut,
				CostCents:          r.CostCents,
			})
			out.TotalCost += r.CostCents
			out.TotalIn += r.TokensIn
			out.TotalOut += r.TokensOut
			out.TotalRuns += r.RunCount
		}
		return nil
	})
	if err != nil {
		return Summary{}, mapAgentRunErr(err)
	}
	return out, nil
}
```

(If the generated param field is `Fromts`/`Tots` rather than `FromTs`/`ToTs`, match the generator. If `MonthlyBudgetCents` is `int32`, the `int(...)` cast still holds.)

- [ ] **Step 4: Write the failing integration test**

Create `internal/agents/accounting_integration_test.go`. Mirror the harness in the existing agent integration tests (find one, e.g. `mcp_integration_test.go` or `agent_run_*_test.go`, and reuse its testcontainers Postgres + seed helpers — DB setup, a `newTestDB(t)`, principal/business/agent seed helpers). Replace `<seed helpers>` with the actual ones in this package:

```go
//go:build integration

package agents

import (
	"context"
	"testing"
	"time"
)

func TestAccountingSummary_Integration(t *testing.T) {
	ctx := context.Background()
	tdb := newTestDB(t) // existing helper: testcontainers PG, migrated, returns *db.DB

	// Seed: principal + business + two agents (one with runs, one with none) + runs.
	pid, bid := seedPrincipalAndBusiness(t, tdb) // existing helper
	agentA := seedAgent(t, tdb, pid, bid, "Agent A", 10_000 /* budget cents */)
	agentB := seedAgent(t, tdb, pid, bid, "Agent B", 0)
	now := time.Now().UTC()
	// agentA: 2 runs this month, costs 120 + 80 = 200 cents, tokens 100/200 + 50/60.
	seedRun(t, tdb, pid, bid, agentA, 100, 200, 120, now.Add(-1*time.Hour))
	seedRun(t, tdb, pid, bid, agentA, 50, 60, 80, now.Add(-2*time.Hour))
	// agentB: a run LAST month — must NOT count in a this_month window.
	seedRun(t, tdb, pid, bid, agentB, 999, 999, 999, now.AddDate(0, -1, -2))

	store := &AccountingStore{DB: tdb}
	w, err := ResolveWindow("this_month", "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := store.SummaryForWindow(ctx, pid, bid, w)
	if err != nil {
		t.Fatal(err)
	}

	if sum.TotalCost != 200 || sum.TotalRuns != 2 {
		t.Fatalf("totals: cost=%d runs=%d, want 200/2", sum.TotalCost, sum.TotalRuns)
	}
	if len(sum.Agents) != 2 {
		t.Fatalf("want 2 agent rows (incl. zero-run Agent B), got %d", len(sum.Agents))
	}
	// Agent A first (ORDER BY cost_cents DESC); Agent B present with zeros.
	if sum.Agents[0].CostCents != 200 || sum.Agents[0].RunCount != 2 {
		t.Fatalf("agent A row wrong: %+v", sum.Agents[0])
	}
	if sum.Agents[1].CostCents != 0 || sum.Agents[1].RunCount != 0 {
		t.Fatalf("agent B (zero-run) should be all zeros: %+v", sum.Agents[1])
	}
}

func TestAccountingSummary_CrossTenantInvisible(t *testing.T) {
	ctx := context.Background()
	tdb := newTestDB(t)
	pidA, bidA := seedPrincipalAndBusiness(t, tdb)
	_, bidB := seedPrincipalAndBusiness(t, tdb) // a different tenant
	store := &AccountingStore{DB: tdb}
	w, _ := ResolveWindow("this_month", "", "", time.Now().UTC())

	// Principal A asking about business B sees an empty summary (RLS → no rows),
	// never B's agents. No oracle: same shape as an unknown business id.
	sum, err := store.SummaryForWindow(ctx, pidA, bidB, w)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Agents) != 0 || sum.TotalCost != 0 {
		t.Fatalf("cross-tenant leak: %+v", sum)
	}
}
```

If this package has no `newTestDB`/`seedAgent`/`seedRun` helpers, add them (or adapt to the existing ones — read a sibling `*_integration_test.go` first and copy its setup verbatim, renaming as needed). Do NOT invent a new harness if one exists.

- [ ] **Step 5: Run to verify it fails, then passes**

Run: `go test -tags integration ./internal/agents/ -run TestAccountingSummary -p 1 -v`
Expected: first FAIL if helpers/store missing → implement/adjust → PASS (both tests). (Requires Docker.)

- [ ] **Step 6: gofmt + commit**

```bash
gofmt -w internal/agents/accounting.go internal/agents/accounting_integration_test.go
git add db/query/accounting.sql internal/platform/db/dbgen/ internal/agents/accounting.go internal/agents/accounting_integration_test.go .beads/issues.jsonl
git commit -m "feat(agents): accounting summary store + RLS-scoped aggregate (US7)"
```

---

## Task 5: Accounting summary HTTP endpoint + OpenAPI

**Files:**
- Create: `internal/agents/accounting_handler.go`
- Modify: `cmd/manyforge/main.go`
- Modify: `specs/003-agent-runtime/contracts/openapi.yaml`

- [ ] **Step 1: Write the handler**

Create `internal/agents/accounting_handler.go`:

```go
package agents

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"manyforge/internal/platform/errs"
	"manyforge/internal/platform/httpx"
)

type summaryOps interface {
	SummaryForWindow(ctx context.Context, principalID, businessID uuid.UUID, w Window) (Summary, error)
}

type AccountingHandler struct{ svc summaryOps }

func NewAccountingHandler(svc summaryOps) *AccountingHandler { return &AccountingHandler{svc: svc} }

func (h *AccountingHandler) ProtectedRoutes(r chi.Router) {
	r.Get("/businesses/{id}/accounting", h.getSummary)
}

type agentUsageResp struct {
	AgentID            uuid.UUID `json:"agent_id"`
	Name               string    `json:"name"`
	MonthlyBudgetCents int       `json:"monthly_budget_cents"`
	RunCount           int64     `json:"run_count"`
	TokensIn           int64     `json:"tokens_in"`
	TokensOut          int64     `json:"tokens_out"`
	CostCents          int64     `json:"cost_cents"`
	BudgetPct          *int      `json:"budget_pct,omitempty"`
}

type summaryResp struct {
	Window struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	} `json:"window"`
	Totals struct {
		CostCents int64 `json:"cost_cents"`
		TokensIn  int64 `json:"tokens_in"`
		TokensOut int64 `json:"tokens_out"`
		RunCount  int64 `json:"run_count"`
	} `json:"totals"`
	Agents []agentUsageResp `json:"agents"`
}

func (h *AccountingHandler) getSummary(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	q := r.URL.Query()
	winName := q.Get("window")
	win, err := ResolveWindow(winName, q.Get("from"), q.Get("to"), time.Now().UTC())
	if err != nil {
		httpx.WriteError(w, r, err) // ErrValidation -> 400 (safe message)
		return
	}
	sum, err := h.svc.SummaryForWindow(r.Context(), pid, bid, win)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toSummaryResp(sum, winName))
}

// budget_pct is only meaningful for the monthly budget, so it is populated only when
// the requested window is the current month and the agent has a budget set.
func toSummaryResp(s Summary, winName string) summaryResp {
	var out summaryResp
	out.Window.From, out.Window.To = s.Window.From, s.Window.To
	out.Totals.CostCents, out.Totals.TokensIn = s.TotalCost, s.TotalIn
	out.Totals.TokensOut, out.Totals.RunCount = s.TotalOut, s.TotalRuns
	thisMonth := winName == "" || winName == "this_month"
	for _, a := range s.Agents {
		item := agentUsageResp{
			AgentID: a.AgentID, Name: a.Name, MonthlyBudgetCents: a.MonthlyBudgetCents,
			RunCount: a.RunCount, TokensIn: a.TokensIn, TokensOut: a.TokensOut, CostCents: a.CostCents,
		}
		if thisMonth && a.MonthlyBudgetCents > 0 {
			pct := int(a.CostCents * 100 / int64(a.MonthlyBudgetCents))
			item.BudgetPct = &pct
		}
		out.Agents = append(out.Agents, item)
	}
	return out
}
```

- [ ] **Step 2: Wire main.go**

In `cmd/manyforge/main.go`: construct the store + handler near the other agent wiring (after `agentRunStore` exists), add the handler to the `apiHandlers` struct, and mount it in the agents group.

Construction (near line ~173, next to `agentRunH`):

```go
	accountingStore := &agents.AccountingStore{DB: database}
	accountingH := agents.NewAccountingHandler(accountingStore)
```

Add a field to the `apiHandlers` struct definition and populate it (near line ~425 where `agentRuns` is set):

```go
	accounting:      accountingH,
```

Mount inside the existing agents-run group so it inherits the `agents.run` gate (near line ~681):

```go
	pr.Group(func(ag chi.Router) {
		ag.Use(h.agentsRun)
		h.agentRuns.ProtectedRoutes(ag)
		h.accounting.ProtectedRoutes(ag)
	})
```

Add the struct field (find the `apiHandlers` struct and add):

```go
	accounting *agents.AccountingHandler
```

- [ ] **Step 3: Build + run a smoke check**

Run: `go build ./... && go test ./internal/agents/ -run TestResolveWindow 2>&1 | tail -5`
Expected: build clean (wiring compiles), unit tests still PASS.

- [ ] **Step 4: Add OpenAPI paths + schemas**

In `specs/003-agent-runtime/contracts/openapi.yaml`, add the path (under `paths:`):

```yaml
  /businesses/{id}/accounting:
    get:
      operationId: getBusinessAccounting
      summary: Per-agent token/cost rollup for a business over a window
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
        - { name: window, in: query, required: false, schema: { type: string, enum: [this_month, last_month, last_30_days, custom] } }
        - { name: from, in: query, required: false, schema: { type: string } }
        - { name: to, in: query, required: false, schema: { type: string } }
      responses:
        "200":
          description: Accounting summary
          content:
            application/json:
              schema: { $ref: "#/components/schemas/AccountingSummary" }
        "400": { description: Invalid window }
        "404": { description: Not found / not permitted }
```

And under `components: schemas:`:

```yaml
    AccountingSummary:
      type: object
      required: [window, totals, agents]
      properties:
        window:
          type: object
          required: [from, to]
          properties:
            from: { type: string, format: date-time }
            to: { type: string, format: date-time }
        totals:
          type: object
          required: [cost_cents, tokens_in, tokens_out, run_count]
          properties:
            cost_cents: { type: integer }
            tokens_in: { type: integer }
            tokens_out: { type: integer }
            run_count: { type: integer }
        agents:
          type: array
          items: { $ref: "#/components/schemas/AgentUsage" }
    AgentUsage:
      type: object
      required: [agent_id, name, monthly_budget_cents, run_count, tokens_in, tokens_out, cost_cents]
      properties:
        agent_id: { type: string, format: uuid }
        name: { type: string }
        monthly_budget_cents: { type: integer }
        run_count: { type: integer }
        tokens_in: { type: integer }
        tokens_out: { type: integer }
        cost_cents: { type: integer }
        budget_pct: { type: integer, nullable: true }
```

- [ ] **Step 5: Run contract test**

Run: `make contract-test 2>&1 | tail -20`
Expected: PASS (new path/schema recognized; no drift). If the contract test enumerates routes from the router, confirm the new route is included; fix any path-string mismatch.

- [ ] **Step 6: gofmt + commit**

```bash
gofmt -w internal/agents/accounting_handler.go cmd/manyforge/main.go
git add internal/agents/accounting_handler.go cmd/manyforge/main.go specs/003-agent-runtime/contracts/openapi.yaml .beads/issues.jsonl
git commit -m "feat(agents): GET /businesses/{id}/accounting summary endpoint + OpenAPI (US7)"
```

---

## Task 6: Runs-list store + keyset cursor (drill-down)

**Files:**
- Modify: `internal/agents/agent_run.go`
- Create: `internal/agents/run_cursor.go`
- Modify: `db/query/agent_run.sql`

- [ ] **Step 1: Add `CreatedAt` to the `AgentRun` struct + mapping**

In `internal/agents/agent_run.go`, add to the `AgentRun` struct (after `Error *string`):

```go
	CreatedAt     time.Time
```

(Add `"time"` to the imports if not present.) And in `toAgentRun`, add the mapping line before `return out`:

```go
	out.CreatedAt = r.CreatedAt
```

This does not change `getRun`'s response (`runResp` does not include it).

- [ ] **Step 2: Write the failing cursor test**

Create `internal/agents/run_cursor_test.go`:

```go
package agents

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRunCursorRoundTrip(t *testing.T) {
	ts := time.Date(2026, 6, 5, 14, 30, 0, 123456789, time.UTC)
	id := uuid.New()
	tok := encodeRunCursor(runKeyset{ts: ts, id: id})
	got, err := decodeRunCursor(tok)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ts.Equal(ts) || got.id != id {
		t.Fatalf("round-trip mismatch: got %v/%v want %v/%v", got.ts, got.id, ts, id)
	}
	if _, err := decodeRunCursor("not-base64!!"); err == nil {
		t.Fatal("expected error on garbage cursor")
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/agents/ -run TestRunCursorRoundTrip -v`
Expected: FAIL — `undefined: encodeRunCursor`.

- [ ] **Step 4: Write the cursor helper**

Create `internal/agents/run_cursor.go` (mirrors `internal/ticketing/cursor.go`, kind `run`):

```go
package agents

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"manyforge/internal/platform/errs"
)

type runKeyset struct {
	ts time.Time
	id uuid.UUID
}

const runCursorSep = "|"

func encodeRunCursor(k runKeyset) string {
	raw := "run" + runCursorSep + k.ts.UTC().Format(time.RFC3339Nano) + runCursorSep + k.id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeRunCursor(token string) (runKeyset, error) {
	bad := func() (runKeyset, error) {
		return runKeyset{}, fmt.Errorf("invalid cursor: %w", errs.ErrValidation)
	}
	dec, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return bad()
	}
	parts := strings.SplitN(string(dec), runCursorSep, 3)
	if len(parts) != 3 || parts[0] != "run" {
		return bad()
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return bad()
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return bad()
	}
	return runKeyset{ts: ts, id: id}, nil
}
```

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/agents/ -run TestRunCursorRoundTrip -v`
Expected: PASS.

- [ ] **Step 6: Add the runs-list query**

Append to `db/query/agent_run.sql`:

```sql
-- name: ListAgentRuns :many
-- Keyset-paginated runs for one agent over [from_ts, to_ts), newest first. The cursor
-- tuple (cur_created_at, cur_id) is passed as ('infinity', max-uuid) for page 1. RLS
-- (under WithPrincipal) scopes to the caller's businesses; business_id+agent_id narrow.
SELECT * FROM agent_run
WHERE business_id = sqlc.arg('business_id')::uuid
  AND agent_id = sqlc.arg('agent_id')::uuid
  AND created_at >= sqlc.arg('from_ts')::timestamptz
  AND created_at <  sqlc.arg('to_ts')::timestamptz
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
  AND (created_at, id) < (sqlc.arg('cur_created_at')::timestamptz, sqlc.arg('cur_id')::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('lim');
```

- [ ] **Step 7: Generate dbgen**

Run: `make generate && go build ./...`
Expected: `ListAgentRuns` + params generated; build clean. Read the generated `ListAgentRunsParams` field names for Step (Task 6.8).

- [ ] **Step 8: Add `ListRuns` to `AgentRunStore`**

In `internal/agents/agent_run.go`, add (mirrors `Get`; clamps limit, fetches `limit+1` to detect overflow, encodes next cursor):

```go
// RunListFilter narrows a run list. Status "" = all statuses.
type RunListFilter struct {
	Status string
	Window Window
}

const (
	runListDefaultLimit = 50
	runListMaxLimit     = 100
)

func clampRunLimit(n int) int {
	if n <= 0 {
		return runListDefaultLimit
	}
	if n > runListMaxLimit {
		return runListMaxLimit
	}
	return n
}

// ListRuns returns a keyset page of an agent's runs (newest first) plus the next
// cursor (nil when exhausted). cursor "" starts at the newest run.
func (s *AgentRunStore) ListRuns(ctx context.Context, principalID, businessID, agentID uuid.UUID, f RunListFilter, cursor string, limit int) ([]AgentRun, *string, error) {
	lim := clampRunLimit(limit)

	// Page 1 sentinel: a tuple greater than any real row in (created_at, id) DESC.
	curTs := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	curID := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	if cursor != "" {
		k, err := decodeRunCursor(cursor)
		if err != nil {
			return nil, nil, err
		}
		curTs, curID = k.ts, k.id
	}

	var status *string
	if f.Status != "" {
		status = &f.Status
	}

	var rows []dbgen.AgentRun
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, e := dbgen.New(tx).ListAgentRuns(ctx, dbgen.ListAgentRunsParams{
			BusinessID:   businessID,
			AgentID:      agentID,
			FromTs:       f.Window.From,
			ToTs:         f.Window.To,
			Status:       status,
			CurCreatedAt: curTs,
			CurID:        curID,
			Lim:          int32(lim + 1),
		})
		rows = r
		return e
	})
	if err != nil {
		return nil, nil, mapAgentRunErr(err)
	}

	var next *string
	if len(rows) > lim {
		last := rows[lim-1]
		tok := encodeRunCursor(runKeyset{ts: last.CreatedAt, id: last.ID})
		next = &tok
		rows = rows[:lim]
	}
	out := make([]AgentRun, 0, len(rows))
	for _, r := range rows {
		out = append(out, toAgentRun(r))
	}
	return out, next, nil
}
```

(Match `dbgen.ListAgentRunsParams` field names + nullable `Status` type exactly to the generated code — if `Status` is generated as `pgtype.Text`, adapt; if `*string`, the above is correct. `Lim` type may be `int32` or `int64`.)

- [ ] **Step 9: Build**

Run: `go build ./...`
Expected: clean.

- [ ] **Step 10: gofmt + commit**

```bash
gofmt -w internal/agents/agent_run.go internal/agents/run_cursor.go internal/agents/run_cursor_test.go
git add internal/agents/agent_run.go internal/agents/run_cursor.go internal/agents/run_cursor_test.go db/query/agent_run.sql internal/platform/db/dbgen/ .beads/issues.jsonl
git commit -m "feat(agents): AgentRunStore.ListRuns keyset pagination + run cursor (US7)"
```

---

## Task 7: Runs-list endpoint + integration test

**Files:**
- Modify: `internal/agents/agent_run_handler.go`
- Modify: `internal/agents/run_service.go`
- Modify: `specs/003-agent-runtime/contracts/openapi.yaml`
- Modify: `internal/agents/accounting_integration_test.go`

- [ ] **Step 1: Add `ListRuns` to the `runOps` interface + `RunService`**

In `internal/agents/agent_run_handler.go`, extend the `runOps` interface:

```go
type runOps interface {
	Trigger(ctx context.Context, principalID, businessID, agentID uuid.UUID, trigger string, targetType *string, targetID *uuid.UUID) (AgentRun, error)
	GetRun(ctx context.Context, principalID, businessID, agentID, runID uuid.UUID) (AgentRun, error)
	ListRuns(ctx context.Context, principalID, businessID, agentID uuid.UUID, f RunListFilter, cursor string, limit int) ([]AgentRun, *string, error)
}
```

In `internal/agents/run_service.go`, add a delegating method (find the `RunService` struct; it holds the run store — match the field name used by `GetRun`, shown here as `runs`):

```go
// ListRuns delegates to the run store (keyset pagination).
func (s *RunService) ListRuns(ctx context.Context, principalID, businessID, agentID uuid.UUID, f RunListFilter, cursor string, limit int) ([]AgentRun, *string, error) {
	return s.runs.ListRuns(ctx, principalID, businessID, agentID, f, cursor, limit)
}
```

(Read `run_service.go` first: use the SAME receiver field that `GetRun` uses to reach the store. If `GetRun` calls `s.store.Get(...)`, write `s.store.ListRuns(...)`.)

- [ ] **Step 2: Add the `listRuns` handler + route + response type**

In `internal/agents/agent_run_handler.go`, register the collection GET inside the existing `ProtectedRoutes` Route block:

```go
func (h *RunHandler) ProtectedRoutes(r chi.Router) {
	r.Route("/businesses/{id}/agents/{agentID}/runs", func(r chi.Router) {
		r.Post("/", h.triggerRun)
		r.Get("/", h.listRuns)
		r.Get("/{runID}", h.getRun)
	})
}
```

Add the response type (extends `runResp` with `created_at`) + parse helpers + handler:

```go
type runListItem struct {
	ID            uuid.UUID `json:"id"`
	AgentID       uuid.UUID `json:"agent_id"`
	Trigger       string    `json:"trigger"`
	Status        string    `json:"status"`
	TokensIn      int       `json:"tokens_in"`
	TokensOut     int       `json:"tokens_out"`
	CostCents     int64     `json:"cost_cents"`
	CorrelationID string    `json:"correlation_id"`
	Error         *string   `json:"error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

func toRunListItem(r AgentRun) runListItem {
	return runListItem{
		ID: r.ID, AgentID: r.AgentID, Trigger: r.Trigger, Status: r.Status,
		TokensIn: r.TokensIn, TokensOut: r.TokensOut, CostCents: r.CostCents,
		CorrelationID: r.CorrelationID, Error: r.Error, CreatedAt: r.CreatedAt,
	}
}

func (h *RunHandler) listRuns(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := runBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	aid, err := runAgentID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	q := r.URL.Query()
	win, err := ResolveWindow(q.Get("window"), q.Get("from"), q.Get("to"), time.Now().UTC())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			limit = n
		}
	}
	runs, next, err := h.svc.ListRuns(r.Context(), pid, bid, aid, RunListFilter{Status: q.Get("status"), Window: win}, q.Get("cursor"), limit)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	items := make([]runListItem, 0, len(runs))
	for _, rn := range runs {
		items = append(items, toRunListItem(rn))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}
```

Add `"strconv"` and `"time"` to the handler's imports.

- [ ] **Step 3: Build**

Run: `go build ./...`
Expected: clean.

- [ ] **Step 4: Add a runs-list integration test**

Append to `internal/agents/accounting_integration_test.go`:

```go
func TestListAgentRuns_Pagination(t *testing.T) {
	ctx := context.Background()
	tdb := newTestDB(t)
	pid, bid := seedPrincipalAndBusiness(t, tdb)
	ag := seedAgent(t, tdb, pid, bid, "Agent A", 0)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		seedRun(t, tdb, pid, bid, ag, 10, 20, int64(10+i), now.Add(-time.Duration(i)*time.Hour))
	}
	store := &AgentRunStore{DB: tdb}
	w, _ := ResolveWindow("this_month", "", "", now)

	page1, next, err := store.ListRuns(ctx, pid, bid, ag, RunListFilter{Window: w}, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || next == nil {
		t.Fatalf("page1: len=%d next=%v, want 2 + a cursor", len(page1), next)
	}
	if page1[0].CreatedAt.Before(page1[1].CreatedAt) {
		t.Fatal("runs must be newest-first")
	}
	page2, next2, err := store.ListRuns(ctx, pid, bid, ag, RunListFilter{Window: w}, *next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 || next2 != nil {
		t.Fatalf("page2: len=%d next=%v, want 1 + nil", len(page2), next2)
	}
}
```

- [ ] **Step 5: Run integration tests**

Run: `go test -tags integration ./internal/agents/ -run 'TestAccountingSummary|TestListAgentRuns' -p 1 -v`
Expected: PASS (3 tests; Docker required).

- [ ] **Step 6: Add OpenAPI for the runs-list**

In `specs/003-agent-runtime/contracts/openapi.yaml`, add a `get` to the existing `/businesses/{id}/agents/{agentID}/runs` path (alongside the existing `post`), and a `RunSummary` schema:

```yaml
    get:
      operationId: listAgentRuns
      summary: List an agent's runs (keyset paginated)
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
        - { name: agentID, in: path, required: true, schema: { type: string, format: uuid } }
        - { name: status, in: query, required: false, schema: { type: string } }
        - { name: window, in: query, required: false, schema: { type: string, enum: [this_month, last_month, last_30_days, custom] } }
        - { name: from, in: query, required: false, schema: { type: string } }
        - { name: to, in: query, required: false, schema: { type: string } }
        - { name: cursor, in: query, required: false, schema: { type: string } }
        - { name: limit, in: query, required: false, schema: { type: integer } }
      responses:
        "200":
          description: A page of runs
          content:
            application/json:
              schema:
                type: object
                properties:
                  items: { type: array, items: { $ref: "#/components/schemas/RunSummary" } }
                  next_cursor: { type: string, nullable: true }
```

And the schema (mirrors `Run` + `created_at`):

```yaml
    RunSummary:
      type: object
      required: [id, agent_id, trigger, status, tokens_in, tokens_out, cost_cents, correlation_id, created_at]
      properties:
        id: { type: string, format: uuid }
        agent_id: { type: string, format: uuid }
        trigger: { type: string }
        status: { type: string }
        tokens_in: { type: integer }
        tokens_out: { type: integer }
        cost_cents: { type: integer }
        correlation_id: { type: string }
        error: { type: string, nullable: true }
        created_at: { type: string, format: date-time }
```

- [ ] **Step 7: Contract test + commit**

Run: `make contract-test 2>&1 | tail -20`
Expected: PASS.

```bash
gofmt -w internal/agents/agent_run_handler.go internal/agents/run_service.go internal/agents/accounting_integration_test.go
git add internal/agents/agent_run_handler.go internal/agents/run_service.go internal/agents/accounting_integration_test.go specs/003-agent-runtime/contracts/openapi.yaml .beads/issues.jsonl
git commit -m "feat(agents): GET runs list (drill-down) + OpenAPI RunSummary (US7)"
```

---

## Task 8: Frontend accounting service

**Files:**
- Create: `web/src/app/core/accounting.service.ts`
- Create: `web/src/app/core/accounting.service.spec.ts`

- [ ] **Step 1: Write the service**

Create `web/src/app/core/accounting.service.ts` (mirrors `ticket.service.ts`):

```typescript
import { HttpClient, HttpParams } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';

export type WindowName = 'this_month' | 'last_month' | 'last_30_days' | 'custom';

export interface AgentUsage {
  agent_id: string;
  name: string;
  monthly_budget_cents: number;
  run_count: number;
  tokens_in: number;
  tokens_out: number;
  cost_cents: number;
  budget_pct?: number | null;
}

export interface AccountingSummary {
  window: { from: string; to: string };
  totals: { cost_cents: number; tokens_in: number; tokens_out: number; run_count: number };
  agents: AgentUsage[];
}

export interface RunSummary {
  id: string;
  agent_id: string;
  trigger: string;
  status: string;
  tokens_in: number;
  tokens_out: number;
  cost_cents: number;
  correlation_id: string;
  error?: string | null;
  created_at: string;
}

export interface RunPage {
  items: RunSummary[];
  next_cursor: string | null;
}

export interface RunListFilters {
  status?: string;
  window?: WindowName;
  from?: string;
  to?: string;
  cursor?: string;
  limit?: number;
}

@Injectable({ providedIn: 'root' })
export class AccountingService {
  private http = inject(HttpClient);

  getSummary(businessId: string, window?: WindowName, from?: string, to?: string): Observable<AccountingSummary> {
    let params = new HttpParams();
    if (window) params = params.set('window', window);
    if (from) params = params.set('from', from);
    if (to) params = params.set('to', to);
    return this.http.get<AccountingSummary>(`/api/v1/businesses/${businessId}/accounting`, { params });
  }

  listRuns(businessId: string, agentId: string, filters: RunListFilters = {}): Observable<RunPage> {
    let params = new HttpParams();
    if (filters.status) params = params.set('status', filters.status);
    if (filters.window) params = params.set('window', filters.window);
    if (filters.from) params = params.set('from', filters.from);
    if (filters.to) params = params.set('to', filters.to);
    if (filters.cursor) params = params.set('cursor', filters.cursor);
    if (filters.limit != null) params = params.set('limit', String(filters.limit));
    return this.http.get<RunPage>(`/api/v1/businesses/${businessId}/agents/${agentId}/runs`, { params });
  }
}
```

- [ ] **Step 2: Write the spec (Vitest + HttpTestingController)**

Create `web/src/app/core/accounting.service.spec.ts` (mirror `auth.interceptor.spec.ts` setup):

```typescript
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import { AccountingService } from './accounting.service';

describe('AccountingService', () => {
  let svc: AccountingService;
  let mock: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    svc = TestBed.inject(AccountingService);
    mock = TestBed.inject(HttpTestingController);
  });

  it('getSummary hits the accounting endpoint with the window param', () => {
    svc.getSummary('biz-1', 'last_month').subscribe();
    const req = mock.expectOne((r) => r.url === '/api/v1/businesses/biz-1/accounting');
    expect(req.request.params.get('window')).toBe('last_month');
    req.flush({ window: { from: '', to: '' }, totals: { cost_cents: 0, tokens_in: 0, tokens_out: 0, run_count: 0 }, agents: [] });
  });

  it('listRuns hits the runs endpoint with status + cursor', () => {
    svc.listRuns('biz-1', 'ag-1', { status: 'succeeded', cursor: 'abc' }).subscribe();
    const req = mock.expectOne((r) => r.url === '/api/v1/businesses/biz-1/agents/ag-1/runs');
    expect(req.request.params.get('status')).toBe('succeeded');
    expect(req.request.params.get('cursor')).toBe('abc');
    req.flush({ items: [], next_cursor: null });
  });
});
```

- [ ] **Step 3: Run the unit tests**

Run (from `web/`): `npm run test -- --run accounting.service`
Expected: PASS (2 tests). (If `ng test` doesn't accept `--run`, run `npm run test` and confirm the file passes.)

- [ ] **Step 4: Commit**

```bash
git add web/src/app/core/accounting.service.ts web/src/app/core/accounting.service.spec.ts ../.beads/issues.jsonl
git commit -m "feat(web): accounting API service + unit tests (US7)"
```

(Adjust the `.beads/issues.jsonl` relative path to repo root as needed.)

---

## Task 9: Frontend summary page + route + nav

**Files:**
- Create: `web/src/app/pages/accounting/summary.ts`
- Modify: `web/src/app/app.routes.ts`
- Modify: the dashboard component template

- [ ] **Step 1: Write the summary page**

Create `web/src/app/pages/accounting/summary.ts` (clones `TicketListComponent` structure: business dropdown, window selector, totals, per-agent table, three-signal loading; uses `.card`/`.row`/`.spread`/`.linklike` classes):

```typescript
import { CurrencyPipe, DatePipe } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router, RouterLink } from '@angular/router';
import { BusinessService } from '../../core/business.service';
import { AccountingService, AccountingSummary, WindowName } from '../../core/accounting.service';
import { Business } from '../../core/tree';

const WINDOWS: WindowName[] = ['this_month', 'last_month', 'last_30_days'];

@Component({
  selector: 'app-accounting-summary',
  imports: [FormsModule, RouterLink, DatePipe, CurrencyPipe],
  template: `
    <section class="card">
      <div class="spread">
        <div>
          <h1>Accounting</h1>
          <p class="sub">Token and cost usage by agent for the selected business.</p>
        </div>
        <a class="linklike" routerLink="/dashboard" data-testid="back-to-dashboard">Back to dashboard</a>
      </div>

      <div class="row" style="margin-top:6px">
        <div style="flex:1 1 220px">
          <label for="biz-select">Business</label>
          <select id="biz-select" data-testid="business-select" [ngModel]="businessId()" (ngModelChange)="selectBusiness($event)">
            <option value="" disabled>Choose a business…</option>
            @for (b of businesses(); track b.id) {
              <option [value]="b.id">{{ b.is_tenant_root ? b.name + ' (master)' : b.name }}</option>
            }
          </select>
        </div>
        <div style="flex:1 1 160px">
          <label for="window-select">Window</label>
          <select id="window-select" data-testid="window-select" [ngModel]="window()" (ngModelChange)="setWindow($event)">
            @for (w of windows; track w) {
              <option [value]="w">{{ w }}</option>
            }
          </select>
        </div>
      </div>

      @if (!businessId()) {
        <p class="empty" data-testid="no-business">Select a business to view usage.</p>
      } @else if (loading()) {
        <p class="empty">Loading usage…</p>
      } @else if (loadFailed()) {
        <div class="empty">
          <p>We couldn't load usage.</p>
          <button class="ghost compact" (click)="reload()">Try again</button>
        </div>
      } @else if (summary(); as s) {
        <div class="row" data-testid="totals" style="margin-top:10px">
          <div class="card compact"><span class="muted">Total cost</span><strong data-testid="total-cost">{{ s.totals.cost_cents / 100 | currency }}</strong></div>
          <div class="card compact"><span class="muted">Tokens in</span><strong data-testid="total-in">{{ s.totals.tokens_in }}</strong></div>
          <div class="card compact"><span class="muted">Tokens out</span><strong data-testid="total-out">{{ s.totals.tokens_out }}</strong></div>
          <div class="card compact"><span class="muted">Runs</span><strong data-testid="total-runs">{{ s.totals.run_count }}</strong></div>
        </div>
        <ul class="tree" data-testid="agent-list">
          @for (a of s.agents; track a.agent_id) {
            <li class="biz" data-testid="agent-row" [attr.data-agent-id]="a.agent_id" (click)="openAgent(a.agent_id)" style="cursor:pointer">
              <div class="biz-main">
                <span class="name" data-testid="agent-name">{{ a.name }}</span>
                <span class="badge" data-testid="agent-cost">{{ a.cost_cents / 100 | currency }}</span>
                @if (a.budget_pct != null) {
                  <span class="pill" data-testid="agent-budget-pct">{{ a.budget_pct }}% of budget</span>
                }
              </div>
              <div class="ticket-meta">
                <span data-testid="agent-runs">{{ a.run_count }} runs</span>
                <span>{{ a.tokens_in }} in / {{ a.tokens_out }} out</span>
              </div>
            </li>
          } @empty {
            <li class="empty" data-testid="agent-empty">No agents for this business.</li>
          }
        </ul>
      }

      @if (error()) {
        <p class="msg error" data-testid="list-error">{{ error() }}</p>
      }
    </section>
  `,
  styles: [
    `
      .card.compact { flex: 1 1 120px; display: flex; flex-direction: column; gap: 4px; }
      .muted { color: var(--muted); font-size: 12px; }
      .ticket-meta { display: flex; gap: 12px; color: var(--muted); font-size: 12.5px; }
    `,
  ],
})
export class AccountingSummaryComponent implements OnInit {
  private bizApi = inject(BusinessService);
  private api = inject(AccountingService);
  private router = inject(Router);

  readonly windows = WINDOWS;
  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  window = signal<WindowName>('this_month');
  summary = signal<AccountingSummary | null>(null);
  loading = signal(false);
  loadFailed = signal(false);
  error = signal('');

  ngOnInit(): void {
    this.bizApi.list().subscribe({
      next: (r) => {
        const items = r.items ?? [];
        this.businesses.set(items);
        if (items.length && !this.businessId()) {
          this.businessId.set(items[0].id);
          this.reload();
        }
      },
      error: () => this.loadFailed.set(true),
    });
  }

  selectBusiness(id: string): void {
    this.businessId.set(id);
    this.reload();
  }

  setWindow(w: WindowName): void {
    this.window.set(w);
    this.reload();
  }

  openAgent(agentId: string): void {
    this.router.navigate(['/accounting', this.businessId(), agentId]);
  }

  reload(): void {
    if (!this.businessId()) return;
    this.loading.set(true);
    this.loadFailed.set(false);
    this.error.set('');
    this.api.getSummary(this.businessId(), this.window()).subscribe({
      next: (s) => {
        this.summary.set(s);
        this.loading.set(false);
      },
      error: (e: HttpErrorResponse) => {
        this.loading.set(false);
        this.loadFailed.set(true);
        this.error.set(e.status === 403 || e.status === 404 ? "You don't have access to do that." : 'Could not load usage. Please try again.');
      },
    });
  }
}
```

(If `web/src/app/core/tree.ts` is not the `Business` location, import from wherever `business.service.ts` imports it.)

- [ ] **Step 2: Add the route**

In `web/src/app/app.routes.ts`, add before the `{ path: '**' ... }` catch-all:

```typescript
  {
    path: 'accounting',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/accounting/summary').then((m) => m.AccountingSummaryComponent),
  },
  {
    path: 'accounting/:businessId/:agentId',
    canActivate: [authGuard],
    loadComponent: () => import('./pages/accounting/agent-runs').then((m) => m.AgentRunsComponent),
  },
```

(The `agent-runs` import is satisfied in Task 10; if building between tasks, comment the second route until then.)

- [ ] **Step 3: Add the dashboard nav link**

Read the dashboard component template (`web/src/app/pages/dashboard.ts`). Add an "Accounting" link mirroring the existing navigation pattern (an `<a class="linklike" routerLink="/support">` likely exists; place Accounting beside it). If a nav row exists:

```html
<a class="linklike" routerLink="/accounting" data-testid="nav-accounting">Accounting</a>
```

If the dashboard has no nav row, add one in its main card:

```html
<div class="row">
  <a class="linklike" routerLink="/support" data-testid="nav-support">Support</a>
  <a class="linklike" routerLink="/accounting" data-testid="nav-accounting">Accounting</a>
</div>
```

Ensure `RouterLink` is in the dashboard component's `imports`.

- [ ] **Step 4: Build the frontend**

Run (from `web/`): `npm run build`
Expected: build succeeds (route lazy-loads compile; the `agent-runs` route will fail to build only if Task 10 isn't done — do Task 10 before this build, or temporarily comment that route).

- [ ] **Step 5: Commit**

```bash
git add web/src/app/pages/accounting/summary.ts web/src/app/app.routes.ts web/src/app/pages/dashboard.ts ../.beads/issues.jsonl
git commit -m "feat(web): accounting summary page + route + dashboard nav (US7)"
```

---

## Task 10: Frontend agent-runs drill-down page

**Files:**
- Create: `web/src/app/pages/accounting/agent-runs.ts`

- [ ] **Step 1: Write the page**

Create `web/src/app/pages/accounting/agent-runs.ts` (reads `:businessId`/`:agentId` from the route, paginated run list with "Load more"):

```typescript
import { CurrencyPipe, DatePipe } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { AccountingService, RunSummary } from '../../core/accounting.service';

@Component({
  selector: 'app-agent-runs',
  imports: [RouterLink, DatePipe, CurrencyPipe],
  template: `
    <section class="card">
      <div class="spread">
        <div>
          <h1>Agent runs</h1>
          <p class="sub">Per-run token and cost breakdown.</p>
        </div>
        <a class="linklike" routerLink="/accounting" data-testid="back-to-accounting">Back to accounting</a>
      </div>

      @if (loading()) {
        <p class="empty">Loading runs…</p>
      } @else if (loadFailed()) {
        <div class="empty">
          <p>We couldn't load these runs.</p>
          <button class="ghost compact" (click)="reload()">Try again</button>
        </div>
      } @else {
        <ul class="tree" data-testid="run-list">
          @for (r of runs(); track r.id) {
            <li class="biz" data-testid="run-row" [attr.data-run-id]="r.id">
              <div class="biz-main">
                <span class="badge" data-testid="run-status">{{ r.status }}</span>
                <span class="name" data-testid="run-cost">{{ r.cost_cents / 100 | currency }}</span>
              </div>
              <div class="ticket-meta">
                <span data-testid="run-tokens">{{ r.tokens_in }} in / {{ r.tokens_out }} out</span>
                <span>{{ r.created_at | date: 'short' }}</span>
              </div>
            </li>
          } @empty {
            <li class="empty" data-testid="run-empty">No runs in this window.</li>
          }
        </ul>
        @if (nextCursor()) {
          <button class="ghost compact" data-testid="load-more" [disabled]="busy()" (click)="loadMore()">
            {{ busy() ? 'Loading…' : 'Load more' }}
          </button>
        }
      }

      @if (error()) {
        <p class="msg error" data-testid="list-error">{{ error() }}</p>
      }
    </section>
  `,
  styles: [`.ticket-meta { display: flex; gap: 12px; color: var(--muted); font-size: 12.5px; }`],
})
export class AgentRunsComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private api = inject(AccountingService);

  businessId = '';
  agentId = '';
  runs = signal<RunSummary[]>([]);
  nextCursor = signal<string | null>(null);
  loading = signal(false);
  loadFailed = signal(false);
  busy = signal(false);
  error = signal('');

  ngOnInit(): void {
    this.businessId = this.route.snapshot.paramMap.get('businessId') ?? '';
    this.agentId = this.route.snapshot.paramMap.get('agentId') ?? '';
    this.reload();
  }

  reload(): void {
    if (!this.businessId || !this.agentId) return;
    this.loading.set(true);
    this.loadFailed.set(false);
    this.error.set('');
    this.api.listRuns(this.businessId, this.agentId, { limit: 50 }).subscribe({
      next: (page) => {
        this.runs.set(page.items ?? []);
        this.nextCursor.set(page.next_cursor);
        this.loading.set(false);
      },
      error: (e: HttpErrorResponse) => {
        this.loading.set(false);
        this.loadFailed.set(true);
        this.error.set(e.status === 403 || e.status === 404 ? "You don't have access to do that." : 'Could not load runs. Please try again.');
      },
    });
  }

  loadMore(): void {
    const cursor = this.nextCursor();
    if (!cursor || this.busy()) return;
    this.busy.set(true);
    this.api.listRuns(this.businessId, this.agentId, { limit: 50, cursor }).subscribe({
      next: (page) => {
        this.runs.update((cur) => [...cur, ...(page.items ?? [])]);
        this.nextCursor.set(page.next_cursor);
        this.busy.set(false);
      },
      error: () => this.busy.set(false),
    });
  }
}
```

- [ ] **Step 2: Build the frontend (both routes now resolve)**

Run (from `web/`): `npm run build`
Expected: build succeeds.

- [ ] **Step 3: Commit**

```bash
git add web/src/app/pages/accounting/agent-runs.ts ../.beads/issues.jsonl
git commit -m "feat(web): agent-runs drill-down page (US7)"
```

---

## Task 11: Playwright e2e regression + real-browser verification

**Files:**
- Create: `web/e2e/accounting.spec.ts`

- [ ] **Step 1: Write the e2e spec**

Create `web/e2e/accounting.spec.ts` (mirrors `support.spec.ts`: localStorage auth, broad routes first then specific, `getByTestId`):

```typescript
import { expect, Page, test } from '@playwright/test';

const BIZ_ID = '11111111-1111-1111-1111-111111111111';
const AGENT_ID = '22222222-2222-2222-2222-222222222222';

const business = { id: BIZ_ID, parent_id: null, tenant_root_id: BIZ_ID, name: 'Acme', status: 'active', is_tenant_root: true };

const summary = {
  window: { from: '2026-06-01T00:00:00Z', to: '2026-06-05T14:30:00Z' },
  totals: { cost_cents: 200, tokens_in: 150, tokens_out: 260, run_count: 2 },
  agents: [
    { agent_id: AGENT_ID, name: 'Support Agent', monthly_budget_cents: 10000, run_count: 2, tokens_in: 150, tokens_out: 260, cost_cents: 200, budget_pct: 2 },
    { agent_id: '33333333-3333-3333-3333-333333333333', name: 'Idle Agent', monthly_budget_cents: 0, run_count: 0, tokens_in: 0, tokens_out: 0, cost_cents: 0 },
  ],
};

const runsPage = {
  items: [
    { id: 'aaaa1111-0000-0000-0000-000000000001', agent_id: AGENT_ID, trigger: 'manual', status: 'succeeded', tokens_in: 100, tokens_out: 200, cost_cents: 120, correlation_id: 'c1', created_at: '2026-06-05T13:30:00Z' },
    { id: 'aaaa1111-0000-0000-0000-000000000002', agent_id: AGENT_ID, trigger: 'event', status: 'failed', tokens_in: 50, tokens_out: 60, cost_cents: 80, correlation_id: 'c2', created_at: '2026-06-05T12:30:00Z' },
  ],
  next_cursor: null,
};

async function installStack(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('mf_access', 'test-access');
    localStorage.setItem('mf_refresh', 'test-refresh');
  });
  await page.route('**/api/v1/me', (route) =>
    route.fulfill({ json: { id: 'u1', email: 'owner@manyforge.test', display_name: 'Owner', email_verified: true, status: 'active' } }),
  );
  await page.route('**/api/v1/businesses', (route) => route.fulfill({ json: { items: [business] } }));
  // Specific routes registered AFTER the broad ones win (Playwright: last-registered-first).
  await page.route(`**/api/v1/businesses/${BIZ_ID}/accounting**`, (route) => route.fulfill({ json: summary }));
  await page.route(`**/api/v1/businesses/${BIZ_ID}/agents/${AGENT_ID}/runs**`, (route) => route.fulfill({ json: runsPage }));
}

test('accounting summary renders totals + per-agent rows (incl. zero-run agent)', async ({ page }) => {
  await installStack(page);
  await page.goto('/accounting');

  await expect(page.getByRole('heading', { name: 'Accounting' })).toBeVisible();
  await expect(page.getByTestId('business-select')).toHaveValue(BIZ_ID);
  await expect(page.getByTestId('total-cost')).toContainText('2.00');
  await expect(page.getByTestId('total-runs')).toHaveText('2');

  const rows = page.getByTestId('agent-row');
  await expect(rows).toHaveCount(2);
  await expect(rows.first().getByTestId('agent-name')).toHaveText('Support Agent');
  await expect(rows.first().getByTestId('agent-budget-pct')).toContainText('2%');
});

test('clicking an agent drills into its run list', async ({ page }) => {
  await installStack(page);
  await page.goto('/accounting');
  await page.getByTestId('agent-row').first().click();

  await expect(page).toHaveURL(new RegExp(`/accounting/${BIZ_ID}/${AGENT_ID}`));
  const runRows = page.getByTestId('run-row');
  await expect(runRows).toHaveCount(2);
  await expect(runRows.first().getByTestId('run-status')).toHaveText('succeeded');
  await expect(runRows.first().getByTestId('run-cost')).toContainText('1.20');
});
```

- [ ] **Step 2: Run the e2e spec in a real browser**

Ensure the frontend dev server is up (`npm start` in `web/`, serving :4300) OR rely on Playwright's config. Run (from `web/`):

```bash
npm run e2e -- accounting.spec.ts
```

Expected: 2 tests PASS in Chromium. If the dev server isn't running and `playwright.config.ts` has no `webServer`, start `npm start` first in another shell, then run.

- [ ] **Step 3: Manual real-browser sanity (per CLAUDE.md UI rule)**

With `npm start` running, open `http://localhost:4300/accounting` in a browser (or via the Playwright MCP / gstack browse). Confirm: the page renders without a provider/injection console error, the business + window selectors work, and clicking an agent navigates to the run list. (The e2e covers this, but a live check catches CDK/provider issues unit tests miss.)

- [ ] **Step 4: Commit**

```bash
git add web/e2e/accounting.spec.ts ../.beads/issues.jsonl
git commit -m "test(web): accounting e2e regression — summary + drill-down (US7)"
```

---

## Task 12: Security regression pins + full gate

**Files:**
- Create: `internal/security_regression/accounting_us7_pins_test.go`

Source-level pins (no build tag → run in `make test` + `make sec-test`) so a refactor that drops a US7 protection fails CI loudly.

- [ ] **Step 1: Write the pins**

Create `internal/security_regression/accounting_us7_pins_test.go`:

```go
// No build tag: source-level pins run in `make test` and `make sec-test` with NO
// infrastructure, complementing the behavioral tests in internal/agents/.
//
// US7 contract: Spec 003 design §3 (docs/superpowers/specs/2026-06-05-us7-accounting-design.md);
// epic manyforge-deo / issue manyforge-deo.3. Each Test pins one contract item; the
// strings.Contains fragments are the load-bearing assertions (CLAUDE.md: source-level
// pins so a refactor that drops a fix fails CI even if a behavioral test is weakened).

package security_regression

import (
	"os"
	"strings"
	"testing"
)

func mustReadUS7(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// model_pricing is a system catalog: no business_id, no RLS, SELECT-only grant, and
// NO HTTP write surface. Pin the migration shape + the marker.
func TestPin_ModelPricingIsSystemCatalog(t *testing.T) {
	up := mustReadUS7(t, "../../migrations/0038_model_pricing.up.sql")
	for _, frag := range []string{
		"CREATE TABLE model_pricing",
		"GRANT SELECT ON model_pricing TO manyforge_app",
		"security: system catalog",
	} {
		if !strings.Contains(up, frag) {
			t.Errorf("0038 up: missing %q — model_pricing must stay a SELECT-only system catalog", frag)
		}
	}
	for _, bad := range []string{"business_id", "ENABLE ROW LEVEL SECURITY", "INSERT ON model_pricing TO", "UPDATE ON model_pricing TO"} {
		if strings.Contains(up, bad) {
			t.Errorf("0038 up: model_pricing must NOT contain %q (it is global, read-only reference data)", bad)
		}
	}
}

// The migration seed must match ai.RegisterDefaults so the DB source of truth and the
// test fixture agree (a drift would make prod cost ≠ test cost).
func TestPin_ModelPricingSeedMatchesDefaults(t *testing.T) {
	up := mustReadUS7(t, "../../migrations/0038_model_pricing.up.sql")
	seed := mustReadUS7(t, "../../internal/platform/ai/seed.go")
	for _, id := range []string{"claude-sonnet-4-5", "claude-opus-4-1", "claude-haiku-4-5", "gpt-4o", "gpt-4o-mini"} {
		if !strings.Contains(up, id) {
			t.Errorf("0038 seed missing model %q", id)
		}
		if !strings.Contains(seed, id) {
			t.Errorf("seed.go missing model %q (migration + RegisterDefaults must agree)", id)
		}
	}
}

// Accounting aggregate scopes by business_id and runs under WithPrincipal (RLS) — the
// cross-tenant invisibility guarantee. Pin the query predicate + the principal wrap.
func TestPin_AccountingScopedByBusinessAndRLS(t *testing.T) {
	q := mustReadUS7(t, "../../db/query/accounting.sql")
	if !strings.Contains(q, "a.business_id = sqlc.arg('business_id')") {
		t.Error("accounting.sql: summary must filter by business_id (tenant scoping)")
	}
	svc := mustReadUS7(t, "../../internal/agents/accounting.go")
	if !strings.Contains(svc, "WithPrincipal(") {
		t.Error("accounting.go: summary must run under WithPrincipal (RLS) — no principal => no rows")
	}
}

// Pagination + custom window are both capped (no full-table scan / unbounded range).
func TestPin_AccountingCaps(t *testing.T) {
	run := mustReadUS7(t, "../../internal/agents/agent_run.go")
	if !strings.Contains(run, "runListMaxLimit") || !strings.Contains(run, "100") {
		t.Error("agent_run.go: ListRuns must clamp the page size (runListMaxLimit=100)")
	}
	win := mustReadUS7(t, "../../internal/agents/accounting_window.go")
	if !strings.Contains(win, "maxWindowSpan") {
		t.Error("accounting_window.go: custom window must be span-capped (maxWindowSpan)")
	}
}
```

- [ ] **Step 2: Run the pins**

Run: `go test ./internal/security_regression/ -run 'TestPin_ModelPricing|TestPin_Accounting' -v`
Expected: PASS (4 tests).

- [ ] **Step 3: gofmt + run the full backend gate**

```bash
export PATH="$PATH:$HOME/go/bin"
gofmt -l internal/ cmd/ | tee /dev/stderr | (! read)   # fail loudly if any file is gofmt-dirty
make test && make contract-test && make lint && make sec-test && make int-test
```

Expected: `gofmt -l` prints nothing (all clean); every `make` target PASSES; `lint` reports **0 issues**. `int-test` ≈ 6 min (Docker). Fix anything red before continuing — no "pre-existing failure" exceptions.

- [ ] **Step 4: Run the full frontend gate**

Run (from `web/`):

```bash
npm run test && npm run build && npm run e2e
```

Expected: unit tests PASS, build succeeds, all e2e specs PASS (Chromium).

- [ ] **Step 5: Commit + close the issue**

```bash
gofmt -w internal/security_regression/accounting_us7_pins_test.go
git add internal/security_regression/accounting_us7_pins_test.go .beads/issues.jsonl
git commit -m "test(sec): pin US7 contract — model_pricing catalog, RLS scoping, caps (US7)"
export PATH="$PATH:$HOME/go/bin"
bd close manyforge-deo.3
git add .beads/issues.jsonl && git commit -m "chore(bd): close US7 (manyforge-deo.3)"
```

- [ ] **Step 6: Push (session-close protocol)**

```bash
git pull --rebase
bd dolt push 2>/dev/null || true
git push
git status   # MUST show "up to date with origin"
```

---

## Self-Review

**Spec coverage** (each design section → task):
- §3.1/§3.2 pricing table + DB-backed registry → Tasks 1, 2. ✓ (Registry loader in `agents`, not `ai`, keeps `ai` DB-free as designed.)
- §3.3 window semantics → Task 3. ✓
- §3.4 summary + runs queries → Tasks 4 (summary), 6 (runs). ✓
- §3.5 HTTP surface (summary + runs endpoints, `agents.run` gate, typed errors, no oracle) → Tasks 5, 7. ✓ (gate reused via mounting in the existing `ag.Use(h.agentsRun)` group.)
- §3.6 frontend (service, summary page, agent-runs page, nav, two routes) → Tasks 8, 9, 10. ✓
- §3.7 wiring (DB registry at startup, store/handler construction) → Tasks 2 (registry), 5 (handler). ✓
- §4 test plan: window unit (T3), DB-backed registry (T2), summary+runs integration incl. zero-run LEFT JOIN + cross-tenant (T4, T7), contract drift (T5, T7), security pins incl. system-catalog/RLS/caps/seed-match (T12), Playwright e2e + real-browser (T11). ✓
- §6 open questions resolved: reference-table convention = `permission` precedent (no RLS, GRANT SELECT) [T1]; `created_at` added to `AgentRun` + `RunSummary` [T6/T7]; span cap = 366d [T3]; two routes [T9/T10]. ✓

**Placeholder scan:** Each code step contains complete code. Two explicit "match the generated field names" notes (Tasks 2, 4, 6) are unavoidable sqlc-generation reconciliations, not placeholders — the surrounding code is complete and the adjustment is mechanical (read the generated struct, match casing).

**Type consistency:** `Window{From,To}` used in `ResolveWindow` (T3), `AccountingStore.SummaryForWindow` (T4), `RunListFilter.Window` (T6), and both handlers (T5/T7). `AgentRun.CreatedAt` added in T6, consumed by `toRunListItem` in T7. `AccountingService`/`AccountingSummary`/`RunSummary`/`RunPage` consistent across T8/T9/T10/T11. `manyforge-deo.3` referenced throughout. Endpoint paths consistent: `/businesses/{id}/accounting` (T5, T8, T11) and `/businesses/{id}/agents/{agentID}/runs` (T7, T8, T11). ✓

**Risk note for the executor:** sqlc field-name casing (e.g. `FromTs` vs `Fromts`, `InputCentsPerMtok` vs `InputCentsPerMTok`) and nullable types (`*string` vs `pgtype.Text` for `status`) depend on the generator config — always read the freshly generated `dbgen` struct and match it; don't trust the names in this plan verbatim where a "match the generator" note appears.
