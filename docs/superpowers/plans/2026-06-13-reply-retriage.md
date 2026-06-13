# Opt-in Re-triage on Customer Reply + Claim Hardening — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a customer replies to an existing ticket, re-invoke each opted-in, enabled agent in that business — bounded so no loop can run away — and harden the run-claim so a queued run whose agent is missing never stalls the queue head.

**Architecture:** Part A adds an opt-in per-agent flag (`agent.retriage_on_reply`), a new `ReplyRetriageTrigger` subscribed to `message.received` (the existing `TriageTrigger` stays `ticket.created`-only), and ONE atomic `SECURITY DEFINER` function `enqueue_reply_retriage_run` that does guard (`inbound AND NOT is_auto_reply`) → per-`(ticket, agent)` hourly cap → dedup-insert of a `trigger='reply'` queued run. Part B rewrites `claim_next_queued_agent_run` from a single SQL statement into a plpgsql loop that marks an orphaned run `failed` and drains the next instead of stalling.

**Tech Stack:** Go 1.x, PostgreSQL (RLS + `SECURITY DEFINER` functions), sqlc **v1.27.0** (generate with `/opt/homebrew/bin/sqlc generate` ONLY), pgx/v5, an in-process outbox event bus (`internal/platform/events`), integration tests gated by `//go:build integration` against an ephemeral Postgres (`internal/platform/db/testdb`).

**Issue:** manyforge-deo.1 (parent epic manyforge-deo — Spec 003 Agent Runtime). Spec: `docs/superpowers/specs/2026-06-13-reply-retriage-design.md`.

---

## Spec reconciliations (decisions locked before coding)

These correct/​refine the design spec based on the actual code. Treat them as authoritative:

