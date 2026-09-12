package mailing

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// MailingReportRate is a weighted message rate, not an average of campaign rates.
// A cohort with no eligible messages has no defined percentage.
type MailingReportRate struct {
	Numerator   int64    `json:"numerator"`
	Denominator int64    `json:"denominator"`
	Percent     *float64 `json:"percent"`
}

// MailingReport contains aggregates only. Subscriber counts describe consent,
// not deliverability; engagement includes bots and privacy proxies. Rates use
// accepted deliveries queued in the trailing seven days, not a send-date cohort.
type MailingReport struct {
	AsOf                     time.Time         `json:"as_of"`
	WindowStart              time.Time         `json:"window_start"`
	SubscriberWindowStart    time.Time         `json:"subscriber_window_start"`
	SubscriberWindowComplete bool              `json:"subscriber_window_complete"`
	BusinessCount            int64             `json:"business_count"`
	TenantCount              int64             `json:"tenant_count"`
	ActiveAutomations        int64             `json:"active_automations"`
	ActiveEnrollments        int64             `json:"active_enrollments"`
	ActiveSubscribers        int64             `json:"active_subscribers"`
	SubscriberNetAdditions   int64             `json:"subscriber_net_additions"`
	OpenRate                 MailingReportRate `json:"open_rate"`
	ClickRate                MailingReportRate `json:"click_rate"`
	UnsubscribeRate          MailingReportRate `json:"unsubscribe_rate"`
}

// ReportingRoutes belongs in the authenticated group, NOT the business-path
// permission group. Reporting applies mailing.read to every business in SQL.
func (h *Handler) ReportingRoutes(r chi.Router) {
	r.Get("/mailing/reporting", h.reporting)
}

func (h *Handler) reporting(w http.ResponseWriter, r *http.Request) {
	principalID, ok := httpx.PrincipalFromContext(r.Context())
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, httpx.ErrorBody{Code: "UNAUTHORIZED", Message: "authentication required"})
		return
	}
	var businessID *uuid.UUID
	if values, present := r.URL.Query()["business_id"]; present {
		if len(values) != 1 {
			httpx.WriteError(w, r, errs.ErrNotFound)
			return
		}
		id, err := uuid.Parse(values[0])
		if err != nil {
			httpx.WriteError(w, r, errs.ErrNotFound)
			return
		}
		businessID = &id
	}
	out, err := h.svc.Reporting(r.Context(), principalID, businessID)
	write(w, r, http.StatusOK, out, err)
}

// Reporting reads one MVCC snapshot with a fixed aggregate query. There is no
// per-business/list loop, row cap, cached membership authorization, or report-time
// write. Both subscriber balances use the SAME current authorized business set.
// Active enrollments intentionally include those on paused automations.
func (s *Service) Reporting(ctx context.Context, principalID uuid.UUID, businessID *uuid.UUID) (MailingReport, error) {
	var out MailingReport
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, mailingReportingSQL, businessID).Scan(
			&out.AsOf, &out.WindowStart, &out.SubscriberWindowStart, &out.SubscriberWindowComplete,
			&out.BusinessCount, &out.TenantCount, &out.ActiveAutomations, &out.ActiveEnrollments,
			&out.ActiveSubscribers, &out.SubscriberNetAdditions,
			&out.OpenRate.Numerator, &out.OpenRate.Denominator,
			&out.ClickRate.Numerator, &out.ClickRate.Denominator,
			&out.UnsubscribeRate.Numerator, &out.UnsubscribeRate.Denominator,
		)
	})
	if err != nil {
		return MailingReport{}, mapErr(err)
	}
	for _, rate := range []*MailingReportRate{&out.OpenRate, &out.ClickRate, &out.UnsubscribeRate} {
		if rate.Denominator != 0 {
			percent := 100 * float64(rate.Numerator) / float64(rate.Denominator)
			rate.Percent = &percent
		}
	}
	return out, nil
}

