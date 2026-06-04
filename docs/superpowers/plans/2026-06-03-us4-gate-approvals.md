# US4 — Autonomy Gate + Approvals Queue Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the US3 run-loop's degenerate "non-Safe ⇒ propose" rule with the real fail-closed autonomy gate (tool effect-class × agent autonomy-mode), persist gated tool-calls as `approval_item` rows, and add a human approvals queue (list/approve/deny) whose approvals execute the tool exactly-once through the existing transactional outbox.

**Architecture:** A pure `gate(effect, mode)` function decides `exec | approval` for each tool call; the loop's `execTool` either invokes inline (as today) or writes a `pending` `approval_item`. A human with `agents.approve` approves an item → the service flips it to `approved` and enqueues an `agent.action.approved` outbox event in one tx → the existing outbox `Worker` dispatches it to a new `ApprovalExecutor` subscriber, which re-uses the **same internal tool registry** to invoke the approved tool as the agent principal, guarded by the approval state-machine plus a reply-dedup key for true exactly-once. Everything is tenant-isolated via RLS (mirroring `agent_run`), no-oracle on reads, and audited.

**Tech Stack:** Go 1.x, chi router, pgx v5, sqlc (`db/query` → `internal/platform/db/dbgen`), golang-migrate (`migrations/`), PostgreSQL RLS, testcontainers (integration), `internal/platform/events` outbox/bus, `internal/platform/ai` mock provider for deterministic loop tests.

---

## Context the worker needs (read these first)

- **Spec/design:** `docs/superpowers/specs/2026-06-02-agent-runtime-design.md` — §3.2 (`approval_item` DDL), §3.3 (gate flow), §5 US4 row, §4 (regression contract), §6 (spec-002 reuse), §8 (open Q: expires TTL — **resolved here**: 7-day TTL, sweep every 60s).
- **The seam US4 replaces:** `internal/agents/runner.go:227-254` (`execTool`) — the line `if tool.Effect != EffectSafe { audit "proposed"; return ...proposed=true }` is the degenerate gate. US4 swaps it for `gate(tool.Effect, ag.AutonomyMode)`.
- **Tool registry / effect classes:** `internal/agents/tools.go` (`EffectClass`, `Tool`, `NewToolRegistry`).
- **Run store / status constants / RLS store pattern:** `internal/agents/agent_run.go`.
- **Outbox + bus (reuse, do NOT rebuild):** `internal/platform/events/outbox.go` (`Enqueue`, `Worker`), `internal/platform/events/bus.go` (`Bus.Subscribe`, `Handler`, topic consts). Handlers run inside the worker tx with **no principal context** — the notify subscriber (`internal/platform/notify/sender_subscriber.go`) is the template for doing principal-scoped work via its own `WithPrincipal` txs + dedup on a durable marker.
- **ticketing reuse:** `internal/ticketing/service.go` `Reply` (line 231) and `Triage`; the approval executor invokes these **through the existing tool registry**, not re-implemented.
- **Migration template:** `migrations/0028_agent_run.up.sql` / `.down.sql`; permission template `migrations/0029_agent_runtime_role.up.sql`; bare schema mirror `db/schema.sql` (agent section ~lines 287-349).
- **HTTP + perm wiring:** `internal/agents/agent_run_handler.go` (handler shape), `cmd/manyforge/main.go` (`apiHandlers` struct ~402-444, population ~320-341, route mount ~526-529, outbox/bus wiring ~190-192/271-272/350-352, Engine wiring ~142-158), `internal/platform/httpx` (`RequirePermission`, `WriteError`, `DecodeJSON`).
- **Drift:** `cmd/manyforge/drift_003_test.go` (`inScope003Ops`, `is003Op`), contract `specs/003-agent-runtime/contracts/openapi.yaml`.
- **Pins:** `internal/security_regression/agent_run_us3_pins_test.go` (the `TestPin_FailClosedExecutor` pin greps `tool.Effect != EffectSafe` — **this plan rewrites that pin**).

**Branch:** continue on `003-agent-runtime` (US1–US3 live here; no worktree). **bd issue:** `manyforge-6cb` (claim it: `bd update manyforge-6cb --claim`). Commits: **no `Co-Authored-By`** trailer (project rule).

---

## Key design decisions (locked; flagged for review)

1. **EffectClass split.** `EffectSafe` (currently = reads **and** reversible writes) splits into `EffectRead` (pure reads) + `EffectReversible` (reversible internal writes: status/priority/tags/assignee). Required because Mode 2 = "queue every **write**, reads still inline" cannot be expressed while reads and writes share one class. `EffectExternal`/`EffectIrreversible` unchanged. The US3 pin `tool.Effect != EffectSafe` is rewritten to the new gate contract.

2. **Three-mode matrix** (`agent.autonomy_mode`, default 1):

   | effect \ mode | 1 (assist) | 2 (queue-writes) | 3 (autonomous) |
   |---|---|---|---|
   | `EffectRead` | exec | exec | exec |
   | `EffectReversible` | exec | **approval** | exec |
   | `EffectExternal` | approval | approval | exec |
   | `EffectIrreversible` | approval | approval | exec |
   | *unknown effect / unknown mode* | **approval** | **approval** | **approval** |

   **Fail-closed:** an unclassified effect class OR an out-of-range mode ⇒ approval, in every branch. (Mode 3 auto-runs Irreversible per design §3.3 "all → exec inline (tenant scope)"; there are **no** Irreversible tools in the registry yet, so this is forward-looking. Noted for reviewer.)

3. **Exactly-once approval execution = state-machine guard + reply dedup key.** approve = `state→approved` + outbox event (one tx). The `ApprovalExecutor` (outbox subscriber) executes the approved tool **via the existing tool registry** as the agent principal in its own `WithPrincipal` tx, then flips `state→executed`. Idempotency: (a) a pre-check skips non-`approved` items; (b) the `draft_reply` tool, when executing an approval, carries the `approval_item.id` as an idempotency key → `ticket_message.source_approval_item_id` is `UNIQUE`, so a redelivered event inserts **zero** second messages (no second email). `Triage` is naturally idempotent (absolute SET). This mirrors the notify subscriber (durable-marker dedup) rather than threading the agent principal through the shared worker tx.
   *Alternative considered:* run the tool inside the worker savepoint (tx-threaded `ReplyTx`/`TriageTx`) for single-tx atomicity — rejected: it forces fragile per-event `manyforge.principal_id` GUC juggling on the shared multi-event worker tx and a deeper ticketing refactor. The dedup-key approach matches CLAUDE.md's "single-use token = jti + consumed-set".

4. **`agents.approve` is human-only.** Granted to `admin` (owner via the locked-owner short-circuit); **never** to the `agent_runtime` preset role — an agent holding `agents.approve` could self-approve its gated actions and collapse the gate. New security pin enforces this.

5. **Approvals queue is business-scoped & flat:** `GET/POST /businesses/{id}/approvals…` (a human works one queue), not nested under a run. `drift_003_test.go:is003Op` is widened to also match `/approvals`.

6. **expires_at = created + 7 days; a 60s sweep** marks stale `pending` items `expired` (resolves design §8). approve/deny of an already-expired/decided item ⇒ 409 (no execution).

---

## File map

**Create:**
- `migrations/0030_approval_item.up.sql` / `.down.sql` — `approval_item` table (RLS) + `ticket_message.source_approval_item_id` dedup column.
- `migrations/0031_agents_approve_perm.up.sql` / `.down.sql` — `agents.approve` permission (admin only, NOT agent_runtime).
- `db/query/approval_item.sql` — sqlc queries (create/get/list/decide/mark-executed/expire).
- `internal/agents/gate.go` + `gate_test.go` — the pure gate function + autonomy-mode consts.
- `internal/agents/approval.go` + `approval_test.go` — domain type + `ApprovalStore` (RLS) + `ApprovalService`.
- `internal/agents/approval_handler.go` + `approval_handler_test.go` — list/approve/deny HTTP.
- `internal/agents/approval_executor.go` + `approval_executor_test.go` — outbox subscriber.
- `internal/agents/approval_integration_test.go` — RLS cross-tenant + idempotent-replay (testcontainers).
- `internal/security_regression/agent_run_us4_pins_test.go` — US4 source pins.

**Modify:**
- `internal/agents/tools.go` — split `EffectClass`; reclassify tools; `draft_reply` honors a ctx idempotency key.
- `internal/agents/runner.go` — `execTool` consumes `gate(...)`; threads `autonomy_mode` + writes `approval_item`.
- `internal/agents/runner_test.go` — Mode-matrix behavioral tests; fix `TestRun_NonSafeToolProposedOnly`.
- `internal/agents/tools_test.go` — effect-class assertions.
- `internal/ticketing/service.go` — `ReplyInput.IdempotencyKey`; `Reply` dedups via the new column.
- `db/query/ticketing*.sql` (the file holding `InsertOutboundMessage`) — dedup-aware insert.
- `db/schema.sql` — add bare `approval_item`; add `source_approval_item_id` to `ticket_message`.
- `cmd/manyforge/main.go` — wire ApprovalService/Handler/Executor + `agentsApprove` middleware + `Subscribe` + sweep goroutine.
- `specs/003-agent-runtime/contracts/openapi.yaml` — 3 approval ops + schemas.
- `cmd/manyforge/drift_003_test.go` — extend `inScope003Ops` + `is003Op`.
- `internal/security_regression/agent_run_us3_pins_test.go` — rewrite `TestPin_FailClosedExecutor` to the new gate contract.

---

## ⚠️ Execution ordering (green build at every commit)

The plan's tasks are written as discrete units, but **Tasks 1, 2, 4, and the US3-pin rewrite (part of Task 14) MUST land as one commit** — Task 1 removes `EffectSafe`, which `runner.go:243` and `TestPin_FailClosedExecutor` still reference, so committing Task 1 alone breaks the build (violates the project's no-broken-commits rule). Treat them as a single "gate refactor" task: split `EffectClass` + reclassify tools + add `gate.go` + wire `execTool` + add the `approvalWriter` interface & fake + update `runner_test.go` Mode tests + rewrite the fail-closed pin, then `make test && make sec-test` green, commit once. The remaining tasks (3/5/10 migrations+sqlc, 6 store, 7 ticketing, 8 executor, 9 handlers, 11 wiring, 12 openapi/drift, rest of 14 pins, 13 integration, 15 gate) each already produce a green build on their own and commit independently. **Migrations+sqlc (3/5/10) can land before the store (6) because the generated `dbgen` methods are simply unused until then** — `InsertOutboundMessageParams` gaining a nil-defaulted `*uuid.UUID` field keeps `ticketing.Reply` compiling and behaviorally identical.

---

## Task 1: Split `EffectClass` (Read vs Reversible) + reclassify tools

**Files:**
- Modify: `internal/agents/tools.go:16-25` (enum) and the `Effect:` field of each `add(Tool{…})`.
- Test: `internal/agents/tools_test.go`

- [ ] **Step 1: Write the failing test** — append to `internal/agents/tools_test.go`:

```go
func TestEffectClasses(t *testing.T) {
	reg := NewToolRegistry(&fakeTicketSvc{})
	want := map[string]EffectClass{
		"read_ticket":  EffectRead,
		"read_thread":  EffectRead,
		"set_status":   EffectReversible,
		"set_priority": EffectReversible,
		"set_tags":     EffectReversible,
		"set_assignee": EffectReversible,
		"draft_reply":  EffectExternal,
	}
	for name, eff := range want {
		tl, ok := reg.Get(name)
		if !ok {
			t.Fatalf("tool %q missing from registry", name)
		}
		if tl.Effect != eff {
			t.Errorf("%s effect = %d, want %d", name, tl.Effect, eff)
		}
	}
}
```

