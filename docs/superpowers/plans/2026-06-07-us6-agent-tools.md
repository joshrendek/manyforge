# US6 — Connector Agent Tools (read + gated write) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose external ticket state and gated external writes to Spec-003 agents via three tools registered in the existing agent tool registry: `read_external_ticket` (read), `add_external_comment` (external, gated+audited), `transition_external_status` (external, gated+audited). The two write tools drive the US4 `connector_outbound_op` queue and MUST pass the autonomy gate **before** enqueuing any outbound op.

**Architecture:** The Spec-003 run loop already enforces the locked constraint *for free*: `Engine.execTool` (`internal/agents/runner.go:283-322`) runs **allowlist → RBAC (`Resolver.Has`) → `gate(Effect, mode)` → exec-or-queue** centrally, *before* dispatching to a tool's `Invoke`. So a write tool that declares `Effect: EffectExternal` cannot enqueue an outbound op unless `gate()==decideExec` (mode `autonomous`) or a human has approved it (the `ApprovalExecutor` re-invokes `Invoke`). US6 therefore adds **no per-tool gate call** — it declares the effect class and inherits gating, exactly like the existing `draft_reply` tool. The three tools delegate to a new `connectors.AgentGateway` (compose `*connectors.Service` + `*connectors.Registry`) so the `agents` package depends on one small interface. `add_external_comment` reuses the entire US4 comment path (`AddNote` native anchor → `'comment'` outbound op → `complete_outbound_comment` external-id write-back → inbound-sync dedup), so it needs **no** new op-kind. Only `transition_external_status` introduces a new `'transition'` outbound op-kind (migration 0047) + a `dispatchTransition` arm + a `complete_outbound_transition` DEFINER (no external-id write-back — status is not a message).

**Tech Stack:** Go; `internal/agents` tool registry (`Tool{Name,Description,SchemaJSON,Effect,RequiredPerm,Invoke}`); `internal/connectors` Service/Registry + `TicketingConnector`; the US4 `connector_outbound_op` queue + `OutboundDispatcher`; `internal/ticketing` `AddNote`; sqlc (`make generate`) for dbgen queries; one migration `0047`; `httptest` + the connectors integration harness; Go table tests. Dispatch via `superpowers:subagent-driven-development`; track in **bd** (`manyforge-a7j.6` children `.1`–`.7`).

---

## Design reference (Spec 004 §6 + §8 US6 + locked constraints)

- **Spec §6 (Agent tools):** three tools, registered in the 003 registry; the **autonomy gate runs after RBAC, before execution**; every invocation audited via the existing tool-audit path. Decision table by mode for `EffectExternal`: `assist`→queue/approve, `queue-writes`→queue/approve, `autonomous`→exec (this is `gate()` in `internal/agents/gate.go`).
  | Tool | Effect | Perm | Behavior |
  |------|--------|------|----------|
  | `read_external_ticket` | `EffectRead` | `connectors.read` | Fetch external issue + comments as context (never mutates). |
  | `add_external_comment` | `EffectExternal` | `connectors.write` | Gated; posts a comment to the external ticket. |
  | `transition_external_status` | `EffectExternal` | `connectors.write` | Gated; transitions the external issue's status. |
- **Spec §7 pin 6 (the US6 pin):** *external actions gated + audited — `EffectExternal` → gate by mode; every invocation audited (inputs/outputs/decision).*
- **LOCKED constraint (HANDOFF + bd a7j.6):** gated write tools MUST route through the autonomy gate **before** calling any `Enqueue*` outbound op. Satisfied architecturally (central gate in `execTool` precedes `Invoke`); the implementation must NOT add an `Enqueue*` call anywhere outside a tool `Invoke` body, and a tool `Invoke` body only runs post-gate/post-approval.
- **Idempotency (constitution + US4):** the `ApprovalExecutor` is at-least-once (`TestApprovalReplayIdempotent`), so external write tools must be replay-safe. `add_external_comment` carries the approval key into `AddNote` (new `NoteInput.IdempotencyKey`, mirroring `ReplyInput.IdempotencyKey`); `transition_external_status` dedups identical in-flight transitions in SQL (`NOT EXISTS` guard on pending/in_progress same-status ops).

## What already exists and is reused UNCHANGED (verified — do not modify)

