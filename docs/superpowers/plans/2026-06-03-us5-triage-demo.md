# US5 — AI Triage Application (the demo) + l29 async run trigger — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An enabled agent auto-runs on `ticket.created`, Mode-1 triages the ticket (auto-applies reversible status/priority/tags/assignee) and queues a gated `draft_reply`; a human approves in the US4 queue and the reply is sent via the existing reply outbox — loop-guarded so an agent reply never re-triggers triage.

**Architecture:** Two-stage, mirroring US4's `approve→outbox→idempotent-executor` symmetrically. **Stage 1** — a `ticket.created` outbox subscriber (`TriageTrigger`) is *fast*: it resolves enabled agents (system-wide `SECURITY DEFINER` fn) and creates a `queued` `agent_run` per agent, idempotent on the triggering message id. **Stage 2** — a background `RunDrainer` poller (modeled on the US4 60s expire sweep) atomically claims `queued` runs (`queued→running` state claim via `SKIP LOCKED` definer fn) and runs the bounded loop as the agent via a new reusable `Engine.Execute`. The trigger is intentionally *not* allowed to run the loop inline: a subscriber executes inside the outbox worker's batch tx, so a long run would stall all other event delivery.

**Tech Stack:** Go 1.x, pgx/v5, sqlc (reads `db/schema.sql`), golang-migrate (paired `NNNN_*.up.sql`/`.down.sql`), testcontainers Postgres 16, the in-house `events` outbox/bus, `internal/platform/ai` mock provider.

**bd:** epic `manyforge-deo`; this delivers `manyforge-l29` (async trigger, Stage 1+2 foundation) and `manyforge-ehe` (US5 triage demo). Both claimed/in_progress.

---

## Loop-guard decision (read first — it shapes the whole design)

US5 subscribes to **`ticket.created` ONLY**, never `message.received`. Rationale (verified against spec-002):
- `ticket.created` fires once, only for a **brand-new** ticket (`inbox/service.go` `out.Created`). An agent's own **outbound** reply emits `ticket.replied`, never `ticket.created` — so **an agent reply can never re-trigger triage** through our subscription. This is the primary loop-guard, inherited structurally.
- A customer's later reply emits `message.received` (not `ticket.created`) — we do not subscribe to it, so no re-trigger. (Re-triage-on-customer-reply is deliberately out of scope; if wanted later it needs its own guard — filed as a follow-up, not built here.)
- A machine auto-responder that opens a **new** ticket is bounded upstream by spec-002's `is_auto_reply` suppression cap (migration 0024): once a requester exceeds the per-hour auto-reply cap, ingest is **suppressed** and the `ticket.created` enqueue is skipped entirely. So a single vacation auto-reply triages once (fine); a storm is suppressed (no loop).

Therefore US5 needs **no new `is_auto_reply` plumbing** — only the structural "subscribe to ticket.created, not message.received" choice, pinned as a security regression test.

---

## File structure

**New files:**
- `migrations/0034_agent_run_trigger.up.sql` / `.down.sql` — `trigger_dedup_key` column + partial unique index + two `SECURITY DEFINER` fns.
- `internal/agents/trigger.go` — `TriageTrigger` (the `ticket.created` subscriber) + `ticketCreatedPayload`.
- `internal/agents/trigger_test.go` — unit tests (fakes) for the subscriber.
- `internal/agents/drainer.go` — `RunDrainer` (Stage-2 claim+execute).
- `internal/agents/drainer_test.go` — unit tests (fakes) for the drainer.
- `internal/agents/us5_triage_integration_test.go` — the end-to-end acceptance test (+ idempotency + loop-guard + claim/dedup integration cases).
- `internal/security_regression/agent_run_us5_pins_test.go` — source-level pins.

**Modified files:**
- `internal/agents/runner.go` — split `run()` into create + a reusable `execute()`; add exported `Execute`.
- `internal/agents/agent_run.go` — widen `agentRunDB` with `WithTx`; add `CreateEventRun`, `EnabledAgentsForBusiness`, `ClaimNextQueuedRun`; add `AgentRef`/`ClaimedRun` types.
- `db/query/agent_run.sql` — add `CreateEventAgentRun`.
- `db/schema.sql` — add `agent_run.trigger_dedup_key` + the partial unique index (sqlc reads this, not migrations).
- `internal/platform/events/bus.go` — add `TopicTicketCreated`.
- `internal/inbox/service.go` — use `events.TopicTicketCreated` const for the existing enqueue.
- `cmd/manyforge/main.go` — subscribe `TriageTrigger`; start the `RunDrainer` poller goroutine.

**No OpenAPI/drift change:** US5 adds no HTTP surface (auto-trigger is internal; the manual "Run triage" endpoint `POST .../agents/{id}/runs` already shipped in US3).

---

## PHASE A — l29: async event-driven run trigger (the foundation)

### Task 1: Split `Engine.Run` into create + reusable `Execute`

**Files:**
- Modify: `internal/agents/runner.go:100-231`
- Test: `internal/agents/runner_test.go` (add one test; all existing tests must stay green)

The Stage-2 drainer needs to run the loop on an **already-created** (claimed) run. Extract everything after `CreateRun` into `execute(...)`, leaving `run()` as `enabled-check → CreateRun → execute`. The `started` audit moves into `execute` (semantically: the run "starts executing" when drained).

- [ ] **Step 1: Write the failing test** — append to `internal/agents/runner_test.go`:

```go
func TestExecute_OnPreCreatedRun(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "set_status", `{"ticket_id":"`+tid.String()+`","status":"open"}`),
		finalText("done"),
	)
	fts := &fakeTicketSvc{}
	store := &fakeRunStore{}
	eng, _, _ := newTestEngine(prov, store, map[string]bool{"tickets.write": true}, NewToolRegistry(fts))

	// A run that already exists (drain path): Execute must NOT create another run.
	ttype := "ticket"
	pre := AgentRun{ID: uuid.New(), AgentID: uuid.New(), BusinessID: uuid.New(), Trigger: "event", TargetType: &ttype, TargetID: &tid, Status: RunRunning, CorrelationID: uuid.NewString()}
	run, err := eng.Execute(context.Background(), uuid.New(), loadedAgent("set_status"), pre)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if run.Status != RunSucceeded {
		t.Fatalf("status = %s, want succeeded; transitions=%v", run.Status, store.progress)
	}
	if store.created != 0 {
		t.Fatalf("Execute created %d runs, want 0 (run pre-exists)", store.created)
	}
	if fts.triageIn.Status == nil || *fts.triageIn.Status != "open" {
		t.Fatalf("tool did not execute set_status; got %+v", fts.triageIn)
	}
}
```

> NOTE: `fakeRunStore` currently records `Progress` calls in `store.progress`. Add a `created int` counter incremented in its `CreateRun` so the test can assert `Execute` never creates. Check the existing `fakeRunStore` in `runner_test.go` and add the counter if absent.

- [ ] **Step 2: Run it — expect a compile failure** (`Execute` undefined):

```bash
export PATH="$PATH:$HOME/go/bin"
go test ./internal/agents/ -run TestExecute_OnPreCreatedRun 2>&1 | head
```
Expected: `eng.Execute undefined` (or `store.created undefined`).

- [ ] **Step 3: Refactor `runner.go`.** Replace the `Run`/`run` pair (lines 100-231) so the body after `CreateRun` lives in `execute`:

