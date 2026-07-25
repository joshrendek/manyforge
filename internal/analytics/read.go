package analytics

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	appdb "github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// Every read here goes against the ROLLUP tables, never raw analytics_event. Raw events are
// partitioned and aged out at 90 days; the rollups are small, indexed by (client, date), and stay
// fast as volume grows. A dashboard that queried raw events would get slower every day it worked.

// maxRangeDays bounds a query window so a caller cannot ask for an unbounded scan.
const maxRangeDays = 366

// topN bounds the page/referrer lists returned to a dashboard.
const topN = 20

// Summary is everything the dashboard needs for one site over one window.
type Summary struct {
	From            string      `json:"from"`
	To              string      `json:"to"`
	Pageviews       int64       `json:"pageviews"`
	Visitors        int64       `json:"visitors"`
	Series          []DayPoint  `json:"series"`
	TopPages        []PathCount `json:"top_pages"`
	TopReferrers    []HostCount `json:"top_referrers"`
	DirectPageviews int64       `json:"direct_pageviews"`
}

type DayPoint struct {
	Date      string `json:"date"`
	Pageviews int64  `json:"pageviews"`
	Visitors  int64  `json:"visitors"`
}

type PathCount struct {
	Path      string `json:"path"`
	Pageviews int64  `json:"pageviews"`
	Visitors  int64  `json:"visitors"`
}

type HostCount struct {
	Host      string `json:"host"`
	Pageviews int64  `json:"pageviews"`
	Visitors  int64  `json:"visitors"`
}

// Service reads analytics aggregates under the caller's principal, so RLS scopes every query to
// businesses the caller actually belongs to.
type Service struct{ DB *appdb.DB }

func NewService(database *appdb.DB) *Service { return &Service{DB: database} }

// Summary returns aggregates for one client over [from, to] inclusive, in UTC days.
//
// clientID is validated against the URL business in SQL, not in the handler: a caller who owns
// business A must not be able to read business B's site by passing its client id.
func (s *Service) Summary(ctx context.Context, principalID, businessID, clientID uuid.UUID, from, to time.Time) (Summary, error) {
	if to.Before(from) {
		return Summary{}, fmt.Errorf("analytics: end before start: %w", errs.ErrValidation)
	}
	if to.Sub(from) > maxRangeDays*24*time.Hour {
		return Summary{}, fmt.Errorf("analytics: range exceeds %d days: %w", maxRangeDays, errs.ErrValidation)
	}
	fromD, toD := from.UTC().Format("2006-01-02"), to.UTC().Format("2006-01-02")

	out := Summary{From: fromD, To: toD, Series: []DayPoint{}, TopPages: []PathCount{}, TopReferrers: []HostCount{}}
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		// Ownership: the client must belong to the URL business. RLS already scopes
		// telemetry_client, but asserting business_id here stops a sibling business's site id from
		// being read through this route.
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM telemetry_client
			                WHERE id = $1 AND business_id = $2 AND kind = 'analytics')`,
			clientID, businessID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			// Unknown id and foreign id are the same answer — no site-existence oracle.
			return errs.ErrNotFound
		}

		// Daily series + totals.
		rows, err := tx.Query(ctx,
			`SELECT bucket_date::text, pageviews, visitors
			   FROM analytics_daily
			  WHERE client_id = $1 AND business_id = $2
			    AND bucket_date >= $3::date AND bucket_date <= $4::date
			  ORDER BY bucket_date`,
			clientID, businessID, fromD, toD)
		if err != nil {
			return err
		}
		for rows.Next() {
			var d DayPoint
			if err := rows.Scan(&d.Date, &d.Pageviews, &d.Visitors); err != nil {
				rows.Close()
				return err
			}
			out.Series = append(out.Series, d)
			out.Pageviews += d.Pageviews
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		// Visitors is NOT the sum of the daily series: the hash rotates daily, so the same person
		// on two days is two daily visitors. Summing would overstate a multi-day total. There is
		// no cross-day identifier by design, so the honest window figure is the busiest day's
		// count — reported as "daily unique visitors, peak" in the UI rather than implying a
		// deduplicated total we cannot compute.
		for _, d := range out.Series {
			if d.Visitors > out.Visitors {
				out.Visitors = d.Visitors
			}
		}

		if err := s.topPages(ctx, tx, &out, clientID, businessID, fromD, toD); err != nil {
			return err
		}
		return s.topReferrers(ctx, tx, &out, clientID, businessID, fromD, toD)
	})
	if err != nil {
		return Summary{}, mapErr(err)
	}
	var refTotal int64
	for _, r := range out.TopReferrers {
		refTotal += r.Pageviews
	}
	if d := out.Pageviews - refTotal; d > 0 {
		out.DirectPageviews = d
	}
	return out, nil
}

func (s *Service) topPages(ctx context.Context, tx pgx.Tx, out *Summary, clientID, businessID uuid.UUID, fromD, toD string) error {
	rows, err := tx.Query(ctx,
		`SELECT path, sum(pageviews)::bigint, max(visitors)::bigint
		   FROM analytics_page_daily
		  WHERE client_id = $1 AND business_id = $2
		    AND bucket_date >= $3::date AND bucket_date <= $4::date
		  GROUP BY path
		  ORDER BY sum(pageviews) DESC, path
		  LIMIT $5`,
		clientID, businessID, fromD, toD, topN)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p PathCount
		if err := rows.Scan(&p.Path, &p.Pageviews, &p.Visitors); err != nil {
			return err
		}
		out.TopPages = append(out.TopPages, p)
	}
	return rows.Err()
}

func (s *Service) topReferrers(ctx context.Context, tx pgx.Tx, out *Summary, clientID, businessID uuid.UUID, fromD, toD string) error {
	rows, err := tx.Query(ctx,
		`SELECT referrer_host, sum(pageviews)::bigint, max(visitors)::bigint
		   FROM analytics_referrer_daily
		  WHERE client_id = $1 AND business_id = $2
		    AND bucket_date >= $3::date AND bucket_date <= $4::date
		  GROUP BY referrer_host
		  ORDER BY sum(pageviews) DESC, referrer_host
		  LIMIT $5`,
		clientID, businessID, fromD, toD, topN)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var h HostCount
		if err := rows.Scan(&h.Host, &h.Pageviews, &h.Visitors); err != nil {
			return err
		}
		out.TopReferrers = append(out.TopReferrers, h)
	}
	return rows.Err()
}
