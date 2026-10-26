# Spec 019 — Support Overview (cross-business)

Tracking: `manyforge-wuhu`

## Problem

Support is scoped to one selected business at a time (`/support`). An operator who runs several
businesses has to switch context per business to learn where customers are waiting. They need a
single page with every business they can read support for: headline KPIs per business and a short
list of the tickets that need attention first.

## Scope

- `GET /api/v1/support/overview` — authenticated, no business in the path. Like
  `/mailing/reporting` and `/analytics/overview`, the permission is applied **in SQL**: only
  active, non-deleted businesses where the caller holds `tickets.read`
  (`businesses_with_permission(current_principal(), 'tickets.read')`) are included, and the read
  also runs under RLS (`WithPrincipal`). Redacted tickets are excluded everywhere.
- Web page `/support/overview` ("Support overview"), linked from the nav and the ticket list.

Out of scope: SLA targets/policies, historical trend charts, per-agent leaderboards.

## Definitions

"Open set" = status `new`, `open` or `pending`.

Per business (every readable business is listed, including ones with zero tickets):

| Field | Definition |
| --- | --- |
| `open` | tickets in the open set |
| `new` | status `new` |
| `pending` | status `pending` (waiting on the customer) |
| `awaiting_reply` | status `new`/`open` whose latest non-note message is inbound — the customer is waiting on us |
| `unassigned` | open set with no assignee |
| `urgent` / `high` | open set at that priority |
| `oldest_awaiting_at` | `last_message_at` of the longest-waiting `awaiting_reply` ticket (null if none) |
| `created_7d` | tickets created in the trailing 7 days |
| `resolved_7d` | tickets currently `solved`/`closed` whose `updated_at` is in the trailing 7 days |
| `median_first_response_seconds_7d` | median of (first outbound message − ticket creation) for tickets created in the trailing 7 days that have an outbound reply; null when none |

`totals` aggregates the same counters across all listed businesses (median is computed over the
pooled tickets, not averaged; `oldest_awaiting_at` is the minimum).

`attention`: up to 25 `awaiting_reply` tickets across all businesses, ordered by priority
(urgent → low) then longest waiting first. Each item carries business id/name, ticket id,
subject, status, priority, whether it is assigned, and `waiting_since` (`last_message_at`).
No message bodies or requester PII.

Businesses are ordered by `awaiting_reply` desc, then `open` desc, then name.

## Security

- No path id → no `RequirePermission` middleware; the `tickets.read` filter lives in the single
  SQL statement (same reasoning as `mailing.ReportingRoutes`). A caller with no readable business
  gets empty lists and zero totals, never an error that reveals other tenants.
- Under RLS the ticket rows of unauthorized tenants are invisible even if the permission CTE
  were wrong (defence in depth).

## Test plan

- Integration (`internal/ticketing/overview_integration_test.go`, testcontainers):
  - KPI arithmetic on a seeded business: open/new/pending/awaiting (inbound-last vs
    outbound-last vs note-last), unassigned, urgent/high, created/resolved 7d, median first
    response, redacted tickets excluded.
  - Cross-business: two businesses for one principal → both listed, totals sum, attention list
    spans both and is priority-then-age ordered and capped.
  - Authorization: a business where the caller is a member without `tickets.read`, and another
    tenant's business, contribute nothing (not listed, not in totals or attention).
  - HTTP: unauthenticated → 401.
- OpenAPI contract test (`go test -tags contract ./cmd/...`) covers the new route.
- Web unit spec (`pages/support/overview.spec.ts`): renders totals, business rows, attention
  links to `/support/:businessId/:tid`, empty and error states.
- Playwright e2e (`web/e2e/support-overview.spec.ts`): mocked API, page renders KPIs and
  navigating from an attention item opens the thread route.
