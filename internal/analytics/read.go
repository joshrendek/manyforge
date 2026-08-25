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
	From                 string            `json:"from"`
	To                   string            `json:"to"`
	Pageviews            int64             `json:"pageviews"`
	Visitors             int64             `json:"visitors"`
	AverageDailyVisitors float64           `json:"average_daily_visitors"`
	Series               []DayPoint        `json:"series"`
	TopPages             []PathCount       `json:"top_pages"`
	TopReferrers         []HostCount       `json:"top_referrers"`
	DirectPageviews      int64             `json:"direct_pageviews"`
	DirectShare          float64           `json:"direct_share"`
	Comparison           SummaryComparison `json:"comparison"`
	// Breakdowns is keyed by dimension ("device", "country", …). A dimension with no data is
	// present but empty, so a dashboard can distinguish "nothing collected yet" from "not a
	// dimension we track".
	Breakdowns map[string][]ValueCount `json:"breakdowns"`
}

// SummaryComparison is the immediately preceding window with the same inclusive day count.
// Percentage changes are null when the previous value is zero, because no finite percentage is
// mathematically defined from that baseline. DirectShareChange is expressed in percentage points.
type SummaryComparison struct {
	From                              string   `json:"from"`
	To                                string   `json:"to"`
	Pageviews                         int64    `json:"pageviews"`
	AverageDailyVisitors              float64  `json:"average_daily_visitors"`
	DirectPageviews                   int64    `json:"direct_pageviews"`
	DirectShare                       float64  `json:"direct_share"`
	PageviewsChangePercent            *float64 `json:"pageviews_change_percent"`
	AverageDailyVisitorsChangePercent *float64 `json:"average_daily_visitors_change_percent"`
	DirectShareChangePercentagePoints float64  `json:"direct_share_change_percentage_points"`
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
	"utm_source", "utm_medium", "utm_campaign", "device", "browser", "country", "event",
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
	from = from.UTC().Truncate(24 * time.Hour)
	to = to.UTC().Truncate(24 * time.Hour)
	fromD, toD := from.Format("2006-01-02"), to.Format("2006-01-02")
	days := int(to.Sub(from)/(24*time.Hour)) + 1
	previousTo := from.AddDate(0, 0, -1)
	previousFrom := previousTo.AddDate(0, 0, -(days - 1))
	previousFromD, previousToD := previousFrom.Format("2006-01-02"), previousTo.Format("2006-01-02")

	out := Summary{
		From: fromD, To: toD,
		Series: []DayPoint{}, TopPages: []PathCount{}, TopReferrers: []HostCount{},
		Breakdowns: map[string][]ValueCount{},
		Comparison: SummaryComparison{From: previousFromD, To: previousToD},
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

		// Daily series are completed against a UTC date spine before any totals are derived. The
		// dashboard therefore receives one point per requested day even when no rollup row exists.
		series, err := loadDailySeries(ctx, tx, clientID, businessID, from, to)
		if err != nil {
			return err
		}
		previousSeries, err := loadDailySeries(ctx, tx, clientID, businessID, previousFrom, previousTo)
		if err != nil {
			return err
		}
		out.Series = series
		out.Pageviews, out.Visitors, out.AverageDailyVisitors = seriesMetrics(series)
		out.Comparison.Pageviews, _, out.Comparison.AverageDailyVisitors = seriesMetrics(previousSeries)

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
		attributed, err := attributedPageviews(ctx, tx, clientID, businessID, fromD, toD)
		if err != nil {
			return err
		}
		if d := out.Pageviews - attributed; d > 0 {
			out.DirectPageviews = d
		}
		out.DirectShare = percent(out.DirectPageviews, out.Pageviews)

		previousAttributed, err := attributedPageviews(ctx, tx, clientID, businessID, previousFromD, previousToD)
		if err != nil {
			return err
		}
		if d := out.Comparison.Pageviews - previousAttributed; d > 0 {
			out.Comparison.DirectPageviews = d
		}
		out.Comparison.DirectShare = percent(out.Comparison.DirectPageviews, out.Comparison.Pageviews)
		out.Comparison.PageviewsChangePercent = percentChange(float64(out.Pageviews), float64(out.Comparison.Pageviews))
		out.Comparison.AverageDailyVisitorsChangePercent = percentChange(out.AverageDailyVisitors, out.Comparison.AverageDailyVisitors)
		out.Comparison.DirectShareChangePercentagePoints = out.DirectShare - out.Comparison.DirectShare
		return s.breakdowns(ctx, tx, &out, clientID, businessID, fromD, toD)
	})
	if err != nil {
		return Summary{}, mapErr(err)
	}
	return out, nil
}