```go
func (e *Engine) Run(ctx context.Context, agentPrincipalID uuid.UUID, ag Agent, trigger string, targetType *string, targetID *uuid.UUID) (AgentRun, error) {
	return e.run(ctx, agentPrincipalID, ag, trigger, targetType, targetID)
}

func (e *Engine) run(ctx context.Context, agentPrincipalID uuid.UUID, ag Agent, trigger string, targetType *string, targetID *uuid.UUID) (AgentRun, error) {
	// Disabled agents never create a run on the synchronous (manual) path: the caller
	// gets a clean conflict with no row. (The async drain path guards again in execute,
	// marking an already-created run failed if the agent was disabled after enqueue.)
	if !ag.Enabled {
		return AgentRun{}, fmt.Errorf("agents: agent is disabled: %w", errs.ErrConflict)
	}
	run, err := e.Runs.CreateRun(ctx, agentPrincipalID, ag.BusinessID, ag.ID, trigger, uuid.NewString(), targetType, targetID)
	if err != nil {
		return AgentRun{}, err
	}
	return e.execute(ctx, agentPrincipalID, ag, run)
}

// Execute runs the bounded loop on an ALREADY-CREATED run. The Stage-2 RunDrainer calls
// it after claiming a queued run (queued→running); Run calls it right after CreateRun.
// The run carries Trigger/TargetType/TargetID/CorrelationID. It is safe to call on a run
// already marked 'running' (the in-flight Progress write is an idempotent UPDATE).
func (e *Engine) Execute(ctx context.Context, agentPrincipalID uuid.UUID, ag Agent, run AgentRun) (AgentRun, error) {
	return e.execute(ctx, agentPrincipalID, ag, run)
}

func (e *Engine) execute(ctx context.Context, agentPrincipalID uuid.UUID, ag Agent, run AgentRun) (AgentRun, error) {
	limits := e.Limits.withDefaults()
	businessID := ag.BusinessID
	targetType, targetID := run.TargetType, run.TargetID

	_ = e.Auditor.Run(ctx, agentPrincipalID, run, "agent.run.started", map[string]any{"agent_id": ag.ID, "trigger": run.Trigger}, nil, "started")

	finish := func(status, reason string, tokIn, tokOut int, cost int64) (AgentRun, error) {
		var rp *string
		if reason != "" {
			rp = &reason
		}
		r, perr := e.Runs.Progress(ctx, agentPrincipalID, businessID, run.ID, status, tokIn, tokOut, cost, rp)
		if perr != nil {
			slog.ErrorContext(ctx, "agent run: terminal state not persisted", "run_id", run.ID, "status", status, "err", perr)
		}
		action := "agent.run.completed"
		if status == RunFailed {
			action = "agent.run.failed"
		}
		_ = e.Auditor.Run(ctx, agentPrincipalID, run, action, nil, map[string]any{"status": status, "error": reason}, status)
		if r.ID == uuid.Nil {
			r = run
		}
		r.Status = status
		r.Error = rp
		return r, perr
	}

	// Async-path defense: a run created while the agent was enabled, then drained after a
	// disable, must terminate cleanly rather than execute. (Never hit on the manual path —
	// run() already returned for a disabled agent before CreateRun.)
	if !ag.Enabled {
		return finish(RunFailed, "agent disabled", 0, 0, 0)
	}

	var mtdStart int64
	if ag.MonthlyBudgetCents > 0 {
		mtd, mErr := e.Runs.MonthToDateCostCents(ctx, agentPrincipalID, businessID, ag.ID)
		if mErr != nil {
			r, _ := finish(RunFailed, "budget lookup failed", 0, 0, 0)
			return r, mErr
		}
		mtdStart = mtd
		if mtdStart >= int64(ag.MonthlyBudgetCents) {
			r, _ := finish(RunFailed, "monthly budget exceeded", 0, 0, 0)
			return r, ErrBudgetExceeded
		}
	}

	prov, model, pErr := e.NewProvider(ctx, agentPrincipalID, businessID, ag.Provider)
	if pErr != nil {
		r, _ := finish(RunFailed, "provider unavailable", 0, 0, 0)
		return r, pErr
	}

	allow := map[string]bool{}
	var toolDefs []ai.ToolDef
	for _, name := range ag.AllowedTools {
		if t, ok := e.Tools.Get(name); ok {
			allow[name] = true
			toolDefs = append(toolDefs, ai.ToolDef{Name: t.Name, Description: t.Description, Schema: json.RawMessage(t.SchemaJSON)})
		}
	}

	loopCtx, cancel := context.WithTimeout(ctx, limits.WallClock)
	defer cancel()

	if _, perr := e.Runs.Progress(ctx, agentPrincipalID, businessID, run.ID, RunRunning, 0, 0, 0, nil); perr != nil {
		slog.WarnContext(ctx, "agent run: could not mark running", "run_id", run.ID, "err", perr)
	}

	msgs := []ai.Message{{Role: ai.RoleUser, Text: initialTask(targetType, targetID)}}
	var tokIn, tokOut int
	var costCents int64
	proposed := false

	for iter := 0; ; iter++ {
		if iter >= limits.MaxIterations {
			return finish(RunFailed, "max_iterations exceeded", tokIn, tokOut, costCents)
		}
		req := ai.Request{Model: model, System: ag.SystemPrompt, Messages: msgs, Tools: toolDefs, MaxTokens: limits.MaxOutputTokens, Temperature: defaultTemperature}
		resp, cErr := prov.Complete(loopCtx, req)
		if cErr != nil {
			if errors.Is(cErr, context.DeadlineExceeded) {
				return finish(RunFailed, "wall-clock timeout", tokIn, tokOut, costCents)
			}
			return finish(RunFailed, "provider error", tokIn, tokOut, costCents)
		}
		tokIn += resp.Usage.InputTokens
		tokOut += resp.Usage.OutputTokens
		costCents += e.Cost(model, resp.Usage)

		if tokIn+tokOut > limits.MaxTokensPerRun {
			return finish(RunFailed, "max_tokens exceeded", tokIn, tokOut, costCents)
		}
		if ag.MonthlyBudgetCents > 0 && mtdStart+costCents >= int64(ag.MonthlyBudgetCents) {
			return finish(RunFailed, "monthly budget exceeded mid-run", tokIn, tokOut, costCents)
		}

		if resp.FinishReason != ai.FinishToolUse && len(resp.ToolCalls) == 0 {
			status := RunSucceeded
			if proposed {
				status = RunAwaitingApproval
			}
			return finish(status, "", tokIn, tokOut, costCents)
		}

		msgs = append(msgs, ai.Message{Role: ai.RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls})
		var results []ai.ToolResult
		for _, call := range resp.ToolCalls {
			content, isErr, prop := e.execTool(loopCtx, agentPrincipalID, businessID, ag.AutonomyMode, allow, run, call)
			proposed = proposed || prop
			results = append(results, ai.ToolResult{CallID: call.ID, Content: content, IsError: isErr})
		}
		msgs = append(msgs, ai.Message{Role: ai.RoleTool, ToolResults: results})
	}
}
```

Leave `execTool`, `initialTask`, `safeMsg` unchanged.

- [ ] **Step 4: Run the full agents package + the new test:**

```bash
go test ./internal/agents/ 2>&1 | tail -20
```
Expected: PASS (all existing run-loop tests + `TestExecute_OnPreCreatedRun`).

- [ ] **Step 5: Commit:**

```bash
git add internal/agents/runner.go internal/agents/runner_test.go
git commit -m "refactor(agents): split Engine.Run into CreateRun + reusable Execute (l29 async-drain foundation)"
```

---

### Task 2: Migration 0034 — dedup column, claim/list definer fns, schema.sql

**Files:**
- Create: `migrations/0034_agent_run_trigger.up.sql`, `migrations/0034_agent_run_trigger.down.sql`
- Modify: `db/schema.sql` (the `agent_run` table + the new index)

- [ ] **Step 1: Write `migrations/0034_agent_run_trigger.up.sql`:**

```sql
-- 0034: async event-driven agent run trigger (Spec 003 US5 / l29).
--
-- (a) trigger_dedup_key: the triggering ticket_message id for an event-triggered run,
--     so an at-least-once redelivery of ticket.created enqueues at most one run per
--     agent (partial unique index; NULL for manual runs, which are never deduped).
ALTER TABLE agent_run ADD COLUMN trigger_dedup_key text;
CREATE UNIQUE INDEX agent_run_trigger_dedup_idx
    ON agent_run (agent_id, trigger_dedup_key)
    WHERE trigger_dedup_key IS NOT NULL;

-- (b) enabled_agents_for_business: a ticket.created subscriber runs principal-less (the
--     outbox worker tx has no manyforge.principal_id GUC), so it cannot see agent rows
--     through RLS. This SECURITY DEFINER fn lists the enabled agents for ONE business,
--     scoped by BOTH business_id AND tenant_root_id so a cross-tenant event can never
--     surface another tenant's agents. Mirrors the 0016 outbox + 0032 expire definers.
CREATE FUNCTION enabled_agents_for_business(p_business_id uuid, p_tenant_root_id uuid)
RETURNS TABLE(agent_id uuid, principal_id uuid)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT id, principal_id FROM agent
    WHERE business_id = p_business_id
      AND tenant_root_id = p_tenant_root_id
      AND enabled = true;
$$;
REVOKE ALL ON FUNCTION enabled_agents_for_business(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION enabled_agents_for_business(uuid, uuid) TO manyforge_app;

-- (c) claim_next_queued_agent_run: the Stage-2 RunDrainer claims the oldest queued run
--     atomically (queued→running) across all tenants. FOR UPDATE SKIP LOCKED so
--     concurrent drainers never double-claim — this state claim is what makes execution
--     exactly-once. Returns the run's target + the FULL agent config so the drainer needs
--     no second (RLS) lookup. SECURITY DEFINER (system-wide, principal-less).
CREATE FUNCTION claim_next_queued_agent_run()
RETURNS TABLE(
    run_id uuid, business_id uuid, tenant_root_id uuid, correlation_id text,
    target_type text, target_id uuid,
    agent_id uuid, agent_principal_id uuid, provider ai_provider, model text,
    system_prompt text, allowed_tools text[], autonomy_mode smallint,
    enabled boolean, monthly_budget_cents int
)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    WITH claimed AS (
        SELECT id FROM agent_run
        WHERE status = 'queued'
        ORDER BY created_at
        FOR UPDATE SKIP LOCKED
        LIMIT 1
    )
    UPDATE agent_run ar SET status = 'running', updated_at = now()
    FROM claimed c, agent a
    WHERE ar.id = c.id
      AND a.id = ar.agent_id
      AND a.tenant_root_id = ar.tenant_root_id
    RETURNING ar.id, ar.business_id, ar.tenant_root_id, ar.correlation_id,
              ar.target_type, ar.target_id,
              a.id, a.principal_id, a.provider, a.model,
              a.system_prompt, a.allowed_tools, a.autonomy_mode,
              a.enabled, a.monthly_budget_cents;
$$;
REVOKE ALL ON FUNCTION claim_next_queued_agent_run() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_next_queued_agent_run() TO manyforge_app;
```