- [ ] **Step 2: Run it — expect FAIL** (`EffectRead`/`EffectReversible` undefined):

Run: `go test ./internal/agents/ -run TestEffectClasses`
Expected: compile error `undefined: EffectRead`.

- [ ] **Step 3: Edit the enum** in `internal/agents/tools.go` (replace lines 16-25):

```go
// EffectClass is each tool's static side-effect classification (design §2.4). The
// run loop's gate (gate.go) combines this with the agent's autonomy mode to decide
// auto-exec vs. approval. Ordered low→high risk; an UNKNOWN class is fail-closed to
// approval by the gate. Splitting Read from Reversible lets Mode 2 ("queue every
// write, reads inline") be expressed.
type EffectClass int

const (
	EffectRead         EffectClass = iota // pure reads — never mutate (read_ticket/read_thread)
	EffectReversible                      // reversible internal writes (status/priority/tags/assignee)
	EffectExternal                        // leaves the tenant boundary (send email)
	EffectIrreversible                    // destructive (delete/merge/billing)
)
```

- [ ] **Step 4: Reclassify the tools** in `internal/agents/tools.go` — change the `Effect:` on each:
  - `read_ticket` and `read_thread`: `Effect: EffectSafe` → `Effect: EffectRead`.
  - `set_status`, `set_priority`, `set_tags`, `set_assignee`: `Effect: EffectSafe` → `Effect: EffectReversible`.
  - `draft_reply`: stays `Effect: EffectExternal`.

- [ ] **Step 5: Run the tool tests — expect PASS:**

Run: `go test ./internal/agents/ -run 'TestEffectClasses|TestTool'`
Expected: PASS. (`runner_test.go` will not compile yet if it references `EffectSafe` — it does not; the runner does, fixed in Task 4.)

- [ ] **Step 6: Commit:**

```bash
git add internal/agents/tools.go internal/agents/tools_test.go
git commit -m "refactor(agents): split EffectClass into Read+Reversible for the US4 mode matrix"
```

---

## Task 2: The autonomy gate (pure function)

**Files:**
- Create: `internal/agents/gate.go`
- Test: `internal/agents/gate_test.go`

- [ ] **Step 1: Write the failing test** — `internal/agents/gate_test.go`:

```go
package agents

import "testing"

func TestGateMatrix(t *testing.T) {
	cases := []struct {
		effect EffectClass
		mode   int
		want   autonomyDecision
	}{
		// reads: always inline
		{EffectRead, ModeAssist, decideExec},
		{EffectRead, ModeQueueWrites, decideExec},
		{EffectRead, ModeAutonomous, decideExec},
		// reversible writes: inline in 1 & 3, queued in 2
		{EffectReversible, ModeAssist, decideExec},
		{EffectReversible, ModeQueueWrites, decideApproval},
		{EffectReversible, ModeAutonomous, decideExec},
		// external: queued in 1 & 2, inline in 3
		{EffectExternal, ModeAssist, decideApproval},
		{EffectExternal, ModeQueueWrites, decideApproval},
		{EffectExternal, ModeAutonomous, decideExec},
		// irreversible: queued in 1 & 2, inline in 3
		{EffectIrreversible, ModeAssist, decideApproval},
		{EffectIrreversible, ModeQueueWrites, decideApproval},
		{EffectIrreversible, ModeAutonomous, decideExec},
		// fail-closed: unknown effect ⇒ approval in every mode
		{EffectClass(99), ModeAssist, decideApproval},
		{EffectClass(99), ModeAutonomous, decideApproval},
		// fail-closed: unknown mode ⇒ approval even for a reversible write
		{EffectReversible, 0, decideApproval},
		{EffectReversible, 7, decideApproval},
		{EffectRead, 0, decideExec}, // reads still inline regardless of mode
	}
	for _, c := range cases {
		if got := gate(c.effect, c.mode); got != c.want {
			t.Errorf("gate(%d,%d) = %d, want %d", c.effect, c.mode, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run it — expect FAIL** (undefined symbols):

Run: `go test ./internal/agents/ -run TestGateMatrix`
Expected: compile error `undefined: autonomyDecision`.

- [ ] **Step 3: Create `internal/agents/gate.go`:**

```go
package agents

// Autonomy modes (agent.autonomy_mode; design §6). Default is ModeAssist.
const (
	ModeAssist      = 1 // auto-apply reversible internal writes; queue external/irreversible
	ModeQueueWrites = 2 // queue EVERY write; reads still run inline
	ModeAutonomous  = 3 // auto-run everything within tenant scope
)

// autonomyDecision is the gate's verdict for one already-RBAC-allowed tool call.
type autonomyDecision int

const (
	decideExec     autonomyDecision = iota // execute inline now
	decideApproval                         // record a pending approval_item; do NOT execute
)

// gate combines a tool's static effect class with the agent's autonomy mode to decide
// whether the call runs inline or is queued for human approval. It runs strictly AFTER
// RBAC (the caller has already confirmed the agent holds the tool's permission) and
// BEFORE any execution. It is deterministic and FAIL-CLOSED: an unknown/unclassified
// effect class OR an out-of-range mode defaults to approval. No LLM input influences it.
func gate(effect EffectClass, mode int) autonomyDecision {
	switch effect {
	case EffectRead:
		return decideExec // reads never mutate — always safe to run inline
	case EffectReversible:
		// Reversible internal writes auto-apply in assist/autonomous; mode 2 queues them.
		if mode == ModeAssist || mode == ModeAutonomous {
			return decideExec
		}
		return decideApproval // ModeQueueWrites, or any unknown mode → fail-closed
	case EffectExternal, EffectIrreversible:
		if mode == ModeAutonomous {
			return decideExec // fully autonomous within tenant scope
		}
		return decideApproval // assist/queue-writes, or unknown mode → fail-closed
	default:
		// FAIL-CLOSED: unknown/unclassified effect ⇒ approval (never auto-execute).
		return decideApproval
	}
}
```

- [ ] **Step 4: Run it — expect PASS:**

Run: `go test ./internal/agents/ -run TestGateMatrix -v`
Expected: PASS.

- [ ] **Step 5: Commit:**

```bash
git add internal/agents/gate.go internal/agents/gate_test.go
git commit -m "feat(agents): add fail-closed autonomy gate (effect-class × mode matrix)"
```

---

## Task 3: `approval_item` migration + dedup column + schema.sql

**Files:**
- Create: `migrations/0030_approval_item.up.sql`, `migrations/0030_approval_item.down.sql`
- Modify: `db/schema.sql` (agent section + `ticket_message`)

- [ ] **Step 1: Create `migrations/0030_approval_item.up.sql`:**

```sql
-- 0030: per-action approval queue (Spec 003 US4). One row per gated tool-call the
-- autonomy gate defers. RLS-scoped to the owning business, mirroring agent_run (0028).
-- tenant_root_id is derived from the parent agent_run at insert and immutable. state is
-- CHECK-constrained text. effect_class mirrors agents.EffectClass (0=read…3=irreversible).
-- Also adds a dedup key to ticket_message so an approved reply executes exactly once even
-- under outbox at-least-once redelivery (the approval_item.id is the single-use token).

CREATE TABLE approval_item (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_run_id            uuid NOT NULL,
    business_id             uuid NOT NULL,
    tenant_root_id          uuid NOT NULL,
    tool                    text NOT NULL,
    args                    jsonb NOT NULL,
    effect_class            smallint NOT NULL CHECK (effect_class >= 0),
    state                   text NOT NULL DEFAULT 'pending'
                                CHECK (state IN ('pending', 'approved', 'denied', 'executed', 'expired')),
    decided_by_principal_id uuid,
    decided_at              timestamptz,
    executed_at             timestamptz,
    expires_at              timestamptz NOT NULL,
    error                   text,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (agent_run_id, tenant_root_id) REFERENCES agent_run (id, tenant_root_id)
);
CREATE INDEX approval_item_queue_idx ON approval_item (business_id, state, created_at);
CREATE INDEX approval_item_run_idx ON approval_item (agent_run_id, tenant_root_id);

CREATE TRIGGER approval_item_troot_immutable
    BEFORE UPDATE ON approval_item
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();

GRANT SELECT, INSERT, UPDATE, DELETE ON approval_item TO manyforge_app;

ALTER TABLE approval_item ENABLE ROW LEVEL SECURITY;
CREATE POLICY approval_item_rls ON approval_item FOR ALL
    USING (business_id IN (SELECT business_id FROM authorized_businesses(current_principal())))
    WITH CHECK (true);

-- Idempotency key for approval-driven replies: at most one outbound message per approval.
-- NULL for ordinary human replies (NULLs never conflict), so existing behavior is unchanged.
ALTER TABLE ticket_message ADD COLUMN source_approval_item_id uuid;
CREATE UNIQUE INDEX ticket_message_source_approval_idx
    ON ticket_message (source_approval_item_id)
    WHERE source_approval_item_id IS NOT NULL;
```

- [ ] **Step 2: Create `migrations/0030_approval_item.down.sql`:**

```sql
-- Reverse 0030_approval_item.
DROP INDEX IF EXISTS ticket_message_source_approval_idx;
ALTER TABLE ticket_message DROP COLUMN IF EXISTS source_approval_item_id;

DROP POLICY IF EXISTS approval_item_rls ON approval_item;
ALTER TABLE approval_item DISABLE ROW LEVEL SECURITY;
REVOKE SELECT, INSERT, UPDATE, DELETE ON approval_item FROM manyforge_app;
DROP TRIGGER IF EXISTS approval_item_troot_immutable ON approval_item;
DROP TABLE IF EXISTS approval_item;
```

- [ ] **Step 3: Update `db/schema.sql`** — (a) add the bare `approval_item` table right after the `agent_run` table block (the agent section, ~line 349); (b) add `source_approval_item_id uuid` to the `ticket_message` block. Bare form (no triggers/RLS/grants — sqlc only):

```sql
CREATE TABLE approval_item (
    id                      uuid PRIMARY KEY,
    agent_run_id            uuid NOT NULL,
    business_id             uuid NOT NULL,
    tenant_root_id          uuid NOT NULL,
    tool                    text NOT NULL,
    args                    jsonb NOT NULL,
    effect_class            smallint NOT NULL,
    state                   text NOT NULL,
    decided_by_principal_id uuid,
    decided_at              timestamptz,
    executed_at             timestamptz,
    expires_at              timestamptz NOT NULL,
    error                   text,
    created_at              timestamptz NOT NULL,
    updated_at              timestamptz NOT NULL,
    UNIQUE (id, tenant_root_id),
    FOREIGN KEY (business_id, tenant_root_id) REFERENCES business (id, tenant_root_id),
    FOREIGN KEY (agent_run_id, tenant_root_id) REFERENCES agent_run (id, tenant_root_id)
);
```

In the `ticket_message` `CREATE TABLE` in `db/schema.sql`, add this line before the `UNIQUE (...)` constraints:

```sql
    source_approval_item_id uuid,