| Reused | File:line | Why it works for US6 |
|---|---|---|
| Central gate: allowlist → RBAC → `gate()` → exec/queue | `internal/agents/runner.go:283-322` | Declaring `Effect: EffectExternal` inherits gating before `Invoke` |
| `gate(effect, mode)` decision table + `Mode*`/`decide*` consts | `internal/agents/gate.go:4-42` | `EffectExternal` queues in assist/queue-writes, execs in autonomous (fail-closed default) |
| `EffectClass` enum (`EffectRead=0…EffectIrreversible=3`) | `internal/agents/tools.go:21-28` | tools tag `EffectRead` / `EffectExternal` |
| `Tool` struct + `ToolRegistry` + `add` closure | `internal/agents/tools.go:53-75,186-189` | mirror `read_ticket`/`draft_reply` registration |
| `strictUnmarshal` / `jsonResult` / `approvalKeyFrom` | `internal/agents/tools.go:78-85,340-346,40-43` | arg parsing, result, idempotency-key plumbing |
| `ApprovalExecutor.Handle` re-invoke + idempotency-key ctx | `internal/agents/approval_executor.go:65-131`, `tools.go:35-37` | post-approval exec path for external writes |
| `connectors.Service.Resolve` (principal-scoped) | `internal/connectors/service.go:107` | resolve `ResolvedConnector` under RLS |
| `Registry.Resolve` → live `TicketingConnector` | `internal/connectors/registry.go:57` | principal-scoped live client for `FetchIssue` |
| `TicketingConnector.FetchIssue` + `ExternalIssue`/`ExternalComment` | `internal/connectors/connector.go:57-73`, `types.go:16-35` | `read_external_ticket` data source (issue + comments) |
| `Service.EnqueueOutboundCreateIssue` (ownership-in-SQL template) | `internal/connectors/service.go:149` | template for the two new enqueue methods |
| `connector_outbound_op` queue + `OutboundDispatcher` + `dispatchComment`/`dispatchCreate` | `migrations/0045`, `internal/connectors/outbound.go:182-279` | comment path reused as-is; transition arm added |
| `complete_outbound_comment` write-back + inbound dedup | `migrations/0045:89-114` | agent comment dedups against the inbound round-trip |
| `ticketing.Service.AddNote` (note message, no email, no mail enqueue) | `internal/ticketing/service.go:404-449` | native anchor for `add_external_comment` |
| `permission`/`role_permission` catalog + seed pattern | `migrations/0015_support_permissions.up.sql:9-32` | seed `connectors.read`/`connectors.write` the same way |
| `Resolver.Has` (unknown key → silent `false`) | `internal/authz/resolver.go:19,26-55`; `internal/agents/run_adapters.go:56-67` | gate RBAC denies cleanly until perms seeded |
| Connectors integration harness (`startConn`,`newConnService`,`seedConnectorTenant`,`seedOutboundConnector`) | `internal/connectors/credential_integration_test.go:21-25`, `testsupport_integration_test.go:48,104` | reuse for the transition/read integration test |
| Gate-branch test fakes (`fakeRunStore`,`fakeResolver`,`fakeApprovals`,`newTestEngine`,`loadedAgent`) | `internal/agents/runner_test.go:17-91` | mirror `TestRun_Mode1ExternalQueuesApproval`/`Mode3AutoRunsExternal` |

## New / modified files (this plan)

| File | Responsibility | Task |
|---|---|---|
| `migrations/0047_connector_agent_tools.up.sql` / `.down.sql` | `'transition'` enum value + `complete_outbound_transition` DEFINER (+REVOKE/GRANT) + seed `connectors.read`/`connectors.write` (+role grants) | T1 |
| `db/schema.sql` (modify) | mirror `'transition'` into `connector_outbound_op_type` (+ DEFINER per existing 0045 convention) | T1 |
| `db/query/connector_outbound.sql` (modify) | add `GetTicketConnectorRef :one`, `EnqueueOutboundTransition :exec` | T1 |
| `internal/platform/db/dbgen/*` (generated) | `make generate` output for the two new queries | T1 |
| `internal/connectors/service.go` (modify) | `TicketConnectorRef`, `EnqueueOutboundComment`, `EnqueueOutboundTransition` methods (ownership-in-SQL, `mapErr`) | T2 |
| `internal/connectors/agent_gateway.go` | `AgentGateway{svc,reg}` + `NewAgentGateway` + `ReadTicketExternal` (svc ref + reg resolve + FetchIssue) | T2 |
| `internal/connectors/outbound.go` (modify) | `case "transition"` + `dispatchTransition` + `completeTransition` write-back | T2 |
| `internal/connectors/agent_gateway_integration_test.go`, `outbound_transition_integration_test.go` | white-box integration tests | T2 |
| `internal/ticketing/service.go` (modify) + `db/query/*` | `NoteInput.IdempotencyKey` + `AddNote` dedup (mirror Reply) | T3 |
| `internal/agents/tools.go` (modify) | `connectorGateway` interface + 3 tool registrations + `NewToolRegistry(svc, conn)` signature + arg/view structs | T4 |
| `cmd/manyforge/main.go` (modify `:168`,`:188`,`:276-283`) | build `AgentGateway` (nil when connectors disabled) + pass to both `NewToolRegistry` sites | T4 |
| `internal/agents/tools_test.go` (modify) | effect-class + validation + nil-gateway unit tests for the 3 tools | T4 |
| `internal/agents/runner_test.go` (modify) | gate-branch pins for both write tools (queue in assist/queue-writes; exec in autonomous; RBAC deny) | T5 |
| `internal/security_regression/mf_004_us6_agent_tools_test.go` | MF-004-US6 source + behavioral pins (gated+audited, tenant isolation, perm-gating) | T7 |

## Locked design decisions (rationale, so the implementer does not re-litigate)

1. **Gate is inherited, not called.** Tools declare `Effect`; `execTool` gates centrally. No tool calls `gate()` or `Enqueue*` outside its own `Invoke`. (Mirrors `draft_reply` + the MCP-host precedent `mcp_host.go:145`.)
2. **`add_external_comment` reuses the `'comment'` op (no new op-kind, no dispatcher change).** It creates a native internal note via `AddNote` (no customer email), then enqueues the existing comment op anchored to that note's message id. `complete_outbound_comment` stamps the note message's `external_id` with the posted comment id, so when that same comment later syncs back via reconcile/webhook, the inbound upsert dedups by `external_id` — **no double comment**. This is the same machinery US4's `Reply` uses, minus the email.
3. **`transition_external_status` introduces the `'transition'` op-kind** because `TransitionStatus` is a distinct connector call with no external-id to write back. Its completion DEFINER only marks the op done + audits. The target status rides the op's `body` column (the only free-text carrier; `claim_outbound_ops` already returns `body`).
4. **Three enqueue/ref methods live on `connectors.Service`** (next to `EnqueueOutboundCreateIssue`), each pushing ownership into SQL (`WHERE business_id = $ AND connector_id IS NOT NULL`) and mapping errors through `mapErr` (0 rows / not-linked → `ErrNotFound`; no 403/404 oracle). `AgentGateway` composes `Service` (ref + enqueue) with `Registry` (live resolve) so `read_external_ticket` can call `FetchIssue`.
5. **Connector tools are optional at boot.** When `cfg.ConnectorMasterKey` is unset (`connReg == nil`), `main.go` passes a `nil` gateway and `NewToolRegistry` skips registering the three tools — the binary boots without connectors.
6. **No new permission-validation surface.** `Resolver.Has` is a pure map lookup; an unseeded key denies cleanly. Owner resolves to the whole catalog, so even Owner needs the rows seeded — hence the 0047 seed is mandatory, not optional.