- [ ] **Step 2: Write `migrations/0034_agent_run_trigger.down.sql`:**

```sql
-- Reverse 0034_agent_run_trigger.
DROP FUNCTION IF EXISTS claim_next_queued_agent_run();
DROP FUNCTION IF EXISTS enabled_agents_for_business(uuid, uuid);
DROP INDEX IF EXISTS agent_run_trigger_dedup_idx;
ALTER TABLE agent_run DROP COLUMN IF EXISTS trigger_dedup_key;
```

- [ ] **Step 3: Mirror the column + index into `db/schema.sql`** (sqlc reads this file, NOT migrations). Find the `CREATE TABLE agent_run (...)` block and add `trigger_dedup_key text` to it (e.g. after the `error text` column), then add the index immediately after the table:

```sql
    trigger_dedup_key text,
```
and after the table's closing `);`:
```sql
CREATE UNIQUE INDEX agent_run_trigger_dedup_idx
    ON agent_run (agent_id, trigger_dedup_key)
    WHERE trigger_dedup_key IS NOT NULL;
```

> The definer fns do NOT go in `schema.sql` — sqlc never references them (they're called via raw pgx). Only the table column + index (needed for `ON CONFLICT` inference in Task 3's query) must be reflected.

- [ ] **Step 4: Apply the migration against a scratch DB to prove the SQL is valid.** If `make migrate` needs a running Postgres, use the dev DB or a throwaway container:

```bash
# Validate SQL parses + applies + reverses on a throwaway PG (no app needed):
docker run -d --rm --name us5pg -e POSTGRES_PASSWORD=p -e POSTGRES_DB=manyforge -p 55432:5432 postgres:16 >/dev/null
sleep 4
export MANYFORGE_DATABASE_URL="postgres://postgres:p@localhost:55432/manyforge?sslmode=disable"
# manyforge_app role + ai_provider enum are created by earlier migrations, so run the whole chain:
migrate -path migrations -database "$MANYFORGE_DATABASE_URL" up 2>&1 | tail -5
migrate -path migrations -database "$MANYFORGE_DATABASE_URL" down 1 2>&1 | tail -3
docker stop us5pg >/dev/null
```
Expected: clean `up` to 0034 then `down 1` (0034 reverses). If `migrate` is unavailable locally, this is also covered by `make int-test` later (testdb runs all migrations) — note that and proceed.

- [ ] **Step 5: Commit:**

```bash
git add migrations/0034_agent_run_trigger.up.sql migrations/0034_agent_run_trigger.down.sql db/schema.sql
git commit -m "feat(agents): mig 0034 — agent_run dedup key + claim/list SECURITY DEFINER fns (l29)"
```

---

### Task 3: `CreateEventAgentRun` query + `AgentRunStore.CreateEventRun`

**Files:**
- Modify: `db/query/agent_run.sql` (add query), then `make generate`
- Modify: `internal/agents/agent_run.go` (widen `agentRunDB`; add `CreateEventRun`)
- Test: `internal/agents/us5_triage_integration_test.go` (idempotency case)

- [ ] **Step 1: Add the query to `db/query/agent_run.sql`:**

```sql
-- name: CreateEventAgentRun :one
-- Idempotent event-triggered run. Dedups on (agent_id, trigger_dedup_key) — the conflict
-- target matches the partial unique index — so an at-least-once redelivery of
-- ticket.created creates at most one run per agent. ON CONFLICT DO NOTHING ⇒ 0 rows ⇒
-- pgx.ErrNoRows in the caller, which maps it to "already enqueued" (created=false).
-- tenant_root_id is derived from the (agent-principal-visible) agent row, never supplied.
INSERT INTO agent_run (id, agent_id, business_id, tenant_root_id, trigger, target_type, target_id, status, correlation_id, trigger_dedup_key)
SELECT sqlc.arg('id')::uuid, a.id, a.business_id, a.tenant_root_id,
       'event', sqlc.narg('target_type')::text, sqlc.narg('target_id')::uuid,
       'queued', sqlc.arg('correlation_id')::text, sqlc.arg('trigger_dedup_key')::text
FROM agent a
WHERE a.id = sqlc.arg('agent_id')::uuid AND a.business_id = sqlc.arg('business_id')::uuid
ON CONFLICT (agent_id, trigger_dedup_key) WHERE trigger_dedup_key IS NOT NULL DO NOTHING
RETURNING *;
```

- [ ] **Step 2: Regenerate sqlc:**

```bash
export PATH="$PATH:$HOME/go/bin"
make generate
git diff --stat internal/platform/db/dbgen/ | tail -5
```
Expected: `agent_run.sql.go` gains `CreateEventAgentRun` + `CreateEventAgentRunParams` (fields `ID, TargetType *string, TargetID pgtype.UUID, CorrelationID, TriggerDedupKey, AgentID, BusinessID`). If sqlc errors on the `ON CONFLICT ... WHERE` clause, confirm the partial index is present in `db/schema.sql` (Task 2 Step 3).

- [ ] **Step 3: Widen `agentRunDB` and add `CreateEventRun` to `internal/agents/agent_run.go`.** Replace the interface (lines 42-44) and add the method + types after `CreateRun`:

```go
type agentRunDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
	WithTx(ctx context.Context, fn func(pgx.Tx) error) error
}
```

```go
// CreateEventRun idempotently inserts a queued, event-triggered run for the agent under
// the AGENT's own principal (so the insert passes RLS as the acting identity), deduped on
// dedupKey (the triggering ticket_message id). created=false means a prior at-least-once
// delivery already enqueued this (agent, dedupKey) — a benign replay; the caller skips it.
func (s *AgentRunStore) CreateEventRun(ctx context.Context, agentPrincipalID, businessID, agentID uuid.UUID, dedupKey string, targetType *string, targetID *uuid.UUID) (created bool, err error) {
	e := s.DB.WithPrincipal(ctx, agentPrincipalID, func(tx pgx.Tx) error {
		_, ie := dbgen.New(tx).CreateEventAgentRun(ctx, dbgen.CreateEventAgentRunParams{
			ID: uuid.New(), AgentID: agentID, BusinessID: businessID,
			CorrelationID: uuid.NewString(), TriggerDedupKey: dedupKey,
			TargetType: targetType, TargetID: db.PGUUIDPtr(targetID),
		})
		return ie
	})
	if e != nil {
		// ErrNoRows ⇒ ON CONFLICT DO NOTHING (deduped) — under the agent's own principal the
		// agent row is always visible, so the only zero-row cause is the dedup conflict.
		if errors.Is(e, pgx.ErrNoRows) {
			return false, nil
		}
		return false, mapAgentRunErr(e)
	}
	return true, nil
}
```

> The integration test that proves idempotency is written in Task 8 (it needs a real DB + a created agent). Here, just ensure it compiles.

- [ ] **Step 4: Build:**

```bash
go build ./... 2>&1 | tail
go vet ./internal/agents/ 2>&1 | tail
```
Expected: clean.

- [ ] **Step 5: Commit:**

```bash
git add db/query/agent_run.sql internal/platform/db/dbgen/ internal/agents/agent_run.go
git commit -m "feat(agents): idempotent CreateEventRun (dedup on triggering message id) (l29)"
```

---

### Task 4: `EnabledAgentsForBusiness` + `ClaimNextQueuedRun` store methods

**Files:**
- Modify: `internal/agents/agent_run.go` (raw-pgx calls to the two definer fns + `AgentRef`/`ClaimedRun` types)

- [ ] **Step 1: Add the types + methods to `internal/agents/agent_run.go`** (after `CreateEventRun`). Add `"github.com/jackc/pgx/v5/pgtype"` to imports:

```go
// AgentRef identifies an agent and its acting principal (the SECURITY DEFINER lister
// returns these for a business; the trigger creates one queued run per ref).
type AgentRef struct {
	AgentID     uuid.UUID
	PrincipalID uuid.UUID
}

// ClaimedRun is one run atomically claimed (queued→running) for execution, carrying the
// full agent config so the drainer needs no second (RLS) lookup.
type ClaimedRun struct {
	RunID         uuid.UUID
	CorrelationID string
	TargetType    *string
	TargetID      *uuid.UUID
	Agent         Agent
}

// EnabledAgentsForBusiness lists the enabled agents for a business via the system-wide
// SECURITY DEFINER fn. The caller (a ticket.created subscriber) runs principal-less, so
// it cannot use an RLS-scoped query; the fn is scoped by business_id AND tenant_root_id.
func (s *AgentRunStore) EnabledAgentsForBusiness(ctx context.Context, businessID, tenantRootID uuid.UUID) ([]AgentRef, error) {
	var refs []AgentRef
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx,
			"SELECT agent_id, principal_id FROM enabled_agents_for_business($1, $2)",
			businessID, tenantRootID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var r AgentRef
			if se := rows.Scan(&r.AgentID, &r.PrincipalID); se != nil {
				return se
			}
			refs = append(refs, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("agents: list enabled agents: %w", err)
	}
	return refs, nil
}

// ClaimNextQueuedRun atomically claims the oldest queued run (queued→running) across all
// tenants via the SECURITY DEFINER fn (SKIP LOCKED ⇒ concurrent drainers never
// double-claim). Returns (nil, nil) when nothing is queued.
func (s *AgentRunStore) ClaimNextQueuedRun(ctx context.Context) (*ClaimedRun, error) {
	var (
		c        ClaimedRun
		ag       Agent
		tt       *string
		tid      pgtype.UUID
		provider string
		mode     int16
		budget   int32
		found    bool
	)
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT run_id, business_id, tenant_root_id, correlation_id,
			target_type, target_id, agent_id, agent_principal_id, provider, model,
			system_prompt, allowed_tools, autonomy_mode, enabled, monthly_budget_cents
			FROM claim_next_queued_agent_run()`)
		var tenantRootID uuid.UUID
		e := row.Scan(&c.RunID, &ag.BusinessID, &tenantRootID, &c.CorrelationID,
			&tt, &tid, &ag.ID, &ag.PrincipalID, &provider, &ag.Model,
			&ag.SystemPrompt, &ag.AllowedTools, &mode, &ag.Enabled, &budget)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil // nothing queued
		}
		if e != nil {
			return e
		}
		found = true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("agents: claim queued run: %w", err)
	}
	if !found {
		return nil, nil
	}
	ag.Provider = provider
	ag.AutonomyMode = int(mode)
	ag.MonthlyBudgetCents = int(budget)
	if ag.AllowedTools == nil {
		ag.AllowedTools = []string{}
	}
	c.Agent = ag
	c.TargetType = tt
	if tid.Valid {
		v := uuid.UUID(tid.Bytes)
		c.TargetID = &v
	}
	return &c, nil
}
```

- [ ] **Step 2: Build:**

```bash
go build ./... 2>&1 | tail
```
Expected: clean. (Behavioral coverage is the integration cases in Task 8.)

- [ ] **Step 3: Commit:**

```bash
git add internal/agents/agent_run.go
git commit -m "feat(agents): EnabledAgentsForBusiness + ClaimNextQueuedRun (definer-fn store methods) (l29)"
```

---

### Task 5: `TriageTrigger` — the `ticket.created` subscriber

**Files:**
- Create: `internal/agents/trigger.go`
- Test: `internal/agents/trigger_test.go`

- [ ] **Step 1: Write the failing tests `internal/agents/trigger_test.go`:**

```go
package agents

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/events"
)

type fakeTriggerStore struct {
	refs    []AgentRef
	created []struct {
		principal, agent uuid.UUID
		dedup            string
	}
	dedupSeen map[string]bool // (agent|dedup) already created
}

func (f *fakeTriggerStore) EnabledAgentsForBusiness(_ context.Context, _, _ uuid.UUID) ([]AgentRef, error) {
	return f.refs, nil
}

func (f *fakeTriggerStore) CreateEventRun(_ context.Context, principalID, businessID, agentID uuid.UUID, dedupKey string, _ *string, _ *uuid.UUID) (bool, error) {
	if f.dedupSeen == nil {
		f.dedupSeen = map[string]bool{}
	}
	k := agentID.String() + "|" + dedupKey
	if f.dedupSeen[k] {
		return false, nil
	}
	f.dedupSeen[k] = true
	f.created = append(f.created, struct {
		principal, agent uuid.UUID
		dedup            string
	}{principalID, agentID, dedupKey})
	return true, nil
}

func ticketCreatedEvent(t *testing.T, tenant, business, ticket, message uuid.UUID) events.Event {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"ticket_id": ticket, "business_id": business, "message_id": message,
	})
	return events.Event{ID: uuid.New(), TenantRootID: tenant, Topic: events.TopicTicketCreated, Payload: payload}
}

func TestTriageTrigger_CreatesEventRunPerEnabledAgent(t *testing.T) {
	tenant, business, ticket, message := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	a1, p1 := uuid.New(), uuid.New()
	a2, p2 := uuid.New(), uuid.New()
	store := &fakeTriggerStore{refs: []AgentRef{{a1, p1}, {a2, p2}}}
	trig := &TriageTrigger{Runs: store}

	if err := trig.Handle(context.Background(), nil, ticketCreatedEvent(t, tenant, business, ticket, message)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(store.created) != 2 {
		t.Fatalf("created %d runs, want 2 (one per enabled agent)", len(store.created))
	}
	for _, c := range store.created {
		if c.dedup != message.String() {
			t.Errorf("dedup key = %s, want triggering message id %s", c.dedup, message)
		}
	}
}

func TestTriageTrigger_DedupsRedelivery(t *testing.T) {
	tenant, business, ticket, message := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	a1, p1 := uuid.New(), uuid.New()
	store := &fakeTriggerStore{refs: []AgentRef{{a1, p1}}}
	trig := &TriageTrigger{Runs: store}
	ev := ticketCreatedEvent(t, tenant, business, ticket, message)

	for i := 0; i < 3; i++ { // at-least-once redelivery
		if err := trig.Handle(context.Background(), nil, ev); err != nil {
			t.Fatalf("handle %d: %v", i, err)
		}
	}
	if len(store.created) != 1 {
		t.Fatalf("created %d runs across 3 deliveries, want 1 (idempotent)", len(store.created))
	}
}

func TestTriageTrigger_PoisonPayloadIsProcessed(t *testing.T) {
	store := &fakeTriggerStore{}
	trig := &TriageTrigger{Runs: store}
	ev := events.Event{ID: uuid.New(), TenantRootID: uuid.New(), Topic: events.TopicTicketCreated, Payload: []byte("{not json")}
	if err := trig.Handle(context.Background(), pgx.Tx(nil), ev); err != nil {
		t.Fatalf("poison payload must be treated as processed (nil err), got %v", err)
	}
	if len(store.created) != 0 {
		t.Fatalf("poison payload created %d runs, want 0", len(store.created))
	}
}
```

- [ ] **Step 2: Run — expect compile failure** (`TriageTrigger`/`TopicTicketCreated` undefined):

```bash
go test ./internal/agents/ -run TestTriageTrigger 2>&1 | head
```

> `events.TopicTicketCreated` is added in Task 7 Step 1; if you are executing strictly in order, add that one-line const now (it's harmless) or temporarily use the literal `"ticket.created"` in the test helper and switch to the const in Task 7. Recommended: do Task 7 Step 1 (the const) first, then return here.

- [ ] **Step 3: Write `internal/agents/trigger.go`:**

```go
package agents

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/events"
)

// ticketCreatedPayload is the consumer-owned decode of the inbox-produced ticket.created
// outbox event (producer enqueues {ticket_id, business_id, message_id}).
type ticketCreatedPayload struct {
	TicketID   uuid.UUID `json:"ticket_id"`
	BusinessID uuid.UUID `json:"business_id"`
	MessageID  uuid.UUID `json:"message_id"`
}

// triggerStore is the trigger's view of the run store (fakeable).
type triggerStore interface {
	EnabledAgentsForBusiness(ctx context.Context, businessID, tenantRootID uuid.UUID) ([]AgentRef, error)
	CreateEventRun(ctx context.Context, agentPrincipalID, businessID, agentID uuid.UUID, dedupKey string, targetType *string, targetID *uuid.UUID) (bool, error)
}

// TriageTrigger subscribes to ticket.created and enqueues a queued agent_run for each
// enabled agent in the ticket's business. It runs inside the outbox worker, so it MUST be
// fast — it does NOT run the agent loop (the RunDrainer does, decoupled) — and idempotent
// (it dedups on the triggering message id; at-least-once redelivery enqueues at most one
// run per agent).
//
// LOOP-GUARD (Spec 003 §3.3): it subscribes ONLY to ticket.created (a brand-new ticket),
// never message.received. An agent's own outbound reply emits ticket.replied, never
// ticket.created, so an agent reply can NEVER re-trigger triage. Inbound auto-responders
// that open new tickets are bounded upstream by spec-002's is_auto_reply suppression cap
// (migration 0024), which suppresses ingest — and thus the ticket.created enqueue.
type TriageTrigger struct {
	Runs   triggerStore
	Logger *slog.Logger
}

// Handle implements events.Handler. It ignores the worker tx (its store opens its own
// principal-scoped txs) and is idempotent: at-least-once delivery is expected.
func (t *TriageTrigger) Handle(ctx context.Context, _ pgx.Tx, ev events.Event) error {
	var p ticketCreatedPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		// Poison payload: log + treat as processed (the producer is trusted, in-process)
		// so it doesn't retry forever.
		t.logger().ErrorContext(ctx, "triage trigger: bad ticket.created payload", "event_id", ev.ID, "err", err)
		return nil
	}
	refs, err := t.Runs.EnabledAgentsForBusiness(ctx, p.BusinessID, ev.TenantRootID)
	if err != nil {
		return err // transient → reschedule
	}
	targetType := "ticket"
	dedup := p.MessageID.String()
	for _, ref := range refs {
		if _, err := t.Runs.CreateEventRun(ctx, ref.PrincipalID, p.BusinessID, ref.AgentID, dedup, &targetType, &p.TicketID); err != nil {
			// Reschedule the whole event; runs already created dedup on retry (exactly-once
			// per agent), so partial progress is safe.
			return err
		}
	}
	return nil
}