```

- [ ] **Step 4: Verify the migration applies** (testcontainers up/down via the integration harness, fastest check is `make int-test` later; for now lint the SQL by regenerating sqlc in Task 5). Commit:

```bash
git add migrations/0030_approval_item.up.sql migrations/0030_approval_item.down.sql db/schema.sql
git commit -m "feat(agents): approval_item RLS table (mig 0030) + ticket_message dedup key"
```

---

## Task 4: Wire the gate into `execTool` (replace the degenerate rule)

**Files:**
- Modify: `internal/agents/runner.go` — `Engine` (add `Approvals` collaborator), `run` (pass `ag.AutonomyMode`), `execTool` (consume `gate`, write `approval_item`).
- Modify: `internal/agents/runner_test.go` — fix `TestRun_NonSafeToolProposedOnly`, add Mode-matrix tests.

> The `ApprovalStore.CreatePending` method is defined in Task 6; this task introduces the `approvalWriter` interface the engine depends on and a fake for unit tests. Do Task 6's store after, or interleave — the interface is the contract.

- [ ] **Step 1: Add the collaborator interface + Engine field.** In `internal/agents/runner.go`, after the `providerFactory` type (line 70) add:

```go
// approvalWriter persists a pending approval_item for a gated tool-call (US4). The
// engine calls it under the agent principal's context; the row is RLS-scoped to the
// run's business and ties back to the run via agentRunID.
type approvalWriter interface {
	CreatePending(ctx context.Context, principalID, businessID, agentRunID uuid.UUID, tool string, args json.RawMessage, effect int) (uuid.UUID, error)
}
```

Add a field to `Engine` (the struct at line 73):

```go
	Approvals approvalWriter
```

- [ ] **Step 2: Thread the mode into `execTool`.** In `run` (the tool-call loop ~line 216-220), change the `execTool` call to pass the agent's mode:

```go
		for _, call := range resp.ToolCalls {
			content, isErr, prop := e.execTool(loopCtx, agentPrincipalID, businessID, ag.AutonomyMode, allow, run, call)
			proposed = proposed || prop
			results = append(results, ai.ToolResult{CallID: call.ID, Content: content, IsError: isErr})
		}
```

- [ ] **Step 3: Replace `execTool`** (lines 227-254) with the gated version:

```go
// execTool runs the US4 fail-closed gate for one tool call. Order: allowlist → RBAC
// (Resolver.Has) → gate(effect, mode) → execute-or-queue. Returns the tool-result
// content, whether it is an error result, and whether it was queued for approval
// (which drives the run's terminal awaiting_approval status).
func (e *Engine) execTool(ctx context.Context, principalID, businessID uuid.UUID, mode int, allow map[string]bool, run AgentRun, call ai.ToolCall) (string, bool, bool) {
	audit := func(decision, detail string, inputs any) {
		_ = e.Auditor.Run(ctx, principalID, run, "agent.tool."+decision, inputs, map[string]any{"tool": call.Name, "detail": detail}, decision)
	}
	tool, ok := e.Tools.Get(call.Name)
	if !ok || !allow[call.Name] {
		audit("denied", "tool not allowed", map[string]any{"tool": call.Name})
		return "tool not permitted", true, false
	}
	// RBAC FIRST: the agent must hold the tool's permission (same authz as a human).
	if tool.RequiredPerm != "" {
		has, err := e.Resolver.Has(ctx, principalID, businessID, tool.RequiredPerm)
		if err != nil || !has {
			audit("denied", "missing permission "+tool.RequiredPerm, map[string]any{"tool": call.Name})
			return "permission denied", true, false
		}
	}
	// GATE SECOND (after RBAC, before exec): decide auto-exec vs. queue for approval.
	if gate(tool.Effect, mode) == decideApproval {
		apID, err := e.Approvals.CreatePending(ctx, principalID, businessID, run.ID, call.Name, json.RawMessage(call.Args), int(tool.Effect))
		if err != nil {
			audit("error", safeMsg(err), map[string]any{"tool": call.Name, "args": json.RawMessage(call.Args)})
			return "tool error: " + safeMsg(err), true, false
		}
		audit("proposed", "queued for human approval", map[string]any{"tool": call.Name, "args": json.RawMessage(call.Args), "approval_item_id": apID})
		return "action queued for approval (id " + apID.String() + ")", false, true
	}
	out, err := tool.Invoke(ctx, principalID, businessID, call.Args)
	if err != nil {
		audit("error", safeMsg(err), map[string]any{"tool": call.Name, "args": json.RawMessage(call.Args)})
		return "tool error: " + safeMsg(err), true, false
	}
	audit("executed", out, map[string]any{"tool": call.Name, "args": json.RawMessage(call.Args)})
	return out, false, false
}
```

- [ ] **Step 4: Add a fake approval writer + Mode tests.** In `internal/agents/runner_test.go`, add a fake near the other fakes (after `fakeAuditor`, ~line 49):

```go
type fakeApprovals struct {
	created []string // "tool:effect"
	ids     []uuid.UUID
}

func (f *fakeApprovals) CreatePending(_ context.Context, _, _, _ uuid.UUID, tool string, _ json.RawMessage, effect int) (uuid.UUID, error) {
	id := uuid.New()
	f.created = append(f.created, fmt.Sprintf("%s:%d", tool, effect))
	f.ids = append(f.ids, id)
	return id, nil
}
```

Add `"fmt"` to that file's imports. Then wire the fake into `newTestEngine` by giving the `Engine` an `Approvals` field. Change `newTestEngine` to also return the fake and set the field:

```go
func newTestEngine(prov ai.Provider, store runStore, perms map[string]bool, reg *ToolRegistry) (*Engine, *fakeAuditor, *fakeApprovals) {
	aud := &fakeAuditor{}
	ap := &fakeApprovals{}
	eng := &Engine{
		Runs:      store,
		Tools:     reg,
		Auditor:   aud,
		Resolver:  fakeResolver{perms: perms},
		Approvals: ap,
		NewProvider: func(_ context.Context, _, _ uuid.UUID, _ string) (ai.Provider, string, error) {
			return prov, "claude-sonnet-4-5", nil
		},
		Cost:   func(_ string, u ai.Usage) int64 { return int64(u.Total()) },
		Limits: RunLimits{MaxIterations: 4, MaxTokensPerRun: 1000, MaxOutputTokens: 256, WallClock: defaultWallClock},
	}
	return eng, aud, ap
}
```

Update every `newTestEngine(...)` call site in the file (they currently bind two values) to bind three — e.g. `eng, _, _ := newTestEngine(...)`, `eng, aud, _ := newTestEngine(...)`, and the `errResolver` test's literal `Engine{...}` (line 294) must also set `Approvals: &fakeApprovals{}`.

`loadedAgent` defaults `AutonomyMode: 0`; set it to assist so reversible writes run inline by default. Edit `loadedAgent`:

```go
func loadedAgent(tools ...string) Agent {
	return Agent{ID: uuid.New(), BusinessID: uuid.New(), PrincipalID: uuid.New(), Provider: "anthropic", Model: "claude-sonnet-4-5", SystemPrompt: "be helpful", AllowedTools: tools, AutonomyMode: ModeAssist, Enabled: true, MonthlyBudgetCents: 0}
}
```

- [ ] **Step 5: Rewrite `TestRun_NonSafeToolProposedOnly`** (it asserts US3 propose-only; under US4 a Mode-1 external tool now writes an approval_item and ends awaiting_approval). Replace it:

```go
func TestRun_Mode1ExternalQueuesApproval(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "draft_reply", `{"ticket_id":"`+tid.String()+`","body_text":"hi"}`),
		finalText("ok"),
	)
	fts := &fakeTicketSvc{}
	store := &fakeRunStore{}
	eng, aud, ap := newTestEngine(prov, store, map[string]bool{"tickets.reply": true}, NewToolRegistry(fts))
	run, _ := eng.run(context.Background(), uuid.New(), loadedAgent("draft_reply"), "manual", nil, nil)
	if fts.gotTicket != (uuid.UUID{}) {
		t.Fatal("Mode-1 external tool must be queued (no execution)")
	}
	if len(ap.created) != 1 || ap.created[0] != "draft_reply:2" { // 2 == EffectExternal
		t.Fatalf("expected one queued draft_reply approval; got %v", ap.created)
	}
	if run.Status != RunAwaitingApproval {
		t.Fatalf("status=%s want awaiting_approval", run.Status)
	}
	if !containsDecision(aud.actions, "proposed") {
		t.Fatalf("queued action must be audited; actions=%v", aud.actions)
	}
}

func TestRun_Mode2QueuesReversibleWrite(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "set_status", `{"ticket_id":"`+tid.String()+`","status":"open"}`),
		finalText("done"),
	)
	fts := &fakeTicketSvc{}
	eng, _, ap := newTestEngine(prov, &fakeRunStore{}, map[string]bool{"tickets.write": true}, NewToolRegistry(fts))
	ag := loadedAgent("set_status")
	ag.AutonomyMode = ModeQueueWrites
	run, _ := eng.run(context.Background(), uuid.New(), ag, "manual", nil, nil)
	if fts.triageIn.Status != nil {
		t.Fatal("Mode-2 must queue reversible writes, not execute them")
	}
	if len(ap.created) != 1 || ap.created[0] != "set_status:1" { // 1 == EffectReversible
		t.Fatalf("expected one queued set_status approval; got %v", ap.created)
	}
	if run.Status != RunAwaitingApproval {
		t.Fatalf("status=%s want awaiting_approval", run.Status)
	}
}

func TestRun_Mode3AutoRunsExternal(t *testing.T) {
	tid := uuid.New()
	prov := ai.NewMockProvider(
		toolUse("c1", "draft_reply", `{"ticket_id":"`+tid.String()+`","body_text":"hi"}`),
		finalText("ok"),
	)
	fts := &fakeTicketSvc{}
	eng, _, ap := newTestEngine(prov, &fakeRunStore{}, map[string]bool{"tickets.reply": true}, NewToolRegistry(fts))
	ag := loadedAgent("draft_reply")
	ag.AutonomyMode = ModeAutonomous
	run, _ := eng.run(context.Background(), uuid.New(), ag, "manual", nil, nil)
	if fts.gotTicket != tid {
		t.Fatal("Mode-3 must auto-run external tools inline")
	}
	if len(ap.created) != 0 {
		t.Fatalf("Mode-3 must not queue; got %v", ap.created)
	}
	if run.Status != RunSucceeded {
		t.Fatalf("status=%s want succeeded", run.Status)
	}
}
```

(Confirm `fakeTicketSvc.gotTicket` is set by the reply path; if the reply fake only records on `Reply`, ensure `gotTicket` is assigned there. Check `internal/agents/tools_test.go` for the `fakeTicketSvc` definition and adjust if `gotTicket` isn't wired for `Reply`.)

- [ ] **Step 6: Run the agents unit tests — expect PASS:**

Run: `go test ./internal/agents/ -run TestRun`
Expected: PASS (all Mode tests green; old propose-only test gone).

- [ ] **Step 7: Commit:**

```bash
git add internal/agents/runner.go internal/agents/runner_test.go
git commit -m "feat(agents): wire autonomy gate into execTool (queue gated calls as approval_items)"
```

---

## Task 5: sqlc queries for `approval_item` + reply dedup query

**Files:**
- Create: `db/query/approval_item.sql`
- Modify: the query file holding `InsertOutboundMessage` (find with `grep -rl 'name: InsertOutboundMessage' db/query`)
- Run: `make generate`

- [ ] **Step 1: Create `db/query/approval_item.sql`:**

```sql
-- name: CreateApprovalItem :one
-- Insert a pending item, deriving tenant_root_id from the (RLS-visible) parent run.
-- An invisible/foreign run yields no row -> pgx.ErrNoRows -> no-oracle not-found.
INSERT INTO approval_item (id, agent_run_id, business_id, tenant_root_id, tool, args, effect_class, state, expires_at)
SELECT sqlc.arg('id')::uuid, ar.id, ar.business_id, ar.tenant_root_id,
       sqlc.arg('tool')::text, sqlc.arg('args')::jsonb, sqlc.arg('effect_class')::smallint,
       'pending', now() + sqlc.arg('ttl')::interval