const mailingReportingSQL = `
WITH scope AS MATERIALIZED (
    SELECT b.id, b.tenant_root_id
    FROM business b
    WHERE b.status = 'active' AND b.deleted_at IS NULL
      AND ($1::uuid IS NULL OR b.id = $1::uuid)
      AND b.id IN (SELECT business_id FROM businesses_with_permission(current_principal(), 'mailing.read'))
), bounds AS MATERIALIZED (
    SELECT statement_timestamp() AS as_of,
           statement_timestamp() - interval '7 days' AS window_start,
           GREATEST(statement_timestamp() - interval '7 days',
               (SELECT history_started_at FROM mailing_reporting_state),
               (SELECT max(h.history_started_at) FROM mailing_reporting_business h
                JOIN scope s ON s.id = h.business_id AND s.tenant_root_id = h.tenant_root_id)
           ) AS subscriber_window_start
), current_subscribers AS (
    SELECT count(DISTINCT (s.tenant_root_id, lower(m.email::text))) AS total
    FROM scope s
    JOIN list_subscriber m ON m.business_id = s.id AND m.tenant_root_id = s.tenant_root_id
    JOIN mailing_list l ON l.id = m.list_id AND l.business_id = s.id AND l.tenant_root_id = s.tenant_root_id
    WHERE m.status = 'active' AND l.status = 'active'
), previous_subscribers AS (
    SELECT count(DISTINCT (s.tenant_root_id, m.identity_fingerprint)) AS total
    FROM scope s
    JOIN mailing_reporting_membership m ON m.business_id = s.id AND m.tenant_root_id = s.tenant_root_id
    CROSS JOIN bounds b
    WHERE m.started_at <= b.subscriber_window_start
      AND COALESCE(m.ended_at, 'infinity'::timestamptz) > b.subscriber_window_start
      AND EXISTS (
          SELECT 1 FROM mailing_reporting_list l
          WHERE l.list_id = m.list_id AND l.business_id = s.id AND l.tenant_root_id = s.tenant_root_id
            AND l.started_at <= b.subscriber_window_start
            AND COALESCE(l.ended_at, 'infinity'::timestamptz) > b.subscriber_window_start
      )
), cohort AS MATERIALIZED (
    SELECT d.id, d.business_id, d.tenant_root_id, d.opened_at, d.first_clicked_at,
           COALESCE(d.track_opens_override, c.track_opens, false) AS track_opens,
           COALESCE(d.track_clicks_override, c.track_clicks, false) AS track_clicks
    FROM mailing_delivery d
    JOIN scope s ON s.id = d.business_id AND s.tenant_root_id = d.tenant_root_id
    LEFT JOIN campaign c ON c.id = d.campaign_id AND c.business_id = s.id AND c.tenant_root_id = s.tenant_root_id
    CROSS JOIN bounds b
    WHERE d.created_at >= b.window_start AND d.created_at < b.as_of
      AND d.status IN ('sent', 'delivered', 'bounced', 'complained')
), engagement AS (
    SELECT count(*) FILTER (WHERE d.track_opens AND d.opened_at <= b.as_of) AS opens,
           count(*) FILTER (WHERE d.track_opens) AS open_eligible,
           count(*) FILTER (WHERE d.track_clicks AND d.first_clicked_at <= b.as_of) AS clicks,
           count(*) FILTER (WHERE d.track_clicks) AS click_eligible,
           count(*) FILTER (WHERE EXISTS (
               SELECT 1 FROM mailing_tracking_event e
               WHERE e.delivery_id = d.id AND e.business_id = d.business_id AND e.tenant_root_id = d.tenant_root_id
                 AND e.kind = 'unsubscribe' AND e.occurred_at <= b.as_of
           )) AS unsubscribes,
           count(*) AS accepted
    FROM cohort d CROSS JOIN bounds b
)
SELECT b.as_of, b.window_start, b.subscriber_window_start,
       b.subscriber_window_start = b.window_start,
       (SELECT count(*) FROM scope), (SELECT count(DISTINCT tenant_root_id) FROM scope),
       (SELECT count(*) FROM automation a JOIN scope s ON s.id = a.business_id AND s.tenant_root_id = a.tenant_root_id
        WHERE a.status = 'active'),
       (SELECT count(*) FROM automation_enrollment e JOIN scope s ON s.id = e.business_id AND s.tenant_root_id = e.tenant_root_id
        WHERE e.status = 'active'),
       cur.total, cur.total - prev.total,
       e.opens, e.open_eligible, e.clicks, e.click_eligible, e.unsubscribes, e.accepted
FROM bounds b CROSS JOIN current_subscribers cur CROSS JOIN previous_subscribers prev CROSS JOIN engagement e
WHERE $1::uuid IS NULL OR EXISTS (SELECT 1 FROM scope)
`