func (t *TriageTrigger) logger() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}
```

- [ ] **Step 4: Run the tests:**

```bash
go test ./internal/agents/ -run TestTriageTrigger -v 2>&1 | tail -20
```
Expected: 3 PASS.

- [ ] **Step 5: Commit:**

```bash
git add internal/agents/trigger.go internal/agents/trigger_test.go
git commit -m "feat(agents): TriageTrigger ticket.created subscriber (loop-guarded, idempotent) (l29/US5)"
```

---

### Task 6: `RunDrainer` — claim queued runs and execute

**Files:**
- Create: `internal/agents/drainer.go`
- Test: `internal/agents/drainer_test.go`

- [ ] **Step 1: Write the failing tests `internal/agents/drainer_test.go`:**

```go
package agents

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

type fakeClaimer struct {
	queue []*ClaimedRun // popped front-to-back; nil entries simulate an empty queue
	calls int
}

func (f *fakeClaimer) ClaimNextQueuedRun(_ context.Context) (*ClaimedRun, error) {
	f.calls++
	if len(f.queue) == 0 {
		return nil, nil
	}
	c := f.queue[0]
	f.queue = f.queue[1:]
	return c, nil
}

type fakeExecutor struct {
	ran []struct {
		principal uuid.UUID
		runID     uuid.UUID
		agentID   uuid.UUID
	}
}

func (f *fakeExecutor) Execute(_ context.Context, principalID uuid.UUID, ag Agent, run AgentRun) (AgentRun, error) {
	f.ran = append(f.ran, struct {
		principal uuid.UUID
		runID     uuid.UUID
		agentID   uuid.UUID
	}{principalID, run.ID, ag.ID})
	run.Status = RunSucceeded
	return run, nil
}

func claimedFixture() *ClaimedRun {
	tt := "ticket"
	tid := uuid.New()
	return &ClaimedRun{
		RunID: uuid.New(), CorrelationID: uuid.NewString(), TargetType: &tt, TargetID: &tid,
		Agent: Agent{ID: uuid.New(), BusinessID: uuid.New(), PrincipalID: uuid.New(), Enabled: true, AllowedTools: []string{"read_ticket"}},
	}
}

func TestRunDrainer_ClaimsAndExecutesAsAgent(t *testing.T) {
	c := claimedFixture()
	exec := &fakeExecutor{}
	d := &RunDrainer{Runs: &fakeClaimer{queue: []*ClaimedRun{c}}, Engine: exec}

	ran, err := d.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !ran {
		t.Fatal("DrainOnce returned false, want true (a run was claimed)")
	}
	if len(exec.ran) != 1 {
		t.Fatalf("executed %d runs, want 1", len(exec.ran))
	}
	got := exec.ran[0]
	if got.principal != c.Agent.PrincipalID {
		t.Errorf("executed as principal %s, want the AGENT principal %s", got.principal, c.Agent.PrincipalID)
	}
	if got.runID != c.RunID || got.agentID != c.Agent.ID {
		t.Errorf("executed run/agent = %s/%s, want %s/%s", got.runID, got.agentID, c.RunID, c.Agent.ID)
	}
}

func TestRunDrainer_EmptyQueue(t *testing.T) {
	exec := &fakeExecutor{}
	d := &RunDrainer{Runs: &fakeClaimer{}, Engine: exec}
	ran, err := d.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if ran {
		t.Fatal("DrainOnce returned true on an empty queue, want false")
	}
	if len(exec.ran) != 0 {
		t.Fatalf("executed %d runs on empty queue, want 0", len(exec.ran))
	}
}
```

- [ ] **Step 2: Run — expect compile failure** (`RunDrainer` undefined):

```bash
go test ./internal/agents/ -run TestRunDrainer 2>&1 | head
```

- [ ] **Step 3: Write `internal/agents/drainer.go`:**

```go
package agents

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
)

// runClaimer + runExecutor are the drainer's narrow views (fakeable; *AgentRunStore and
// *Engine satisfy them in production).
type runClaimer interface {
	ClaimNextQueuedRun(ctx context.Context) (*ClaimedRun, error)
}
type runExecutor interface {
	Execute(ctx context.Context, agentPrincipalID uuid.UUID, ag Agent, run AgentRun) (AgentRun, error)
}

// RunDrainer claims queued agent_runs and executes them as the agent, decoupled from the
// outbox worker so a long run (up to the wall-clock bound) never stalls event delivery.
// One DrainOnce claims+runs a single queued run; the background loop in main.go calls it
// until the queue drains, then ticks. The queued→running claim (SKIP LOCKED, in the
// definer fn) is the exactly-once gate: no two drainers ever execute the same run.
type RunDrainer struct {
	Runs   runClaimer
	Engine runExecutor
	Logger *slog.Logger
}

// DrainOnce claims and executes at most one queued run. Returns (true, nil) when a run was
// claimed+executed (caller should loop to drain more), (false, nil) when the queue is empty.
func (d *RunDrainer) DrainOnce(ctx context.Context) (bool, error) {
	claimed, err := d.Runs.ClaimNextQueuedRun(ctx)
	if err != nil {
		return false, err
	}
	if claimed == nil {
		return false, nil
	}
	run := AgentRun{
		ID: claimed.RunID, AgentID: claimed.Agent.ID, BusinessID: claimed.Agent.BusinessID,
		Trigger: "event", TargetType: claimed.TargetType, TargetID: claimed.TargetID,
		Status: RunRunning, CorrelationID: claimed.CorrelationID,
	}
	if _, eerr := d.Engine.Execute(ctx, claimed.Agent.PrincipalID, claimed.Agent, run); eerr != nil {
		// Execute already persisted a terminal (failed) state + audit; log and continue so
		// one bad run never wedges the drain loop.
		d.logger().ErrorContext(ctx, "run drainer: execute", "run_id", claimed.RunID, "err", eerr)
	}
	return true, nil
}

func (d *RunDrainer) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}
```

- [ ] **Step 4: Run the tests + confirm `*Engine`/`*AgentRunStore` satisfy the interfaces:**

```bash
go test ./internal/agents/ -run TestRunDrainer -v 2>&1 | tail
go build ./... 2>&1 | tail
```
Expected: 2 PASS, clean build.

- [ ] **Step 5: Commit:**

```bash
git add internal/agents/drainer.go internal/agents/drainer_test.go
git commit -m "feat(agents): RunDrainer — claim queued runs + Execute as the agent (l29)"
```

---

### Task 7: Wire it — `TopicTicketCreated` const, inbox producer, main.go

**Files:**
- Modify: `internal/platform/events/bus.go` (const)
- Modify: `internal/inbox/service.go` (use the const)
- Modify: `cmd/manyforge/main.go` (Subscribe + drain poller goroutine)

- [ ] **Step 1: Add the const to `internal/platform/events/bus.go`** (in the cross-module topics `const (...)` block, alongside `TopicTicketReplied`):

```go
	// TopicTicketCreated fires once per brand-new ticket (inbox ingest). The agent-runtime
	// TriageTrigger subscribes to it (and ONLY it — never message.received — so an agent's
	// own reply can't re-trigger triage).
	TopicTicketCreated = "ticket.created"
```

- [ ] **Step 2: Use the const in `internal/inbox/service.go`** (the existing enqueue ~line 266 currently passes the literal `"ticket.created"`):

```go
		if out.Created {
			if err := events.Enqueue(ctx, tx, r.tenantRootID, events.TopicTicketCreated, map[string]any{
				"ticket_id":   out.TicketID,
				"business_id": r.businessID,
				"message_id":  out.MessageID,
			}); err != nil {
				return err
			}
		}
```

- [ ] **Step 3: Wire the subscriber + poller in `cmd/manyforge/main.go`.** After `agentRunStore` and `agentEngine` are built (and after the US4 approvals block), construct the trigger + drainer:

```go
	// US5 triage trigger + run drainer (l29 async path). The trigger subscribes to
	// ticket.created and enqueues a queued run per enabled agent (fast, idempotent); the
	// drainer poller claims queued runs and runs the loop as the agent — decoupled from
	// the outbox worker so a long run never stalls event delivery.
	triageTrigger := &agents.TriageTrigger{Runs: agentRunStore, Logger: logger}
	runDrainer := &agents.RunDrainer{Runs: agentRunStore, Engine: agentEngine, Logger: logger}
```

Add the subscription next to the US4 one (after `eventBus.Subscribe(agents.TopicAgentApproved, approvalExec.Handle)`):

```go
	// US5: an enabled agent auto-runs on a brand-new ticket. ONLY ticket.created is
	// subscribed (never message.received) — the structural loop-guard (an agent reply
	// emits ticket.replied, not ticket.created).
	eventBus.Subscribe(events.TopicTicketCreated, triageTrigger.Handle)
```

Start the drain poller alongside the US4 expire sweep (after `go outboxWorker.Run(workerCtx)`):

```go
	// US5 run drainer: every 2s, drain all queued agent_runs (claim queued→running via the
	// SKIP-LOCKED definer fn, then run the loop as the agent). Serial per tick for v1; the
	// SKIP-LOCKED claim already supports horizontal scaling if we add workers later.
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-t.C:
				for {
					ran, err := runDrainer.DrainOnce(workerCtx)
					if err != nil {
						logger.WarnContext(workerCtx, "agent run drain", "err", err)
						break
					}
					if !ran {
						break
					}
				}
			}
		}
	}()