FROM agent_run ar
WHERE ar.id = sqlc.arg('agent_run_id')::uuid AND ar.business_id = sqlc.arg('business_id')::uuid
RETURNING *;

-- name: GetApprovalItem :one
-- Business-scoped read (RLS + predicate). Unknown/foreign id -> pgx.ErrNoRows -> 404.
SELECT * FROM approval_item WHERE id = $1 AND business_id = $2;

-- name: ListPendingApprovals :many
SELECT * FROM approval_item
WHERE business_id = $1 AND state = sqlc.arg('state')::text
ORDER BY created_at DESC LIMIT $2;

-- name: DecideApprovalItem :one
-- Transition pending -> approved|denied iff still pending AND not past expiry. A row
-- that is already decided/expired returns no row (caller maps to 409 conflict).
UPDATE approval_item
SET state = sqlc.arg('state')::text,
    decided_by_principal_id = sqlc.arg('decided_by')::uuid,
    decided_at = now(),
    updated_at = now()
WHERE id = sqlc.arg('id')::uuid AND business_id = sqlc.arg('business_id')::uuid
  AND state = 'pending' AND expires_at > now()
RETURNING *;

-- name: MarkApprovalExecuted :one
-- Idempotency claim: flip approved -> executed iff still approved. Zero rows means a
-- prior delivery already executed it (or it was denied) -> the executor skips.
UPDATE approval_item
SET state = 'executed', executed_at = now(), updated_at = now()
WHERE id = sqlc.arg('id')::uuid AND business_id = sqlc.arg('business_id')::uuid AND state = 'approved'
RETURNING *;

-- name: ExpireStaleApprovals :execrows
-- Sweep: mark every past-expiry pending item expired. Returns the count swept.
UPDATE approval_item SET state = 'expired', updated_at = now()
WHERE state = 'pending' AND expires_at <= now();
```

- [ ] **Step 2: Make `InsertOutboundMessage` dedup-aware.** In the query file holding it, change the insert to set the new column and skip on conflict, then add a sibling lookup. Replace the `InsertOutboundMessage` body so the column list includes `source_approval_item_id`, the values include `sqlc.narg('source_approval_item_id')`, and append `ON CONFLICT (source_approval_item_id) WHERE source_approval_item_id IS NOT NULL DO NOTHING` before `RETURNING *`. Then add:

```sql
-- name: GetOutboundMessageByApproval :one
-- Look up the (single) message a given approval produced, for the dedup short-circuit.
SELECT * FROM ticket_message
WHERE source_approval_item_id = $1 AND business_id = $2;
```

(Exact column list: open the file first; mirror the existing param order and add the one nullable column. `narg` → `*uuid.UUID` in Go.)

- [ ] **Step 3: Regenerate:**

Run: `make generate`
Expected: clean; new `dbgen` methods `CreateApprovalItem`, `GetApprovalItem`, `ListPendingApprovals`, `DecideApprovalItem`, `MarkApprovalExecuted`, `ExpireStaleApprovals`, `GetOutboundMessageByApproval`; `InsertOutboundMessageParams` gains `SourceApprovalItemID *uuid.UUID`.

> **Gotcha (HANDOFF):** gopls/IDE will scream `undefined: dbgen.X` on the new files — STALE. Trust only `make generate` + `go build`.

- [ ] **Step 4: Build:**

Run: `go build ./...`
Expected: the agents/ticketing packages may not yet compile if they reference not-yet-written methods — that's fine; the **dbgen** package must build. Confirm `go build ./internal/platform/db/dbgen/`.

- [ ] **Step 5: Commit:**

```bash
git add db/query/ internal/platform/db/dbgen/
git commit -m "feat(agents): sqlc queries for approval_item + dedup-aware outbound insert"
```

---

## Task 6: `ApprovalStore` + `ApprovalService` (domain + RLS persistence)

**Files:**
- Create: `internal/agents/approval.go`, `internal/agents/approval_test.go`

- [ ] **Step 1: Write `internal/agents/approval.go`:**

```go
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// Approval-item states (mirror the approval_item CHECK constraint).
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalDenied   = "denied"
	ApprovalExecuted = "executed"
	ApprovalExpired  = "expired"
)

// defaultApprovalTTL is how long a pending item stays actionable before the sweep
// expires it (design §8, resolved in US4).
const defaultApprovalTTL = 7 * 24 * time.Hour

// ApprovalItem is the domain view of an approval_item row.
type ApprovalItem struct {
	ID                   uuid.UUID
	AgentRunID           uuid.UUID
	BusinessID           uuid.UUID
	TenantRootID         uuid.UUID
	Tool                 string
	Args                 json.RawMessage
	EffectClass          int
	State                string
	DecidedByPrincipalID *uuid.UUID
	ExecutedAt           *time.Time
	ExpiresAt            time.Time
	Error                *string
}

type approvalDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
}

// ApprovalStore persists approval_item rows over the RLS DB.
type ApprovalStore struct {
	DB  approvalDB
	TTL time.Duration // 0 ⇒ defaultApprovalTTL
}

func (s *ApprovalStore) ttl() time.Duration {
	if s.TTL <= 0 {
		return defaultApprovalTTL
	}
	return s.TTL
}

// CreatePending inserts a pending item for a gated call (called by the engine under the
// agent principal). Implements approvalWriter.
func (s *ApprovalStore) CreatePending(ctx context.Context, principalID, businessID, agentRunID uuid.UUID, tool string, args json.RawMessage, effect int) (uuid.UUID, error) {
	id := uuid.New()
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		_, e := dbgen.New(tx).CreateApprovalItem(ctx, dbgen.CreateApprovalItemParams{
			ID: id, AgentRunID: agentRunID, BusinessID: businessID,
			Tool: tool, Args: []byte(args), EffectClass: int16(effect),
			Ttl: pgInterval(s.ttl()),
		})
		return e
	})
	if err != nil {
		return uuid.Nil, mapAgentRunErr(err)
	}
	return id, nil
}

// Get reads one item (business-scoped, no oracle).
func (s *ApprovalStore) Get(ctx context.Context, principalID, businessID, id uuid.UUID) (ApprovalItem, error) {
	var row dbgen.ApprovalItem
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, e := dbgen.New(tx).GetApprovalItem(ctx, dbgen.GetApprovalItemParams{ID: id, BusinessID: businessID})
		row = r
		return e
	})
	if err != nil {
		return ApprovalItem{}, mapAgentRunErr(err)
	}
	return toApprovalItem(row), nil
}

// ListPending returns the business's pending queue (most recent first).
func (s *ApprovalStore) ListPending(ctx context.Context, principalID, businessID uuid.UUID, limit int) ([]ApprovalItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows []dbgen.ApprovalItem
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, e := dbgen.New(tx).ListPendingApprovals(ctx, dbgen.ListPendingApprovalsParams{BusinessID: businessID, State: ApprovalPending, Limit: int32(limit)})
		rows = r
		return e
	})
	if err != nil {
		return nil, mapAgentRunErr(err)
	}
	out := make([]ApprovalItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, toApprovalItem(r))
	}
	return out, nil
}

// Decide transitions a pending item to approved/denied (caller = the deciding human).
// A non-pending / expired item yields pgx.ErrNoRows → ErrConflict (409).
func (s *ApprovalStore) Decide(ctx context.Context, principalID, businessID, id uuid.UUID, decidedBy uuid.UUID, approve bool) (ApprovalItem, error) {
	state := ApprovalDenied
	if approve {
		state = ApprovalApproved
	}
	var row dbgen.ApprovalItem
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, e := dbgen.New(tx).DecideApprovalItem(ctx, dbgen.DecideApprovalItemParams{
			ID: id, BusinessID: businessID, State: state, DecidedBy: decidedBy,
		})
		row = r
		return e
	})
	if err != nil {
		// ErrNoRows here means "not pending anymore" (already decided/expired) OR unknown.
		// We cannot distinguish without a second read; treat the not-pending case as 409
		// only when the row EXISTS. Disambiguate with a follow-up existence check.
		if errors.Is(err, pgx.ErrNoRows) {
			if _, gerr := s.Get(ctx, principalID, businessID, id); gerr == nil {
				return ApprovalItem{}, fmt.Errorf("agents: approval already decided/expired: %w", errs.ErrConflict)
			}
			return ApprovalItem{}, fmt.Errorf("agents: approval not found: %w", errs.ErrNotFound)
		}
		return ApprovalItem{}, mapAgentRunErr(err)
	}
	return toApprovalItem(row), nil
}

// MarkExecuted is the executor's idempotency claim: approved → executed iff still
// approved. ok=false means a prior delivery already executed it (skip).
func (s *ApprovalStore) MarkExecuted(ctx context.Context, principalID, businessID, id uuid.UUID) (ok bool, err error) {
	e := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		_, ie := dbgen.New(tx).MarkApprovalExecuted(ctx, dbgen.MarkApprovalExecutedParams{ID: id, BusinessID: businessID})
		return ie
	})
	if e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return false, nil
		}
		return false, mapAgentRunErr(e)
	}
	return true, nil
}

// ExpireStale marks past-expiry pending items expired (the sweep). Runs RLS-exempt
// because the sweep is system-wide; pass a service principal with broad visibility OR
// run via the Super pool — here it uses WithPrincipal under the supplied principal.
func (s *ApprovalStore) ExpireStale(ctx context.Context, principalID uuid.UUID) (int64, error) {
	var n int64
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		c, e := dbgen.New(tx).ExpireStaleApprovals(ctx)
		n = c
		return e
	})
	return n, err
}

func toApprovalItem(r dbgen.ApprovalItem) ApprovalItem {
	out := ApprovalItem{
		ID: r.ID, AgentRunID: r.AgentRunID, BusinessID: r.BusinessID, TenantRootID: r.TenantRootID,
		Tool: r.Tool, Args: json.RawMessage(r.Args), EffectClass: int(r.EffectClass),
		State: r.State, ExpiresAt: r.ExpiresAt, Error: r.Error,
	}
	if r.DecidedByPrincipalID.Valid {
		v := uuid.UUID(r.DecidedByPrincipalID.Bytes)
		out.DecidedByPrincipalID = &v
	}
	if r.ExecutedAt.Valid {
		out.ExecutedAt = &r.ExecutedAt.Time
	}
	return out
}