1. **`enqueue_reply_retriage_run` is a NEW `SECURITY DEFINER` function.** There is no existing run-insert DEFINER to copy — the `ticket.created` path uses the sqlc query `CreateEventAgentRun` run under the agent's own RLS principal. The new function copies its **column list** from `CreateEventAgentRun` and its **DEFINER scaffolding** (`SET search_path = public`, `REVOKE … FROM PUBLIC`, `GRANT EXECUTE … TO manyforge_app`) from `claim_next_queued_agent_run` / `enabled_agents_for_business` (migration 0034).
2. **The DEFINER does NOT take `p_agent_principal_id`.** Unlike `CreateEventRun` (which runs the insert under the agent's RLS principal and therefore needs it), `enqueue_reply_retriage_run` is principal-less (called via `WithTx`, like `claim_next_queued_agent_run`). It derives `business_id`/`tenant_root_id` from the `ticket_message` row. Signature: `enqueue_reply_retriage_run(p_message_id uuid, p_agent_id uuid, p_cap integer) RETURNS text`.
3. **`message.received` has no topic constant.** Only `events.TopicTicketCreated = "ticket.created"` exists; `message.received` is a bare string literal in `internal/inbox/service.go`. This plan **adds** `events.TopicMessageReceived = "message.received"` for symmetry and uses it at both the emit and subscribe sites.
4. **`ticket_message.author` does not exist** — the column is `author_principal_id uuid` (NULL for inbound). **`direction` is a 3-value enum** `ticket_message_direction ('inbound','outbound','note')`. A genuine human reply is `direction='inbound', author_principal_id IS NULL, is_auto_reply=false`.
5. **The first message of a NEW ticket emits BOTH `ticket.created` AND `message.received`** (same tx, `ticket.created` first). The dedup index `agent_run (agent_id, trigger_dedup_key)` is keyed on the `ticket_message` row id and is **not** partitioned by `trigger`, so the `event` run created by `TriageTrigger` and the would-be `reply` run share a dedup key → the reply insert hits `ON CONFLICT DO NOTHING` → `skipped_dedup`. This is the intended loop-guard for new tickets; an integration test pins it. (Outbox events drain in `created_at` order, so the surviving run is the `'event'` one.)
6. **`db/schema.sql` is a sqlc-introspection mirror with no CHECK constraints.** Only the new **column** is added there. The `agent_run.trigger` CHECK (`IN ('event','manual')` → add `'reply'`) is altered **only in the live migration** (constraint name `agent_run_trigger_check`, Postgres' auto-name for the inline check in 0028).
7. **`'reply'` is NOT added to Go `validTrigger`** (`internal/agents/agent_run.go`). That predicate gates the *caller-supplied* `trigger` of the manual-run path; `reply` is system-generated only. The agent loop treats a `'reply'` run like any ticket-targeted run (reads the full thread via its read tools) — feeding the conversation differently is explicitly out of scope.

---

## File structure

**New files:**
- `migrations/0052_agent_retriage.up.sql` / `0052_agent_retriage.down.sql` — `agent.retriage_on_reply` column; `agent_run.trigger` CHECK adds `'reply'`; new DEFINERs `enabled_retriage_agents_for_business` + `enqueue_reply_retriage_run`; rewritten `claim_next_queued_agent_run` (plpgsql, orphan-tolerant).
- `internal/agents/reply_trigger.go` — `ReplyRetriageTrigger`, its `replyTriggerStore` interface, the `messageReceivedPayload` decode, and the `cap()` zero-backstop.
- `internal/agents/reply_trigger_test.go` — unit test with a fake store (no infra).
- `internal/agents/reply_retriage_integration_test.go` — the behavioral cases (`//go:build integration`).
- `internal/security_regression/reply_retriage_pins_test.go` — source-level pins for the new DEFINER + the new subscription + claim hardening.

**Modified files:**
- `db/schema.sql` — add `retriage_on_reply boolean NOT NULL` to the `agent` table (sqlc field generation).
- `db/query/agent.sql` — `retriage_on_reply` in `CreateAgent` + `UpdateAgent`.
- `internal/platform/db/dbgen/*` — regenerated (DO NOT hand-edit).
- `internal/agents/agent.go` — plumb `RetriageOnReply` through `Agent`, `CreateAgentInput`, `UpdateAgentInput`, `toAgent`, `Create`, `Update`.
- `internal/agents/agent_handler.go` — request structs + `agentResp` + `toAgentResp`.
- `internal/agents/agent_run.go` — new store methods `EnabledRetriageAgentsForBusiness` + `EnqueueReplyRetriageRun`.
- `internal/platform/events/bus.go` — `TopicMessageReceived` constant.
- `internal/inbox/service.go` — emit `message.received` via the new constant.
- `internal/platform/config/config.go` (+ `config_test.go`) — `AgentRetriageCapPerHour` / `MANYFORGE_AGENT_RETRIAGE_CAP_PER_HOUR`.
- `.env.example` — the new key.
- `cmd/manyforge/main.go` — construct + subscribe `ReplyRetriageTrigger`.
- `internal/security_regression/agent_run_us5_pins_test.go` — narrow `TestPin_TriageTriggerOnlyTicketCreated`.
- `specs/003-agent-runtime/contracts/openapi.yaml` — `retriage_on_reply` on the Agent schema + create/update bodies.

---

## Conventions for every task

- **Go on PATH:** prefix Go commands with `export PATH="$HOME/go/bin:$PATH"`.
- **Build/gates:** `go build ./...`, `make test`, `make sec-test`, `make lint` must all exit 0.
- **Integration:** `go test -tags integration ./internal/agents/... -run <Name>` (Docker up; the agents suite is ~80s — use `-run` while iterating).
- **sqlc (CRITICAL):** regenerate ONLY with `/opt/homebrew/bin/sqlc generate` (that binary is the pinned v1.27.0). NEVER `make generate` and NEVER the PATH `sqlc` (v1.31.1 churns the whole dbgen layer). After generating, `git status -s internal/platform/db/dbgen/` must show ONLY `agent.sql.go` + `models.go` touched.
- **gopls inline diagnostics are STALE/misleading** for connectors/agents/dbgen, especially right after a sqlc regen. Trust `go build`/`go test`, never the squiggles.
- **Never `git add -A`** (the bd hook auto-stages `.beads/issues.jsonl`; a `-A` sweeps untracked `CLAUDE.md` files). Commit explicit paths.

---

## Task 1: Migration 0052 — column, CHECK, DEFINERs, claim rewrite

**Files:**
- Create: `migrations/0052_agent_retriage.up.sql`
- Create: `migrations/0052_agent_retriage.down.sql`
- Modify: `db/schema.sql` (agent table — add column)

- [ ] **Step 1: Write the up migration**

Create `migrations/0052_agent_retriage.up.sql`:

```sql
-- 0052: opt-in re-triage on customer reply + claim hardening (manyforge-deo.1, Spec 003 US5).
--
-- Part A: a customer reply to an existing ticket re-invokes each opted-in enabled agent,
-- bounded by a per-(ticket, agent) hourly cap and deduped on the reply message id. The
-- existing TriageTrigger stays ticket.created-only; this is a SEPARATE, separately-guarded
-- path (enqueue_reply_retriage_run). Part B: claim_next_queued_agent_run tolerates a queued
-- run whose agent row is missing (marks it failed, drains the next) instead of stalling.

-- (A1) Opt-in flag. Default false: existing agents do NOT re-triage until explicitly enabled.
ALTER TABLE agent ADD COLUMN retriage_on_reply boolean NOT NULL DEFAULT false;

-- (A2) Allow the new run trigger value. The inline CHECK from 0028 is auto-named
-- agent_run_trigger_check; if the DROP fails, find the name with:
--   SELECT conname FROM pg_constraint WHERE conrelid='agent_run'::regclass AND contype='c';
ALTER TABLE agent_run DROP CONSTRAINT agent_run_trigger_check;
ALTER TABLE agent_run ADD CONSTRAINT agent_run_trigger_check
    CHECK (trigger IN ('event', 'manual', 'reply'));

-- (A3) enabled_retriage_agents_for_business: like enabled_agents_for_business (0034) but
-- additionally filtered to retriage_on_reply = true. Principal-less (the message.received
-- subscriber has no principal GUC), scoped by business_id AND tenant_root_id so a
-- cross-tenant event can never surface another tenant's agents.
CREATE FUNCTION enabled_retriage_agents_for_business(p_business_id uuid, p_tenant_root_id uuid)
RETURNS TABLE(agent_id uuid, principal_id uuid)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
    SELECT id, principal_id FROM agent
    WHERE business_id = p_business_id
      AND tenant_root_id = p_tenant_root_id
      AND enabled = true
      AND retriage_on_reply = true;
$$;
REVOKE ALL ON FUNCTION enabled_retriage_agents_for_business(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION enabled_retriage_agents_for_business(uuid, uuid) TO manyforge_app;

-- (A4) enqueue_reply_retriage_run: the guard + cap + dedup-insert as ONE atomic DEFINER
-- (principal-less, mirrors claim_next_queued_agent_run). Returns a text outcome the caller
-- logs. Column list of the INSERT mirrors CreateEventAgentRun (db/query/agent_run.sql).
CREATE FUNCTION enqueue_reply_retriage_run(p_message_id uuid, p_agent_id uuid, p_cap integer)
RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    v_ticket_id      uuid;
    v_business_id    uuid;
    v_tenant_root_id uuid;
    v_direction      ticket_message_direction;
    v_is_auto_reply  boolean;
    v_recent         integer;
BEGIN
    -- Load the triggering message. No row => defensively skip (treat as not-inbound).
    SELECT ticket_id, business_id, tenant_root_id, direction, is_auto_reply
      INTO v_ticket_id, v_business_id, v_tenant_root_id, v_direction, v_is_auto_reply
      FROM ticket_message WHERE id = p_message_id;
    IF NOT FOUND THEN
        RETURN 'skipped_not_inbound';
    END IF;

    -- Loop-guard (line 1): only a genuine inbound customer message re-triages. An agent's
    -- own reply is outbound/note, so it can never re-trigger here.
    IF v_direction <> 'inbound' THEN
        RETURN 'skipped_not_inbound';
    END IF;
    -- Loop-guard (line 2): auto-responders are mostly suppressed at ingest (0024); this is
    -- the second line of defense against bot ping-pong.
    IF v_is_auto_reply THEN
        RETURN 'skipped_auto_reply';
    END IF;

    -- Per-(ticket, agent) hourly cap. Counts PRIOR reply runs only (this one not yet
    -- inserted), so a cap of N permits exactly N reply runs/hour for this agent on this
    -- ticket. Per-(ticket, agent) so N opted-in agents do NOT share one budget.
    SELECT count(*) INTO v_recent
      FROM agent_run
      WHERE agent_id = p_agent_id
        AND target_id = v_ticket_id
        AND trigger = 'reply'
        AND created_at > now() - interval '1 hour';
    IF p_cap > 0 AND v_recent >= p_cap THEN
        -- Audit the suppression (principal-less, mirrors ticket.loop_suppressed in 0024).
        INSERT INTO audit_entry (id, business_id, tenant_root_id, actor_principal_id, action,
                target_type, target_id, inputs, new_value)
            VALUES (gen_random_uuid(), v_business_id, v_tenant_root_id, NULL,
                'agent.retriage_suppressed', 'ticket', v_ticket_id,
                jsonb_build_object('agent_id', p_agent_id, 'message_id', p_message_id),
                jsonb_build_object('recent_replies', v_recent, 'bound', p_cap, 'window', '1 hour'));
        RETURN 'skipped_capped';
    END IF;

    -- Enqueue (dedup on the reply message id). The partial unique index
    -- agent_run_trigger_dedup_idx (agent_id, trigger_dedup_key) is NOT partitioned by
    -- trigger, so a new ticket's first message (which also emits ticket.created and gets an
    -- 'event' run with this same key) collapses the would-be 'reply' run to skipped_dedup.
    INSERT INTO agent_run (id, agent_id, business_id, tenant_root_id, trigger,
            target_type, target_id, status, correlation_id, trigger_dedup_key)
        VALUES (gen_random_uuid(), p_agent_id, v_business_id, v_tenant_root_id, 'reply',
            'ticket', v_ticket_id, 'queued', gen_random_uuid()::text, p_message_id::text)
        ON CONFLICT (agent_id, trigger_dedup_key) WHERE trigger_dedup_key IS NOT NULL DO NOTHING;
    IF FOUND THEN
        RETURN 'enqueued';
    END IF;
    RETURN 'skipped_dedup';
END;
$$;
REVOKE ALL ON FUNCTION enqueue_reply_retriage_run(uuid, uuid, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION enqueue_reply_retriage_run(uuid, uuid, integer) TO manyforge_app;

-- (B) Claim hardening. Rewrite claim_next_queued_agent_run from a single SQL statement to a
-- plpgsql loop: a queued run whose agent row is missing is marked failed (terminal, so the
-- next SELECT won't re-pick it) and the loop drains the next run instead of stalling the
-- queue head. Return shape is IDENTICAL to 0034 (callers unaffected). The agent_run->agent
-- FK (NO ACTION) makes a missing agent unreachable today; this removes the latent stall.
DROP FUNCTION claim_next_queued_agent_run();
CREATE FUNCTION claim_next_queued_agent_run()
RETURNS TABLE(
    run_id uuid, business_id uuid, tenant_root_id uuid, correlation_id text,
    target_type text, target_id uuid,
    agent_id uuid, agent_principal_id uuid, provider ai_provider, model text,
    system_prompt text, allowed_tools text[], autonomy_mode smallint,
    enabled boolean, monthly_budget_cents int
)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    v_run   agent_run%ROWTYPE;
    v_agent agent%ROWTYPE;
BEGIN
    LOOP
        SELECT * INTO v_run FROM agent_run
            WHERE status = 'queued'
            ORDER BY created_at
            FOR UPDATE SKIP LOCKED
            LIMIT 1;
        EXIT WHEN NOT FOUND;  -- queue empty

        SELECT * INTO v_agent FROM agent
            WHERE id = v_run.agent_id AND tenant_root_id = v_run.tenant_root_id;
        IF NOT FOUND THEN
            -- Orphan: agent row gone. Terminal-fail it and drain the next; never stall.
            UPDATE agent_run SET status = 'failed', error = 'agent no longer exists',
                   updated_at = now()
                WHERE id = v_run.id;
            CONTINUE;
        END IF;

        UPDATE agent_run SET status = 'running', updated_at = now() WHERE id = v_run.id;
        RETURN QUERY SELECT
            v_run.id, v_run.business_id, v_run.tenant_root_id, v_run.correlation_id,
            v_run.target_type, v_run.target_id,
            v_agent.id, v_agent.principal_id, v_agent.provider, v_agent.model,
            v_agent.system_prompt, v_agent.allowed_tools, v_agent.autonomy_mode,
            v_agent.enabled, v_agent.monthly_budget_cents;
        RETURN;
    END LOOP;
    RETURN;  -- nothing claimable
END;
$$;
REVOKE ALL ON FUNCTION claim_next_queued_agent_run() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION claim_next_queued_agent_run() TO manyforge_app;
```

- [ ] **Step 2: Write the down migration**

Create `migrations/0052_agent_retriage.down.sql` (restores the exact 0034 `claim_next_queued_agent_run` SQL body):

```sql
-- Revert 0052. Note: the trigger CHECK revert fails if any agent_run.trigger='reply' rows
-- exist; delete them first in a dev rollback if needed.
DROP FUNCTION IF EXISTS enqueue_reply_retriage_run(uuid, uuid, integer);
DROP FUNCTION IF EXISTS enabled_retriage_agents_for_business(uuid, uuid);

-- Restore claim_next_queued_agent_run to the original 0034 single-statement SQL version.
DROP FUNCTION claim_next_queued_agent_run();
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

ALTER TABLE agent_run DROP CONSTRAINT agent_run_trigger_check;
ALTER TABLE agent_run ADD CONSTRAINT agent_run_trigger_check
    CHECK (trigger IN ('event', 'manual'));

ALTER TABLE agent DROP COLUMN retriage_on_reply;
```

- [ ] **Step 3: Mirror the new column in `db/schema.sql`**

In `db/schema.sql`, find the `CREATE TABLE agent (` block and add `retriage_on_reply` as the final data column (after `allowed_mcp_servers`, before the `UNIQUE (...)` lines), matching the no-DEFAULT mirror style:

```sql
    allowed_mcp_servers  uuid[] NOT NULL,
    retriage_on_reply    boolean NOT NULL,
    UNIQUE (business_id, name),
```

Do NOT add the CHECK change or the DEFINERs to `schema.sql` — it carries no CHECKs and sqlc does not introspect functions (that's why the DEFINERs are called via raw pgx).

- [ ] **Step 4: Apply the migration against the dev DB and verify it round-trips**

Run (adjust the migrate command to the repo's tool — check `Makefile` for a `migrate`/`make migrate` target):

```bash
export PATH="$HOME/go/bin:$PATH"
make migrate        # or the repo's up command; then verify:
psql "$DATABASE_URL" -c "\d agent" | grep retriage_on_reply
psql "$DATABASE_URL" -c "\df enqueue_reply_retriage_run"
psql "$DATABASE_URL" -c "\df enabled_retriage_agents_for_business"
psql "$DATABASE_URL" -c "SELECT pg_get_functiondef('claim_next_queued_agent_run'::regproc) LIKE '%plpgsql%';"
```

Expected: the column exists; both functions exist; the claim function is now plpgsql. If the `DROP CONSTRAINT agent_run_trigger_check` step errors with "constraint does not exist", run the `pg_constraint` query in the migration comment to find the real name and update the migration.

- [ ] **Step 5: Commit**

```bash
git add migrations/0052_agent_retriage.up.sql migrations/0052_agent_retriage.down.sql db/schema.sql
git commit -m "feat(agents): migration for reply re-triage + claim hardening (manyforge-deo.1)"
```

---

## Task 2: sqlc queries — plumb `retriage_on_reply` into create/update

**Files:**
- Modify: `db/query/agent.sql` (`CreateAgent`, `UpdateAgent`)
- Regenerate: `internal/platform/db/dbgen/agent.sql.go`, `internal/platform/db/dbgen/models.go`

- [ ] **Step 1: Add the column to `CreateAgent`**

In `db/query/agent.sql`, the `CreateAgent` INSERT column list and SELECT value list both need `retriage_on_reply`. Change the column list to include it (after `allowed_mcp_servers`) and add the SELECT value `sqlc.arg('retriage_on_reply')::boolean` (placed to match column order, before `now(), now()`):

```sql
-- name: CreateAgent :one
INSERT INTO agent (
    id, business_id, tenant_root_id, principal_id, name, provider, model,
    system_prompt, allowed_tools, autonomy_mode, enabled, monthly_budget_cents,
    allowed_mcp_servers, retriage_on_reply, created_at, updated_at)
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
    sqlc.arg('allowed_mcp_servers')::uuid[],
    sqlc.arg('retriage_on_reply')::boolean,
    now(), now()
FROM business b
WHERE b.id = sqlc.arg('business_id')::uuid
RETURNING *;
```

- [ ] **Step 2: Add the column to `UpdateAgent`**

In the same file, add a COALESCE line to `UpdateAgent` (after the `allowed_mcp_servers` line, before `updated_at`):

```sql
    allowed_mcp_servers  = COALESCE(sqlc.narg('allowed_mcp_servers')::uuid[], allowed_mcp_servers),
    retriage_on_reply    = COALESCE(sqlc.narg('retriage_on_reply')::boolean, retriage_on_reply),
    updated_at           = now()
```

`GetAgent`/`ListAgents` are `SELECT *`, so they pick up the column automatically.

- [ ] **Step 3: Regenerate dbgen with the pinned sqlc**

```bash
/opt/homebrew/bin/sqlc generate
git status -s internal/platform/db/dbgen/
```

Expected: ONLY `internal/platform/db/dbgen/agent.sql.go` and `internal/platform/db/dbgen/models.go` show as modified. If any other `*.sql.go` file churns, you used the wrong sqlc — `git checkout internal/platform/db/dbgen/` and re-run with `/opt/homebrew/bin/sqlc generate`.

- [ ] **Step 4: Verify the generated fields exist**

```bash
export PATH="$HOME/go/bin:$PATH"
grep -n "RetriageOnReply" internal/platform/db/dbgen/agent.sql.go internal/platform/db/dbgen/models.go
go build ./internal/platform/db/...
```

Expected: `models.go` `Agent` struct has `RetriageOnReply bool`; `CreateAgentParams` has `RetriageOnReply bool`; `UpdateAgentParams` has `RetriageOnReply *bool`. Build passes.

- [ ] **Step 5: Commit**

```bash
git add db/query/agent.sql internal/platform/db/dbgen/agent.sql.go internal/platform/db/dbgen/models.go
git commit -m "feat(agents): retriage_on_reply in agent create/update queries (manyforge-deo.1)"
```

---

## Task 3: Domain plumbing in `internal/agents/agent.go`

**Files:**
- Modify: `internal/agents/agent.go` (`Agent`, `CreateAgentInput`, `UpdateAgentInput`, `toAgent`, `Create`, `Update`)

- [ ] **Step 1: Add the field to the three structs**

In `internal/agents/agent.go`, add `RetriageOnReply` to `Agent` (after `AllowedMCPServers`):

```go
	AllowedMCPServers  []uuid.UUID
	RetriageOnReply    bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}
```

Add to `CreateAgentInput` (after `AllowedMCPServers`):

```go
	MonthlyBudgetCents int
	AllowedMCPServers  []uuid.UUID
	RetriageOnReply    bool
}
```

Add to `UpdateAgentInput` (pointer = absent, PATCH semantics; after `AllowedMCPServers`):

```go
	MonthlyBudgetCents *int
	AllowedMCPServers  *[]uuid.UUID
	RetriageOnReply    *bool
}
```

- [ ] **Step 2: Map it in `toAgent`**

In `toAgent`, add the field to the returned `Agent` literal:

```go
		MonthlyBudgetCents: int(r.MonthlyBudgetCents),
		AllowedMCPServers:  mcpServers,
		RetriageOnReply:    r.RetriageOnReply,
		CreatedAt:          r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
```

- [ ] **Step 3: Pass it in `Create`**

In `AgentService.Create`, add `RetriageOnReply` to the `dbgen.CreateAgentParams` literal:

```go
			AllowedMcpServers:  mcpServers,
			RetriageOnReply:    in.RetriageOnReply,
			BusinessID:         businessID,
		})
```

- [ ] **Step 4: Pass it in `Update`**

In `AgentService.Update`, set the narg pointer field (alongside the other pointer passthroughs, before the `WithPrincipal` call):

```go
	if in.AllowedMCPServers != nil {
		params.AllowedMcpServers = *in.AllowedMCPServers
	}
	params.RetriageOnReply = in.RetriageOnReply
```

- [ ] **Step 5: Build**

```bash
export PATH="$HOME/go/bin:$PATH"
go build ./internal/agents/...
```

Expected: PASS. (Commit happens at the end of Task 4 with the handler, as one coherent "API surface" change.)

---

## Task 4: Handler + OpenAPI plumbing

**Files:**
- Modify: `internal/agents/agent_handler.go` (`agentResp`, `toAgentResp`, `createAgent` body, `updateAgent` body)
- Modify: `specs/003-agent-runtime/contracts/openapi.yaml`

- [ ] **Step 1: Add the field to the response DTO**

In `internal/agents/agent_handler.go`, add to `agentResp` (after `AllowedMCPServers`):

```go
	AllowedMCPServers  []uuid.UUID `json:"allowed_mcp_servers"`
	RetriageOnReply    bool        `json:"retriage_on_reply"`
	CreatedAt          time.Time   `json:"created_at"`
	UpdatedAt          time.Time   `json:"updated_at"`
}
```

And map it in `toAgentResp`:

```go
		MonthlyBudgetCents: a.MonthlyBudgetCents, AllowedMCPServers: mcpServers,
		RetriageOnReply: a.RetriageOnReply,
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
```

- [ ] **Step 2: Accept it on create (defaults false when omitted, like `enabled`)**

In `createAgent`, add `RetriageOnReply *bool` to the anonymous `in` struct (after `AllowedMCPServers`), resolve the default, and pass it:

```go
		MonthlyBudgetCents int         `json:"monthly_budget_cents"`
		AllowedMCPServers  []uuid.UUID `json:"allowed_mcp_servers"`
		RetriageOnReply    *bool       `json:"retriage_on_reply"`
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
	retriageOnReply := false
	if in.RetriageOnReply != nil {
		retriageOnReply = *in.RetriageOnReply
	}
	created, err := h.svc.Create(r.Context(), pid, bid, CreateAgentInput{
		Name: in.Name, Provider: in.Provider, Model: in.Model, SystemPrompt: in.SystemPrompt,
		AllowedTools: in.AllowedTools, AutonomyMode: mode, Enabled: enabled,
		MonthlyBudgetCents: in.MonthlyBudgetCents, AllowedMCPServers: in.AllowedMCPServers,
		RetriageOnReply: retriageOnReply,
	})
```

- [ ] **Step 3: Accept it on update (PATCH passthrough)**

In `updateAgent`, add `RetriageOnReply *bool` to the anonymous `in` struct and pass it straight through:

```go
		MonthlyBudgetCents *int         `json:"monthly_budget_cents"`
		AllowedMCPServers  *[]uuid.UUID `json:"allowed_mcp_servers"`
		RetriageOnReply    *bool        `json:"retriage_on_reply"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	a, err := h.svc.Update(r.Context(), pid, bid, aid, UpdateAgentInput{
		Name: in.Name, Model: in.Model, SystemPrompt: in.SystemPrompt,
		AllowedTools: in.AllowedTools, AutonomyMode: in.AutonomyMode,
		Enabled: in.Enabled, MonthlyBudgetCents: in.MonthlyBudgetCents,
		AllowedMCPServers: in.AllowedMCPServers, RetriageOnReply: in.RetriageOnReply,
	})
```

- [ ] **Step 4: Document it in OpenAPI**

In `specs/003-agent-runtime/contracts/openapi.yaml`, find the `Agent` response schema and the create/update request schemas (grep for `monthly_budget_cents` to locate them). Add to each, mirroring the `enabled` property:

```yaml
        retriage_on_reply:
          type: boolean
          default: false
          description: >-
            When true, a customer reply on an existing ticket re-invokes this agent
            (bounded by a per-ticket hourly cap). Default false.
```

- [ ] **Step 5: Build + commit the whole API-surface change (Tasks 3+4)**

```bash
export PATH="$HOME/go/bin:$PATH"
go build ./...
git add internal/agents/agent.go internal/agents/agent_handler.go specs/003-agent-runtime/contracts/openapi.yaml
git commit -m "feat(agents): expose retriage_on_reply on the agent API (manyforge-deo.1)"
```

Expected: build PASS.

---

## Task 5: Store methods — `EnabledRetriageAgentsForBusiness` + `EnqueueReplyRetriageRun`

**Files:**
- Modify: `internal/agents/agent_run.go` (add two raw-pgx methods on `*AgentRunStore`)

These call the new DEFINERs via raw pgx (the repo convention — sqlc cannot introspect `RETURNS TABLE`/scalar functions). Mirror the existing `EnabledAgentsForBusiness` / `ClaimNextQueuedRun` shapes exactly.

- [ ] **Step 1: Add `EnabledRetriageAgentsForBusiness`**

In `internal/agents/agent_run.go`, directly below `EnabledAgentsForBusiness`, add:

```go
// EnabledRetriageAgentsForBusiness lists the enabled agents for a business that have opted
// in to reply re-triage (retriage_on_reply = true), via the system-wide SECURITY DEFINER fn.
// Principal-less (the message.received subscriber has no principal GUC); the fn is scoped by
// business_id AND tenant_root_id so a cross-tenant event can never surface another tenant's
// agents. Mirrors EnabledAgentsForBusiness.
func (s *AgentRunStore) EnabledRetriageAgentsForBusiness(ctx context.Context, businessID, tenantRootID uuid.UUID) ([]AgentRef, error) {
	var refs []AgentRef
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx,
			"SELECT agent_id, principal_id FROM enabled_retriage_agents_for_business($1, $2)",
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
		return nil, fmt.Errorf("agents: list retriage agents: %w", err)
	}
	return refs, nil
}
```

- [ ] **Step 2: Add `EnqueueReplyRetriageRun`**

Below that, add the scalar-returning DEFINER call (principal-less, `WithTx`):

```go
// EnqueueReplyRetriageRun runs the atomic guard+cap+dedup-insert DEFINER for one
// (message, agent) pair and returns its text outcome (one of: enqueued, skipped_not_inbound,
// skipped_auto_reply, skipped_capped, skipped_dedup). Principal-less (mirrors
// ClaimNextQueuedRun): the DEFINER derives business/tenant from the message row.
func (s *AgentRunStore) EnqueueReplyRetriageRun(ctx context.Context, messageID, agentID uuid.UUID, cap int) (string, error) {
	var outcome string
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			"SELECT enqueue_reply_retriage_run($1, $2, $3)",
			messageID, agentID, cap).Scan(&outcome)
	})
	if err != nil {
		return "", fmt.Errorf("agents: enqueue reply retriage run: %w", err)
	}
	return outcome, nil
}
```

- [ ] **Step 3: Build**

```bash
export PATH="$HOME/go/bin:$PATH"
go build ./internal/agents/...
```

Expected: PASS. (Commit with the trigger in Task 8.)

---

## Task 6: `events.TopicMessageReceived` constant

**Files:**
- Modify: `internal/platform/events/bus.go` (add the constant)
- Modify: `internal/inbox/service.go` (emit via the constant)

- [ ] **Step 1: Add the constant**

In `internal/platform/events/bus.go`, alongside `TopicTicketCreated = "ticket.created"` (around line 31), add:

```go
	// TopicMessageReceived fans out on every non-duplicate inbound message (including the
	// first message of a brand-new ticket, which ALSO emits TopicTicketCreated). Consumed by
	// the reply re-triage trigger (manyforge-deo.1).
	TopicMessageReceived = "message.received"