```

- [ ] **Step 4: Build + vet + the full unit gate:**

```bash
export PATH="$PATH:$HOME/go/bin"
go build ./... 2>&1 | tail
make test 2>&1 | tail -15
make lint 2>&1 | tail -5
```
Expected: clean build; `make test` PASS; `make lint` **0 issues** (the merge gate — confirm golangci-lint actually ran, not vet-only).

- [ ] **Step 5: Commit:**

```bash
git add internal/platform/events/bus.go internal/inbox/service.go cmd/manyforge/main.go
git commit -m "feat(agents): wire TriageTrigger (ticket.created) + RunDrainer poller (l29)"
```

This completes **l29** — the async event-driven run trigger end-to-end.

---

## PHASE B — US5: the triage demo (acceptance + pins)

### Task 8: End-to-end acceptance integration test (+ idempotency, loop-guard, claim/dedup)

**Files:**
- Create: `internal/agents/us5_triage_integration_test.go` (`//go:build integration`)

This is THE demo, plus the lower-level integration cases for Tasks 3/4. Reuse the harness verbatim from `internal/agents/run_integration_test.go` (`testdb.Start`, `seedRunTenant`, `seedRunTicket`, `strptr`) and the inbox ingest helpers (`internal/inbox/ingest_integration_test.go`). The agents package version of the seed (`seedRunTenant`) gives an owner principal (can create agents + approve) and a requester.

- [ ] **Step 1: Write the integration tests.** Sub-tests:
  1. **`TestUS5_CreateEventRun_Idempotent`** — `store.CreateEventRun` twice with the same dedup key ⇒ first `true`, second `false`; exactly 1 `agent_run` row.
  2. **`TestUS5_EnabledAgentsForBusiness`** — one enabled + one disabled agent ⇒ lister returns only the enabled one.
  3. **`TestUS5_ClaimNextQueuedRun`** — a queued event run ⇒ claim returns it (status now `running`, full agent config); a second claim returns nil.
  4. **`TestUS5_TriageAcceptanceThread`** — the full demo (ingest → trigger → drain → Mode-1 triage + queued draft_reply → approve → reply sent), with idempotency + loop-guard assertions.

```go
//go:build integration

package agents

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/ai"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/internal/ticketing"
)

func TestUS5_CreateEventRun_Idempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	seed := seedRunTenant(ctx, t, tdb)

	agentSvc := &AgentService{DB: tdb.App}
	ag, err := agentSvc.Create(ctx, seed.ownerID, seed.businessID, CreateAgentInput{
		Name: "Triage", Provider: "anthropic", Model: "claude-sonnet-4-5",
		SystemPrompt: "triage", AllowedTools: []string{"read_ticket", "set_status"},
		AutonomyMode: ModeAssist, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	store := &AgentRunStore{DB: tdb.App}
	tt := "ticket"
	tid := uuid.New()
	dedup := uuid.NewString()

	created1, err := store.CreateEventRun(ctx, ag.PrincipalID, seed.businessID, ag.ID, dedup, &tt, &tid)
	if err != nil || !created1 {
		t.Fatalf("first CreateEventRun: created=%v err=%v, want true/nil", created1, err)
	}
	created2, err := store.CreateEventRun(ctx, ag.PrincipalID, seed.businessID, ag.ID, dedup, &tt, &tid)
	if err != nil {
		t.Fatalf("second CreateEventRun err: %v", err)
	}
	if created2 {
		t.Fatal("second CreateEventRun created a duplicate run, want deduped (false)")
	}
	var n int
	if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM agent_run WHERE agent_id=$1 AND trigger_dedup_key=$2", ag.ID, dedup).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("agent_run rows = %d, want 1 (idempotent on dedup key)", n)
	}
}

func TestUS5_EnabledAgentsForBusiness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	seed := seedRunTenant(ctx, t, tdb)
	agentSvc := &AgentService{DB: tdb.App}

	on, err := agentSvc.Create(ctx, seed.ownerID, seed.businessID, CreateAgentInput{
		Name: "On", Provider: "anthropic", Model: "claude-sonnet-4-5", SystemPrompt: "x",
		AllowedTools: []string{"read_ticket"}, AutonomyMode: ModeAssist, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create enabled: %v", err)
	}
	if _, err := agentSvc.Create(ctx, seed.ownerID, seed.businessID, CreateAgentInput{
		Name: "Off", Provider: "anthropic", Model: "claude-sonnet-4-5", SystemPrompt: "x",
		AllowedTools: []string{"read_ticket"}, AutonomyMode: ModeAssist, Enabled: false,
	}); err != nil {
		t.Fatalf("create disabled: %v", err)
	}

	store := &AgentRunStore{DB: tdb.App}
	refs, err := store.EnabledAgentsForBusiness(ctx, seed.businessID, seed.tenantRootID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(refs) != 1 || refs[0].AgentID != on.ID || refs[0].PrincipalID != on.PrincipalID {
		t.Fatalf("enabled refs = %+v, want exactly the enabled agent %s/%s", refs, on.ID, on.PrincipalID)
	}
}

func TestUS5_ClaimNextQueuedRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	seed := seedRunTenant(ctx, t, tdb)
	agentSvc := &AgentService{DB: tdb.App}
	ag, err := agentSvc.Create(ctx, seed.ownerID, seed.businessID, CreateAgentInput{
		Name: "Claim", Provider: "anthropic", Model: "claude-sonnet-4-5", SystemPrompt: "sys",
		AllowedTools: []string{"read_ticket", "draft_reply"}, AutonomyMode: ModeAssist, Enabled: true, MonthlyBudgetCents: 500,
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	store := &AgentRunStore{DB: tdb.App}
	tt := "ticket"
	tid := uuid.New()
	if _, err := store.CreateEventRun(ctx, ag.PrincipalID, seed.businessID, ag.ID, uuid.NewString(), &tt, &tid); err != nil {
		t.Fatalf("create event run: %v", err)
	}

	claimed, err := store.ClaimNextQueuedRun(ctx)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %+v err=%v, want a claimed run", claimed, err)
	}
	if claimed.Agent.ID != ag.ID || claimed.Agent.PrincipalID != ag.PrincipalID {
		t.Errorf("claimed agent = %s/%s, want %s/%s", claimed.Agent.ID, claimed.Agent.PrincipalID, ag.ID, ag.PrincipalID)
	}
	if claimed.Agent.SystemPrompt != "sys" || claimed.Agent.MonthlyBudgetCents != 500 || len(claimed.Agent.AllowedTools) != 2 {
		t.Errorf("claimed agent config not fully hydrated: %+v", claimed.Agent)
	}
	var status string
	if err := tdb.Super.QueryRow(ctx, "SELECT status FROM agent_run WHERE id=$1", claimed.RunID).Scan(&status); err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != RunRunning {
		t.Errorf("claimed run status = %s, want running (claim transitions queued→running)", status)
	}
	if again, err := store.ClaimNextQueuedRun(ctx); err != nil || again != nil {
		t.Fatalf("second claim = %+v err=%v, want nil/nil (nothing left queued)", again, err)
	}
}

func TestUS5_TriageAcceptanceThread(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	seed := seedRunTenant(ctx, t, tdb)
	// A brand-new ticket in 'new' status (the trigger's target).
	ticketID := seedRunTicket(ctx, t, tdb, seed, "new")

	agentSvc := &AgentService{DB: tdb.App}
	tktSvc := &ticketing.Service{DB: tdb.App}
	store := &AgentRunStore{DB: tdb.App}

	ag, err := agentSvc.Create(ctx, seed.ownerID, seed.businessID, CreateAgentInput{
		Name: "Triage Bot", Provider: "anthropic", Model: "claude-sonnet-4-5",
		SystemPrompt: "Triage and draft a reply.",
		AllowedTools: []string{"read_ticket", "set_priority", "set_tags", "draft_reply"},
		AutonomyMode: ModeAssist, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	// --- Stage 1: simulate the ticket.created delivery to the TriageTrigger using the REAL
	// payload shape. (drainOnce is unexported in package events, so we invoke Handle directly
	// with a constructed event — the dominant house pattern for asserting a subscriber ran.)
	msgID := uuid.New()
	ev := events.Event{
		ID: uuid.New(), TenantRootID: seed.tenantRootID, Topic: events.TopicTicketCreated,
		Payload: mustJSON(t, map[string]any{"ticket_id": ticketID, "business_id": seed.businessID, "message_id": msgID}),
	}
	trigger := &TriageTrigger{Runs: store}
	if err := trigger.Handle(ctx, nil, ev); err != nil {
		t.Fatalf("trigger handle: %v", err)
	}
	// Exactly one queued run now exists for our agent.
	if n := countSuperRows(ctx, t, tdb, "SELECT count(*) FROM agent_run WHERE agent_id=$1 AND status='queued'", ag.ID); n != 1 {
		t.Fatalf("queued runs after trigger = %d, want 1", n)
	}

	// --- Stage 2: drain via the RunDrainer with a mock provider scripting Mode-1 triage +
	// a gated draft_reply, then a final text. set_priority/set_tags are Reversible (auto-
	// applied inline); draft_reply is External (queued for approval).
	setPriArgs := mustJSON(t, map[string]string{"ticket_id": ticketID.String(), "priority": "high"})
	setTagArgs := mustJSON(t, map[string]any{"ticket_id": ticketID.String(), "tags": []string{"billing"}})
	replyArgs := mustJSON(t, map[string]string{"ticket_id": ticketID.String(), "body_text": "Thanks — we're on it."})
	mock := ai.NewMockProvider(
		ai.Response{FinishReason: ai.FinishToolUse, ToolCalls: []ai.ToolCall{
			{ID: "c1", Name: "set_priority", Args: setPriArgs},
			{ID: "c2", Name: "set_tags", Args: setTagArgs},
			{ID: "c3", Name: "draft_reply", Args: replyArgs},
		}, Usage: ai.Usage{InputTokens: 120, OutputTokens: 40}},
		ai.Response{Text: "Triaged and drafted a reply.", FinishReason: ai.FinishStop, Usage: ai.Usage{InputTokens: 30, OutputTokens: 8}},
	)
	reg := ai.NewRegistry()
	ai.RegisterDefaults(reg)
	approvalStore := &ApprovalStore{DB: tdb.App}
	engine := &Engine{
		Runs: store, Tools: NewToolRegistry(tktSvc), Auditor: NewDBAuditor(tdb.App),
		Resolver: NewAuthzChecker(tdb.App), Approvals: approvalStore,
		NewProvider: func(_ context.Context, _, _ uuid.UUID, _ string) (ai.Provider, string, error) { return mock, ag.Model, nil },
		Cost: func(model string, u ai.Usage) int64 { m, ok := reg.Lookup(model); if !ok { return 0 }; return m.CostCents(u) },
	}
	drainer := &RunDrainer{Runs: store, Engine: engine}
	ran, err := drainer.DrainOnce(ctx)
	if err != nil || !ran {
		t.Fatalf("drain: ran=%v err=%v, want true/nil", ran, err)
	}

	// Mode-1 applied the reversible triage inline:
	var pri string
	var tags []string
	if err := tdb.Super.QueryRow(ctx, "SELECT priority, tags FROM ticket WHERE id=$1", ticketID).Scan(&pri, &tags); err != nil {
		t.Fatalf("read ticket: %v", err)
	}
	if pri != "high" {
		t.Errorf("priority = %s, want high (Mode-1 auto-applied set_priority)", pri)
	}
	if len(tags) != 1 || tags[0] != "billing" {
		t.Errorf("tags = %v, want [billing] (Mode-1 auto-applied set_tags)", tags)
	}
	// draft_reply was queued, not sent: the run is awaiting_approval with one pending item.
	var runStatus string
	if err := tdb.Super.QueryRow(ctx, "SELECT status FROM agent_run WHERE agent_id=$1 ORDER BY created_at DESC LIMIT 1", ag.ID).Scan(&runStatus); err != nil {
		t.Fatalf("run status: %v", err)
	}
	if runStatus != RunAwaitingApproval {
		t.Fatalf("run status = %s, want awaiting_approval (draft_reply gated)", runStatus)
	}
	var apID uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM approval_item WHERE business_id=$1 AND tool='draft_reply' AND state='pending'", seed.businessID).Scan(&apID); err != nil {
		t.Fatalf("expected one pending draft_reply approval: %v", err)
	}
	// No reply has been sent yet.
	if n := countSuperRows(ctx, t, tdb, "SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND direction='outbound'", ticketID); n != 0 {
		t.Fatalf("outbound messages before approval = %d, want 0 (still gated)", n)
	}

	// --- Stage 3: a human approves, then the ApprovalExecutor sends the reply (mirrors
	// approval_integration_test: Approve enqueues TopicAgentApproved; we run the executor on
	// the constructed event).
	approvalSvc := NewApprovalService(approvalStore)
	if _, err := approvalSvc.Approve(ctx, seed.ownerID, seed.businessID, apID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	exec := &ApprovalExecutor{Approvals: approvalStore, Tools: NewToolRegistry(tktSvc), Auditor: NewDBAuditor(tdb.App)}
	approvedEv := drainApprovedEvent(ctx, t, tdb, seed.tenantRootID, apID)
	if err := exec.Handle(ctx, nil, approvedEv); err != nil {
		t.Fatalf("approval executor: %v", err)
	}

	// The reply is sent: one outbound message tied to the approval, and a ticket.replied
	// outbox event enqueued (the notify subscriber does the actual send, tested elsewhere).
	if n := countSuperRows(ctx, t, tdb, "SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND direction='outbound' AND source_approval_item_id=$2", ticketID, apID); n != 1 {
		t.Fatalf("outbound reply tied to approval = %d, want 1 (reply sent on approval)", n)
	}
	if n := countSuperRows(ctx, t, tdb, "SELECT count(*) FROM outbox WHERE tenant_root_id=$1 AND topic='ticket.replied'", seed.tenantRootID); n < 1 {
		t.Fatalf("ticket.replied outbox events = %d, want >= 1 (reply queued to send)", n)
	}

	// --- Idempotency: a redelivered ticket.created creates NO second run.
	if err := trigger.Handle(ctx, nil, ev); err != nil {
		t.Fatalf("trigger redelivery: %v", err)
	}
	if n := countSuperRows(ctx, t, tdb, "SELECT count(*) FROM agent_run WHERE agent_id=$1", ag.ID); n != 1 {
		t.Fatalf("total runs after redelivery = %d, want 1 (dedup on message id)", n)
	}

	// --- Loop-guard: the agent's own reply emitted ticket.replied, NOT ticket.created —
	// so nothing in this flow can re-trigger triage.
	if n := countSuperRows(ctx, t, tdb, "SELECT count(*) FROM outbox WHERE tenant_root_id=$1 AND topic='ticket.created'", seed.tenantRootID); n != 0 {
		t.Fatalf("ticket.created outbox events = %d, want 0 (no agent action emits ticket.created in this test)", n)
	}
}

// --- helpers (this file) ---

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func countSuperRows(ctx context.Context, t *testing.T, tdb *testdb.TestDB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := tdb.Super.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

// drainApprovedEvent reads the TopicAgentApproved payload the Approve tx enqueued and
// returns it as an events.Event (mirrors approval_integration_test's manual event build).
func drainApprovedEvent(ctx context.Context, t *testing.T, tdb *testdb.TestDB, tenant, approvalID uuid.UUID) events.Event {
	t.Helper()
	var payload []byte
	if err := tdb.Super.QueryRow(ctx,
		"SELECT payload FROM outbox WHERE tenant_root_id=$1 AND topic=$2 ORDER BY id DESC LIMIT 1",
		tenant, TopicAgentApproved).Scan(&payload); err != nil {
		t.Fatalf("read approved outbox event: %v", err)
	}
	return events.Event{ID: uuid.New(), TenantRootID: tenant, Topic: TopicAgentApproved, Payload: payload}
}
```