var _ pgconn.PgError // keep pgconn imported if unused after edits
```

> **sqlc mapping check (HANDOFF gotcha):** confirm the generated `dbgen.ApprovalItem` field types — nullable `uuid` → `pgtype.UUID` (use `.Valid`/`.Bytes`), nullable `timestamptz` → `pgtype.Timestamptz` (`.Valid`/`.Time`), nullable `text` → `*string`, `jsonb` → `[]byte`, `smallint` → `int16`. Adjust `toApprovalItem` + the param helpers (`DecidedBy uuid.UUID` may need `db.PGUUID(decidedBy)`; `Ttl` interval may map to `pgtype.Interval` — add a `pgInterval(time.Duration)` helper, or change the SQL to take seconds: `make_interval(secs => $n)` and pass an `int32`). **Prefer the seconds form** to avoid the interval type: change `CreateApprovalItem`'s `now() + sqlc.arg('ttl')::interval` to `now() + make_interval(secs => sqlc.arg('ttl_seconds')::int)` and pass `int32(s.ttl().Seconds())`. Update the SQL + helper accordingly and drop `pgInterval`. Remove the `var _ pgconn.PgError` line if `pgconn` is otherwise used; it's only a placeholder.

- [ ] **Step 2: Unit test the store's pure mapping** (`internal/agents/approval_test.go`) — the DB methods need infra; pin the value mapping + TTL default with a table test on `toApprovalItem` and `ttl()`:

```go
package agents

import (
	"testing"
	"time"
)