```

- [ ] **Step 2: Emit via the constant**

In `internal/inbox/service.go`, change the bare-string emit to use the constant:

```go
		if err := events.Enqueue(ctx, tx, r.tenantRootID, events.TopicMessageReceived, map[string]any{
			"ticket_id":   out.TicketID,
			"business_id": r.businessID,
			"message_id":  out.MessageID,
		}); err != nil {
			return err
		}
```

- [ ] **Step 3: Build + run inbox tests**

```bash
export PATH="$HOME/go/bin:$PATH"
go build ./...
make test 2>&1 | tail -5
```

Expected: build PASS; existing inbox tests still pass (the emitted string value is unchanged — only the literal became a constant). Commit with Task 8.

---

## Task 7: Config key `MANYFORGE_AGENT_RETRIAGE_CAP_PER_HOUR` (TDD)

**Files:**
- Modify: `internal/platform/config/config_test.go` (extend `TestLoadAgentRunLimits`)
- Modify: `internal/platform/config/config.go` (struct field + load line)
- Modify: `.env.example`

- [ ] **Step 1: Write the failing test assertions**

In `internal/platform/config/config_test.go`, extend `TestLoadAgentRunLimits`. In the `defaults` subtest add:

```go
		if cfg.AgentRetriageCapPerHour != 5 {
			t.Errorf("AgentRetriageCapPerHour = %d, want 5", cfg.AgentRetriageCapPerHour)
		}
