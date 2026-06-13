# Opt-in re-triage on customer reply + claim hardening — design (manyforge-deo.1)

**Status:** approved (brainstorm), pending implementation plan
**Date:** 2026-06-13
**Issue:** manyforge-deo.1 (parent epic manyforge-deo — Spec 003 Agent Runtime)

## Problem

Triage fires only on `ticket.created` (`internal/agents/trigger.go`). The loop-guard
is structural: an agent's own reply emits `ticket.replied`, never `ticket.created`,
so an agent reply can't re-trigger triage. A customer's *later reply* on an existing
ticket emits `message.received` (`internal/inbox/service.go:274`) — which nothing
consumes — so an agent is never re-invoked when the conversation continues.

Adding re-triage on `message.received` reintroduces loop risk (there is a security
pin, `TestPin_TriageTriggerOnlyTicketCreated`, forbidding exactly this), so it needs
a dedicated guard. A related latent bug: `claim_next_queued_agent_run()`
(migration 0034) inner-joins `agent` and the CTE picks the oldest queued run, so a
run whose `agent` row is missing would silently return "nothing to claim" and stall
the entire queue head.

## Goal

1. **Re-triage (opt-in):** when a customer replies to an existing ticket, re-invoke
   each opted-in, enabled agent in the business — bounded so no loop can run away.
2. **Claim hardening:** a queued run whose agent is missing must never stall the
   queue; resolve it terminally and drain the next run.

## Part A — re-triage on `message.received`

### Opt-in (per-agent)
- New column `agent.retriage_on_reply boolean NOT NULL DEFAULT false` (migration).
- Plumb through `CreateAgentInput` / `UpdateAgentInput`, the agent handler, the
  `agent` create/update sqlc queries, and `AgentView`. Regenerate dbgen with
  **`/opt/homebrew/bin/sqlc` (v1.27.0)**.

### New trigger
`ReplyRetriageTrigger` (new type in `internal/agents/`) subscribes to
`message.received`. `TriageTrigger` is untouched (still `ticket.created`-only).
Payload (already emitted): `{ticket_id, business_id, message_id}` where `message_id`
is the `ticket_message` **row id** (uuid) — `TriageTrigger` decodes it as `uuid.UUID`,
which an RFC text Message-ID could not be, confirming it is the row id. (Verify
`inbox.Service`'s `out.MessageID` is the row `id` before writing the DEFINER's
`WHERE id = p_message_id`.)

Handler (thin, runs in the outbox worker, principal-less, idempotent):
```
decode {ticket_id, business_id, message_id}
refs := Runs.EnabledRetriageAgentsForBusiness(business_id, tenant)  // enabled AND retriage_on_reply
for ref := range refs:
    outcome := Runs.EnqueueReplyRetriageRun(message_id, ref.AgentID, ref.PrincipalID, cap)
    // outcome ∈ {enqueued, skipped_not_inbound, skipped_auto_reply, skipped_capped, skipped_dedup}
    log at debug; the DEFINER already audits the capped case
return nil  // transient store errors → return err to reschedule
```
`EnabledRetriageAgentsForBusiness` is a new query = the existing
`EnabledAgentsForBusiness` predicate AND `retriage_on_reply = true`.

### The guard + cap + enqueue — ONE atomic SECURITY DEFINER
`enqueue_reply_retriage_run(p_message_id uuid, p_agent_id uuid,
p_agent_principal_id uuid, p_cap int) RETURNS text` (plpgsql, principal-less,
mirrors `claim_next_queued_agent_run` / the migration-0024 cap):

1. Load the message: `SELECT ticket_id, business_id, tenant_root_id, direction,
   is_auto_reply FROM ticket_message WHERE id = p_message_id`. No row → return
   `'skipped_not_inbound'` (defensive).
2. **Loop-guard:** `IF direction <> 'inbound' THEN return 'skipped_not_inbound'`;
   `IF is_auto_reply THEN return 'skipped_auto_reply'`. (A genuine human reply is
   `inbound, author NULL, is_auto_reply=false`; auto-responders are already mostly
   suppressed at ingest by the is_auto_reply cap, migration 0024 — this is the
   second line of defense and also bounds non-RFC bots via the cap below.)
3. **Per-(ticket, agent) cap:** count `agent_run WHERE agent_id = p_agent_id AND
   target_id = v_ticket_id AND trigger = 'reply' AND created_at > now() - interval
   '1 hour'`. `IF count >= p_cap THEN` write an `agent.retriage_suppressed`
   `audit_entry` (principal-less, like `ticket.loop_suppressed`) and return
   `'skipped_capped'`. Per-(ticket, agent) so N opted-in agents don't burn one
   shared budget — each agent is independently bounded.
4. **Enqueue (dedup):** `INSERT INTO agent_run (… trigger='reply',
   trigger_dedup_key = p_message_id::text, target_type='ticket',
   target_id = v_ticket_id, status='queued' …) ON CONFLICT (agent_id,
   trigger_dedup_key) DO NOTHING`. Inserted → `'enqueued'`; conflict →
   `'skipped_dedup'`. The insert column list MUST mirror `CreateEventRun`'s insert
   (read its DEFINER before writing this one).

`trigger='reply'` (distinct from the `ticket.created` trigger value) makes the cap
countable and tells the agent loop this run is a follow-up.