func TestApprovalTTLDefault(t *testing.T) {
	s := &ApprovalStore{}
	if s.ttl() != defaultApprovalTTL {
		t.Fatalf("ttl=%v want %v", s.ttl(), defaultApprovalTTL)
	}
	s.TTL = time.Hour
	if s.ttl() != time.Hour {
		t.Fatalf("override ttl=%v want 1h", s.ttl())
	}
}
```

- [ ] **Step 3: Build + test:**

Run: `go build ./internal/agents/ && go test ./internal/agents/ -run TestApproval`
Expected: PASS.

- [ ] **Step 4: Commit:**

```bash
git add internal/agents/approval.go internal/agents/approval_test.go
git commit -m "feat(agents): ApprovalStore (RLS create/get/list/decide/mark-executed/expire)"
```

---

## Task 7: Reply idempotency key (ticketing)

**Files:**
- Modify: `internal/ticketing/service.go` — `ReplyInput` + `Reply` dedup short-circuit.
- Test: `internal/ticketing/*_test.go` (characterization + behavior) — find the existing reply test file.

- [ ] **Step 1: Characterize current Reply** — confirm the existing reply test(s) pass unchanged (no key set). Run: `go test ./internal/ticketing/ -run Reply`. Expected: PASS (baseline).

- [ ] **Step 2: Add the field.** In `internal/ticketing/service.go` `ReplyInput` (line 43):

```go
type ReplyInput struct {
	BodyText string
	BodyHTML *string
	// IdempotencyKey, when non-nil, dedups the produced outbound message: a second
	// Reply with the same key inserts no new message and enqueues no second send
	// (used by the approvals executor so an at-least-once outbox redelivery sends once).
	IdempotencyKey *uuid.UUID
}
```

- [ ] **Step 3: Dedup short-circuit in `Reply`.** Inside the `WithPrincipal` closure, **before** computing threading/inserting (right after the own-check `GetTicket` at ~line 243), add:

```go
		// Idempotent re-execution: if this approval already produced a message, return it
		// and do nothing else (no second insert, no second send enqueue).
		if in.IdempotencyKey != nil {
			if prior, perr := q.GetOutboundMessageByApproval(ctx, dbgen.GetOutboundMessageByApprovalParams{
				SourceApprovalItemID: db.PGUUID(*in.IdempotencyKey), BusinessID: businessID,
			}); perr == nil {
				out = toMessage(prior, nil)
				return nil
			} else if !errors.Is(perr, pgx.ErrNoRows) {
				return perr
			}
		}
```

And set the column on insert — in the `InsertOutboundMessageParams` (line 301) add:

```go
			SourceApprovalItemID: pgUUIDPtr(in.IdempotencyKey),
```

(Use the repo's existing nullable-UUID helper; HANDOFF says `db.PGUUIDPtr` maps `*uuid.UUID` → `pgtype.UUID`. Use `db.PGUUIDPtr(in.IdempotencyKey)`.)

- [ ] **Step 4: Behavior test** — add to the ticketing reply test file (integration-tagged if it needs DB; mirror the existing reply test's harness):

```go
// Replying twice with the same IdempotencyKey produces exactly one outbound message.
func TestReply_IdempotentByApprovalKey(t *testing.T) {
	// ... arrange a ticket via the existing reply-test fixture ...
	key := uuid.New()
	m1, err := svc.Reply(ctx, agentPID, bid, ticketID, ReplyInput{BodyText: "hello", IdempotencyKey: &key})
	if err != nil { t.Fatalf("reply 1: %v", err) }
	m2, err := svc.Reply(ctx, agentPID, bid, ticketID, ReplyInput{BodyText: "hello", IdempotencyKey: &key})
	if err != nil { t.Fatalf("reply 2: %v", err) }
	if m1.ID != m2.ID {
		t.Fatalf("dedup failed: two distinct messages %s vs %s", m1.ID, m2.ID)
	}
	// assert exactly one ticket_message row carries source_approval_item_id = key
}
```

- [ ] **Step 5: Run + commit:**

Run: `go test ./internal/ticketing/ -run Reply` (and `-tags integration` if the test needs DB).
Expected: PASS.

```bash
git add internal/ticketing/ db/query/
git commit -m "feat(ticketing): optional reply idempotency key for exactly-once approval execution"
```

---

## Task 8: `ApprovalExecutor` (outbox subscriber)

**Files:**
- Create: `internal/agents/approval_executor.go`, `internal/agents/approval_executor_test.go`

The executor reuses the **tool registry** to invoke the approved tool, passing the approval id as a ctx idempotency key that `draft_reply` reads.

- [ ] **Step 1: ctx idempotency plumbing + draft_reply honoring it.** In `internal/agents/tools.go`, add near the top:

```go
type idemKeyCtx struct{}

// withApprovalKey tags ctx with the approval id so the reply tool dedups on execution.
func withApprovalKey(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, idemKeyCtx{}, id)
}
func approvalKeyFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(idemKeyCtx{}).(uuid.UUID)
	return id, ok
}
```

In the `draft_reply` tool's `Invoke` (line ~298), thread the key into `ReplyInput`:

```go
			in := ticketing.ReplyInput{BodyText: a.BodyText}
			if k, ok := approvalKeyFrom(ctx); ok {
				in.IdempotencyKey = &k
			}
			if _, err := svc.Reply(ctx, pid, bid, a.TicketID, in); err != nil {
				return "", err
			}
			return "reply sent", nil
```

- [ ] **Step 2: Write the failing executor test** (`internal/agents/approval_executor_test.go`) using fakes:

```go
package agents

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/events"
)

// fakeApprovalState drives the executor's pre-check + claim deterministically.
type fakeApprovalState struct {
	state     string
	getCalls  int
	markCalls int
	markOK    bool
}

func (f *fakeApprovalState) Get(_ context.Context, _, _, _ uuid.UUID) (ApprovalItem, error) {
	f.getCalls++
	return ApprovalItem{State: f.state, Tool: "set_status", Args: json.RawMessage(`{"ticket_id":"` + uuid.New().String() + `","status":"open"}`)}, nil
}
func (f *fakeApprovalState) MarkExecuted(_ context.Context, _, _, _ uuid.UUID) (bool, error) {
	f.markCalls++
	return f.markOK, nil
}

func payload(t *testing.T, p approvalEventPayload) []byte {
	t.Helper()
	b, _ := json.Marshal(p)
	return b
}

func TestExecutor_ExecutesApprovedOnce(t *testing.T) {
	fts := &fakeTicketSvc{}
	st := &fakeApprovalState{state: ApprovalApproved, markOK: true}
	ex := &ApprovalExecutor{Approvals: st, Tools: NewToolRegistry(fts), Auditor: &fakeAuditor{}}
	pl := approvalEventPayload{ApprovalID: uuid.New(), AgentPrincipalID: uuid.New(), BusinessID: uuid.New(), TenantRootID: uuid.New(), Tool: "set_status"}
	if err := ex.Handle(context.Background(), nil, events.Event{Topic: TopicAgentApproved, Payload: payload(t, pl)}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if fts.triageIn.Status == nil {
		t.Fatal("approved tool must execute")
	}
	if st.markCalls != 1 {
		t.Fatalf("mark-executed calls=%d want 1", st.markCalls)
	}
}

func TestExecutor_SkipsNonApproved(t *testing.T) {
	fts := &fakeTicketSvc{}
	st := &fakeApprovalState{state: ApprovalExecuted} // already done
	ex := &ApprovalExecutor{Approvals: st, Tools: NewToolRegistry(fts), Auditor: &fakeAuditor{}}
	pl := approvalEventPayload{ApprovalID: uuid.New(), Tool: "set_status"}
	if err := ex.Handle(context.Background(), nil, events.Event{Topic: TopicAgentApproved, Payload: payload(t, pl)}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if fts.triageIn.Status != nil {
		t.Fatal("a non-approved item must NOT execute (idempotent skip)")
	}
}
```

- [ ] **Step 3: Create `internal/agents/approval_executor.go`:**

```go
package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/events"
)

// TopicAgentApproved is the outbox topic an approved action is enqueued under (US4).
const TopicAgentApproved = "agent.action.approved"

// approvalEventPayload is the JSON carried on TopicAgentApproved.
type approvalEventPayload struct {
	ApprovalID       uuid.UUID       `json:"approval_id"`
	AgentRunID       uuid.UUID       `json:"agent_run_id"`
	AgentPrincipalID uuid.UUID       `json:"agent_principal_id"`
	BusinessID       uuid.UUID       `json:"business_id"`
	TenantRootID     uuid.UUID       `json:"tenant_root_id"`
	Tool             string          `json:"tool"`
	Args             json.RawMessage `json:"args"`
}

// approvalReader is the executor's view of the store (pre-check + idempotency claim).
type approvalReader interface {
	Get(ctx context.Context, principalID, businessID, id uuid.UUID) (ApprovalItem, error)
	MarkExecuted(ctx context.Context, principalID, businessID, id uuid.UUID) (bool, error)
}

// ApprovalExecutor runs an approved tool-call as the agent principal, exactly once.
// It subscribes to TopicAgentApproved. It does its DB work in its OWN principal-scoped
// transactions (NOT the worker tx — the worker tx has no principal context), mirroring
// the notify SendSubscriber. Exactly-once comes from (a) the pre-check + MarkExecuted
// state claim and (b) the reply dedup key (the draft_reply tool keys on ApprovalID).
type ApprovalExecutor struct {
	Approvals approvalReader
	Tools     *ToolRegistry
	Auditor   runAuditor
}

// Handle implements events.Handler. It ignores tx (uses its own principal txs).
func (e *ApprovalExecutor) Handle(ctx context.Context, _ pgx.Tx, ev events.Event) error {
	var p approvalEventPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		// Malformed payload is a poison event; log + treat as processed (return nil) so
		// it doesn't retry forever. (The producer is trusted, in-process.)
		slog.ErrorContext(ctx, "approval executor: bad payload", "err", err)
		return nil
	}
	run := AgentRun{ID: p.AgentRunID, BusinessID: p.BusinessID}

	// Pre-check: only an 'approved' item executes (skip denied/expired/already-executed).
	item, err := e.Approvals.Get(ctx, p.AgentPrincipalID, p.BusinessID, p.ApprovalID)
	if err != nil {
		return err // transient → reschedule
	}
	if item.State != ApprovalApproved {
		return nil // idempotent skip
	}

	tool, ok := e.Tools.Get(p.Tool)
	if !ok {
		// A tool removed since proposal: do not execute; mark executed-with-error so it
		// leaves the queue. (Fail-closed: never guess an action.)
		_, _ = e.Approvals.MarkExecuted(ctx, p.AgentPrincipalID, p.BusinessID, p.ApprovalID)
		_ = e.Auditor.Run(ctx, p.AgentPrincipalID, run, "agent.approval.error", map[string]any{"approval_id": p.ApprovalID}, map[string]any{"tool": p.Tool, "detail": "unknown tool"}, "error")
		return nil
	}

	// Execute as the agent, keyed by the approval id (the reply tool dedups on it).
	execCtx := withApprovalKey(ctx, p.ApprovalID)
	out, ierr := tool.Invoke(execCtx, p.AgentPrincipalID, p.BusinessID, p.Args)
	if ierr != nil {
		_ = e.Auditor.Run(ctx, p.AgentPrincipalID, run, "agent.approval.error", map[string]any{"approval_id": p.ApprovalID}, map[string]any{"tool": p.Tool, "detail": safeMsg(ierr)}, "error")
		return fmt.Errorf("approval execute %s: %w", p.Tool, ierr) // reschedule
	}

	// Claim: approved → executed. Already-executed (ok=false) is fine (a prior delivery
	// ran the tool; the dedup key ensured the side effect happened at most once).
	if _, merr := e.Approvals.MarkExecuted(ctx, p.AgentPrincipalID, p.BusinessID, p.ApprovalID); merr != nil {
		return merr // reschedule; re-execution is dedup-safe
	}
	_ = e.Auditor.Run(ctx, p.AgentPrincipalID, run, "agent.approval.executed", map[string]any{"approval_id": p.ApprovalID}, map[string]any{"tool": p.Tool, "result": out}, "executed")
	return nil
}
```

- [ ] **Step 4: Run the executor unit tests — expect PASS:**

Run: `go test ./internal/agents/ -run TestExecutor -v`
Expected: PASS.

- [ ] **Step 5: Commit:**

```bash
git add internal/agents/approval_executor.go internal/agents/approval_executor_test.go internal/agents/tools.go
git commit -m "feat(agents): ApprovalExecutor subscriber — idempotent approved-tool execution via the registry"
```

---

## Task 9: Approve/Deny/List HTTP (service + handler)

**Files:**
- Create: `internal/agents/approval_handler.go`, `internal/agents/approval_handler_test.go`

The approve path: decide (→approved) **and** enqueue `TopicAgentApproved` in one tx. So the service needs the DB to enqueue inside the same tx as the decide. We extend `ApprovalStore.Decide` to also enqueue on approve — simplest is a dedicated `Approve` store method that does both in one `WithPrincipal` tx.

- [ ] **Step 1: Add a transactional `Approve` to the store.** In `internal/agents/approval.go`, add (alongside `Decide`):

```go
// Approve transitions pending→approved AND enqueues the execution event in ONE tx, so
// a committed approval always has its outbox event (no lost action). Returns 409 via
// ErrConflict if the item is not pending/expired.
func (s *ApprovalStore) Approve(ctx context.Context, principalID, businessID, id, decidedBy uuid.UUID) (ApprovalItem, error) {
	var item ApprovalItem
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, e := q.DecideApprovalItem(ctx, dbgen.DecideApprovalItemParams{ID: id, BusinessID: businessID, State: ApprovalApproved, DecidedBy: decidedBy})
		if e != nil {
			return e // ErrNoRows handled by caller (mapped to 409 if the row exists)
		}
		item = toApprovalItem(row)
		// Need the agent principal id to execute as the agent: derive from the run's agent.
		agentPID, ae := q.GetAgentPrincipalForRun(ctx, dbgen.GetAgentPrincipalForRunParams{ID: item.AgentRunID, BusinessID: businessID})
		if ae != nil {
			return ae
		}
		return events.Enqueue(ctx, tx, item.TenantRootID, TopicAgentApproved, approvalEventPayload{
			ApprovalID: item.ID, AgentRunID: item.AgentRunID, AgentPrincipalID: agentPID,
			BusinessID: businessID, TenantRootID: item.TenantRootID, Tool: item.Tool, Args: item.Args,
		})
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if _, gerr := s.Get(ctx, principalID, businessID, id); gerr == nil {
				return ApprovalItem{}, fmt.Errorf("agents: approval already decided/expired: %w", errs.ErrConflict)
			}
			return ApprovalItem{}, fmt.Errorf("agents: approval not found: %w", errs.ErrNotFound)
		}
		return ApprovalItem{}, mapAgentRunErr(err)
	}
	return item, nil
}
```

Add the import `"github.com/manyforge/manyforge/internal/platform/events"` to `approval.go`. Add a sqlc query `GetAgentPrincipalForRun` to `db/query/approval_item.sql`:

```sql
-- name: GetAgentPrincipalForRun :one
-- The acting agent principal for a run (so an approval executes as the agent).
SELECT a.principal_id FROM agent_run ar
JOIN agent a ON a.id = ar.agent_id AND a.tenant_root_id = ar.tenant_root_id
WHERE ar.id = $1 AND ar.business_id = $2;
```

Re-run `make generate` after adding it.

- [ ] **Step 2: Write `internal/agents/approval_handler.go`:**

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

// approvalOps is the surface the approvals HTTP handler needs (fakeable in tests).
type approvalOps interface {
	ListPending(ctx context.Context, principalID, businessID uuid.UUID, limit int) ([]ApprovalItem, error)
	Approve(ctx context.Context, principalID, businessID, id, decidedBy uuid.UUID) (ApprovalItem, error)
	Deny(ctx context.Context, principalID, businessID, id, decidedBy uuid.UUID) (ApprovalItem, error)
}

// ApprovalService is approvalOps over the store.
type ApprovalService struct{ store *ApprovalStore }

func NewApprovalService(s *ApprovalStore) *ApprovalService { return &ApprovalService{store: s} }

func (s *ApprovalService) ListPending(ctx context.Context, pid, bid uuid.UUID, limit int) ([]ApprovalItem, error) {
	return s.store.ListPending(ctx, pid, bid, limit)
}
func (s *ApprovalService) Approve(ctx context.Context, pid, bid, id, by uuid.UUID) (ApprovalItem, error) {
	return s.store.Approve(ctx, pid, bid, id, by)
}
func (s *ApprovalService) Deny(ctx context.Context, pid, bid, id, by uuid.UUID) (ApprovalItem, error) {
	return s.store.Decide(ctx, pid, bid, id, by, false)
}

var _ approvalOps = (*ApprovalService)(nil)

// ApprovalHandler is the thin HTTP layer (caller must gate with agents.approve).
type ApprovalHandler struct{ svc approvalOps }

func NewApprovalHandler(svc approvalOps) *ApprovalHandler { return &ApprovalHandler{svc: svc} }

func (h *ApprovalHandler) ProtectedRoutes(r chi.Router) {
	r.Route("/businesses/{id}/approvals", func(r chi.Router) {
		r.Get("/", h.list)
		r.Post("/{approvalID}/approve", h.approve)
		r.Post("/{approvalID}/deny", h.deny)
	})
}

type approvalResp struct {
	ID          uuid.UUID `json:"id"`
	AgentRunID  uuid.UUID `json:"agent_run_id"`
	Tool        string    `json:"tool"`
	EffectClass int       `json:"effect_class"`
	State       string    `json:"state"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func toApprovalResp(a ApprovalItem) approvalResp {
	return approvalResp{ID: a.ID, AgentRunID: a.AgentRunID, Tool: a.Tool, EffectClass: a.EffectClass, State: a.State, ExpiresAt: a.ExpiresAt}
}

func apBusinessID(r *http.Request) (uuid.UUID, error) { return uuid.Parse(chi.URLParam(r, "id")) }
func apItemID(r *http.Request) (uuid.UUID, error)     { return uuid.Parse(chi.URLParam(r, "approvalID")) }

func (h *ApprovalHandler) list(w http.ResponseWriter, r *http.Request) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := apBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	items, err := h.svc.ListPending(r.Context(), pid, bid, 50)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	out := make([]approvalResp, 0, len(items))
	for _, it := range items {
		out = append(out, toApprovalResp(it))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *ApprovalHandler) decide(w http.ResponseWriter, r *http.Request, approve bool) {
	pid, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	bid, err := apBusinessID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	aid, err := apItemID(r)
	if err != nil {
		httpx.WriteError(w, r, errs.ErrNotFound)
		return
	}
	var item ApprovalItem
	if approve {
		item, err = h.svc.Approve(r.Context(), pid, bid, aid, pid)
	} else {
		item, err = h.svc.Deny(r.Context(), pid, bid, aid, pid)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toApprovalResp(item))
}

func (h *ApprovalHandler) approve(w http.ResponseWriter, r *http.Request) { h.decide(w, r, true) }
func (h *ApprovalHandler) deny(w http.ResponseWriter, r *http.Request)    { h.decide(w, r, false) }
```

- [ ] **Step 3: Handler test with a fake** (`internal/agents/approval_handler_test.go`) — mirror the run handler test style: assert list returns items; approve of an unknown id → 404; approve of an already-decided id → 409. (Build the request with chi route context; use `httptest`.)

```go
package agents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

type fakeApprovalOps struct {
	pending  []ApprovalItem
	approveE error
}

func (f fakeApprovalOps) ListPending(context.Context, uuid.UUID, uuid.UUID, int) ([]ApprovalItem, error) {
	return f.pending, nil
}
func (f fakeApprovalOps) Approve(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) (ApprovalItem, error) {
	return ApprovalItem{}, f.approveE
}
func (f fakeApprovalOps) Deny(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) (ApprovalItem, error) {
	return ApprovalItem{}, nil
}

func doApprove(t *testing.T, ops approvalOps, approveErr error) int {
	t.Helper()
	h := NewApprovalHandler(ops)
	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(httpx.WithPrincipal(req.Context(), uuid.New())))
			})
		})
		h.ProtectedRoutes(g)
	})
	req := httptest.NewRequest(http.MethodPost, "/businesses/"+uuid.New().String()+"/approvals/"+uuid.New().String()+"/approve", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code
}

func TestApprovalHandler_ConflictMaps409(t *testing.T) {
	code := doApprove(t, fakeApprovalOps{approveE: errs.ErrConflict}, errs.ErrConflict)
	if code != http.StatusConflict {
		t.Fatalf("approve already-decided = %d, want 409", code)
	}
}

func TestApprovalHandler_NotFoundMaps404(t *testing.T) {
	code := doApprove(t, fakeApprovalOps{approveE: errs.ErrNotFound}, errs.ErrNotFound)
	if code != http.StatusNotFound {
		t.Fatalf("approve unknown = %d, want 404", code)
	}
}
```

(Confirm a `httpx.WithPrincipal` test helper exists; if not, set the principal via the same context key the middleware uses — check `internal/platform/httpx/middleware.go` for an exported setter or add one used only by tests.)

- [ ] **Step 4: Build + test + commit:**

Run: `go test ./internal/agents/ -run TestApprovalHandler -v`
Expected: PASS.

```bash
git add internal/agents/approval.go internal/agents/approval_handler.go internal/agents/approval_handler_test.go db/query/ internal/platform/db/dbgen/
git commit -m "feat(agents): approvals-queue HTTP (list/approve/deny) + transactional approve→outbox"
```

---

## Task 10: `agents.approve` permission migration (human-only)

**Files:**
- Create: `migrations/0031_agents_approve_perm.up.sql`, `migrations/0031_agents_approve_perm.down.sql`

- [ ] **Step 1: `migrations/0031_agents_approve_perm.up.sql`:**

```sql
-- 0031: agents.approve permission (Spec 003 US4). HUMAN-ONLY: it decides approval_items
-- (approve/deny a gated agent action). It is granted to admin (owner is covered by the
-- locked-owner all-permissions short-circuit) and DELIBERATELY NOT to the agent_runtime
-- preset role — an agent holding agents.approve could self-approve its own gated actions
-- and collapse the autonomy gate (separation of duties).

-- security: system catalog, no tenant scoping
INSERT INTO permission (key, module, description) VALUES
    ('agents.approve', 'agents', 'Decide (approve/deny) queued agent approval items');

INSERT INTO role_permission (role_id, permission_key)
    SELECT r.id, p.key FROM role r JOIN permission p ON p.key = 'agents.approve'
    WHERE r.tenant_root_id IS NULL AND r.key = 'admin';
```

- [ ] **Step 2: `migrations/0031_agents_approve_perm.down.sql`:**

```sql
-- Reverse 0031_agents_approve_perm.
DELETE FROM role_permission WHERE permission_key = 'agents.approve';
DELETE FROM permission WHERE key = 'agents.approve';
```

- [ ] **Step 3: Commit:**

```bash
git add migrations/0031_agents_approve_perm.up.sql migrations/0031_agents_approve_perm.down.sql
git commit -m "feat(agents): agents.approve permission (admin only; never agent_runtime — no self-approval)"
```

---

## Task 11: Wire everything in `main.go` + the expire sweep

**Files:**
- Modify: `cmd/manyforge/main.go`

- [ ] **Step 1: Construct the approval store/service/handler/executor.** After the agents Engine + RunService block (~line 158), add:

```go
	approvalStore := &agents.ApprovalStore{DB: database}
	agentEngine.Approvals = approvalStore // wire the gate's approval writer
	approvalSvc := agents.NewApprovalService(approvalStore)
	approvalH := agents.NewApprovalHandler(approvalSvc)
	approvalExec := &agents.ApprovalExecutor{
		Approvals: approvalStore,
		Tools:     agents.NewToolRegistry(ticketSvc),
		Auditor:   agents.NewDBAuditor(database),
	}
	eventBus.Subscribe(agents.TopicAgentApproved, approvalExec.Handle)
```

(`ticketSvc` is the same `*ticketing.Service` already passed to `NewToolRegistry` for the Engine — reuse the existing variable. `agentEngine.Approvals = approvalStore` must run before the worker can produce approvals, which is fine since requests come later.)

- [ ] **Step 2: Add the sweep goroutine.** Near the outbox worker start (`go outboxWorker.Run(workerCtx)`, ~line 352), add a periodic expire sweep. It needs a principal with broad visibility; use the same system/service principal the app uses for housekeeping (search main.go for an existing system principal; if none, run the sweep via the Super pool by adding an `ExpireStaleSuper` that uses `database.Super`). Minimal version using a service principal `systemPID`:

```go
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-t.C:
				if n, err := approvalStore.ExpireStale(workerCtx, systemPID); err != nil {
					logger.WarnContext(workerCtx, "approval expire sweep", "err", err)
				} else if n > 0 {
					logger.InfoContext(workerCtx, "approval items expired", "count", n)
				}
			}
		}
	}()
```

> **Decision point for the implementer:** `ExpireStaleApprovals` is an unscoped UPDATE; under RLS it only touches rows the sweep principal can see. For a global sweep, run it RLS-exempt: add `ExpireStaleSuper(ctx)` to `ApprovalStore` that uses `database.Super.Exec(ctx, "UPDATE approval_item SET state='expired', updated_at=now() WHERE state='pending' AND expires_at<=now()")` and call THAT in the goroutine (no principal needed). Prefer this — it avoids needing a broad-visibility principal. Confirm `database.Super` exists (it does in the test harness; check it's exposed on the prod `*db.DB`).

- [ ] **Step 3: Add the `agentsApprove` middleware field + population + route group.**
  - In the `apiHandlers` struct (~line 444), add:
    ```go
    	// approvals is the US4 approvals-queue handler (Spec 003): list/approve/deny.
    	approvals *agents.ApprovalHandler
    	// agentsApprove gates the US4 approvals slice on the agents.approve permission.
    	agentsApprove func(http.Handler) http.Handler
    ```
  - In the `mountAPIRoutes(mux, apiHandlers{...})` literal (~line 341), add:
    ```go
    		approvals:     approvalH,
    		agentsApprove: httpx.RequirePermission(database, permResolve, "agents.approve", businessIDFromPath),
    ```
  - In `mountAPIRoutes`, near the agent-runs group (~line 526-529), add:
    ```go
    	pr.Group(func(ap chi.Router) {
    		ap.Use(h.agentsApprove)
    		h.approvals.ProtectedRoutes(ap)
    	})
    ```

- [ ] **Step 4: Build:**

Run: `go build ./...`
Expected: clean.

- [ ] **Step 5: Commit:**

```bash
git add cmd/manyforge/main.go internal/agents/approval.go
git commit -m "feat(agents): wire approvals queue + executor subscriber + expire sweep into main"
```

---

## Task 12: OpenAPI + drift

**Files:**
- Modify: `specs/003-agent-runtime/contracts/openapi.yaml`, `cmd/manyforge/drift_003_test.go`

- [ ] **Step 1: Add the 3 paths to `openapi.yaml`** (after the runs paths, before `components`):

```yaml
  /businesses/{id}/approvals:
    parameters:
      - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
    get:
      operationId: listApprovals
      summary: List pending approval items (the queue) for a business
      description: >
        Requires the agents.approve permission at {id}; a lacking permission or an
        invisible business returns the same 404 as an unknown id (no oracle).
      responses:
        "200":
          description: Pending approval items
          content:
            application/json:
              schema: { $ref: "#/components/schemas/ApprovalList" }
        "401": { $ref: "#/components/responses/Unauthorized" }
        "404": { $ref: "#/components/responses/NotFound" }
  /businesses/{id}/approvals/{approvalID}/approve:
    parameters:
      - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      - { name: approvalID, in: path, required: true, schema: { type: string, format: uuid } }
    post:
      operationId: approveApprovalItem
      summary: Approve a queued agent action (enqueues idempotent execution)
      responses:
        "200":
          description: Approved item
          content:
            application/json:
              schema: { $ref: "#/components/schemas/ApprovalItem" }
        "401": { $ref: "#/components/responses/Unauthorized" }
        "404": { $ref: "#/components/responses/NotFound" }
        "409": { $ref: "#/components/responses/Conflict" }
  /businesses/{id}/approvals/{approvalID}/deny:
    parameters:
      - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      - { name: approvalID, in: path, required: true, schema: { type: string, format: uuid } }
    post:
      operationId: denyApprovalItem
      summary: Deny a queued agent action (nothing executes)
      responses:
        "200":
          description: Denied item
          content:
            application/json:
              schema: { $ref: "#/components/schemas/ApprovalItem" }
        "401": { $ref: "#/components/responses/Unauthorized" }
        "404": { $ref: "#/components/responses/NotFound" }
        "409": { $ref: "#/components/responses/Conflict" }
```

Add schemas under `components.schemas`:

```yaml
    ApprovalItem:
      type: object
      required: [id, agent_run_id, tool, effect_class, state, expires_at]
      properties:
        id: { type: string, format: uuid }
        agent_run_id: { type: string, format: uuid }
        tool: { type: string }
        effect_class: { type: integer, enum: [0, 1, 2, 3] }
        state: { type: string, enum: [pending, approved, denied, executed, expired] }
        expires_at: { type: string, format: date-time }
    ApprovalList:
      type: object
      required: [items]
      properties:
        items:
          type: array
          items: { $ref: "#/components/schemas/ApprovalItem" }
```

- [ ] **Step 2: Extend the drift test.** In `cmd/manyforge/drift_003_test.go`, add to `inScope003Ops`:

```go
	"GET /businesses/{}/approvals",
	"POST /businesses/{}/approvals/{}/approve",
	"POST /businesses/{}/approvals/{}/deny",
```

And widen `is003Op` so flat approvals routes are classified as 003:

```go
func is003Op(op string) bool {
	return strings.Contains(op, "/agents") || strings.Contains(op, "/approvals")
}
```

- [ ] **Step 3: Run drift:**

Run: `go test -tags contract ./cmd/manyforge/ -run TestOpenAPIDrift003 -v`
Expected: PASS (router routes ↔ openapi.yaml match both ways).

- [ ] **Step 4: Commit:**

```bash
git add specs/003-agent-runtime/contracts/openapi.yaml cmd/manyforge/drift_003_test.go
git commit -m "docs(api): US4 approvals-queue OpenAPI + drift coverage"
```

---

## Task 13: Integration tests (RLS cross-tenant + idempotent replay)

**Files:**
- Create: `internal/agents/approval_integration_test.go` (build tag `//go:build integration`)

Mirror `internal/agents/run_integration_test.go`'s `seedRunTenant` + testcontainers harness.

- [ ] **Step 1: Cross-tenant no-oracle test** — seed two tenants; create an approval_item in tenant A (insert via Super or by driving a Mode-1 run that queues a draft_reply); assert tenant B's owner `Get`/`Approve` of A's approval id → `ErrNotFound` (RLS yields no row). Assert same-business approve of an unknown id → `ErrNotFound`.

```go
//go:build integration

package agents

// TestApprovalCrossTenantNoOracle: an approval_item in tenant A is invisible to
// tenant B (RLS) — Get/Approve return ErrNotFound, never an existence oracle.
// TestApprovalReplayIdempotent: draining TopicAgentApproved twice for one approved
// reply yields exactly ONE outbound ticket_message (dedup key) and the item ends
// 'executed'.
```

- [ ] **Step 2: Idempotent-replay test** — the core §4 contract. Seed a tenant + ticket; create an `agent_run` + an `approved` `approval_item` for `draft_reply`; build the `approvalEventPayload`; invoke `ApprovalExecutor.Handle` TWICE (simulating outbox redelivery) against the real `ApprovalStore` + real `ticketing.Service` (the same wiring as main); after both, assert:
  - exactly one `ticket_message` row with `source_approval_item_id = approvalID`;
  - the `approval_item.state = 'executed'`.

```go
func TestApprovalReplayIdempotent(t *testing.T) {
	ctx := context.Background()
	tdb := testdb.Start(ctx, t)
	seed := seedRunTenant(ctx, t, tdb)
	// ... create a ticket (mirror the ticketing read fixture) ...
	// ... insert an agent + agent_run + an 'approved' approval_item for draft_reply via Super ...

	store := &agents.ApprovalStore{DB: tdb.App}
	ticketSvc := /* construct *ticketing.Service over tdb.App, like main */
	exec := &agents.ApprovalExecutor{Approvals: store, Tools: agents.NewToolRegistry(ticketSvc), Auditor: agents.NewDBAuditor(tdb.App)}

	ev := events.Event{Topic: agents.TopicAgentApproved, Payload: mustJSON(payloadFor(approvalID, runID, agentPID, seed.businessID, seed.tenantRootID, "draft_reply", argsJSON))}
	if err := exec.Handle(ctx, nil, ev); err != nil { t.Fatalf("handle 1: %v", err) }
	if err := exec.Handle(ctx, nil, ev); err != nil { t.Fatalf("handle 2 (replay): %v", err) }

	var n int
	tdb.Super.QueryRow(ctx, "SELECT count(*) FROM ticket_message WHERE source_approval_item_id=$1", approvalID).Scan(&n)
	if n != 1 { t.Fatalf("replay produced %d messages, want exactly 1", n) }
	var state string
	tdb.Super.QueryRow(ctx, "SELECT state FROM approval_item WHERE id=$1", approvalID).Scan(&state)
	if state != "executed" { t.Fatalf("state=%s want executed", state) }
}
```

(Use the real `payloadFor`/`mustJSON` helpers you define; `argsJSON` = `{"ticket_id":"…","body_text":"…"}`. Because the executor's `approvalEventPayload` type + `Handle` are package-internal, this test lives in `package agents`.)

- [ ] **Step 3: Run integration (Docker required):**

Run: `go test -tags integration ./internal/agents/ -run 'TestApprovalCrossTenantNoOracle|TestApprovalReplayIdempotent' -p 1 -v`
Expected: PASS.

- [ ] **Step 4: Commit:**

```bash
git add internal/agents/approval_integration_test.go
git commit -m "test(agents): US4 integration — approval RLS no-oracle + idempotent outbox replay"
```

---

## Task 14: Security-regression pins (update US3, add US4)

**Files:**
- Modify: `internal/security_regression/agent_run_us3_pins_test.go` (rewrite `TestPin_FailClosedExecutor`; extend `TestPin_AgentRuntimeRoleGuardSafe`).
- Create: `internal/security_regression/agent_run_us4_pins_test.go`

- [ ] **Step 1: Rewrite the fail-closed pin.** Replace `TestPin_FailClosedExecutor` in `agent_run_us3_pins_test.go` — the old `tool.Effect != EffectSafe` fragment no longer exists; pin the new gate contract instead:

```go
// TestPin_FailClosedExecutor pins the fail-closed seam: (1) the run loop still rejects
// non-allowlisted calls, and (2) the autonomy gate defaults UNKNOWN effect/mode to
// approval (never auto-exec). Dropping either reopens auto-execution of unsafe calls.
func TestPin_FailClosedExecutor(t *testing.T) {
	runner := mustRead(t, "../agents/runner.go")
	if !strings.Contains(runner, "!allow[call.Name]") {
		t.Error("runner.go: allowlist enforcement (!allow[call.Name]) dropped?")
	}
	if !strings.Contains(runner, "gate(tool.Effect, mode) == decideApproval") {
		t.Error("runner.go: execTool must consult the autonomy gate before executing — gate seam dropped?")
	}
	g := mustRead(t, "../agents/gate.go")
	// The gate's default branch must fail closed to approval.
	if !strings.Contains(g, "default:") || !strings.Contains(g, "// FAIL-CLOSED") {
		t.Error("gate.go: missing fail-closed default — unknown effect must default to approval")
	}
}
```

- [ ] **Step 2: Extend the guard-safe pin** so `agents.approve` is a forbidden grant on `agent_runtime`. In `TestPin_AgentRuntimeRoleGuardSafe`, add `"agents.approve"` to the `forbidden` slice:

```go
	for _, forbidden := range []string{
		"members.manage",
		"roles.manage",
		"hierarchy.manage",
		"business.delete",
		"ownership.transfer",
		"agents.approve", // an agent must NOT self-approve its gated actions
	} {
```

- [ ] **Step 3: Create `internal/security_regression/agent_run_us4_pins_test.go`:**

```go
// US4 autonomy-gate + approvals security contract (Spec 003 design §4; issue
// manyforge-6cb). Source-level pins: a refactor that drops a US4 protection fails the
// security gate even if a behavioral test is weakened. Complements behavioral tests in
// internal/agents/ (gate_test, runner_test Mode matrix, approval_executor_test,
// approval_integration_test idempotent replay).

package security_regression

import (
	"strings"
	"testing"
)

// TestPin_ApprovalItemRLS: the approval_item table enables RLS scoped to the caller's
// authorized businesses — dropping it would expose another tenant's queue.
func TestPin_ApprovalItemRLS(t *testing.T) {
	mig := mustRead(t, "../../migrations/0030_approval_item.up.sql")
	for _, frag := range []string{
		"ENABLE ROW LEVEL SECURITY",
		"CREATE POLICY approval_item_rls",
		"authorized_businesses(current_principal())",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0030_approval_item.up.sql: missing RLS fragment %q", frag)
		}
	}
}