```

In the `overrides` subtest add the env set and assertion:

```go
		t.Setenv("MANYFORGE_AGENT_RETRIAGE_CAP_PER_HOUR", "9")
```

and extend the combined override assertion to include `|| cfg.AgentRetriageCapPerHour != 9`.

- [ ] **Step 2: Run it to verify it fails**

```bash
export PATH="$HOME/go/bin:$PATH"
go test ./internal/platform/config/ -run TestLoadAgentRunLimits -v
```

Expected: FAIL — `cfg.AgentRetriageCapPerHour` undefined (compile error) or 0 ≠ 5.

- [ ] **Step 3: Add the struct field**

In `internal/platform/config/config.go`, in the agent-bounds block (around lines 88–92), add:

```go
	AgentTemperature        float64       // MANYFORGE_AGENT_TEMPERATURE (default 0.0; deterministic)
	AgentRetriageCapPerHour int           // MANYFORGE_AGENT_RETRIAGE_CAP_PER_HOUR (default 5; per-ticket/agent reply re-triage cap)
}
```

- [ ] **Step 4: Add the load line**

In `Load()`, after the `AgentTemperature` load line (before `return cfg, nil`), add:

```go
	if cfg.AgentRetriageCapPerHour, err = envInt("MANYFORGE_AGENT_RETRIAGE_CAP_PER_HOUR", 5); err != nil {
		return Config{}, fmt.Errorf("MANYFORGE_AGENT_RETRIAGE_CAP_PER_HOUR: %w", err)
	}