### Config key
`MANYFORGE_AGENT_RETRIAGE_CAP_PER_HOUR` (default **5**), loaded in `config` like the
manyforge-ji7 keys, stored on the `ReplyRetriageTrigger` (`RetriageCap int`,
defaulting to 5), passed as `p_cap` per call.

### Wiring (`cmd/manyforge/main.go`)
Subscribe `ReplyRetriageTrigger` to `message.received` alongside the existing
`TriageTrigger` (`ticket.created`) subscription; inject `RetriageCap` from config.

## Part B — claim hardening

Rewrite `claim_next_queued_agent_run()` (new migration; DROP + CREATE the DEFINER)
from a single SQL statement to a plpgsql loop that tolerates a missing agent:

```
LOOP
  SELECT id INTO v_id FROM agent_run
    WHERE status = 'queued'
      AND (no per-iteration skip set needed — see note)
    ORDER BY created_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1;
  EXIT WHEN v_id IS NULL;            -- queue empty
  SELECT ... INTO agent fields FROM agent WHERE id = ar.agent_id AND tenant_root_id = ...
  IF agent NOT FOUND THEN
    UPDATE agent_run SET status='failed', error='agent no longer exists', updated_at=now()
      WHERE id = v_id;              -- terminal; no longer 'queued', won't be re-selected
    CONTINUE;                        -- drain the next queued run, never stall the head
  END IF;
  UPDATE agent_run SET status='running', updated_at=now() WHERE id = v_id;
  RETURN QUERY SELECT <run + agent columns>;  -- same shape as today
  RETURN;
END LOOP;
```

Because the orphan is set to `failed` (no longer `queued`), the next `SELECT … status
= 'queued'` won't re-pick it — the loop terminates. Return shape is unchanged
(callers unaffected). `agent_run` already has `failed` status + an `error` column.

This is defensive: the `agent_run → agent` FK (`NO ACTION`, migration 0028:25) makes
a missing agent unreachable today (you can't delete an agent that has runs — which
correctly preserves accounting/audit provenance; retiring an agent is done via
`enabled=false`). Hardening removes the latent single-point queue stall. Full
agent-deletion-with-cleanup is intentionally **out of scope** (a separate
retention-policy decision).

## Security pins
- Narrow `TestPin_TriageTriggerOnlyTicketCreated`: assert the *ticket.created*
  `TriageTrigger` still does NOT subscribe to `message.received` (the re-triage path
  is the new, separately-guarded `ReplyRetriageTrigger`).
- New pin: `enqueue_reply_retriage_run` enforces the `inbound`/`is_auto_reply` skip
  and the per-(ticket,agent) cap (source pin on the DEFINER body + a behavioral
  integration assertion).

## Testing

Integration (`internal/agents`, `//go:build integration`):
1. Opted-in agent + genuine customer reply (`inbound`, `is_auto_reply=false`) → one
   `queued` run with `trigger='reply'`.
2. Opted-out agent (`retriage_on_reply=false`) → no run.
3. `is_auto_reply=true` reply → no run (`skipped_auto_reply`).
4. `outbound`/`note` message → no run (`skipped_not_inbound`).
5. Cap: the (cap+1)th reply within an hour on one ticket for one agent →
   `skipped_capped` + an `agent.retriage_suppressed` audit row; earlier ones enqueued.
6. Two opted-in agents, one reply → two runs (per-agent cap, not shared).
7. Claim hardening: seed a `queued` run, force-delete its `agent` row via `Super`
   (bypassing the FK is impossible — instead insert a run with a non-existent
   `agent_id`+`tenant_root_id` directly via Super to simulate the orphan), call the
   claim → orphan marked `failed`, and a second valid queued run still drains.

Note for case 7: since the FK blocks deleting an agent with runs, the orphan is
simulated by a raw Super insert of an `agent_run` whose `(agent_id, tenant_root_id)`
has no `agent` row — only possible by also disabling the FK check for that insert
(`SET session_replication_role = replica;` in the seed tx) OR by pointing at a
tenant_root_id mismatch. Use the `session_replication_role` approach in the seed
helper, scoped to the seed tx, to exercise the hardened claim path.

## Files (anticipated)
- `migrations/0052_agent_retriage.up/down.sql` (next free number; 0051 is the latest) — `agent.retriage_on_reply` column;
  `enqueue_reply_retriage_run` DEFINER; rewritten `claim_next_queued_agent_run`.
- `db/query/agent.sql`, `db/query/agent_run.sql` — `retriage_on_reply` in agent
  create/update + view; `EnabledRetriageAgentsForBusiness`; `EnqueueReplyRetriageRun`
  wrapper. Regenerate dbgen with `/opt/homebrew/bin/sqlc`.
- `internal/agents/trigger.go` (or a new `reply_trigger.go`) — `ReplyRetriageTrigger`.
- `internal/agents/agent.go`, agent handler/types — plumb `retriage_on_reply`.
- `internal/platform/config/config.go` (+ test), `.env.example` — the cap key.
- `cmd/manyforge/main.go` — subscribe the trigger + inject the cap.
- `internal/security_regression/agent_run_us5_pins_test.go` — narrow + add pins.
- `internal/agents/*_integration_test.go` — the seven cases.

## Out of scope
- Agent hard-delete with run cleanup (separate retention-policy issue).
- Re-triage feeding the prior conversation differently to the model (the run targets
  the ticket; the agent reads the full thread via its read tools as today).
