# Approvals Queue UI + Safe Action-Summary — Design

**Status:** Approved (design)
**Date:** 2026-06-11
**Author:** Josh Rendek
**Builds on:** the Stream-1 design system (`docs/superpowers/specs/2026-06-10-ui-redesign-design-system-design.md`) — reuses its tokens + component kit.

---

## 1. Context

This is **Stream 2** of the four-stream UI program (`bd manyforge-4zs`). The autonomy-gate **approvals queue** backend (Spec 003) is fully built: external/irreversible agent actions are routed to an `approval_item` queue that a human approves or denies. Endpoints exist and are gated by `agents.approve`; the frontend has **no approvals page** yet (the Stream-1 nav reserved an "Approvals" slot but `NAV_ITEMS` ships only Dashboard/Support/Accounting).

**Scope finding that shaped this spec:** the original "Stream 2 = connectors + approvals UI" was split. **Connectors has no management HTTP API** (only an inbound webhook handler) — building a connectors UI is a separate *full-stack* effort (backend API first). This spec covers **only the approvals queue**. Connectors is tracked separately.

**The key constraint:** the approvals API **deliberately withholds the action `args`** (proposed payload) from responses — per item it returns only `id, agent_run_id, tool, effect_class, state, expires_at`. A human approving "create_jira_issue · external · expires 6d" can't see *what* it does. We therefore add a small, security-reviewed backend addition: a **server-computed, redacted action-summary**.

## 2. Goals & non-goals

**Goals**
- A usable `/approvals` page on the Stream-1 design system: list pending agent actions, approve/deny, with enough context to decide.
- A safe `summary` field on the approval API response (redacted, server-computed; never raw args).
- An Approvals nav item with a pending-count badge.

**Non-goals (out of scope)**
- **Connectors management** (API + UI) — separate full-stack effort (`bd manyforge-conn-ui`).
- A **cross-business** pending-count endpoint / aggregate badge — later follow-up.
- **Bulk** approve/deny — one-at-a-time only (YAGNI).
- Exposing raw `args` to the client — explicitly rejected; only the curated summary.
- Changing the gate / approval lifecycle logic — UI + read-shape only.

## 3. Decisions (locked, with rationale)

| Decision | Choice | Why |
|---|---|---|
| Action context | Server-computed **safe summary** field | API hides `args`; approver needs to know what they're approving without leaking the raw payload. |
| Summary generation | **Compute-at-read** pure function (no migration) | Pending queues are small; recompute is cheap; one testable/pinnable place; no denormalization/drift. |
| Layout | Compact **`mf-table`** rows | Consistent with tickets/accounting; user-selected over cards. |
| Freshness | **Poll ~20s** + refresh after each action | Items arrive/expire over time; keeps list + badge current. |
| Nav badge scope | Pending count for the **currently-selected business** | Approvals are per-business; sidebar is global; no cross-business count endpoint exists. |
| Decision UX | **One-at-a-time** approve/deny; **409 → toast + refresh** | Already-decided/expired is a race, not an error. |

## 4. Backend — safe action-summary

**Endpoint (unchanged):** `GET /api/v1/businesses/{id}/approvals` → list; `POST …/approvals/{approvalID}/approve`; `POST …/approvals/{approvalID}/deny`. Gated by `agents.approve`; 404 for foreign/unauthorized/unknown (no oracle); 409 if state != `pending`.

**Change:** add a `summary string` field to the wire response (`approvalResp` / `toApprovalResp` in `internal/agents/approval_handler.go`), populated by a new pure function in `internal/agents`:

```
func approvalSummary(tool string, args json.RawMessage) string
```

Per-tool, redacted formats (free-text truncated to ~80 chars, ellipsis):
- `add_comment` → `Comment on <ticket ref>: "<body…>"`
- `transition_ticket` → `Transition <ticket ref> → <status>`
- `create_jira_issue` → `Create Jira issue in <project> — "<summary…>"`
- **any unhandled tool → `"<tool>"` generic fallback** — NEVER an echo of raw args.