> ADAPT WHILE EXECUTING — verify against the real code before asserting green:
> - `NewApprovalService(...)` / `approvalSvc.Approve(ctx, principalID, businessID, approvalID)` arg order — confirm against `internal/agents/approval_handler.go` / `approval.go`. If `Approve` needs a separate `decidedBy`, pass `seed.ownerID` for both.
> - `ApprovalExecutor` field names (`Approvals`, `Tools`, `Auditor`) — confirm against `approval_executor.go`.
> - `ticket.tags` column type (it may be a separate `ticket_tag` table rather than a `text[]` column). If so, assert tags via a join instead of `SELECT tags FROM ticket`. Check `migrations/0013_support_desk.up.sql` / the `set_tags` tool's SQL.
> - `set_priority`/`set_tags`/`draft_reply` arg JSON shapes — confirm against each tool's schema in `internal/agents/tools.go`.
> - whether `seedRunTicket`'s message row already counts as the triggering message (the dedup key here is a fresh `msgID`, which is fine — dedup only needs to be stable across redeliveries).

- [ ] **Step 2: Run the US5 integration tests** (Docker/Colima must be up; `-p 1`):

```bash
export PATH="$PATH:$HOME/go/bin"
go test -tags integration ./internal/agents/ -run 'TestUS5' -p 1 -v 2>&1 | tail -40
```
Expected: 4 PASS. Fix adaptation points above until green.

- [ ] **Step 3: Commit:**

```bash
git add internal/agents/us5_triage_integration_test.go
git commit -m "test(agents): US5 end-to-end triage acceptance + idempotency + loop-guard + claim/dedup (manyforge-ehe)"
```

---

### Task 9: Security-regression pins

**Files:**
- Create: `internal/security_regression/agent_run_us5_pins_test.go` (NO build tag — runs in `make test` + `make sec-test`)

- [ ] **Step 1: Write the pins** (uses the shared `mustRead` helper already in the package):