func loadDailySeries(ctx context.Context, tx pgx.Tx, clientID, businessID uuid.UUID, from, to time.Time) ([]DayPoint, error) {
	fromD, toD := from.Format("2006-01-02"), to.Format("2006-01-02")
	rows, err := tx.Query(ctx,
		`SELECT bucket_date::text, pageviews, visitors
		   FROM analytics_daily
		  WHERE client_id = $1 AND business_id = $2
		    AND bucket_date >= $3::date AND bucket_date <= $4::date
		  ORDER BY bucket_date`,
		clientID, businessID, fromD, toD)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sparse := []DayPoint{}
	for rows.Next() {
		var d DayPoint
		if err := rows.Scan(&d.Date, &d.Pageviews, &d.Visitors); err != nil {
			return nil, err
		}
		sparse = append(sparse, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return completeDailySeries(sparse, from, to), nil
}

func completeDailySeries(sparse []DayPoint, from, to time.Time) []DayPoint {
	byDate := make(map[string]DayPoint, len(sparse))
	for _, point := range sparse {
		byDate[point.Date] = point
	}
	series := make([]DayPoint, 0, int(to.Sub(from)/(24*time.Hour))+1)
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		point := byDate[date]
		point.Date = date
		series = append(series, point)
	}
	return series
}

// seriesMetrics returns pageviews, peak daily visitors, and average daily visitors. The average
// includes zero-traffic days from the completed series and is therefore comparable across windows.
func seriesMetrics(series []DayPoint) (int64, int64, float64) {
	if len(series) == 0 {
		return 0, 0, 0
	}
	var pageviews, peakVisitors, visitorDays int64
	for _, point := range series {
		pageviews += point.Pageviews
		visitorDays += point.Visitors
		if point.Visitors > peakVisitors {
			peakVisitors = point.Visitors
		}
	}
	return pageviews, peakVisitors, float64(visitorDays) / float64(len(series))
}

func attributedPageviews(ctx context.Context, tx pgx.Tx, clientID, businessID uuid.UUID, fromD, toD string) (int64, error) {
	var attributed int64
	err := tx.QueryRow(ctx,
		`SELECT coalesce(sum(pageviews), 0)::bigint
		   FROM analytics_referrer_daily
		  WHERE client_id = $1 AND business_id = $2
		    AND bucket_date >= $3::date AND bucket_date <= $4::date`,
		clientID, businessID, fromD, toD).Scan(&attributed)
	return attributed, err
}

func percent(part, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

func percentChange(current, previous float64) *float64 {
	if previous == 0 {
		return nil
	}
	change := (current - previous) / previous * 100
	return &change
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

// ---------------------------------------------------------------------------
// Multi-site overview
// ---------------------------------------------------------------------------

// maxOverviewSites bounds the overview response. A tenant with a large business tree could
// otherwise pull every site it owns into one payload, and the sparkline series multiplies that by
// the window length. Sites are ordered by traffic, so the cap drops the quietest ones — the ones a
// scanning eye reaches last anyway.
const maxOverviewSites = 200

// OverviewSite is one card on the overview grid.
type OverviewSite struct {
	ClientID             string     `json:"client_id"`
	Name                 string     `json:"name"`
	BusinessID           string     `json:"business_id"`
	BusinessName         string     `json:"business_name"`
	Pageviews            int64      `json:"pageviews"`
	Visitors             int64      `json:"visitors"`
	AverageDailyVisitors float64    `json:"average_daily_visitors"`
	Series               []DayPoint `json:"series"`
}

// Overview lists every analytics site the caller can see, across every business they belong to,
// with totals and a per-day series for the sparkline.
//
// There is deliberately NO business id parameter. authorized_businesses() expands a principal's
// memberships down business_closure, so running unfiltered under the caller's principal returns
// exactly the businesses they may see and no others. Adding a handler-side business filter on top
// would not make this safer — it would just be a second predicate to drift out of step with the
// first. The RLS policy is the single source of truth.
func (s *Service) Overview(ctx context.Context, principalID uuid.UUID, from, to time.Time) ([]OverviewSite, error) {
	if to.Before(from) {
		return nil, fmt.Errorf("analytics: end before start: %w", errs.ErrValidation)
	}
	from = from.UTC().Truncate(24 * time.Hour)
	to = to.UTC().Truncate(24 * time.Hour)
	inclusiveDays := int(to.Sub(from)/(24*time.Hour)) + 1
	if inclusiveDays > maxRangeDays {
		return nil, fmt.Errorf("analytics: range of %d days exceeds the %d day cap: %w",
			inclusiveDays, maxRangeDays, errs.ErrValidation)
	}
	fromD, toD := from.Format("2006-01-02"), to.Format("2006-01-02")

	out := []OverviewSite{}
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		// LEFT JOIN, not INNER: a site registered a minute ago has no rollup rows yet, and omitting
		// it would read as "your tag is broken" at exactly the moment someone is checking whether
		// their tag works. It appears with zeroes instead.
		rows, err := tx.Query(ctx,
			`SELECT c.id::text, c.name, b.id::text, b.name,
			        coalesce(sum(d.pageviews), 0)::bigint,
			        -- Peak remains secondary context. The sum is divided by every requested day for
			        -- the headline average; it is never exposed as a cross-day "unique" total because
			        -- daily salt rotation makes that deduplication impossible by design.
			        coalesce(max(d.visitors),  0)::bigint,
			        coalesce(sum(d.visitors), 0)::double precision / $3::double precision
			   FROM telemetry_client c
			   JOIN business b ON b.id = c.business_id
			   LEFT JOIN analytics_daily d
			     ON d.client_id = c.id
			    AND d.bucket_date >= $1::date AND d.bucket_date <= $2::date
			  WHERE c.kind = 'analytics'
			    AND c.status = 'active' AND c.revoked_at IS NULL
			    -- PERMISSION, not just visibility. RLS already limits this to businesses the caller
			    -- is a member of, but membership is not telemetry.read: a member whose role lacks it
			    -- would otherwise see sites here that the per-site dashboard refuses to open. The
			    -- business-scoped routes get this from RequirePermission; this route has no business
			    -- in its path, so the same rule has to be expressed as a set.
			    AND c.business_id IN (
			        SELECT business_id FROM businesses_with_permission(current_principal(), 'telemetry.read')
			    )
			  GROUP BY c.id, c.name, b.id, b.name
			  -- Ordered by the SAME measure the card headlines and the API contract documents:
			  -- average daily visitors. This was ORDER BY 5, a positional reference to pageviews —
			  -- which silently ordered by a different metric than everything describing it said,
			  -- and at the 200-site cap would have dropped higher-average sites in favour of ones
			  -- with more pageviews. Spelled out rather than positional so it cannot drift again
			  -- if a column is inserted above it.
			  ORDER BY coalesce(sum(d.visitors), 0)::double precision / $3::double precision DESC,
			           coalesce(sum(d.pageviews), 0) DESC,
			           c.name
			  LIMIT $4`,
			fromD, toD, inclusiveDays, maxOverviewSites)
		if err != nil {
			return err
		}
		defer rows.Close()
		idx := map[string]int{}
		for rows.Next() {
			var o OverviewSite
			if err := rows.Scan(&o.ClientID, &o.Name, &o.BusinessID, &o.BusinessName,
				&o.Pageviews, &o.Visitors, &o.AverageDailyVisitors); err != nil {
				return err
			}
			o.Series = []DayPoint{}
			idx[o.ClientID] = len(out)
			out = append(out, o)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(out) == 0 {
			return nil
		}

		// Series for the sites we kept, in one query rather than one per site.
		ids := make([]string, 0, len(out))
		for _, o := range out {
			ids = append(ids, o.ClientID)
		}
		srows, err := tx.Query(ctx,
			`SELECT client_id::text, bucket_date::text, pageviews, visitors
			   FROM analytics_daily
			  WHERE client_id = ANY($1::uuid[])
			    AND bucket_date >= $2::date AND bucket_date <= $3::date
			  ORDER BY client_id, bucket_date`,
			ids, fromD, toD)
		if err != nil {
			return err
		}
		defer srows.Close()
		for srows.Next() {
			var cid string
			var d DayPoint
			if err := srows.Scan(&cid, &d.Date, &d.Pageviews, &d.Visitors); err != nil {
				return err
			}
			if i, ok := idx[cid]; ok {
				out[i].Series = append(out[i].Series, d)
			}
		}
		if err := srows.Err(); err != nil {
			return err
		}
		for i := range out {
			out[i].Series = completeDailySeries(out[i].Series, from, to)
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}