The function unmarshals `args` into a known per-tool shape; on any unmarshal error or unknown tool, returns the safe generic fallback. `<ticket ref>` uses the external id / native ticket id present in args (safe identifiers, not free text).

**Security (pinned in `internal/security_regression/`, per the program's three-commit discipline):**
1. **No raw args in any response** — the JSON for list/approve/deny never contains the `args` blob; only `summary`. (Source + behavioral pin.)
2. **Redaction** — summary truncates free-text; never includes credentials/secrets (tool args don't carry creds, but pin it).
3. **Authorization unchanged** — endpoints remain gated by `agents.approve` + tenant scope; foreign/unauthorized/unknown → 404.

## 5. Frontend — approvals queue

**Service** `web/src/app/core/approvals.service.ts` (Observable, matches existing core services):
- `list(businessId): Observable<{ items: ApprovalItem[]; next_cursor?: string|null }>` — GET `…/approvals` (confirm at plan time whether the list is keyset-paginated or a capped list; consume whatever it returns; badge count = `items.length`, display `50+` if capped).
- `approve(businessId, approvalId): Observable<ApprovalItem>` · `deny(businessId, approvalId): Observable<ApprovalItem>`.
- `ApprovalItem` type: `{ id, agent_run_id, tool, effect_class: 0|1|2|3, state, expires_at, summary }`.

**Page** `web/src/app/pages/approvals/approvals.ts` + route `/approvals` (authGuard):
- `mf-page-header` "Approvals" + subtitle (N pending); business selector (`mf-field`/`mf-select`, persisted current-business signal shared with the nav badge).
- `mf-table`: header (Effect · Action · Tool · Expires · ⋯) + rows: effect badge (new `effect-class` tone map), `summary` (truncate, one line), tool (mono muted), relative expiry (amber when < ~6h), Deny (`mf-btn-ghost`) + Approve (`mf-btn-primary`). Row click → agent-run detail (`/accounting/:businessId/:agentId`-style or the run route — confirm at plan time).
- `mf-empty-state` when none pending.
- Approve/Deny: optimistic remove + `ToastService` success; **409 → `ToastService` + re-list**; other errors → toast.
- **Polling:** re-list every ~20s (clear on destroy) + after each action.

**Effect-class tone map** (`web/src/app/ui/status.ts`, extend): `0 read → neutral`, `1 reversible → accent`, `2 external → warn`, `3 irreversible → danger`; labels Read/Reversible/External/Irreversible.

**Nav** (`web/src/app/ui/nav.ts`): add `{ label: 'Approvals', route: '/approvals', testid: 'nav-approvals', badge }`. Badge = pending count for the current business, fed by a small shared `ApprovalsService` signal polled ~20s; hidden when 0.

## 6. Testing plan

- **Backend unit:** `approvalSummary` per tool (incl. truncation, the unknown-tool fallback, malformed args → fallback).
- **Backend security pins** (`internal/security_regression/`): response contains `summary` but never `args`; redaction/truncation; `agents.approve` + tenant 404 gating intact. Run under `make sec-test`.
- **Frontend unit (Vitest):** `ApprovalsService`; `ApprovalsComponent` (renders rows + effect badges in both themes, approve removes + toasts, 409 path re-lists, empty state, polling timer set/cleared); effect-class tone map.
- **Playwright e2e** (mocked API): list renders with summaries + effect badges; approve removes row + toast; deny; 409 re-fetch; nav badge reflects count; both themes render with zero console errors.
- **Real-browser** verification of `/approvals` in light + dark before done. Full `make test` + `make sec-test` + frontend build + e2e gate.

## 7. Open items to confirm at plan time (not blockers)
- Exact list response shape (paginated vs capped) and the agent-run detail route to link rows to — read the live handler/routes.
- Whether `add_comment`/`create_jira_issue`/`transition_ticket` are the exact tool names + arg field names (read `internal/agents/tools.go` + `internal/connectors/agent_gateway.go`).