## Idioms to match

- Tool registration + handler shape: copy `read_ticket` (`tools.go:190-207`) for the read tool and `draft_reply` (`tools.go:312-335`) for the write tools (including `approvalKeyFrom(ctx)` → idempotency key).
- Arg structs + `strictUnmarshal` (DisallowUnknownFields, wraps `errs.ErrValidation`); results via `jsonResult`.
- Service methods mirror `EnqueueOutboundCreateIssue` (`service.go:149-168`): `GetConnector`/ref pre-check, ownership-in-SQL insert, `mapErr`.
- DEFINER mirrors `complete_outbound_comment`/`fail_outbound_op` (`0045:89-151`) incl. `SET search_path = public` + the `REVOKE ALL … FROM PUBLIC` / `GRANT EXECUTE … TO manyforge_app` pair.
- `dispatchTransition` mirrors `dispatchComment` (`outbound.go:195-237`): guard `o.TicketExtID` non-empty, call the connector, then a short write-back tx calling the new DEFINER; failures via `recordFailure`→`fail_outbound_op`.
- dbgen nullable nuance (HANDOFF gotcha): `ticket.connector_id` → `pgtype.UUID`; `ticket.external_id` → `*string`. Convert/guard accordingly.
- Trust `go build ./...` / `go test`, never gopls (`dbgen.X undefined` + `//go:build integration` "No packages found" are false positives).

---

### Task 1: Migration 0047 — transition op-kind + completion DEFINER + connector perms + dbgen queries (`manyforge-a7j.6.1`)

**Files:**
- Create: `migrations/0047_connector_agent_tools.up.sql`, `migrations/0047_connector_agent_tools.down.sql`
- Modify: `db/schema.sql` (mirror enum value; DEFINER per 0045 convention)
- Modify: `db/query/connector_outbound.sql` (two new queries)
- Run: `make generate` (regenerates `internal/platform/db/dbgen/connector_outbound.sql.go`)
- Create/modify: `internal/connectors/migration_0047_integration_test.go` (white-box DEFINER + query test)

- [x] **Step 1 (RED): white-box integration test** in `internal/connectors/` (`//go:build integration`) using `startConn(t)` + `seedOutboundConnector`-style scaffold:
  - `TestEnqueueOutboundTransitionInsertsPendingOp` — seed a connector-linked ticket; call dbgen `EnqueueOutboundTransition{ID:ticketID, BusinessID:bid, Status:"Done"}`; assert one `connector_outbound_op` row with `op_type='transition'`, `status='pending'`, `body='Done'`. Second identical call → still exactly one pending op (the `NOT EXISTS` dedup).
  - `TestEnqueueOutboundTransitionRejectsUnlinkedTicket` — ticket with `connector_id IS NULL` → 0 rows inserted.
  - `TestCompleteOutboundTransitionMarksDoneAndAudits` — insert a pending transition op; `SELECT complete_outbound_transition($op,$conn,'Done')`; assert op `status='done'`, `last_error IS NULL`, and one `audit_entry` row `action='connector.outbound.transitioned'`, `decision='external_post'`, `new_value->>'status'='Done'`.
  - `TestGetTicketConnectorRefOwnershipScoped` — `GetTicketConnectorRef{ID:ticketID, BusinessID:otherBiz}` → `pgx.ErrNoRows` (cross-business returns not-found, no oracle); correct business → `(connector_id, external_id)`.