```

- [ ] **Step 5: Add to `.env.example`**

In the agent bounds section of `.env.example`, add:

```
MANYFORGE_AGENT_RETRIAGE_CAP_PER_HOUR=5
```

- [ ] **Step 6: Run the test to verify it passes**

```bash
export PATH="$HOME/go/bin:$PATH"
go test ./internal/platform/config/ -run TestLoadAgentRunLimits -v
```

Expected: PASS. Commit with Task 8.

---

## Task 8: `ReplyRetriageTrigger` (TDD with a fake store)

**Files:**
- Create: `internal/agents/reply_trigger.go`
- Create: `internal/agents/reply_trigger_test.go`

- [ ] **Step 1: Write the failing unit test**

Create `internal/agents/reply_trigger_test.go` (mirrors the `fakeTriggerStore` pattern in `trigger_test.go`; no build tag, no infra):

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

type fakeReplyStore struct {
	refs        []AgentRef
	refsErr     error
	enqueued    []enqCall
	enqOutcome  string
	enqErr      error
	lastTenant  uuid.UUID
	lastBizID   uuid.UUID
}

type enqCall struct {
	messageID uuid.UUID
	agentID   uuid.UUID
	cap       int
}

func (f *fakeReplyStore) EnabledRetriageAgentsForBusiness(_ context.Context, businessID, tenantRootID uuid.UUID) ([]AgentRef, error) {
	f.lastBizID, f.lastTenant = businessID, tenantRootID
	return f.refs, f.refsErr
}

func (f *fakeReplyStore) EnqueueReplyRetriageRun(_ context.Context, messageID, agentID uuid.UUID, cap int) (string, error) {
	f.enqueued = append(f.enqueued, enqCall{messageID, agentID, cap})
	if f.enqOutcome == "" {
		return "enqueued", f.enqErr
	}
	return f.enqOutcome, f.enqErr
}

func replyEvent(t *testing.T, ticketID, businessID, messageID, tenantRootID uuid.UUID) events.Event {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"ticket_id": ticketID, "business_id": businessID, "message_id": messageID,
	})
	return events.Event{ID: uuid.New(), TenantRootID: tenantRootID, Payload: payload}
}

// One enqueue per opted-in agent, with the configured cap and the payload's message id.
func TestReplyRetriageTrigger_EnqueuesPerAgent(t *testing.T) {
	a1, a2 := AgentRef{AgentID: uuid.New(), PrincipalID: uuid.New()}, AgentRef{AgentID: uuid.New(), PrincipalID: uuid.New()}
	store := &fakeReplyStore{refs: []AgentRef{a1, a2}}
	trig := &ReplyRetriageTrigger{Runs: store, RetriageCap: 7}
	tid, bid, mid, troot := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	if err := trig.Handle(context.Background(), pgx.Tx(nil), replyEvent(t, tid, bid, mid, troot)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(store.enqueued) != 2 {
		t.Fatalf("enqueued %d times, want 2", len(store.enqueued))
	}
	for i, c := range store.enqueued {
		if c.messageID != mid {
			t.Errorf("call %d messageID=%s, want %s", i, c.messageID, mid)
		}
		if c.cap != 7 {
			t.Errorf("call %d cap=%d, want 7", i, c.cap)
		}
	}
	if store.lastTenant != troot || store.lastBizID != bid {
		t.Errorf("lister scoped to (%s,%s), want (%s,%s)", store.lastBizID, store.lastTenant, bid, troot)
	}
}

// Zero/unset cap backstops to the default 5 (a misconfig must not disable re-triage).
func TestReplyRetriageTrigger_CapDefaults(t *testing.T) {
	store := &fakeReplyStore{refs: []AgentRef{{AgentID: uuid.New(), PrincipalID: uuid.New()}}}
	trig := &ReplyRetriageTrigger{Runs: store} // RetriageCap unset => 0
	if err := trig.Handle(context.Background(), pgx.Tx(nil), replyEvent(t, uuid.New(), uuid.New(), uuid.New(), uuid.New())); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(store.enqueued) != 1 || store.enqueued[0].cap != 5 {
		t.Fatalf("cap backstop: got %+v, want one call with cap=5", store.enqueued)
	}
}

// A poison payload is logged and treated as processed (return nil — do not retry forever).
func TestReplyRetriageTrigger_BadPayloadIsProcessed(t *testing.T) {
	store := &fakeReplyStore{}
	trig := &ReplyRetriageTrigger{Runs: store}
	ev := events.Event{ID: uuid.New(), TenantRootID: uuid.New(), Payload: []byte("{not json")}
	if err := trig.Handle(context.Background(), pgx.Tx(nil), ev); err != nil {
		t.Fatalf("bad payload should return nil, got %v", err)
	}
	if len(store.enqueued) != 0 {
		t.Fatalf("bad payload must not enqueue, got %d", len(store.enqueued))
	}
}

// A transient lister error reschedules (returns the error).
func TestReplyRetriageTrigger_ListerErrorReschedules(t *testing.T) {
	store := &fakeReplyStore{refsErr: context.DeadlineExceeded}
	trig := &ReplyRetriageTrigger{Runs: store}
	if err := trig.Handle(context.Background(), pgx.Tx(nil), replyEvent(t, uuid.New(), uuid.New(), uuid.New(), uuid.New())); err == nil {
		t.Fatal("transient lister error must be returned (reschedule), got nil")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH="$HOME/go/bin:$PATH"
go test ./internal/agents/ -run TestReplyRetriageTrigger -v
```

Expected: FAIL — `ReplyRetriageTrigger` undefined (compile error).

- [ ] **Step 3: Write `reply_trigger.go`**

Create `internal/agents/reply_trigger.go`:

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

// defaultRetriageCap backstops a zero/unset RetriageCap so a config miss never silently
// disables (cap=0) OR uncaps re-triage.
const defaultRetriageCap = 5

// messageReceivedPayload is the consumer-owned decode of the inbox-produced message.received
// event. message_id is the ticket_message ROW id (uuid), same shape as ticket.created.
type messageReceivedPayload struct {
	TicketID   uuid.UUID `json:"ticket_id"`
	BusinessID uuid.UUID `json:"business_id"`
	MessageID  uuid.UUID `json:"message_id"`
}

// replyTriggerStore is the trigger's view of the run store (fakeable).
type replyTriggerStore interface {
	EnabledRetriageAgentsForBusiness(ctx context.Context, businessID, tenantRootID uuid.UUID) ([]AgentRef, error)
	EnqueueReplyRetriageRun(ctx context.Context, messageID, agentID uuid.UUID, cap int) (string, error)
}

// ReplyRetriageTrigger subscribes to message.received and re-invokes each opted-in, enabled
// agent in the business when a customer replies to an existing ticket. It runs in the outbox
// worker — fast, principal-less, idempotent.
//
// SEPARATE from TriageTrigger (which is ticket.created-only): the loop-guard lives in the
// enqueue_reply_retriage_run DEFINER (inbound AND NOT is_auto_reply + per-ticket/agent hourly
// cap + dedup on the reply message id). The same dedup key as the ticket.created 'event' run
// collapses a new ticket's first message to skipped_dedup, so a fresh ticket runs once.
type ReplyRetriageTrigger struct {
	Runs        replyTriggerStore
	RetriageCap int
	Logger      *slog.Logger
}

func (t *ReplyRetriageTrigger) cap() int {
	if t.RetriageCap <= 0 {
		return defaultRetriageCap
	}
	return t.RetriageCap
}

// Handle implements events.Handler. Idempotent: the DEFINER dedups on the reply message id.
func (t *ReplyRetriageTrigger) Handle(ctx context.Context, _ pgx.Tx, ev events.Event) error {
	var p messageReceivedPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		// Poison payload: log + treat as processed (the producer is trusted, in-process).
		t.logger().ErrorContext(ctx, "reply retriage: bad message.received payload", "event_id", ev.ID, "err", err)
		return nil
	}
	refs, err := t.Runs.EnabledRetriageAgentsForBusiness(ctx, p.BusinessID, ev.TenantRootID)
	if err != nil {
		return err // transient → reschedule
	}
	for _, ref := range refs {
		outcome, eErr := t.Runs.EnqueueReplyRetriageRun(ctx, p.MessageID, ref.AgentID, t.cap())
		if eErr != nil {
			return eErr // reschedule; the DEFINER is idempotent (dedup), so a retry is safe
		}
		t.logger().DebugContext(ctx, "reply retriage",
			"agent_id", ref.AgentID, "message_id", p.MessageID, "outcome", outcome)
	}
	return nil
}