// TestPin_GateAfterRBAC: in execTool the RBAC check (Resolver.Has) precedes the gate,
// which precedes tool execution (tool.Invoke). Reordering would let the gate or exec
// run before authorization.
func TestPin_GateAfterRBAC(t *testing.T) {
	src := mustRead(t, "../agents/runner.go")
	rbac := strings.Index(src, "e.Resolver.Has(ctx, principalID, businessID, tool.RequiredPerm)")
	g := strings.Index(src, "gate(tool.Effect, mode)")
	exec := strings.LastIndex(src, "tool.Invoke(ctx, principalID, businessID, call.Args)")
	if rbac < 0 || g < 0 || exec < 0 {
		t.Fatalf("execTool markers missing (rbac=%d gate=%d exec=%d)", rbac, g, exec)
	}
	if !(rbac < g && g < exec) {
		t.Errorf("execTool order must be RBAC(%d) < gate(%d) < exec(%d)", rbac, g, exec)
	}
}

// TestPin_AgentsApproveHumanOnly: agents.approve is granted to admin and NOT to the
// agent_runtime role (separation of duties — no agent self-approval).
func TestPin_AgentsApproveHumanOnly(t *testing.T) {
	mig := mustRead(t, "../../migrations/0031_agents_approve_perm.up.sql")
	if !strings.Contains(mig, "agents.approve") || !strings.Contains(mig, "'admin'") {
		t.Error("0031: agents.approve must exist and be granted to admin")
	}
	if strings.Contains(mig, "agent_runtime") {
		t.Error("0031: agents.approve must NOT be granted to agent_runtime (no self-approval)")
	}
	role := mustRead(t, "../../migrations/0029_agent_runtime_role.up.sql")
	if strings.Contains(role, "agents.approve") {
		t.Error("0029: agent_runtime role must NOT include agents.approve")
	}
}