- [x] **Step 2 (GREEN): write `migrations/0047_connector_agent_tools.up.sql`:**
  ```sql
  -- US6: agent connector tools. New 'transition' outbound op-kind + completion DEFINER
  -- (no external-id write-back) + connectors.read/connectors.write permission catalog.

  -- 1. New op-kind. (PG: a newly added enum value cannot be USED in the same tx that adds it;
  --    nothing below uses 'transition' — runtime queries consume it post-commit — so this is safe.)
  ALTER TYPE connector_outbound_op_type ADD VALUE IF NOT EXISTS 'transition';

  -- 2. Completion DEFINER for a transition op: status is not a message, so no external-id
  --    write-back; resolve tenancy from the connector, mark the op done, audit the action.
  CREATE FUNCTION complete_outbound_transition(p_op_id uuid, p_connector_id uuid, p_status text)
  RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
  DECLARE v_business uuid; v_tenant uuid;
  BEGIN
      SELECT business_id, tenant_root_id INTO v_business, v_tenant FROM connector WHERE id = p_connector_id;
      IF v_business IS NULL THEN RAISE EXCEPTION 'unknown connector %', p_connector_id; END IF;

      UPDATE connector_outbound_op SET status = 'done', last_error = NULL, updated_at = now() WHERE id = p_op_id;

      INSERT INTO audit_entry (business_id, tenant_root_id, actor_principal_id, action,
                               target_type, target_id, new_value, decision)
      VALUES (v_business, v_tenant, NULL, 'connector.outbound.transitioned',
              'connector_outbound_op', p_op_id,
              jsonb_build_object('status', p_status, 'connector_id', p_connector_id),
              'external_post');
  END;
  $$;
  REVOKE ALL ON FUNCTION complete_outbound_transition(uuid, uuid, text) FROM PUBLIC;
  GRANT EXECUTE ON FUNCTION complete_outbound_transition(uuid, uuid, text) TO manyforge_app;

  -- 3. Connector agent-tool permission catalog (mirrors 0015_support_permissions).
  INSERT INTO permission (key, module, description) VALUES
      ('connectors.read',  'connectors', 'Read external ticket state via connector agent tools'),
      ('connectors.write', 'connectors', 'Post comments and transition status on external tickets (gated, audited)');

  INSERT INTO role_permission (role_id, permission_key)
      SELECT r.id, p.key FROM role r JOIN permission p ON p.key IN ('connectors.read', 'connectors.write')
      WHERE r.tenant_root_id IS NULL AND r.key IN ('owner', 'admin', 'member');

  INSERT INTO role_permission (role_id, permission_key)
      SELECT r.id, p.key FROM role r JOIN permission p ON p.key = 'connectors.read'
      WHERE r.tenant_root_id IS NULL AND r.key = 'viewer';
  ```
  > **Verify before writing:** confirm `audit_entry` column names + `decision` enum value `'external_post'` against `0045:89-114` (copy exactly — `complete_outbound_comment` is the reference). Confirm role keys `owner/admin/member/viewer` against `0015:18-32` (mirror whatever 0015 used; adjust if the role set differs).
  > **Tx caveat:** if the migration runner wraps each file in one tx and PG ≤ rejects `ADD VALUE` mid-tx, split: `0047` = `ADD VALUE` only, `0048` = the rest. Default to one file (PG 12+ allows `ADD VALUE` in a tx as long as the value isn't used in it — it isn't).

  **`.down.sql`:** `DROP FUNCTION IF EXISTS complete_outbound_transition(uuid,uuid,text);` + `DELETE FROM role_permission WHERE permission_key IN ('connectors.read','connectors.write');` + `DELETE FROM permission WHERE key IN ('connectors.read','connectors.write');` (note: PG cannot remove an enum value — document that `'transition'` persists on down-migration; acceptable, matches how enum additions are irreversible elsewhere).