func (t *ReplyRetriageTrigger) logger() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
export PATH="$HOME/go/bin:$PATH"
go test ./internal/agents/ -run TestReplyRetriageTrigger -v
```

Expected: PASS (all four subtests). Also confirm `*AgentRunStore` satisfies `replyTriggerStore`:

```bash
go build ./internal/agents/...
```

- [ ] **Step 5: Commit Tasks 5–8 as one coherent runtime change**

```bash
export PATH="$HOME/go/bin:$PATH"
make test 2>&1 | tail -5   # full unit suite green
git add internal/agents/agent_run.go internal/agents/reply_trigger.go internal/agents/reply_trigger_test.go \
        internal/platform/events/bus.go internal/inbox/service.go \
        internal/platform/config/config.go internal/platform/config/config_test.go .env.example
git commit -m "feat(agents): ReplyRetriageTrigger + store + config + message.received topic (manyforge-deo.1)"
```

---

## Task 9: Wire the trigger into `main.go`

**Files:**
- Modify: `cmd/manyforge/main.go` (construct + subscribe)

- [ ] **Step 1: Construct the trigger next to `triageTrigger`**

In `cmd/manyforge/main.go`, next to the `triageTrigger := &agents.TriageTrigger{...}` line, add:

```go
	triageTrigger := &agents.TriageTrigger{Runs: agentRunStore, Logger: logger}
	replyRetriageTrigger := &agents.ReplyRetriageTrigger{Runs: agentRunStore, RetriageCap: cfg.AgentRetriageCapPerHour, Logger: logger}
```

- [ ] **Step 2: Subscribe it to `message.received` next to the `ticket.created` subscription**

After the `eventBus.Subscribe(events.TopicTicketCreated, triageTrigger.Handle)` line, add:

```go
	// US5 follow-up (manyforge-deo.1): an OPTED-IN agent re-runs when a customer replies to
	// an existing ticket. message.received also fires for a new ticket's first message, but
	// that shares the ticket.created run's dedup key, so a fresh ticket still runs once.
	// Guarded in the enqueue_reply_retriage_run DEFINER (inbound-only + per-ticket/agent cap).
	eventBus.Subscribe(events.TopicMessageReceived, replyRetriageTrigger.Handle)
```

- [ ] **Step 3: Build**

```bash
export PATH="$HOME/go/bin:$PATH"
go build ./...
```

Expected: PASS. (Commit with the pins in Task 10 so the narrowed pin and the new subscription land together — otherwise `make sec-test` is briefly red.)

---

## Task 10: Security pins — narrow the old, add the new

**Files:**
- Modify: `internal/security_regression/agent_run_us5_pins_test.go` (narrow `TestPin_TriageTriggerOnlyTicketCreated`)
- Create: `internal/security_regression/reply_retriage_pins_test.go`

- [ ] **Step 1: Narrow the existing triage pin**

The current `TestPin_TriageTriggerOnlyTicketCreated` forbids ANY `Subscribe("message.received"...)` — which the new (correctly-guarded) `replyRetriageTrigger` subscription would trip. Narrow the forbidden list to target the **triage** trigger binding specifically. In `internal/security_regression/agent_run_us5_pins_test.go`, replace the forbidden-fragment loop:

```go
	// The TRIAGE trigger must NEVER subscribe to message.received (that reopens the
	// agent-reply loop). The separately-guarded ReplyRetriageTrigger (manyforge-deo.1) may —
	// so forbid only the triage handler binding to message.received, not the bare topic.
	for _, bad := range []string{
		"Subscribe(events.TopicMessageReceived, triageTrigger.Handle)",
		`Subscribe("message.received", triageTrigger.Handle)`,
	} {
		if strings.Contains(mainGo, bad) {
			t.Errorf("main.go: triage must NOT subscribe to message.received (%q) — that reopens the agent-reply loop", bad)
		}
	}
```

- [ ] **Step 2: Run the narrowed pin to confirm it still passes**

```bash
export PATH="$HOME/go/bin:$PATH"
go test ./internal/security_regression/ -run TestPin_TriageTriggerOnlyTicketCreated -v
```

Expected: PASS (the triage trigger still subscribes only to `ticket.created`; the reply trigger uses a different handler var, so it is not flagged).

- [ ] **Step 3: Add the new pins**

Create `internal/security_regression/reply_retriage_pins_test.go`:

```go
// No build tag: these source-level pins run in `make test` and `make sec-test` with NO
// infrastructure, complementing the behavioral integration tests in internal/agents/. They
// make a refactor that silently drops a manyforge-deo.1 protection fail the security gate.
//
// Contract: Spec 003 US5 follow-up (manyforge-deo.1) — opt-in reply re-triage is loop-guarded
// (inbound-only + per-(ticket,agent) hourly cap + dedup) and the run-claim tolerates a missing
// agent instead of stalling the queue head.

package security_regression

import (
	"strings"
	"testing"
)

// TestPin_ReplyRetriageSubscribed pins that the SEPARATE reply trigger is wired to
// message.received (distinct handler from triageTrigger, which stays ticket.created-only).
func TestPin_ReplyRetriageSubscribed(t *testing.T) {
	mainGo := mustRead(t, "../../cmd/manyforge/main.go")
	if !strings.Contains(mainGo, "eventBus.Subscribe(events.TopicMessageReceived, replyRetriageTrigger.Handle)") {
		t.Error("main.go: ReplyRetriageTrigger must subscribe to events.TopicMessageReceived")
	}
}

// TestPin_ReplyRetriageGuarded pins the loop-guard + cap + dedup + suppression audit inside
// the enqueue DEFINER. Dropping any of these reopens unbounded agent↔customer amplification.
func TestPin_ReplyRetriageGuarded(t *testing.T) {
	mig := mustRead(t, "../../migrations/0052_agent_retriage.up.sql")
	for _, frag := range []string{
		"CREATE FUNCTION enqueue_reply_retriage_run(p_message_id uuid, p_agent_id uuid, p_cap integer)",
		"v_direction <> 'inbound'",                   // loop-guard: inbound only
		"IF v_is_auto_reply THEN",                    // loop-guard: skip auto-replies
		"trigger = 'reply'",                          // cap counts reply runs only
		"now() - interval '1 hour'",                  // per-hour window
		"v_recent >= p_cap",                          // the cap comparison
		"'agent.retriage_suppressed'",                // capped-case audit
		"ON CONFLICT (agent_id, trigger_dedup_key) WHERE trigger_dedup_key IS NOT NULL DO NOTHING", // dedup
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0052 up: missing reply-retriage guard fragment %q", frag)
		}
	}
}

// TestPin_ReplyRetriageTenantScopedAndHardened pins tenant isolation on the lister + the
// SECURITY DEFINER hardening (pinned search_path + REVOKE FROM PUBLIC + GRANT to app role).
func TestPin_ReplyRetriageTenantScopedAndHardened(t *testing.T) {
	mig := mustRead(t, "../../migrations/0052_agent_retriage.up.sql")
	for _, frag := range []string{
		"enabled_retriage_agents_for_business",
		"retriage_on_reply = true",
		"business_id = p_business_id",
		"tenant_root_id = p_tenant_root_id",
		"SECURITY DEFINER SET search_path = public",
		"REVOKE ALL ON FUNCTION enqueue_reply_retriage_run(uuid, uuid, integer) FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION enqueue_reply_retriage_run(uuid, uuid, integer) TO manyforge_app",
		"REVOKE ALL ON FUNCTION enabled_retriage_agents_for_business(uuid, uuid) FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION enabled_retriage_agents_for_business(uuid, uuid) TO manyforge_app",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0052 up: missing tenant-scope/hardening fragment %q", frag)
		}
	}
}

// TestPin_ClaimToleratesMissingAgent pins the Part B hardening: the rewritten claim is
// plpgsql, marks an orphaned run failed, and CONTINUEs the loop (drains next, never stalls).
func TestPin_ClaimToleratesMissingAgent(t *testing.T) {
	mig := mustRead(t, "../../migrations/0052_agent_retriage.up.sql")
	for _, frag := range []string{
		"DROP FUNCTION claim_next_queued_agent_run();",
		"LANGUAGE plpgsql SECURITY DEFINER SET search_path = public",
		"FOR UPDATE SKIP LOCKED",
		"status = 'failed', error = 'agent no longer exists'",
		"CONTINUE;",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0052 up: missing claim-hardening fragment %q", frag)
		}
	}
}
```

- [ ] **Step 4: Run sec-test + commit Tasks 9+10 together**

```bash
export PATH="$HOME/go/bin:$PATH"
make test 2>&1 | tail -5
make sec-test 2>&1 | tail -15
git add cmd/manyforge/main.go internal/security_regression/agent_run_us5_pins_test.go \
        internal/security_regression/reply_retriage_pins_test.go