```go
// No build tag: these source-level pins run in `make test` and `make sec-test` with NO
// infrastructure. They make a refactor that silently drops a US5/l29 protection fail the
// security gate loudly, complementing the behavioral tests in internal/agents/ (trigger,
// drainer, and the US5 acceptance integration test).
//
// US5/l29 contract: Spec 003 design §3.3/§5/§6; epic manyforge-deo / issues manyforge-l29
// (async trigger) + manyforge-ehe (triage demo).

package security_regression

import (
	"strings"
	"testing"
)

// TestPin_TriageTriggerOnlyTicketCreated pins the loop-guard: the agent-runtime subscribes
// the triage trigger to ticket.created ONLY — never message.received. An agent's own reply
// emits ticket.replied (not ticket.created), so subscribing only to ticket.created means an
// agent reply can never re-trigger triage. A subscription to message.received would reopen
// the loop.
func TestPin_TriageTriggerOnlyTicketCreated(t *testing.T) {
	mainGo := mustRead(t, "../../cmd/manyforge/main.go")
	if !strings.Contains(mainGo, "eventBus.Subscribe(events.TopicTicketCreated, triageTrigger.Handle)") {
		t.Error("main.go: triage trigger must subscribe to events.TopicTicketCreated")
	}
	if strings.Contains(mainGo, "message.received") || strings.Contains(mainGo, "TopicMessageReceived") {
		t.Error("main.go: triage must NOT subscribe to message.received — that reopens the agent-reply loop")
	}
	trig := mustRead(t, "../agents/trigger.go")
	if !strings.Contains(trig, "LOOP-GUARD") {
		t.Error("trigger.go: the loop-guard rationale comment must be present (documents why only ticket.created)")
	}
}

// TestPin_QueuedRunClaimIsStateClaim pins exactly-once execution: the claim transitions
// queued→running under FOR UPDATE SKIP LOCKED, so no two drainers ever run the same row.
func TestPin_QueuedRunClaimIsStateClaim(t *testing.T) {
	mig := mustRead(t, "../../migrations/0034_agent_run_trigger.up.sql")
	for _, frag := range []string{
		"claim_next_queued_agent_run",
		"status = 'queued'",
		"FOR UPDATE SKIP LOCKED",
		"SET status = 'running'",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0034 up: missing claim fragment %q — queued→running state claim weakened?", frag)
		}
	}
}

// TestPin_EventRunDedupUnique pins idempotency: the partial unique index on
// (agent_id, trigger_dedup_key) turns at-least-once outbox delivery into exactly-one run
// per agent per triggering message.
func TestPin_EventRunDedupUnique(t *testing.T) {
	mig := mustRead(t, "../../migrations/0034_agent_run_trigger.up.sql")
	if !strings.Contains(mig, "CREATE UNIQUE INDEX agent_run_trigger_dedup_idx") ||
		!strings.Contains(mig, "(agent_id, trigger_dedup_key)") ||
		!strings.Contains(mig, "WHERE trigger_dedup_key IS NOT NULL") {
		t.Error("0034 up: the partial unique dedup index on (agent_id, trigger_dedup_key) is required for exactly-once event runs")
	}
}

// TestPin_TriggerTenantScoped pins tenant isolation: the enabled-agents lister scopes by
// BOTH business_id AND tenant_root_id, so a ticket.created in tenant A can never surface
// (and trigger) tenant B's agents.
func TestPin_TriggerTenantScoped(t *testing.T) {
	mig := mustRead(t, "../../migrations/0034_agent_run_trigger.up.sql")
	if !strings.Contains(mig, "enabled_agents_for_business") {
		t.Fatal("0034 up: enabled_agents_for_business fn missing")
	}
	if !strings.Contains(mig, "business_id = p_business_id") || !strings.Contains(mig, "tenant_root_id = p_tenant_root_id") {
		t.Error("0034 up: enabled_agents_for_business must scope by business_id AND tenant_root_id (cross-tenant isolation)")
	}
}

// TestPin_DefinerFnsHardened pins the SECURITY DEFINER hardening on both new fns: pinned
// search_path + REVOKE FROM PUBLIC + GRANT EXECUTE only to the app role (the 0016/0032
// convention).
func TestPin_DefinerFnsHardened(t *testing.T) {
	mig := mustRead(t, "../../migrations/0034_agent_run_trigger.up.sql")
	for _, frag := range []string{
		"SECURITY DEFINER SET search_path = public",
		"REVOKE ALL ON FUNCTION enabled_agents_for_business(uuid, uuid) FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION enabled_agents_for_business(uuid, uuid) TO manyforge_app",
		"REVOKE ALL ON FUNCTION claim_next_queued_agent_run() FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION claim_next_queued_agent_run() TO manyforge_app",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0034 up: missing definer-hardening fragment %q", frag)
		}
	}
}

// TestPin_TriggerRunsAsAgentPrincipal pins that event runs are created under the AGENT's
// own principal (RLS identity), not principal-less: the trigger passes ref.PrincipalID into
// CreateEventRun, and CreateEventRun opens a WithPrincipal tx.
func TestPin_TriggerRunsAsAgentPrincipal(t *testing.T) {
	trig := mustRead(t, "../agents/trigger.go")
	if !strings.Contains(trig, "ref.PrincipalID") {
		t.Error("trigger.go: CreateEventRun must be called with ref.PrincipalID (the agent principal)")
	}
	store := mustRead(t, "../agents/agent_run.go")
	createIdx := strings.Index(store, "func (s *AgentRunStore) CreateEventRun")
	if createIdx < 0 {
		t.Fatal("agent_run.go: CreateEventRun missing")
	}
	body := store[createIdx:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "WithPrincipal(ctx, agentPrincipalID") {
		t.Error("agent_run.go: CreateEventRun must run under WithPrincipal(agentPrincipalID) so the insert passes RLS as the agent")
	}
}
```

- [ ] **Step 2: Run the pins (no infra needed):**

```bash
go test ./internal/security_regression/ -run 'TestPin_(TriageTrigger|QueuedRunClaim|EventRunDedup|TriggerTenant|DefinerFns|TriggerRunsAsAgent)' -v 2>&1 | tail -25
```
Expected: 6 PASS.

- [ ] **Step 3: Commit:**

```bash
git add internal/security_regression/agent_run_us5_pins_test.go
git commit -m "test(sec): pin US5/l29 contract — loop-guard, exactly-once claim, dedup, tenant-scope, definer hardening"
```

---

### Task 10: Full gate, bd close, plan commit

- [ ] **Step 1: Run the FULL merge gate** (`int-test` ~6 min, needs Docker):

```bash
export PATH="$PATH:$HOME/go/bin"
make test && make contract-test && make lint && make sec-test && make int-test 2>&1 | tail -30
git checkout -- .beads/issues.jsonl 2>/dev/null || true  # drop cosmetic bd churn
```
Expected: every target green; `make lint` **0 issues** (confirm golangci-lint ran).

- [ ] **Step 2: Close bd issues + commit the plan:**

```bash
export PATH="$PATH:$HOME/go/bin"
bd close manyforge-l29   # async trigger foundation shipped
bd close manyforge-ehe   # US5 triage demo shipped
# File the deferred follow-up surfaced in this plan:
bd create --title="US5 follow-up: optional re-triage on message.received (customer reply) with its own loop-guard" \
  --description="US5 deliberately triggers only on ticket.created. Re-triaging on a customer's later reply (message.received) needs a dedicated guard (don't re-trigger from an agent-provoked inbound). Scope + design separately." \
  --type=feature --priority=3 --parent manyforge-deo
git add docs/superpowers/plans/2026-06-03-us5-triage-demo.md .beads/
git commit -m "docs(agents): US5 triage demo + l29 async-trigger plan (executed; full gate green)"
```

- [ ] **Step 3: Push (session-completion mandate):**

```bash
git pull --rebase && git push && git status
```
Expected: "up to date with origin".

---

## Self-review (run against the spec)

- **Spec §2.7 "both triggers":** auto-on-`ticket.created` (Tasks 5-7) ✓; manual "Run triage" already shipped (US3) ✓.
- **Spec §3.3 "enqueue agent_run via outbox; worker(principal=agent): loop":** Stage 1 trigger enqueues the queued run (Task 5); Stage 2 drainer runs the loop as the agent (Task 6) ✓.
- **Spec §5 US5 "Mode 1 proposes triage (auto-applies reversible) + gated draft reply; approve→reply sent":** acceptance test asserts exactly this (Task 8 §`TestUS5_TriageAcceptanceThread`) ✓.
- **Spec §6 "loop-guard / reuse is_auto_reply":** structural (subscribe ticket.created only) + spec-002 suppression inherited; pinned (Task 9) ✓.
- **Spec §4 regression contract "approval execution idempotent":** US4 already pinned; US5 adds event-run dedup (exactly-one run per delivery) + queued→running claim (exactly-once execution), both pinned (Task 9) ✓.
- **Tenant isolation:** definer fns scoped by tenant_root_id; pinned (Task 9 `TestPin_TriggerTenantScoped`) ✓.
- **Placeholder scan:** none — every step has concrete code/commands. Test files carry an explicit "ADAPT WHILE EXECUTING" list where a signature must be confirmed against existing code (not a placeholder — a verification checklist).
- **Type consistency:** `CreateEventRun`/`EnabledAgentsForBusiness`/`ClaimNextQueuedRun` signatures match between `agent_run.go` (Tasks 3-4), the `triggerStore`/`runClaimer` interfaces (Tasks 5-6), and the fakes (Tasks 5-6). `Execute` signature matches between `runner.go` (Task 1), `runExecutor` (Task 6), and the fake (Task 6). `ClaimedRun.Agent` is a full `Agent` consumed identically in the drainer and the claim store method.