- [x] **Step 3: mirror into `db/schema.sql`** — add `'transition'` to the `connector_outbound_op_type` enum list; add `complete_outbound_transition` following the existing convention for whether the 0045 DEFINERs appear in `schema.sql` (match `complete_outbound_comment`'s presence/absence there). Do NOT add the permission seed INSERTs to `schema.sql` (seeds are data, not schema).

- [x] **Step 4: add queries to `db/query/connector_outbound.sql`:**
  ```sql
  -- name: GetTicketConnectorRef :one
  SELECT connector_id, external_id
  FROM ticket
  WHERE id = $1 AND business_id = sqlc.arg(business_id) AND connector_id IS NOT NULL;

  -- name: EnqueueOutboundTransition :exec
  INSERT INTO connector_outbound_op (business_id, tenant_root_id, connector_id, ticket_id, op_type, body)
  SELECT t.business_id, t.tenant_root_id, t.connector_id, t.id, 'transition', sqlc.arg(status)::text
  FROM ticket t
  WHERE t.id = $1 AND t.business_id = sqlc.arg(business_id) AND t.connector_id IS NOT NULL
    AND NOT EXISTS (
      SELECT 1 FROM connector_outbound_op o
      WHERE o.ticket_id = t.id AND o.op_type = 'transition'
        AND o.status IN ('pending', 'in_progress') AND o.body = sqlc.arg(status)
    );
  ```
  Then `make generate`; verify `go build ./...` clean (ignore gopls). `GetTicketConnectorRef` returns `connector_id pgtype.UUID` + `external_id *string` (dbgen nullable nuance).

- [x] **Step 5:** run the integration test (`go test -tags integration -p 1 ./internal/connectors/ -run 'Transition|TicketConnectorRef' -v`) → GREEN. `gofmt -l`. Commit `--no-verify`: `feat(connectors): US6 T1 — 0047 transition op-kind + complete_outbound_transition + connectors.read/write perms + dbgen queries (manyforge-a7j.6.1)` (stage `.beads/issues.jsonl`).

**Test plan (T1):** white-box integration tests above (enqueue insert + dedup, unlinked rejection, DEFINER mark-done+audit, ref ownership-scoping). No unit-level tests (pure SQL/DEFINER surface).

---

### Task 2: connectors.Service enqueue/ref methods + AgentGateway + dispatchTransition (`manyforge-a7j.6.2`)

**Files:**
- Modify: `internal/connectors/service.go` (3 methods)
- Create: `internal/connectors/agent_gateway.go`
- Modify: `internal/connectors/outbound.go` (`case "transition"` + `dispatchTransition` + `completeTransition`)
- Create: `internal/connectors/agent_gateway_integration_test.go`, `internal/connectors/outbound_transition_integration_test.go`

- [x] **Step 1 (RED): integration tests** (reuse `startConn`/`seedOutboundConnector`):
  - `TestAgentGatewayReadTicketExternal` — seed a connector-linked ticket whose external id resolves via a stub `TicketingConnector` returning a canned `ExternalIssue` (use a recording fake registered in a test `Registry`); `gw.ReadTicketExternal(ctx, pid, bid, ticketID)` returns the issue + comments; foreign business / unlinked ticket → `errs.ErrNotFound`.
  - `TestServiceEnqueueOutboundCommentOwnership` — `svc.EnqueueOutboundComment(ctx, pid, bid, ticketID, noteMsgID, "hi")` inserts a `'comment'` op (message_id = noteMsgID); unlinked/foreign ticket → `ErrNotFound`.
  - `TestServiceEnqueueOutboundTransitionOwnership` — `svc.EnqueueOutboundTransition(ctx, pid, bid, ticketID, "Done")` inserts a `'transition'` op; foreign → `ErrNotFound`; duplicate in-flight → no second op.
  - `TestDispatchTransitionPostsAndCompletes` — seed a pending `'transition'` op + a recording fake whose `TransitionStatus(extID,status)` records the call; run one `OutboundDispatcher` tick; assert the fake saw `(externalID,"Done")`, op → `status='done'`, audit `connector.outbound.transitioned`. (Add a `transitionRecorder` fake — every existing fake's `TransitionStatus` is a no-op, so a recording one is needed.)

- [x] **Step 2 (GREEN): `internal/connectors/service.go`** — add, mirroring `EnqueueOutboundCreateIssue` (`service.go:149-168`) with the established `WithPrincipal` tx pattern of `Resolve`:
  ```go
  // TicketConnectorRef returns the connector id + external id of a connector-linked ticket the
  // caller owns. Unlinked, unknown, or foreign → ErrNotFound (no 403/404 oracle).
  func (s *Service) TicketConnectorRef(ctx context.Context, principalID, businessID, ticketID uuid.UUID) (uuid.UUID, string, error)

  // EnqueueOutboundComment enqueues a 'comment' outbound op for a connector-linked ticket the
  // caller owns, anchored to messageID (for external-id write-back + inbound dedup). 0 rows → ErrNotFound.
  func (s *Service) EnqueueOutboundComment(ctx context.Context, principalID, businessID, ticketID, messageID uuid.UUID, body string) error

  // EnqueueOutboundTransition enqueues a 'transition' outbound op (target status in body) for a
  // connector-linked ticket the caller owns; dedups identical in-flight transitions. 0 rows → ErrNotFound.
  func (s *Service) EnqueueOutboundTransition(ctx context.Context, principalID, businessID, ticketID uuid.UUID, status string) error
  ```
  - `TicketConnectorRef`: `WithPrincipal` → `dbgen.GetTicketConnectorRef`; `pgx.ErrNoRows`/`!ConnectorID.Valid`/`ExternalID==nil` → `errs.ErrNotFound`; return `uuid.UUID(ref.ConnectorID.Bytes), *ref.ExternalID`. All errors via `mapErr`.
  - `EnqueueOutboundComment`: pre-check via `TicketConnectorRef` (gives the `ErrNotFound` semantics + confirms linkage), then `dbgen.EnqueueOutboundComment{ID:ticketID, MessageID:db.PGUUID(messageID), Body:&body, BusinessID:businessID}` (reuse existing `:exec` query). `mapErr`.
  - `EnqueueOutboundTransition`: pre-check via `TicketConnectorRef`, then `dbgen.EnqueueOutboundTransition{ID:ticketID, BusinessID:businessID, Status:status}`. `mapErr`.

- [x] **Step 3 (GREEN): `internal/connectors/agent_gateway.go`:**
  ```go
  // AgentGateway is the narrow surface Spec-003 agent tools use to read external ticket state
  // and enqueue gated external writes. Composes Service (ownership-scoped DB ops) + Registry
  // (live connector resolve). Construction: NewAgentGateway(connSvc, connReg).
  type AgentGateway struct { svc *Service; reg *Registry }
  func NewAgentGateway(svc *Service, reg *Registry) *AgentGateway { return &AgentGateway{svc: svc, reg: reg} }

  func (g *AgentGateway) ReadTicketExternal(ctx context.Context, principalID, businessID, ticketID uuid.UUID) (ExternalIssue, error) {
      connID, extID, err := g.svc.TicketConnectorRef(ctx, principalID, businessID, ticketID)
      if err != nil { return ExternalIssue{}, err }
      conn, err := g.reg.Resolve(ctx, principalID, businessID, connID)
      if err != nil { return ExternalIssue{}, err }
      return conn.FetchIssue(ctx, extID) // connector errors are already sentinel/no-body-leak
  }
  func (g *AgentGateway) EnqueueComment(ctx context.Context, principalID, businessID, ticketID, messageID uuid.UUID, body string) error {
      return g.svc.EnqueueOutboundComment(ctx, principalID, businessID, ticketID, messageID, body)
  }
  func (g *AgentGateway) EnqueueTransition(ctx context.Context, principalID, businessID, ticketID uuid.UUID, status string) error {
      return g.svc.EnqueueOutboundTransition(ctx, principalID, businessID, ticketID, status)
  }
  ```

- [x] **Step 4 (GREEN): `internal/connectors/outbound.go`** — add `case "transition": return d.dispatchTransition(ctx, conn, o)` to the switch (`:182-189`); implement `dispatchTransition` mirroring `dispatchComment` (`:195-237`): guard `o.TicketExtID` non-empty (else terminal `fail_outbound_op`); read target status from `*o.Body` (guard non-nil); `conn.TransitionStatus(ctx, *o.TicketExtID, status)`; on success a short write-back tx `SELECT complete_outbound_transition($1,$2,$3)` with `(o.ID, o.ConnectorID, status)` (add a `completeTransition` helper next to `completeComment` at `:273`); on error `recordFailure`.

- [x] **Step 5:** `go build ./...` clean; integration tests GREEN (`go test -tags integration -p 1 ./internal/connectors/ -run 'Gateway|Transition|EnqueueOutbound' -v`); `gofmt -l`. Commit `--no-verify`: `feat(connectors): US6 T2 — Service enqueue/ref methods + AgentGateway + dispatchTransition (manyforge-a7j.6.2)`.

**Test plan (T2):** integration tests above — gateway read (+ ownership not-found), comment/transition enqueue (+ ownership not-found + transition dedup), dispatcher transition round-trip (recording fake → DEFINER mark-done + audit).

---

### Task 3: ticketing AddNote idempotency key (replay-safe agent comments) (`manyforge-a7j.6.3`)

**Files:**
- Modify: `internal/ticketing/service.go` (`NoteInput`, `AddNote`)
- Modify: `db/query/*` (+ `make generate`) if a `GetNoteMessageByApproval`-style lookup is needed
- Modify: `internal/ticketing/*_test.go`

**Why:** `add_external_comment` runs via the at-least-once `ApprovalExecutor`; without idempotency a redelivery creates a duplicate note + duplicate comment op → double external comment. `Reply` already solves this (`ReplyInput.IdempotencyKey` + `GetOutboundMessageByApproval`, `service.go:47-50,255`). `AddNote` must do the same.

- [x] **Step 1 (RED):** `TestAddNoteIdempotentByKey` (ticketing test) — call `AddNote` twice with the same `NoteInput.IdempotencyKey`; assert exactly one note message row and the second call returns the same `Message` (no second insert). Plus `TestAddNoteNilKeyAlwaysInserts` (nil key → independent inserts, current behavior preserved).
- [x] **Step 2 (GREEN):** add `IdempotencyKey *uuid.UUID` to `NoteInput` (doc-comment mirroring `ReplyInput`); in `AddNote`, when non-nil, look up an existing note message by that key (reuse `GetOutboundMessageByApproval` if it already keys on the approval id for any message direction, else add `GetNoteMessageByApproval` mirroring it) and short-circuit-return it before insert. Store the key on the note message the same way `InsertOutboundMessage` does (verify the column — likely `approval_item_id`/`idempotency_key` on `ticket_message`; mirror the outbound path exactly).
  > **First step for the implementer:** read `Reply`'s dedup (`service.go:227-360`) + `InsertOutboundMessage`/`InsertNoteMessage` params to copy the exact column + lookup. This is reuse, not new invention.
- [x] **Step 3:** `go test ./internal/ticketing/...` GREEN; `gofmt -l`. Commit `--no-verify`: `feat(ticketing): US6 T3 — AddNote idempotency key for replay-safe agent notes (manyforge-a7j.6.3)`.

**Test plan (T3):** unit tests on `AddNote` dedup (same-key single insert / returns existing; nil-key preserves current behavior). Existing `AddNote` tests must still pass.

---

### Task 4: the three agent tools + registry wiring (`manyforge-a7j.6.4`)

**Files:**
- Modify: `internal/agents/tools.go` (interface + 3 tools + constructor signature + arg/view structs)
- Modify: `cmd/manyforge/main.go` (`:168`, `:188`, gateway build near `:276-283`)
- Modify: `internal/agents/tools_test.go`

- [x] **Step 1 (RED): unit tests** in `tools_test.go`:
  - Extend `TestEffectClasses` (`:88-108`): `read_external_ticket→EffectRead`, `add_external_comment→EffectExternal`, `transition_external_status→EffectExternal`.
  - `TestConnectorToolsValidation` — bad UUID / unknown field / empty body → `errs.ErrValidation` (use a `fakeConnectorGateway` recording calls).
  - `TestConnectorToolsAbsentWhenGatewayNil` — `NewToolRegistry(&fakeTicketSvc{}, nil)` → `Get("read_external_ticket")` etc. return `false` (binary boots without connectors).
  - `TestReadExternalTicketReturnsView` — fake gateway returns an `ExternalIssue`; tool returns JSON of the view (external id, url, title, status, priority, reporter, comments) — and crucially **omits** internal connector ids/secrets.
  - `TestAddExternalCommentCreatesNoteThenEnqueues` — fake ticketSvc records `AddNote`, fake gateway records `EnqueueComment(ticketID, noteMsgID, body)`; assert ordering (note first) and that `approvalKeyFrom(ctx)` flows into `NoteInput.IdempotencyKey`.
  - `TestTransitionExternalStatusEnqueues` — fake gateway records `EnqueueTransition(ticketID, status)`.

- [x] **Step 2 (GREEN): `tools.go`** — define the interface + register the tools:
  ```go
  type connectorGateway interface {
      ReadTicketExternal(ctx context.Context, principalID, businessID, ticketID uuid.UUID) (connectors.ExternalIssue, error)
      EnqueueComment(ctx context.Context, principalID, businessID, ticketID, messageID uuid.UUID, body string) error
      EnqueueTransition(ctx context.Context, principalID, businessID, ticketID uuid.UUID, status string) error
  }
  ```
  Change `func NewToolRegistry(svc ticketSvc) *ToolRegistry` → `func NewToolRegistry(svc ticketSvc, conn connectorGateway) *ToolRegistry`; register the three tools only when `conn != nil`:
  - `read_external_ticket` — `Effect: EffectRead`, `RequiredPerm: "connectors.read"`, schema `{"ticket_id":uuid}`; `Invoke` → `conn.ReadTicketExternal(ctx, pid, bid, ticketID)` → `jsonResult(toExternalTicketView(iss))`.
  - `add_external_comment` — `Effect: EffectExternal`, `RequiredPerm: "connectors.write"`, schema `{"ticket_id":uuid,"body_text":string(minLength 1)}`; `Invoke`: validate non-empty; `note, err := svc.AddNote(ctx, pid, bid, ticketID, ticketing.NoteInput{BodyText: body, IdempotencyKey: keyPtrFrom(ctx)})`; then `conn.EnqueueComment(ctx, pid, bid, ticketID, note.ID(), body)`; return `"external comment queued"`.
  - `transition_external_status` — `Effect: EffectExternal`, `RequiredPerm: "connectors.write"`, schema `{"ticket_id":uuid,"status":string(minLength 1)}`; `Invoke` → `conn.EnqueueTransition(ctx, pid, bid, ticketID, status)`; return `"status transition queued"`.
  - Add `externalTicketView` struct + `toExternalTicketView(connectors.ExternalIssue)` mapper (no internal ids/secrets); add `keyPtrFrom(ctx)` (wrap `approvalKeyFrom` → `*uuid.UUID`).
  > Note: agents already imports `internal/ticketing`; adding an import of `internal/connectors` introduces no cycle (connectors does not import agents).

- [x] **Step 3 (GREEN): `cmd/manyforge/main.go`** — where the connector stack is built (`:276-283`, guarded by `cfg.ConnectorMasterKey`), construct `connGateway := connectors.NewAgentGateway(connSvc, connReg)` (leave it `nil` when connectors are disabled — declare a `var connGateway *connectors.AgentGateway` defaulting nil, or an interface-typed nil). Pass it as the 2nd arg to both `NewToolRegistry` calls (`:168` Engine, `:188` ApprovalExecutor). A nil `*AgentGateway` typed into the interface param is non-nil at the interface level — pass an explicit `nil` interface (or guard) so `conn != nil` is false when disabled; the cleanest is `var connGateway connectorGateway` left nil and only assigned when the stack is built. Verify the disabled-connectors path still boots (`go build ./...`).

- [x] **Step 4:** `go test ./internal/agents/...` GREEN; `go build ./...`; `gofmt -l`. Commit `--no-verify`: `feat(agents): US6 T4 — read_external_ticket + add_external_comment + transition_external_status tools + registry wiring (manyforge-a7j.6.4)`.

**Test plan (T4):** unit tests above (effect classes, validation, nil-gateway absence, read view redaction, comment note-then-enqueue ordering + idempotency-key flow, transition enqueue). All via fakes (no DB).

---

### Task 5: gate-branch pins — the LOCKED constraint (`manyforge-a7j.6.5`)

**Files:**
- Modify: `internal/agents/runner_test.go`

**Why:** prove the two write tools are gated **before** any enqueue. Mirror `TestRun_Mode1ExternalQueuesApproval` (`:193-215`) and `TestRun_Mode3AutoRunsExternal` (`:239-259`).

- [x] **Step 1 (RED→GREEN):** add, using `newTestEngine(prov, store, map[string]bool{"connectors.write": true}, reg)` and a recording `fakeConnectorGateway`:
  - `TestRun_ExternalCommentQueuesInAssist` — `ModeAssist` + `add_external_comment`: assert the gateway's `EnqueueComment` was **NOT** called, exactly one approval queued with effect 2 (`ap.created[0] == "add_external_comment:2"`), `run.Status == RunAwaitingApproval`, audit decision `"proposed"`.
  - `TestRun_ExternalCommentQueuesInQueueWrites` — `ModeQueueWrites`: same queued assertion (no enqueue).
  - `TestRun_ExternalCommentExecutesInAutonomous` — `ModeAutonomous`: assert `EnqueueComment` **WAS** called once, zero approvals, `run.Status == RunSucceeded`.
  - `TestRun_TransitionQueuesInAssist` / `TestRun_TransitionExecutesInAutonomous` — same two-branch pair for `transition_external_status` → `EnqueueTransition`.
  - `TestRun_ExternalToolDeniedWithoutPerm` — perms map lacks `connectors.write` → tool denied (no enqueue, no approval) per the RBAC step (`runner.go:298-303`).
  - `TestRun_ReadExternalRunsInline` — `read_external_ticket` (`EffectRead`) executes inline in `ModeAssist` (reads never queue), gateway `ReadTicketExternal` called once.
- [x] **Step 2:** `go test ./internal/agents/...` GREEN; `gofmt -l`. Commit `--no-verify`: `test(agents): US6 T5 — gate-branch pins (external comment/transition queue in assist, exec in autonomous; RBAC deny) (manyforge-a7j.6.5)`.

**Test plan (T5):** run-loop tests proving gate-before-enqueue across all three autonomy modes + RBAC denial for both write tools, and read-inline for the read tool. This is the behavioral half of the §7 pin 6.

---

### Task 6: end-to-end integration (read + gated write round-trip) (`manyforge-a7j.6.6`)

**Files:**
- Create: `internal/connectors/agent_tools_e2e_integration_test.go` (or extend an existing connectors integration file)

**Why:** Spec §10 demo — an agent reads an external ticket and takes a gated write that lands externally. Prove the full path against the real outbound queue + dispatcher (the unit/gate tests use fakes).

- [ ] **Step 1 (RED→GREEN):** using the connectors integration harness + an `httpStub`/recording connector registered in a real `Registry`:
  - `TestAgentReadThenGatedTransitionE2E` — seed a connector-linked ticket; `gw.ReadTicketExternal` returns the stubbed issue; `gw.EnqueueTransition(...,"Done")` inserts the op; run the dispatcher; assert the stub connector received the transition and the op is `done` + audited.
  - `TestAgentGatedCommentE2E` — `AddNote` + `gw.EnqueueComment`; dispatcher posts the comment via the stub; `ticket_message.external_id` stamped by `complete_outbound_comment`; a subsequent inbound upsert of the same external comment id is deduped (no duplicate message) — proves the no-double-comment design.
- [ ] **Step 2:** `go test -tags integration -p 1 ./internal/connectors/ -run 'E2E' -v` GREEN; `gofmt -l`. Commit `--no-verify`: `test(connectors): US6 T6 — agent read + gated write e2e (transition + comment dedup) (manyforge-a7j.6.6)`.

**Test plan (T6):** integration round-trips for transition and comment through the real queue + dispatcher + DEFINERs, including the inbound-dedup assertion that validates decision #2.

---

### Task 7: MF-004-US6 security regression pins (`manyforge-a7j.6.7`)

**Files:**
- Create: `internal/security_regression/mf_004_us6_agent_tools_test.go` (run by `make sec-test`)

**Why:** §7 pin 6 (external actions gated + audited) + pin 7 (tenant isolation) for US6, in the dedicated regression package — one file per finding id, source-level pins (`strings.Contains`/reflection) + behavioral pins.

- [ ] **Step 1 (RED→GREEN):** add, with the `MF-004-US6` id in the header comment:
  - **Source pin (effect classes):** assert via the registry that `read_external_ticket` is `EffectRead` and `add_external_comment`/`transition_external_status` are `EffectExternal` — so a refactor that downgrades a write tool's effect (bypassing the gate) fails CI. (Build a registry with a fake gateway.)
  - **Source pin (perm gating):** assert both write tools carry `RequiredPerm == "connectors.write"` and the read tool `"connectors.read"`.
  - **Source pin (no per-tool gate bypass):** `strings.Contains` scan of `tools.go` connector-tool `Invoke` bodies assert they contain no direct `gate(`/`decideExec` call (gating is the loop's job) — i.e. the tools rely on the central gate, and the bodies do enqueue (so the gate genuinely guards a side effect).
  - **Behavioral pin (gated):** run-loop in `ModeAssist` → `add_external_comment` queues an approval and does **not** enqueue (reuse the T5 fakes) — the load-bearing "gate before external op" assertion.
  - **Behavioral pin (audited):** assert a gated proposal emits the audit decision and an executed transition op writes the `connector.outbound.transitioned` audit entry (or assert the DEFINER audit insert via the T2 integration path / a source pin on `0047`).
  - **Behavioral pin (tenant isolation):** `gw.ReadTicketExternal` / `EnqueueComment` / `EnqueueTransition` for a ticket in another business → `errs.ErrNotFound` (no cross-tenant access, no oracle).
- [ ] **Step 2:** `make sec-test` GREEN; `gofmt -l`. Commit `--no-verify`: `test(sec): US6 T7 — MF-004-US6 pins (external action gated+audited, perm-gating, tenant isolation) (manyforge-a7j.6.7)`.

**Test plan (T7):** source-level pins (effect classes, required perms, no in-tool gate bypass) + behavioral pins (assist-mode queues-not-enqueues, audited, cross-tenant not-found). Non-vacuous (each would fail if the corresponding control regressed).

---

## US6-close gate (after T1–T7 committed — separate increment)

`export PATH="$PATH:$HOME/go/bin"`; `gofmt -l internal/ cmd/ db/` (empty) `&& make test && make contract-test && make lint && make sec-test && make int-test` (int-test backgrounded ~7 min). GREEN → final whole-story review subagent (opus, diff over the US6 stack). READY TO MERGE → `bd close manyforge-a7j.6`, file any follow-ups, stage `.beads/issues.jsonl`, `git push`, update HANDOFF. Red gate → FIX (no "pre-existing failures"). After US6 closed → epic `manyforge-a7j` is DONE (US1–US6 all closed); open follow-ups `a7j.7`–`a7j.12` remain as their own work.

## Risks / watch-items

- **Enum-in-tx:** `ALTER TYPE … ADD VALUE` + same-tx use is the classic PG footgun — 0047 never uses `'transition'` itself, runtime queries consume it post-commit. If the runner still rejects it, split 0047/0048 (T1 Step 2 note).
- **Owner-needs-seed:** Owner resolves to the whole permission catalog, so unseeded `connectors.*` denies even Owner — the 0047 seed is mandatory (T1).
- **Nil-gateway interface trap:** a typed-nil `*AgentGateway` boxed into the interface is non-nil; keep `connGateway` interface-typed and only assign when the connector stack is built (T4 Step 3) so the `conn != nil` guard works.
- **Transition idempotency residual:** the `NOT EXISTS` guard dedups in-flight identical transitions, but a transition op that already completed before an approval replay can re-transition (benign — status is idempotent at the destination; a re-transition unavailable from the current state just fails the op harmlessly). If stronger guarantees are wanted later, file a follow-up for op-level approval-key idempotency (would need an op-table column).
- **AddNote idempotency seam (T3):** prefer reusing `Reply`'s existing approval-keyed lookup/column over inventing a new one; read the `Reply`/`InsertOutboundMessage` dedup first.