// TestPin_ApprovalIdempotency: the reply dedup key exists (unique partial index) so an
// at-least-once outbox redelivery executes a reply exactly once.
func TestPin_ApprovalIdempotency(t *testing.T) {
	mig := mustRead(t, "../../migrations/0030_approval_item.up.sql")
	if !strings.Contains(mig, "source_approval_item_id") ||
		!strings.Contains(mig, "ticket_message_source_approval_idx") {
		t.Error("0030: ticket_message dedup (source_approval_item_id UNIQUE) dropped — reply idempotency lost")
	}
}
```

- [ ] **Step 4: Run sec-test:**

Run: `make sec-test` (or `go test ./internal/security_regression/ -run 'TestPin_' -v`)
Expected: PASS (all US3 + US4 pins).

- [ ] **Step 5: Commit:**

```bash
git add internal/security_regression/
git commit -m "test(sec): pin US4 gate/approvals contract (RLS, gate-after-RBAC, human-only approve, idempotency)"
```

---

## Task 15: Full gate green + close issue

- [ ] **Step 1: Run the full gate** (Docker up for int-test):

```bash
make test && make contract-test && make lint && make sec-test && make int-test
```
Expected: all PASS; golangci-lint **0 issues** (merge gate). `int-test` ~6 min (`-p 1`, testcontainers).

- [ ] **Step 2: Fix any fallout** — likely spots: `dbgen` nullable mappings in `toApprovalItem`; a `newTestEngine` call site still binding 2 values; the `fakeTicketSvc.gotTicket` wiring for the reply path; `httpx.WithPrincipal` test helper existence. Re-run until green. **No "pre-existing" failures** — fix anything red.

- [ ] **Step 3: Close the bd issue + verify spec coverage:**

```bash
bd close manyforge-6cb --reason "US4 shipped: fail-closed autonomy gate (effect×mode), approval_item RLS table (0030), agents.approve human-only perm (0031), approvals-queue HTTP (list/approve/deny) + OpenAPI/drift, idempotent outbox execution (state-machine + reply dedup key), expire sweep. Full gate green; US3 fail-closed pin rewritten; US4 pins added."
```

- [ ] **Step 4: Final commit (if any uncommitted) + push** (session-close protocol):

```bash
git status
git pull --rebase
git push
git status   # MUST show up to date with origin
```

---

## Test plan summary (what verifies US4 — design §4 mapping)

| §4 contract item | Test |
|---|---|
| Gate fail-closed (unknown effect/mode ⇒ approval) | `gate_test.go::TestGateMatrix` (unit) + `agent_run_us4_pins_test.go::TestPin_FailClosedExecutor` (source) |
| Gate after RBAC, before exec | `agent_run_us4_pins_test.go::TestPin_GateAfterRBAC` (source order) + `runner_test.go` RBAC-deny tests (behavioral) |
| Mode matrix (1 auto-reversible/queue-external; 2 queue-all-writes; 3 auto) | `runner_test.go::TestRun_Mode1ExternalQueuesApproval / TestRun_Mode2QueuesReversibleWrite / TestRun_Mode3AutoRunsExternal` |
| Every proposed/executed action audited | `runner_test.go` (audit assertions) + `approval_executor_test.go` |
| Runs + approval items tenant-isolated (no oracle) | `approval_integration_test.go::TestApprovalCrossTenantNoOracle` + `TestPin_ApprovalItemRLS` |
| Approval execution idempotent (replay = once) | `approval_integration_test.go::TestApprovalReplayIdempotent` + `TestReply_IdempotentByApprovalKey` + `TestPin_ApprovalIdempotency` |
| Agent cannot self-approve (`agents.approve` human-only) | `TestPin_AgentsApproveHumanOnly` + `TestPin_AgentRuntimeRoleGuardSafe` (forbidden set) |
| Approve→outbox→execute (at-least-once) | `approval_executor_test.go` + integration replay |
| LLM output never reaches SQL/shell (carried from US3) | existing `TestPin_TypedArgValidation` (still green) |

---

## Self-review notes (spec coverage check)

- **§5 US4 row** — gate ✅ (T2/T4), `approval_item` ✅ (T3/T6), 3-mode matrix ✅ (T2/T4), approve→outbox→execute idempotent ✅ (T8/T9/T13), deny ✅ (T9), expire ✅ (T6/T11), `agents.approve` ✅ (T10). 
- **§3.3 flow** — gate after RBAC before exec ✅ (T4 order + T14 pin); queued items don't execute, run ends `awaiting_approval` ✅ (existing `proposed` flag, T4); approve in-tx state+outbox ✅ (T9 `Approve`); worker executes with retry/idempotency ✅ (T8 + dedup key); expire sweep ✅ (T11).
- **§6 reuse** — reply via existing `ticketing.Reply`+reply outbox ✅ (T8 invokes the registry's draft_reply tool which calls `Reply`); triage via existing `Triage` ✅ (registry set_* tools); audit via existing `audit_entry` ✅ (`NewDBAuditor`); loop-guard (agent replies don't re-trigger) — deferred to **US5** (the auto-`ticket.created` trigger lives there; US4 ships only the gate+queue, manual run already exists).
- **§8 open Q** — expires TTL ✅ resolved (7 days, 60s sweep).
- **No placeholders:** the only intentionally-spec'd-not-coded spots are the two integration-test fixtures (T13) and the `InsertOutboundMessage` column-list edit (T5/T7), which depend on reading the exact existing query/fixture text at execution time; every novel unit, the gate, the executor, the migrations, the handlers, the store, and all pins are full code.
