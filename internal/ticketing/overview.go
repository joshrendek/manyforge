package ticketing

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// OverviewAttentionLimit caps the cross-business attention list (spec 019).
const OverviewAttentionLimit = 25

// OverviewCounts is one business's (or the pooled) support KPIs. Definitions live in
// specs/019-support-overview/spec.md.
type OverviewCounts struct {
	Open                         int64      `json:"open"`
	New                          int64      `json:"new"`
	Pending                      int64      `json:"pending"`
	AwaitingReply                int64      `json:"awaiting_reply"`
	Unassigned                   int64      `json:"unassigned"`
	Urgent                       int64      `json:"urgent"`
	High                         int64      `json:"high"`
	OldestAwaitingAt             *time.Time `json:"oldest_awaiting_at"`
	Created7d                    int64      `json:"created_7d"`
	Resolved7d                   int64      `json:"resolved_7d"`
	MedianFirstResponseSeconds7d *float64   `json:"median_first_response_seconds_7d"`
}

// OverviewBusiness is one readable business's row.
type OverviewBusiness struct {
	BusinessID   uuid.UUID `json:"business_id"`
	BusinessName string    `json:"business_name"`
	OverviewCounts
}

// OverviewTicket is an attention-list entry: a ticket whose customer is waiting on us.
// Deliberately no bodies or requester PII.
type OverviewTicket struct {
	BusinessID   uuid.UUID `json:"business_id"`
	BusinessName string    `json:"business_name"`
	TicketID     uuid.UUID `json:"ticket_id"`
	Subject      string    `json:"subject"`
	Status       string    `json:"status"`
	Priority     string    `json:"priority"`
	Assigned     bool      `json:"assigned"`
	WaitingSince time.Time `json:"waiting_since"`
}

// Overview is the GET /support/overview body.
type Overview struct {
	AsOf       time.Time          `json:"as_of"`
	Totals     OverviewCounts     `json:"totals"`
	Businesses []OverviewBusiness `json:"businesses"`
	Attention  []OverviewTicket   `json:"attention"`
}

// OverviewRoutes belongs on the authenticated router, NOT a business-path permission group:
// there is no business id to gate on. tickets.read is applied per business in SQL.
func (h *Handler) OverviewRoutes(r chi.Router) {
	r.Get("/support/overview", h.overview)
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	principalID, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, httpx.ErrorBody{Code: "UNAUTHORIZED", Message: "authentication required"})
		return
	}
	out, err := h.svc.Overview(r.Context(), principalID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Overview reports support KPIs for every active business where the caller holds
// tickets.read, plus the most urgent waiting tickets across them. The permission filter is
// in SQL (overviewScopeSQL) and the reads run under RLS, so an unauthorized business can
// contribute nothing even to the totals.
func (s *Service) Overview(ctx context.Context, principalID uuid.UUID) (Overview, error) {
	out := Overview{Businesses: []OverviewBusiness{}, Attention: []OverviewTicket{}}
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, overviewCountsSQL)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				isTotal bool
				id      *uuid.UUID
				name    *string
				c       OverviewCounts
			)
			if err := rows.Scan(&isTotal, &id, &name, &out.AsOf,
				&c.Open, &c.New, &c.Pending, &c.AwaitingReply, &c.Unassigned, &c.Urgent, &c.High,
				&c.OldestAwaitingAt, &c.Created7d, &c.Resolved7d, &c.MedianFirstResponseSeconds7d); err != nil {
				return err
			}
			if isTotal {
				out.Totals = c
				continue
			}
			out.Businesses = append(out.Businesses, OverviewBusiness{BusinessID: *id, BusinessName: *name, OverviewCounts: c})
		}
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.Query(ctx, overviewAttentionSQL, OverviewAttentionLimit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t OverviewTicket
			if err := rows.Scan(&t.BusinessID, &t.BusinessName, &t.TicketID, &t.Subject, &t.Status,
				&t.Priority, &t.Assigned, &t.WaitingSince); err != nil {
				return err
			}
			out.Attention = append(out.Attention, t)
		}
		return rows.Err()
	})
	if err != nil {
		return Overview{}, mapErr(err)
	}
	return out, nil
}

