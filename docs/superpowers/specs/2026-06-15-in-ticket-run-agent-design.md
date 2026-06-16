# In-Ticket "Run Agent" Control — Design

- **Date:** 2026-06-15
- **Status:** Approved (brainstorm) — pending implementation plan
- **Relates to:** Agent runtime (Spec 003). The manual-run backend endpoint already exists; this adds the missing frontend trigger. Builds on the Agent-Management UI (`manyforge-1kv`) + OpenRouter (`manyforge-eca`).

## Goal

Add a control on the support ticket (thread) view that lets an operator **run an enabled agent against the current ticket on demand** — instead of waiting for the automatic event triggers (which only fire on native-inbox tickets) or using the API by hand. The design spec for the agent-management UI deliberately deferred a "run-now" UI; this fills that gap, scoped to the ticket view.

## Background (verified)

- **Backend endpoint exists:** `POST /api/v1/businesses/{id}/agents/{agentID}/runs`, gated by the `agents.run` permission (`internal/agents/agent_run_handler.go:73`, mounted at `cmd/manyforge/main.go:836`). An empty body defaults to `trigger:"manual"`; passing `{"target_type":"ticket","target_id":"<uuid>"}` points the agent at a specific ticket. It runs **synchronously** through `Engine.Run` (not the drainer) and returns the terminal `agent_run` (HTTP 202). `target_id` is a prompt hint, not an ownership gate.
- **No frontend wires it:** `AgentsService` (`web/src/app/core/agents.service.ts`) has CRUD + `tools()/models()/mcpServers()` but **no run method**; the thread view (`web/src/app/pages/support/thread-view.ts`) has no agent UI.
- **Thread view route** is `/support/:businessId/:tid` — it already has `businessId` + the ticket id (`tid`). It has a **triage card** (`data-testid="triage"`) that holds the human "act on this ticket" controls (status/priority/tags/assignee) and uses the shared `ToastService`.
- **Run outcome:** the run's terminal `status` reflects autonomy gating — `succeeded` (acted/finished), `awaiting_approval` (writes parked in the `/approvals` queue, autonomy mode 1/2), or `failed`. `AgentsService.list(businessId)` exists for the enabled-agent picker.

## Scope

**In scope:**
- A `run()` method on `AgentsService`.
- A "Run agent" control in the thread-view triage card: enabled-agent picker (when >1) + a Run button.
- **Fire-and-forget UX:** immediate "started" toast, then a result toast when the (background) run finishes.
- Tests (service unit + thread-view component spec).

**Out of scope (YAGNI):**
- A run-history/inline-transcript panel in the ticket (run history is on the accounting/agent-runs page).
- Streaming/live progress of the agentic loop.
- Running against non-ticket targets from this control.
- A global "run agent" control outside the ticket view.

## Design

### Frontend service — `web/src/app/core/agents.service.ts`

Add an `AgentRun` response interface (the fields the toast needs — at minimum `id` and `status`; the implementation phase confirms the exact `agent_run` response DTO from `agent_run_handler.go` and types the rest as needed) and a method:

```ts
export interface AgentRun {
  id: string;
  agent_id: string;
  status: string; // 'succeeded' | 'awaiting_approval' | 'failed' | ...
  // (other fields as the backend returns; only id+status are consumed by the UI)
}

run(businessId: string, agentId: string, body: { target_type: 'ticket'; target_id: string }): Observable<AgentRun> {
  return this.http.post<AgentRun>(`${this.base(businessId)}/${agentId}/runs`, body);
}
```

### Thread-view control

A focused block in the triage card:

- **On init**, load enabled agents: `agentsSvc.list(businessId)` → keep `enabledAgents = items.filter(a => a.enabled)` in a signal. Pick a default `selectedAgentId` (the first enabled one).
- **Render:**
  - `enabledAgents.length === 0` → a muted hint: "No enabled agents for this business." (control otherwise hidden) — `data-testid="run-agent-none"`.
  - `=== 1` → a single "Run agent" button (`data-testid="run-agent-btn"`).
  - `> 1` → a `<select>` (`data-testid="run-agent-select"`, bound to `selectedAgentId`) + the Run button.
- **Run handler (fire-and-forget):** on click, if not already running this agent: set a `running` flag (disables the button to prevent double-fire), immediately `toast.success('Agent started — it will act on this ticket in the background.')`, then `agentsSvc.run(businessId, selectedAgentId, { target_type: 'ticket', target_id: tid }).subscribe({...})`:
  - `next: (run) =>` clear `running`; toast by `run.status`:
    - `awaiting_approval` → `toast.success('Agent finished — proposed actions are waiting in Approvals.')`
    - `failed` → `toast.error('Agent run failed.')`
    - else (`succeeded`/default) → `toast.success('Agent finished.')`
  - `error: (e) =>` clear `running`; `toast.error(e.status === 403 || e.status === 404 ? "You don't have access to run agents." : 'Could not start the agent run.')`

> The button re-enables on response (not held for the full run). Fire-and-forget means the optimistic "started" toast appears instantly; the outcome toast follows when the synchronous backend call returns. The `running` flag only guards against a double-click on the same control before the first response lands.

### Permissions

The runs endpoint is server-side gated by `agents.run`. No client route guard — a caller lacking the permission gets 404 → the error-toast branch ("no access"). Consistent with the app's existing no-client-guard pattern.

### data-testids (for tests/e2e)

`run-agent-control` (wrapper), `run-agent-select` (multi-agent picker), `run-agent-btn` (the button), `run-agent-none` (the no-enabled-agents hint).

## Test plan

- **`AgentsService.run` unit (vitest):** POSTs to `/api/v1/businesses/{id}/agents/{agentId}/runs` with `{target_type:'ticket', target_id}` and returns the typed run.
- **thread-view component spec (vitest):**
  - renders the Run control when ≥1 enabled agent (and the multi-agent `<select>` when >1; the single button when 1);
  - renders the `run-agent-none` hint when 0 enabled agents;
  - clicking Run shows the immediate "started" toast and POSTs to the runs endpoint with the current ticket as `target_id`;
  - maps a flushed `awaiting_approval` / `failed` / `succeeded` response to the correct outcome toast;
  - a 403/404 error → the "no access" toast.
  - (Mock the agents-list GET the control fires on init; mirror the existing thread-view spec's TestBed/HTTP-mock setup.)
- **e2e (optional):** a thread-view Playwright spec mocking the agents list + the runs POST, asserting the started/outcome toasts. Only if the existing thread-view has an e2e to extend; otherwise the component spec suffices.

## Risks / notes

- **Synchronous backend call:** the run can take 10–60s; fire-and-forget keeps the UI responsive (the request resolves in the background and toasts the result). If the user navigates away from the ticket before it returns, the outcome toast may not fire — acceptable (the run still completes server-side; the result is visible in `/approvals` or accounting/agent-runs).
- **Native-inbox caveat is orthogonal:** this manual control works for ANY ticket (it passes `target_id` directly), unlike the automatic triggers which only fire on native-inbox tickets. That's a feature — it's the way to test/act on connector-sourced tickets too.
- **No exact-action-count in the toast:** the toast says "proposed actions are waiting in Approvals" without a count (the run response may not carry one cleanly); the Approvals page shows the specifics. Keeps the UI simple.
- **`agents.run` permission:** the operator must hold `agents.run` (distinct from `agents.configure` used by the management pages). If they can configure agents but not run them, the button will 404 → "no access"; that's correct.
