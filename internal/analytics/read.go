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
	// Breakdowns is keyed by dimension ("device", "country", …). A dimension with no data is
	// present but empty, so a dashboard can distinguish "nothing collected yet" from "not a
	// dimension we track".
	Breakdowns map[string][]ValueCount `json:"breakdowns"`
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

// ValueCount is one row of a generic dimension breakdown (utm_source, device, browser, country…).
type ValueCount struct {
	Value     string `json:"value"`
	Pageviews int64  `json:"pageviews"`
	Visitors  int64  `json:"visitors"`
}

// Dimension keys served by the breakdowns map. These are matched against an allowlist before
// reaching SQL — the value is a WHERE parameter, but constraining it also keeps a caller from
// probing for dimensions that do not exist.
var knownDimensions = []string{
	"utm_source", "utm_medium", "utm_campaign", "device", "browser", "country",
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
	// The window is INCLUSIVE, so a span of N intervals covers N+1 days. Comparing the span
	// against maxRangeDays with a strict > therefore admitted maxRangeDays+1 actual days.
	if inclusiveDays := int(to.Sub(from)/(24*time.Hour)) + 1; inclusiveDays > maxRangeDays {
		return Summary{}, fmt.Errorf("analytics: range of %d days exceeds the %d day cap: %w",
			inclusiveDays, maxRangeDays, errs.ErrValidation)
	}
	fromD, toD := from.UTC().Format("2006-01-02"), to.UTC().Format("2006-01-02")

	out := Summary{
		From: fromD, To: toD,
		Series: []DayPoint{}, TopPages: []PathCount{}, TopReferrers: []HostCount{},
		Breakdowns: map[string][]ValueCount{},
	}
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
		if err := s.topReferrers(ctx, tx, &out, clientID, businessID, fromD, toD); err != nil {
			return err
		}

		// Direct traffic is total minus ALL attributed referrer pageviews — deliberately a
		// separate SUM rather than adding up TopReferrers, which is capped at topN. Deriving it
		// from the capped list would silently reclassify every referrer beyond the top 20 as
		// "direct", and the error would grow precisely for the sites with the most diverse
		// traffic.
		var attributed int64
		if err := tx.QueryRow(ctx,
			`SELECT coalesce(sum(pageviews), 0)::bigint
			   FROM analytics_referrer_daily
			  WHERE client_id = $1 AND business_id = $2
			    AND bucket_date >= $3::date AND bucket_date <= $4::date`,
			clientID, businessID, fromD, toD).Scan(&attributed); err != nil {
			return err
		}
		if d := out.Pageviews - attributed; d > 0 {
			out.DirectPageviews = d
		}
		return s.breakdowns(ctx, tx, &out, clientID, businessID, fromD, toD)
	})
	if err != nil {
		return Summary{}, mapErr(err)
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

// breakdowns loads every known dimension in ONE query rather than one round trip per dimension.
// Six sequential queries would triple the dashboard's latency for no benefit — they hit the same
// index on the same rows.
func (s *Service) breakdowns(ctx context.Context, tx pgx.Tx, out *Summary, clientID, businessID uuid.UUID, fromD, toD string) error {
	for _, d := range knownDimensions {
		out.Breakdowns[d] = []ValueCount{}
	}
	rows, err := tx.Query(ctx,
		`SELECT dimension, value, pageviews, visitors FROM (
		     SELECT dimension, value,
		            sum(pageviews)::bigint AS pageviews,
		            max(visitors)::bigint  AS visitors,
		            row_number() OVER (PARTITION BY dimension ORDER BY sum(pageviews) DESC, value) AS rn
		       FROM analytics_dimension_daily
		      WHERE client_id = $1 AND business_id = $2
		        AND bucket_date >= $3::date AND bucket_date <= $4::date
		        AND dimension = ANY($5)
		      GROUP BY dimension, value
		 ) ranked
		 WHERE rn <= $6
		 ORDER BY dimension, pageviews DESC, value`,
		clientID, businessID, fromD, toD, knownDimensions, topN)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var dim string
		var v ValueCount
		if err := rows.Scan(&dim, &v.Value, &v.Pageviews, &v.Visitors); err != nil {
			return err
		}
		out.Breakdowns[dim] = append(out.Breakdowns[dim], v)
	}
	return rows.Err()
}