// overviewScopeSQL is the shared prefix: the readable businesses, and only the tickets any
// KPI can need (open set, or touched in the trailing 7 days) — not a business's full history.
// last_* is the newest non-note message, read only for new/open tickets: a note is internal,
// so it neither answers the customer nor makes them wait.
const overviewScopeSQL = `
WITH scope AS MATERIALIZED (
    SELECT b.id, b.tenant_root_id, b.name
    FROM business b
    WHERE b.status = 'active' AND b.deleted_at IS NULL
      AND b.id IN (SELECT business_id FROM businesses_with_permission(current_principal(), 'tickets.read'))
), bounds AS MATERIALIZED (
    SELECT statement_timestamp() AS as_of, statement_timestamp() - interval '7 days' AS since
), tk AS MATERIALIZED (
    SELECT t.id, t.business_id, t.subject, t.status, t.priority, t.assignee_principal_id,
           t.created_at, t.updated_at,
           t.status IN ('new', 'open') AND (lm.direction IS NULL OR lm.direction = 'inbound') AS awaiting,
           COALESCE(lm.created_at, t.created_at) AS waiting_since,
           CASE WHEN t.created_at >= bo.since THEN (
               SELECT min(m.created_at) FROM ticket_message m
               WHERE m.ticket_id = t.id AND m.tenant_root_id = t.tenant_root_id AND m.direction = 'outbound'
           ) END AS first_response_at
    FROM ticket t
    JOIN scope s ON s.id = t.business_id AND s.tenant_root_id = t.tenant_root_id
    CROSS JOIN bounds bo
    LEFT JOIN LATERAL (
        SELECT m.direction, m.created_at FROM ticket_message m
        WHERE m.ticket_id = t.id AND m.tenant_root_id = t.tenant_root_id AND m.direction <> 'note'
        ORDER BY m.created_at DESC, m.id DESC
        LIMIT 1
    ) lm ON t.status IN ('new', 'open')
    WHERE t.redacted_at IS NULL
      AND (t.status IN ('new', 'open', 'pending') OR t.created_at >= bo.since OR t.updated_at >= bo.since)
)
`

// overviewCountsSQL emits one row per readable business plus a pooled totals row (the ()
// grouping set) — so the totals median is over all tickets, not an average of medians. The
// totals row always exists, even with no readable business.
const overviewCountsSQL = overviewScopeSQL + `
SELECT GROUPING(s.id) = 1 AS is_total, s.id, s.name, (SELECT as_of FROM bounds),
       count(tk.id) FILTER (WHERE tk.status IN ('new', 'open', 'pending')),
       count(tk.id) FILTER (WHERE tk.status = 'new'),
       count(tk.id) FILTER (WHERE tk.status = 'pending'),
       count(tk.id) FILTER (WHERE tk.awaiting),
       count(tk.id) FILTER (WHERE tk.status IN ('new', 'open', 'pending') AND tk.assignee_principal_id IS NULL),
       count(tk.id) FILTER (WHERE tk.status IN ('new', 'open', 'pending') AND tk.priority = 'urgent'),
       count(tk.id) FILTER (WHERE tk.status IN ('new', 'open', 'pending') AND tk.priority = 'high'),
       min(tk.waiting_since) FILTER (WHERE tk.awaiting),
       count(tk.id) FILTER (WHERE tk.created_at >= (SELECT since FROM bounds)),
       count(tk.id) FILTER (WHERE tk.status IN ('solved', 'closed') AND tk.updated_at >= (SELECT since FROM bounds)),
       percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM tk.first_response_at - tk.created_at))
           FILTER (WHERE tk.first_response_at IS NOT NULL)
FROM scope s
LEFT JOIN tk ON tk.business_id = s.id
GROUP BY GROUPING SETS ((s.id, s.name), ())
ORDER BY is_total DESC,
         count(tk.id) FILTER (WHERE tk.awaiting) DESC,
         count(tk.id) FILTER (WHERE tk.status IN ('new', 'open', 'pending')) DESC,
         s.name, s.id
`

// overviewAttentionSQL: waiting tickets, most urgent priority first, then longest waiting.
const overviewAttentionSQL = overviewScopeSQL + `
SELECT s.id, s.name, tk.id, tk.subject, tk.status::text, tk.priority::text,
       tk.assignee_principal_id IS NOT NULL, tk.waiting_since
FROM tk
JOIN scope s ON s.id = tk.business_id
WHERE tk.awaiting
ORDER BY tk.priority DESC, tk.waiting_since ASC, tk.id
LIMIT $1
`