git commit -m "feat(agents): wire ReplyRetriageTrigger + security pins (manyforge-deo.1)"
```

Expected: both gates green.

---

## Task 11: Integration tests (behavioral)

**Files:**
- Create: `internal/agents/reply_retriage_integration_test.go` (`//go:build integration`)

Reuses `seedRunTenant` / `seedRunTicket` (in `run_integration_test.go`) and `tdb.Super` / `tdb.App` (`testdb`). The trigger is driven end-to-end via `ReplyRetriageTrigger.Handle` with `&AgentRunStore{DB: tdb.App}`; agents are created via the real `AgentService`. Each case asserts `agent_run` / `audit_entry` rows through `tdb.Super`.

- [ ] **Step 1: Write the test file with helpers + the cases**

Create `internal/agents/reply_retriage_integration_test.go`:

```go
//go:build integration

package agents

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/events"
)

// seedReplyMessage inserts an additional ticket_message on an existing ticket via the
// RLS-exempt Super pool. direction is 'inbound' (author NULL) | 'outbound'|'note' (author set).
func seedReplyMessage(ctx context.Context, t *testing.T, tdb *testdb.TestDB, s runSeed, ticketID uuid.UUID, direction string, isAuto bool) uuid.UUID {
	t.Helper()
	msgID := uuid.New()
	var author any
	if direction == "inbound" {
		author = nil
	} else {
		author = s.ownerID // outbound/note require a non-null author_principal_id (CHECK)
	}
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO ticket_message (id,ticket_id,business_id,tenant_root_id,direction,author_principal_id,message_id,"references",body_text,is_auto_reply,created_at)
		 VALUES ($1,$2,$3,$3,$4::ticket_message_direction,$5,$6,'{}','reply body',$7,now())`,
		msgID, ticketID, s.businessID, direction, author, "rm-"+msgID.String()+"@example.com", isAuto); err != nil {
		t.Fatalf("seed reply message (%s): %v", direction, err)
	}
	return msgID
}

// createRetriageAgent creates an enabled agent via the real service; optIn toggles retriage_on_reply.
func createRetriageAgent(ctx context.Context, t *testing.T, tdb *testdb.TestDB, s runSeed, name string, optIn bool) Agent {
	t.Helper()
	svc := &AgentService{DB: tdb.App}
	ag, err := svc.Create(ctx, s.ownerID, s.businessID, CreateAgentInput{
		Name: name, Provider: "anthropic", Model: "claude-sonnet-4-5",
		SystemPrompt: "Triage.", AllowedTools: []string{"read_ticket", "draft_reply"},
		AutonomyMode: ModeAssist, Enabled: true, RetriageOnReply: optIn,
	})
	if err != nil {
		t.Fatalf("create agent %q: %v", name, err)
	}
	return ag
}

// fireReply drives the trigger end-to-end for one message.received event.
func fireReply(ctx context.Context, t *testing.T, tdb *testdb.TestDB, s runSeed, ticketID, messageID uuid.UUID, cap int) {
	t.Helper()
	trig := &ReplyRetriageTrigger{Runs: &AgentRunStore{DB: tdb.App}, RetriageCap: cap}
	payload, _ := json.Marshal(map[string]any{
		"ticket_id": ticketID, "business_id": s.businessID, "message_id": messageID,
	})
	ev := events.Event{ID: uuid.New(), TenantRootID: s.tenantRootID, Payload: payload}
	if err := trig.Handle(ctx, nil, ev); err != nil {
		t.Fatalf("trigger Handle: %v", err)
	}
}

func replyRunCount(ctx context.Context, t *testing.T, tdb *testdb.TestDB, agentID, ticketID uuid.UUID) int {
	return countSuperRows(ctx, t, tdb,
		`SELECT count(*) FROM agent_run WHERE agent_id=$1 AND target_id=$2 AND trigger='reply'`,
		agentID, ticketID)
}

// Case 1: opted-in agent + genuine customer reply => one queued run with trigger='reply'.
func TestReplyRetriage_OptedInGenuineReply(t *testing.T) {
	ctx := context.Background()
	tdb := testdb.Start(ctx, t)
	s := seedRunTenant(ctx, t, tdb)
	ag := createRetriageAgent(ctx, t, tdb, "Opted In", true)
	ticketID := seedRunTicket(ctx, t, tdb, s, "open")
	mid := seedReplyMessage(ctx, t, tdb, s, ticketID, "inbound", false)

	fireReply(ctx, t, tdb, s, ticketID, mid, 5)

	if got := replyRunCount(ctx, t, tdb, ag.ID, ticketID); got != 1 {
		t.Fatalf("reply runs = %d, want 1", got)
	}
	if n := countSuperRows(ctx, t, tdb,
		`SELECT count(*) FROM agent_run WHERE agent_id=$1 AND target_id=$2 AND trigger='reply' AND status='queued'`,
		ag.ID, ticketID); n != 1 {
		t.Fatalf("queued reply runs = %d, want 1", n)
	}
}

// Case 2: opted-out agent => no run.
func TestReplyRetriage_OptedOutNoRun(t *testing.T) {
	ctx := context.Background()
	tdb := testdb.Start(ctx, t)
	s := seedRunTenant(ctx, t, tdb)
	ag := createRetriageAgent(ctx, t, tdb, "Opted Out", false)
	ticketID := seedRunTicket(ctx, t, tdb, s, "open")
	mid := seedReplyMessage(ctx, t, tdb, s, ticketID, "inbound", false)

	fireReply(ctx, t, tdb, s, ticketID, mid, 5)

	if got := replyRunCount(ctx, t, tdb, ag.ID, ticketID); got != 0 {
		t.Fatalf("reply runs = %d, want 0 (opted out)", got)
	}
}

// Case 3: is_auto_reply reply => no run (skipped_auto_reply).
func TestReplyRetriage_AutoReplySkipped(t *testing.T) {
	ctx := context.Background()
	tdb := testdb.Start(ctx, t)
	s := seedRunTenant(ctx, t, tdb)
	ag := createRetriageAgent(ctx, t, tdb, "Opted In", true)
	ticketID := seedRunTicket(ctx, t, tdb, s, "open")
	mid := seedReplyMessage(ctx, t, tdb, s, ticketID, "inbound", true) // is_auto_reply

	fireReply(ctx, t, tdb, s, ticketID, mid, 5)

	if got := replyRunCount(ctx, t, tdb, ag.ID, ticketID); got != 0 {
		t.Fatalf("reply runs = %d, want 0 (auto-reply)", got)
	}
}

// Case 4: outbound/note message => no run (skipped_not_inbound).
func TestReplyRetriage_OutboundSkipped(t *testing.T) {
	ctx := context.Background()
	tdb := testdb.Start(ctx, t)
	s := seedRunTenant(ctx, t, tdb)
	ag := createRetriageAgent(ctx, t, tdb, "Opted In", true)
	ticketID := seedRunTicket(ctx, t, tdb, s, "open")
	mid := seedReplyMessage(ctx, t, tdb, s, ticketID, "note", false)

	fireReply(ctx, t, tdb, s, ticketID, mid, 5)

	if got := replyRunCount(ctx, t, tdb, ag.ID, ticketID); got != 0 {
		t.Fatalf("reply runs = %d, want 0 (outbound/note)", got)
	}
}

// Case 5: cap — the (cap+1)th reply within an hour is suppressed + audited; earlier ones enqueue.
func TestReplyRetriage_CapSuppressesAndAudits(t *testing.T) {
	ctx := context.Background()
	tdb := testdb.Start(ctx, t)
	s := seedRunTenant(ctx, t, tdb)
	ag := createRetriageAgent(ctx, t, tdb, "Opted In", true)
	ticketID := seedRunTicket(ctx, t, tdb, s, "open")

	const cap = 2
	// cap distinct replies all enqueue (distinct message ids => no dedup collision).
	for i := 0; i < cap; i++ {
		mid := seedReplyMessage(ctx, t, tdb, s, ticketID, "inbound", false)
		fireReply(ctx, t, tdb, s, ticketID, mid, cap)
	}
	if got := replyRunCount(ctx, t, tdb, ag.ID, ticketID); got != cap {
		t.Fatalf("reply runs after %d replies = %d, want %d", cap, got, cap)
	}
	// The (cap+1)th is suppressed (count of prior reply runs >= cap) and audited.
	overflow := seedReplyMessage(ctx, t, tdb, s, ticketID, "inbound", false)
	fireReply(ctx, t, tdb, s, ticketID, overflow, cap)

	if got := replyRunCount(ctx, t, tdb, ag.ID, ticketID); got != cap {
		t.Fatalf("reply runs after overflow = %d, want %d (capped)", got, cap)
	}
	if n := countSuperRows(ctx, t, tdb,
		`SELECT count(*) FROM audit_entry WHERE action='agent.retriage_suppressed' AND target_id=$1`,
		ticketID); n != 1 {
		t.Fatalf("retriage_suppressed audit rows = %d, want 1", n)
	}
}

// Case 6: two opted-in agents, one reply => two runs (per-agent cap, not a shared budget).
func TestReplyRetriage_PerAgentNotShared(t *testing.T) {
	ctx := context.Background()
	tdb := testdb.Start(ctx, t)
	s := seedRunTenant(ctx, t, tdb)
	a1 := createRetriageAgent(ctx, t, tdb, "Agent One", true)
	a2 := createRetriageAgent(ctx, t, tdb, "Agent Two", true)
	ticketID := seedRunTicket(ctx, t, tdb, s, "open")
	mid := seedReplyMessage(ctx, t, tdb, s, ticketID, "inbound", false)

	fireReply(ctx, t, tdb, s, ticketID, mid, 5)

	if got := replyRunCount(ctx, t, tdb, a1.ID, ticketID); got != 1 {
		t.Fatalf("agent1 reply runs = %d, want 1", got)
	}
	if got := replyRunCount(ctx, t, tdb, a2.ID, ticketID); got != 1 {
		t.Fatalf("agent2 reply runs = %d, want 1", got)
	}
}

// Case 7 (dedup loop-guard): two message.received deliveries of the SAME message id => one run.
func TestReplyRetriage_RedeliveryDedups(t *testing.T) {
	ctx := context.Background()
	tdb := testdb.Start(ctx, t)
	s := seedRunTenant(ctx, t, tdb)
	ag := createRetriageAgent(ctx, t, tdb, "Opted In", true)
	ticketID := seedRunTicket(ctx, t, tdb, s, "open")
	mid := seedReplyMessage(ctx, t, tdb, s, ticketID, "inbound", false)

	fireReply(ctx, t, tdb, s, ticketID, mid, 5)
	fireReply(ctx, t, tdb, s, ticketID, mid, 5) // at-least-once redelivery

	if got := replyRunCount(ctx, t, tdb, ag.ID, ticketID); got != 1 {
		t.Fatalf("reply runs after redelivery = %d, want 1 (deduped)", got)
	}
}

// Case 8 (claim hardening): a queued run with a missing agent is failed; a valid run drains.
func TestClaim_ToleratesOrphanedRun(t *testing.T) {
	ctx := context.Background()
	tdb := testdb.Start(ctx, t)
	s := seedRunTenant(ctx, t, tdb)
	ag := createRetriageAgent(ctx, t, tdb, "Valid", false)
	ticketID := seedRunTicket(ctx, t, tdb, s, "open")

	// Orphan: a queued run pointing at a non-existent agent. The agent_run->agent FK blocks
	// this normally, so disable FK/trigger enforcement for just this seed tx (superuser only).
	orphanRunID := uuid.New()
	orphanAgentID := uuid.New()
	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin orphan seed: %v", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("disable FK triggers: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_run (id,agent_id,business_id,tenant_root_id,trigger,status,correlation_id,created_at,updated_at)
		 VALUES ($1,$2,$3,$3,'manual','queued',$4,now()-interval '5 minutes',now())`,
		orphanRunID, orphanAgentID, s.businessID, "corr-"+orphanRunID.String()); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("seed orphan run: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit orphan seed: %v", err)
	}

	// A valid, newer queued run for the real agent.
	validRunID := uuid.New()
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO agent_run (id,agent_id,business_id,tenant_root_id,trigger,target_type,target_id,status,correlation_id,created_at,updated_at)
		 VALUES ($1,$2,$3,$3,'manual','ticket',$4,'queued',$5,now(),now())`,
		validRunID, ag.ID, s.businessID, ticketID, "corr-"+validRunID.String()); err != nil {
		t.Fatalf("seed valid run: %v", err)
	}

	claimed, err := (&AgentRunStore{DB: tdb.App}).ClaimNextQueuedRun(ctx)
	if err != nil {
		t.Fatalf("ClaimNextQueuedRun: %v", err)
	}
	if claimed == nil || claimed.RunID != validRunID {
		t.Fatalf("claimed = %+v, want the valid run %s (orphan must be skipped)", claimed, validRunID)
	}
	if st := superRunStatus(ctx, t, tdb, orphanRunID); st != "failed" {
		t.Fatalf("orphan run status = %q, want failed", st)
	}
}

func superRunStatus(ctx context.Context, t *testing.T, tdb *testdb.TestDB, runID uuid.UUID) string {
	t.Helper()
	var st string
	if err := tdb.Super.QueryRow(ctx, `SELECT status FROM agent_run WHERE id=$1`, runID).Scan(&st); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	return st
}
```

> **Verify before running:** confirm `ModeAssist` is the correct exported autonomy-mode constant in `internal/agents/` (the US5 integration test uses it). Confirm `ClaimNextQueuedRun` returns a `*ClaimedRun` with a `.RunID` field (it does — see `agent_run.go`). Confirm `testdb.Start(ctx, t)` is the constructor used by the other integration tests; if the existing tests use a different setup (e.g. a package-level `testMain` pool), mirror that exact pattern instead.

- [ ] **Step 2: Run the integration suite**

```bash
export PATH="$HOME/go/bin:$PATH"
go test -tags integration ./internal/agents/ -run 'TestReplyRetriage|TestClaim_ToleratesOrphanedRun' -v
```

Expected: all eight tests PASS. If `session_replication_role` is rejected, the `Super` role may not be a true superuser in this harness — fall back to seeding the orphan via a `tenant_root_id` that has no matching `agent` row (the FK is on `(agent_id, tenant_root_id)`, so a run whose `tenant_root_id` differs from any agent's still satisfies the composite FK only if a `business` row matches; if that path is also blocked, seed the orphan by deleting the agent AFTER inserting the run with `session_replication_role`/superuser — document whichever works).

- [ ] **Step 3: Commit**

```bash
git add internal/agents/reply_retriage_integration_test.go
git commit -m "test(agents): integration cases for reply re-triage + claim hardening (manyforge-deo.1)"
```

---

## Task 12: Full gate run + close the issue

**Files:** none (verification + bookkeeping)

- [ ] **Step 1: Run every gate green**

```bash
export PATH="$HOME/go/bin:$PATH"
go build ./...
make test
make sec-test
make lint
go test -tags integration ./internal/agents/... 2>&1 | tail -20
```

Expected: all exit 0. Fix anything red before proceeding (no "pre-existing failure" exceptions).

- [ ] **Step 2: Push and close**

```bash
git pull --rebase    # harmless if the bd hook re-dirties the journal; verify with the next line
git log origin/master..HEAD --oneline   # the 6 deo.1 commits
bd dolt push
git push
git status           # MUST show "up to date with origin/master"
bd close manyforge-deo.1
```

- [ ] **Step 3: Update the handoff** so the next session sees deo.1 done and `k0d` as the remaining feature.

---

## Self-review notes (author checklist — already applied)

- **Spec coverage:** opt-in flag (Tasks 1–4), new `ReplyRetriageTrigger` + `message.received` topic (Tasks 6, 8, 9), the atomic guard/cap/dedup DEFINER (Task 1), config cap key (Task 7), claim hardening (Task 1), pins narrowed+added (Task 10), all seven spec test cases + the redelivery-dedup case (Task 11). The spec's seven integration cases map to Cases 1–6 + 8; Case 7 (redelivery dedup) additionally pins the new-ticket double-emit loop-guard called out in reconciliation #5.
- **Type consistency:** the DEFINER signature is `(uuid, uuid, integer)` everywhere (function def, REVOKE/GRANT, pins, Go `EnqueueReplyRetriageRun`); the store method drops `p_agent_principal_id` (reconciliation #2); trigger var is `replyRetriageTrigger` consistently in `main.go` and both pins; `RetriageOnReply` is the field name across `Agent`/`CreateAgentInput`/`UpdateAgentInput`/`dbgen`/`agentResp`.
- **No placeholders:** every step has the literal code/SQL/command.
- **Known minor (documented, not a defect):** if the outbox ever processed `message.received` before `ticket.created` for a brand-new ticket's first message, the surviving run would be labeled `'reply'` instead of `'event'` (still exactly one run). Outbox drain order is `created_at` and `ticket.created` is enqueued first, so in practice the `'event'` run wins.
```