//go:build integration

package analytics_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/analytics"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/observability"
	"github.com/manyforge/manyforge/internal/platform/timeseries"
	"github.com/manyforge/manyforge/internal/telemetry"
)

const humanUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

type env struct {
	tdb     *testdb.TestDB
	srv     *httptest.Server
	biz     uuid.UUID
	prin    uuid.UUID
	site    uuid.UUID
	key     string
	metrics *observability.Metrics
}

func newEnv(t *testing.T) (context.Context, *env) {
	return newEnvWithCloudflareCountryTrust(t, false)
}

func newEnvWithCloudflareCountryTrust(t *testing.T, trust bool) (context.Context, *env) {
	return newEnvWithCloudflareCountryTrustAndProxy(t, trust, true)
}

func newEnvWithCloudflareCountryTrustAndProxy(t *testing.T, trust, trustLoopbackProxy bool) (context.Context, *env) {
	t.Helper()
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start test db: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	if _, _, err := (&timeseries.MaintenanceWorker{DB: tdb.App}).SweepOnce(ctx); err != nil {
		t.Fatalf("create partitions: %v", err)
	}

	e := &env{tdb: tdb, metrics: observability.NewMetrics()}
	e.seedTenant(t, ctx)
	e.seedSite(t, ctx)

	// Trust loopback so X-Forwarded-For is honoured, matching production where the app sits behind
	// an ingress. Without this the handler correctly ignores XFF and every test request would look
	// like the same visitor.
	_, loopback4, _ := net.ParseCIDR("127.0.0.0/8")
	_, loopback6, _ := net.ParseCIDR("::1/128")
	_, cloudflareTestRange, _ := net.ParseCIDR("203.0.113.0/24")
	var trustedProxies []*net.IPNet
	if trustLoopbackProxy {
		trustedProxies = []*net.IPNet{loopback4, loopback6}
	}
	pub := &analytics.PublicHandler{
		DB:                           tdb.App,
		Logger:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:                      e.metrics,
		TrustedProxies:               trustedProxies,
		CloudflareSourceRanges:       []*net.IPNet{cloudflareTestRange},
		TrustCloudflareCountryHeader: trust,
	}
	rd := analytics.NewHandler(analytics.NewService(tdb.App))

	r := chi.NewRouter()
	pub.SnippetRoutes(r)
	pub.CollectRoutes(r)
	// Inject the principal the way the real auth middleware would, so the read API runs under RLS.
	r.Group(func(pr chi.Router) {
		pr.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(httpx.WithPrincipal(req.Context(), e.prin)))
			})
		})
		rd.ReadRoutes(pr)
		rd.WriteRoutes(pr)
		rd.OverviewRoutes(pr)
	})
	e.srv = httptest.NewServer(r)
	t.Cleanup(e.srv.Close)
	return ctx, e
}

func (e *env) seedTenant(t *testing.T, ctx context.Context) {
	t.Helper()
	var ownerRole uuid.UUID
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("owner role: %v", err)
	}
	e.biz, e.prin = uuid.New(), uuid.New()
	acct := uuid.New()
	for _, st := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'O','active',now(),now(),now())`,
			[]any{acct, "an-" + e.biz.String() + "@x.test"}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`, []any{e.prin, acct}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'AnCo','active',now(),now())`,
			[]any{e.biz}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`, []any{e.biz}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{e.prin, e.biz, ownerRole}},
	} {
		if _, err := e.tdb.Super.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, st.sql)
		}
	}
}

func (e *env) seedSite(t *testing.T, ctx context.Context) {
	t.Helper()
	e.site = uuid.New()
	e.key = "mfk_" + strings.Repeat("A", 32)
	if _, err := e.tdb.Super.Exec(ctx,
		`INSERT INTO telemetry_client (id,business_id,tenant_root_id,kind,name,publishable_key,require_signature,status)
		 VALUES ($1,$2,$2,'analytics','site',$3,false,'active')`,
		e.site, e.biz, e.key); err != nil {
		t.Fatalf("seed site: %v", err)
	}
}

func (e *env) collect(t *testing.T, key, path, ref, ua, ip string) int {
	return e.collectWithOrigins(t, key, path, ref, ua, ip, nil)
}

func (e *env) collectWithOrigins(
	t *testing.T, key, path, ref, ua, ip string, origins []string,
) int {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"k": key, "p": path, "r": ref})
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/a/e", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if ip != "" {
		req.Header.Set("X-Forwarded-For", ip)
	}
	for _, origin := range origins {
		req.Header.Add("Origin", origin)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func (e *env) rollup(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := (&timeseries.RollupWorker{DB: e.tdb.App, Lag: -1}).SweepOnce(ctx); err != nil {
		t.Fatalf("rollup: %v", err)
	}
}

func (e *env) summary(t *testing.T, clientID uuid.UUID) (int, analytics.Summary) {
	t.Helper()
	resp, err := e.srv.Client().Get(e.srv.URL + "/businesses/" + e.biz.String() +
		"/analytics/summary?client_id=" + clientID.String() + "&days=7")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	defer resp.Body.Close()
	var s analytics.Summary
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &s); err != nil {
			t.Fatalf("decode summary: %v (%s)", err, body)
		}
	}
	return resp.StatusCode, s
}

// ---------------------------------------------------------------------------

func TestSnippet_IsServedAndSelfContained(t *testing.T) {
	_, e := newEnv(t)
	resp, err := e.srv.Client().Get(e.srv.URL + "/a.js")
	if err != nil {
		t.Fatalf("get snippet: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("content-type %q", ct)
	}
	if !strings.Contains(string(body), "data-key") {
		t.Fatal("snippet does not read data-key")
	}
	// It must not pull in anything else — a third-party site is trusting this file.
	if strings.Contains(string(body), "import ") || strings.Contains(string(body), "require(") {
		t.Fatal("snippet has an external dependency")
	}
}

func TestCollect_StoresPageview(t *testing.T) {
	ctx, e := newEnv(t)
	if code := e.collect(t, e.key, "/pricing?utm_source=x", "https://news.ycombinator.com/item?id=1", humanUA, "203.0.113.9"); code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", code)
	}
	var path, ref string
	var isBot bool
	var hash []byte
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT path, coalesce(referrer_host,''), is_bot, visitor_hash
		   FROM analytics_event WHERE client_id=$1`, e.site).Scan(&path, &ref, &isBot, &hash); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if path != "/pricing" {
		t.Errorf("query string not stripped: path=%q", path)
	}
	if ref != "news.ycombinator.com" {
		t.Errorf("referrer not reduced to host: %q", ref)
	}
	if isBot {
		t.Error("a real browser UA was flagged as a bot")
	}
	if len(hash) != 16 {
		t.Errorf("visitor hash is %d bytes, want 16", len(hash))
	}
}

// The privacy claim, enforced rather than documented: no column anywhere holds the raw IP or UA.
func TestCollect_NeverStoresRawIPOrUserAgent(t *testing.T) {
	ctx, e := newEnv(t)
	const ip = "198.51.100.77"
	e.collect(t, e.key, "/", "", humanUA, ip)

	// Scan every text/jsonb column of the event row for the raw values.
	rows, err := e.tdb.Super.Query(ctx,
		`SELECT row_to_json(t)::text FROM (SELECT * FROM analytics_event WHERE client_id=$1) t`, e.site)
	if err != nil {
		t.Fatalf("dump row: %v", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var js string
		if err := rows.Scan(&js); err != nil {
			t.Fatal(err)
		}
		found = true
		if strings.Contains(js, ip) {
			t.Errorf("PRIVACY VIOLATION: raw IP %q is stored in the event row: %s", ip, js)
		}
		if strings.Contains(js, "Chrome/120") {
			t.Errorf("PRIVACY VIOLATION: raw User-Agent is stored in the event row: %s", js)
		}
	}
	if !found {
		t.Fatal("no event row was written")
	}
}

// Same visitor, same day ⇒ one visitor, many pageviews. Different IP ⇒ a second visitor.
func TestRollup_VisitorsVsPageviews(t *testing.T) {
	ctx, e := newEnv(t)
	e.collect(t, e.key, "/", "", humanUA, "203.0.113.1")
	e.collect(t, e.key, "/about", "", humanUA, "203.0.113.1")
	e.collect(t, e.key, "/", "", humanUA, "203.0.113.2")
	e.rollup(t, ctx)

	code, s := e.summary(t, e.site)
	if code != http.StatusOK {
		t.Fatalf("summary status %d", code)
	}
	if s.Pageviews != 3 {
		t.Errorf("pageviews = %d, want 3", s.Pageviews)
	}
	if s.Visitors != 2 {
		t.Errorf("visitors = %d, want 2 (same IP+UA twice is one visitor)", s.Visitors)
	}
	if len(s.TopPages) != 2 {
		t.Fatalf("top pages = %d, want 2", len(s.TopPages))
	}
	if s.TopPages[0].Path != "/" || s.TopPages[0].Pageviews != 2 {
		t.Errorf("top page = %+v, want / with 2", s.TopPages[0])
	}
}

func TestSummary_FillsDailyGapsAndComparesEquivalentPriorWindow(t *testing.T) {
	ctx, e := newEnv(t)
	for _, row := range []struct {
		daysAgo, pv, vis int
	}{{0, 10, 4}, {2, 20, 2}, {7, 5, 1}, {9, 5, 3}} {
		if _, err := e.tdb.Super.Exec(ctx,
			`INSERT INTO analytics_daily
			   (tenant_root_id, business_id, client_id, bucket_date, pageviews, visitors, updated_at)
			 VALUES ($1,$2,$3, (now() AT TIME ZONE 'UTC')::date - $4::int, $5, $6, now())`,
			e.biz, e.biz, e.site, row.daysAgo, row.pv, row.vis); err != nil {
			t.Fatalf("seed daily: %v", err)
		}
	}
	for _, row := range []struct {
		daysAgo, pv, vis int
	}{{0, 6, 2}, {7, 2, 1}} {
		if _, err := e.tdb.Super.Exec(ctx,
			`INSERT INTO analytics_referrer_daily
			   (tenant_root_id, business_id, client_id, bucket_date, referrer_host, pageviews, visitors, updated_at)
			 VALUES ($1,$2,$3, (now() AT TIME ZONE 'UTC')::date - $4::int, 'example.test', $5, $6, now())`,
			e.biz, e.biz, e.site, row.daysAgo, row.pv, row.vis); err != nil {
			t.Fatalf("seed referrer daily: %v", err)
		}
	}

	code, got := e.summary(t, e.site)
	if code != http.StatusOK {
		t.Fatalf("summary status = %d, want 200", code)
	}
	if len(got.Series) != 7 {
		t.Fatalf("len(series) = %d, want 7", len(got.Series))
	}
	wantFrom := timeNow().AddDate(0, 0, -6).Format("2006-01-02")
	wantTo := timeNow().Format("2006-01-02")
	if got.Series[0].Date != wantFrom || got.Series[6].Date != wantTo {
		t.Errorf("series boundaries = %s..%s, want %s..%s",
			got.Series[0].Date, got.Series[6].Date, wantFrom, wantTo)
	}
	if got.Series[4].Pageviews != 20 || got.Series[5].Pageviews != 0 || got.Series[6].Pageviews != 10 {
		t.Errorf("gap-accurate current series = %+v", got.Series)
	}
	if got.Pageviews != 30 || got.Visitors != 4 || math.Abs(got.AverageDailyVisitors-(6.0/7.0)) > 0.0001 {
		t.Errorf("current metrics = pv %d peak %d avg %f", got.Pageviews, got.Visitors, got.AverageDailyVisitors)
	}
	if got.DirectPageviews != 24 || math.Abs(got.DirectShare-80) > 0.0001 {
		t.Errorf("current direct = %d / %f%%, want 24 / 80%%", got.DirectPageviews, got.DirectShare)
	}
	comparison := got.Comparison
	if comparison.From != timeNow().AddDate(0, 0, -13).Format("2006-01-02") ||
		comparison.To != timeNow().AddDate(0, 0, -7).Format("2006-01-02") {
		t.Errorf("comparison boundaries = %s..%s", comparison.From, comparison.To)
	}
	if comparison.Pageviews != 10 || math.Abs(comparison.AverageDailyVisitors-(4.0/7.0)) > 0.0001 {
		t.Errorf("comparison metrics = %+v", comparison)
	}
	if comparison.PageviewsChangePercent == nil || math.Abs(*comparison.PageviewsChangePercent-200) > 0.0001 {
		t.Errorf("pageviews change = %v, want 200%%", comparison.PageviewsChangePercent)
	}
	if comparison.AverageDailyVisitorsChangePercent == nil ||
		math.Abs(*comparison.AverageDailyVisitorsChangePercent-50) > 0.0001 {
		t.Errorf("visitor change = %v, want 50%%", comparison.AverageDailyVisitorsChangePercent)
	}
	if math.Abs(comparison.DirectShareChangePercentagePoints) > 0.0001 {
		t.Errorf("direct share change = %f points, want 0", comparison.DirectShareChangePercentagePoints)
	}
}

func TestSummaryAndOverview_UseCommonCompletedDashboardWatermark(t *testing.T) {
	ctx, e := newEnv(t)
	newer := timeNow().Add(-2 * time.Minute)
	middle := timeNow().Add(-7 * time.Minute)
	older := timeNow().Add(-11 * time.Minute)
	if _, err := e.tdb.Super.Exec(ctx,
		`UPDATE rollup_state
		    SET watermark_ingested_at = CASE rollup_name
		          WHEN 'analytics_pageviews' THEN $1::timestamptz
		          WHEN 'analytics_dimensions' THEN $2::timestamptz
		          WHEN 'analytics_properties' THEN $3::timestamptz
		        END,
		        updated_at = now()
		  WHERE rollup_name = ANY($4::text[])`,
		newer, middle, older,
		[]string{"analytics_pageviews", "analytics_dimensions", "analytics_properties"}); err != nil {
		t.Fatalf("seed watermarks: %v", err)
	}

	code, summary := e.summary(t, e.site)
	if code != http.StatusOK || summary.DataAsOf == nil {
		t.Fatalf("summary freshness = code %d, data_as_of %v", code, summary.DataAsOf)
	}
	if got, err := time.Parse(time.RFC3339Nano, *summary.DataAsOf); err != nil || !got.Equal(older) {
		t.Fatalf("summary data_as_of = %q, want common minimum %s (parse err %v)",
			*summary.DataAsOf, older.Format(time.RFC3339Nano), err)
	}

	overview := e.getOverview(t, e.prin)
	if overview.DataAsOf == nil || *overview.DataAsOf != *summary.DataAsOf {
		t.Fatalf("overview data_as_of = %v, summary = %v", overview.DataAsOf, summary.DataAsOf)
	}

	// A never-completed component makes freshness unknown; reporting the other watermark would
	// imply every dashboard panel is current through a point that one pipeline has not reached.
	if _, err := e.tdb.Super.Exec(ctx,
		`UPDATE rollup_state SET watermark_ingested_at = '-infinity'
		  WHERE rollup_name = 'analytics_properties'`); err != nil {
		t.Fatalf("rewind property watermark: %v", err)
	}
	_, unavailable := e.summary(t, e.site)
	if unavailable.DataAsOf != nil {
		t.Errorf("data_as_of = %q with an incomplete rollup, want null", *unavailable.DataAsOf)
	}
	unavailableOverview := e.getOverview(t, e.prin)
	if unavailableOverview.DataAsOf != nil {
		t.Errorf("overview data_as_of = %q with an incomplete rollup, want null",
			*unavailableOverview.DataAsOf)
	}
}

func TestSiteHealth_NeverSeenStaleRecoveredAndRevoked(t *testing.T) {
	ctx, e := newEnv(t)
	svc := telemetry.NewService(e.tdb.App, nil)
	var appCanReadActivity bool
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT has_table_privilege('manyforge_app', 'analytics_site_activity', 'SELECT')`,
	).Scan(&appCanReadActivity); err != nil {
		t.Fatalf("inspect activity-table grant: %v", err)
	}
	if appCanReadActivity {
		t.Fatal("analytics_site_activity is directly readable; tenant-safe function must be the only read path")
	}

	// A completed health sweep with no activity distinguishes "never seen" from a health pipeline
	// that has not checked yet. The dashboard rollups advance in the same worker sweep.
	e.rollup(t, ctx)
	list, err := svc.ListClients(ctx, e.prin, e.biz, 50)
	if err != nil || len(list) != 1 || list[0].AnalyticsHealth == nil {
		t.Fatalf("initial site health = list %d, err %v, value %+v", len(list), err, list)
	}
	if got := list[0].AnalyticsHealth; got.Status != telemetry.SiteHealthNeverSeen ||
		got.ReceivingData || got.ActivityDataAsOf == nil || got.DataAsOf == nil {
		t.Fatalf("never-seen health = %+v", got)
	}

	// A stale activity watermark means the health pipeline itself is delayed, so ListClients must
	// not turn the absence of recent activity into an installation warning. This exercises the
	// full SECURITY DEFINER read and service orchestration rather than only the state helper.
	if _, err := e.tdb.Super.Exec(ctx,
		`UPDATE rollup_state SET watermark_ingested_at = now() - interval '16 minutes'
		  WHERE rollup_name = 'analytics_site_health'`); err != nil {
		t.Fatalf("delay health watermark: %v", err)
	}
	list, err = svc.ListClients(ctx, e.prin, e.biz, 50)
	if err != nil || list[0].AnalyticsHealth.Status != telemetry.SiteHealthChecking {
		t.Fatalf("delayed-rollup health = %+v, err %v", list[0].AnalyticsHealth, err)
	}
	// Let the worker catch the health watermark back up before testing per-site states.
	e.rollup(t, ctx)
	list, err = svc.ListClients(ctx, e.prin, e.biz, 50)
	if err != nil || list[0].AnalyticsHealth.Status != telemetry.SiteHealthNeverSeen {
		t.Fatalf("health after delayed rollup recovered = %+v, err %v",
			list[0].AnalyticsHealth, err)
	}

	// Dashboard freshness is only meaningful when every component rollup exists. A missing state
	// row must degrade to an unknown watermark rather than reporting the surviving pipeline as if
	// every dashboard panel were current.
	if _, err := e.tdb.Super.Exec(ctx,
		`DELETE FROM rollup_state WHERE rollup_name = 'analytics_dimensions'`); err != nil {
		t.Fatalf("remove dimension watermark: %v", err)
	}
	list, err = svc.ListClients(ctx, e.prin, e.biz, 50)
	if err != nil || list[0].AnalyticsHealth.DataAsOf != nil {
		t.Fatalf("health with missing dashboard watermark = %+v, err %v",
			list[0].AnalyticsHealth, err)
	}
	if _, err := e.tdb.Super.Exec(ctx,
		`INSERT INTO rollup_state (rollup_name, watermark_ingested_at)
		 VALUES ('analytics_dimensions', '-infinity')`); err != nil {
		t.Fatalf("restore dimension watermark state: %v", err)
	}
	e.rollup(t, ctx)

	// Simulate a migration-backfilled row recording that historical activity existed without
	// inventing an exact ingest time. It is stale, not never installed, and the next accepted event
	// must replace that NULL with an exact timestamp rather than leaving the site permanently stale.
	if _, err := e.tdb.Super.Exec(ctx,
		`INSERT INTO analytics_site_activity (client_id, last_accepted_at)
		 VALUES ($1, NULL)`, e.site); err != nil {
		t.Fatalf("seed stale activity: %v", err)
	}
	list, err = svc.ListClients(ctx, e.prin, e.biz, 50)
	if err != nil || list[0].AnalyticsHealth.Status != telemetry.SiteHealthStale {
		t.Fatalf("stale health = %+v, err %v", list[0].AnalyticsHealth, err)
	}

	// Exercise the actual public browser collector. It remains a uniform 204, then the async
	// health rollup observes the accepted row and the authenticated site state recovers.
	if code := e.collect(t, e.key, "/verify-installation", "", humanUA, "203.0.113.9"); code != http.StatusNoContent {
		t.Fatalf("public collector status = %d, want 204", code)
	}
	e.rollup(t, ctx)
	list, err = svc.ListClients(ctx, e.prin, e.biz, 50)
	if err != nil {
		t.Fatalf("list recovered health: %v", err)
	}
	recovered := list[0].AnalyticsHealth
	if recovered.Status != telemetry.SiteHealthHealthy || !recovered.ReceivingData ||
		recovered.LastAcceptedAt == nil {
		t.Fatalf("recovered health = %+v", recovered)
	}

	// Pin the time-based healthy-to-stale transition separately from the NULL historical-backfill
	// state above, then revoke while stale to prove the client status always wins.
	if _, err := e.tdb.Super.Exec(ctx,
		`UPDATE analytics_site_activity
		    SET last_accepted_at = now() - interval '25 hours'
		  WHERE client_id = $1`, e.site); err != nil {
		t.Fatalf("age accepted activity: %v", err)
	}
	list, err = svc.ListClients(ctx, e.prin, e.biz, 50)
	if err != nil || list[0].AnalyticsHealth.Status != telemetry.SiteHealthStale {
		t.Fatalf("healthy-to-stale health = %+v, err %v", list[0].AnalyticsHealth, err)
	}

	if _, err := svc.RevokeClient(ctx, e.prin, e.biz, e.site); err != nil {
		t.Fatalf("revoke site: %v", err)
	}
	list, err = svc.ListClients(ctx, e.prin, e.biz, 50)
	if err != nil || list[0].AnalyticsHealth.Status != telemetry.SiteHealthRevoked ||
		list[0].AnalyticsHealth.ReceivingData {
		t.Fatalf("revoked health = %+v, err %v", list[0].AnalyticsHealth, err)
	}
}

func TestSiteHealth_RevokedBeforeFirstEvent(t *testing.T) {
	ctx, e := newEnv(t)
	svc := telemetry.NewService(e.tdb.App, nil)

	// Complete one sweep so this would otherwise be a genuine never-seen site, then revoke it
	// before any collector request. Revocation must win even without an activity row.
	e.rollup(t, ctx)
	if _, err := svc.RevokeClient(ctx, e.prin, e.biz, e.site); err != nil {
		t.Fatalf("revoke never-seen site: %v", err)
	}
	list, err := svc.ListClients(ctx, e.prin, e.biz, 50)
	if err != nil || len(list) != 1 || list[0].AnalyticsHealth == nil {
		t.Fatalf("list revoked never-seen site = %+v, err %v", list, err)
	}
	if got := list[0].AnalyticsHealth; got.Status != telemetry.SiteHealthRevoked ||
		got.ReceivingData || got.LastAcceptedAt != nil {
		t.Fatalf("revoked never-seen health = %+v", got)
	}
}

func TestRollup_ExcludesBots(t *testing.T) {
	ctx, e := newEnv(t)
	e.collect(t, e.key, "/", "", humanUA, "203.0.113.1")
	e.collect(t, e.key, "/", "", "Googlebot/2.1", "203.0.113.50")
	e.collect(t, e.key, "/", "", "curl/8.4.0", "203.0.113.51")
	e.rollup(t, ctx)

	_, s := e.summary(t, e.site)
	if s.Pageviews != 1 {
		t.Errorf("pageviews = %d, want 1 — bots must be excluded from aggregates", s.Pageviews)
	}
	// But the bot hits are still stored, so the filter can be audited.
	var stored int
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM analytics_event WHERE client_id=$1 AND is_bot", e.site).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 2 {
		t.Errorf("bot rows stored = %d, want 2 (kept for auditability)", stored)
	}
}

func TestRollup_IsIdempotent(t *testing.T) {
	ctx, e := newEnv(t)
	for i := 0; i < 5; i++ {
		e.collect(t, e.key, "/", "", humanUA, "203.0.113.1")
	}
	e.rollup(t, ctx)
	_, first := e.summary(t, e.site)

	if _, err := e.tdb.Super.Exec(ctx,
		`UPDATE rollup_state SET watermark_ingested_at='-infinity' WHERE rollup_name='analytics_pageviews'`); err != nil {
		t.Fatal(err)
	}
	e.rollup(t, ctx)
	_, second := e.summary(t, e.site)

	if first.Pageviews != second.Pageviews || first.Visitors != second.Visitors {
		t.Fatalf("pageview rollup is not idempotent: %d/%d then %d/%d",
			first.Pageviews, first.Visitors, second.Pageviews, second.Visitors)
	}
	if first.Pageviews != 5 {
		t.Fatalf("expected 5 pageviews, got %d", first.Pageviews)
	}
}

func TestRollup_ReferrersAndDirect(t *testing.T) {
	ctx, e := newEnv(t)
	e.collect(t, e.key, "/", "news.ycombinator.com", humanUA, "203.0.113.1")
	e.collect(t, e.key, "/", "news.ycombinator.com", humanUA, "203.0.113.2")
	e.collect(t, e.key, "/", "", humanUA, "203.0.113.3") // direct
	e.rollup(t, ctx)

	_, s := e.summary(t, e.site)
	if len(s.TopReferrers) != 1 || s.TopReferrers[0].Host != "news.ycombinator.com" {
		t.Fatalf("top referrers = %+v", s.TopReferrers)
	}
	if s.TopReferrers[0].Pageviews != 2 {
		t.Errorf("referrer pageviews = %d, want 2", s.TopReferrers[0].Pageviews)
	}
	if s.DirectPageviews != 1 {
		t.Errorf("direct = %d, want 1 (total minus attributed referrers)", s.DirectPageviews)
	}
}

// Unknown, revoked, and malformed keys are all 204 and persist nothing — the endpoint is public,
// so a varying status would confirm which keys exist.
func TestCollect_NoOracleAndNoWriteForBadKeys(t *testing.T) {
	ctx, e := newEnv(t)
	revoked := uuid.New()
	revKey := "mfk_" + strings.Repeat("B", 32)
	if _, err := e.tdb.Super.Exec(ctx,
		`INSERT INTO telemetry_client (id,business_id,tenant_root_id,kind,name,publishable_key,require_signature,status,revoked_at)
		 VALUES ($1,$2,$2,'analytics','revoked',$3,false,'revoked',now())`,
		revoked, e.biz, revKey); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{
		"mfk_" + strings.Repeat("Z", 32), // unknown
		revKey,                           // revoked
		"garbage",                        // malformed
		"",                               // empty
	} {
		if code := e.collect(t, k, "/", "", humanUA, "203.0.113.1"); code != http.StatusNoContent {
			t.Errorf("key %q: got %d, want 204 for every case", k, code)
		}
	}
	var n int
	if err := e.tdb.Super.QueryRow(ctx, "SELECT count(*) FROM analytics_event").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("bad keys persisted %d events", n)
	}
}

func TestCollect_LegacyUnrestrictedAcceptsMissingOrAnyOrigin(t *testing.T) {
	ctx, e := newEnv(t)
	for _, origins := range [][]string{nil, {"https://unconfigured.example"}} {
		if code := e.collectWithOrigins(
			t, e.key, "/legacy-origin", "", humanUA, "203.0.113.8", origins,
		); code != http.StatusNoContent {
			t.Fatalf("legacy-unrestricted collect status = %d, want 204", code)
		}
	}
	var rows int
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM analytics_event WHERE client_id=$1", e.site,
	).Scan(&rows); err != nil {
		t.Fatalf("count legacy-unrestricted events: %v", err)
	}
	if rows != 2 {
		t.Fatalf("legacy-unrestricted stored rows = %d, want 2", rows)
	}
}

func TestCollect_AllowedOriginsAreUniformAndObservable(t *testing.T) {
	ctx, e := newEnv(t)
	if _, err := e.tdb.Super.Exec(ctx,
		`UPDATE telemetry_client
		    SET allowed_origins = ARRAY['https://garden.example']
		  WHERE id = $1`, e.site); err != nil {
		t.Fatalf("configure allowed origin: %v", err)
	}

	for _, tc := range []struct {
		name               string
		key                string
		origins            []string
		wantStored         int
		wantOriginRejected int64
	}{
		{"allowed canonicalizes", e.key, []string{"HTTPS://GARDEN.EXAMPLE:443/"}, 1, 0},
		{"mismatch", e.key, []string{"https://other.example"}, 0, 1},
		{"missing", e.key, nil, 0, 1},
		{"duplicate header", e.key, []string{"https://garden.example", "https://other.example"}, 0, 1},
		{"malformed header", e.key, []string{"null"}, 0, 1},
		// An unknown key with a valid Origin remains a generic rejection. The origin counter must
		// not claim a mismatch before the key resolves.
		{"unknown key", "mfk_" + strings.Repeat("Z", 32), []string{"https://other.example"}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rowsBefore int
			if err := e.tdb.Super.QueryRow(ctx,
				"SELECT count(*) FROM analytics_event WHERE client_id=$1", e.site,
			).Scan(&rowsBefore); err != nil {
				t.Fatalf("count events before collect: %v", err)
			}
			rejectedBefore := e.metrics.Get(observability.MetricAnalyticsOriginRejected)
			if code := e.collectWithOrigins(
				t, tc.key, "/origin-check", "", humanUA, "203.0.113.9", tc.origins,
			); code != http.StatusNoContent {
				t.Fatalf("status = %d, want uniform 204", code)
			}
			var rows int
			if err := e.tdb.Super.QueryRow(ctx,
				"SELECT count(*) FROM analytics_event WHERE client_id=$1", e.site,
			).Scan(&rows); err != nil {
				t.Fatalf("count events: %v", err)
			}
			if got := rows - rowsBefore; got != tc.wantStored {
				t.Fatalf("stored row delta = %d, want %d", got, tc.wantStored)
			}
			if got := e.metrics.Get(observability.MetricAnalyticsOriginRejected) - rejectedBefore; got != tc.wantOriginRejected {
				t.Fatalf("origin rejection delta = %d, want %d", got, tc.wantOriginRejected)
			}
		})
	}

	if _, err := e.tdb.Super.Exec(ctx,
		`UPDATE telemetry_client SET status='revoked', revoked_at=now() WHERE id=$1`, e.site,
	); err != nil {
		t.Fatalf("revoke configured site: %v", err)
	}
	rejectedBefore := e.metrics.Get(observability.MetricAnalyticsOriginRejected)
	if code := e.collectWithOrigins(
		t, e.key, "/revoked-origin", "", humanUA, "203.0.113.9",
		[]string{"https://other.example"},
	); code != http.StatusNoContent {
		t.Fatalf("revoked configured site status = %d, want uniform 204", code)
	}
	if got := e.metrics.Get(observability.MetricAnalyticsOriginRejected) - rejectedBefore; got != 0 {
		t.Fatalf("revoked key changed origin rejection delta by %d; revocation must resolve first", got)
	}
}

// A crash-kind key must not be usable to write pageviews.
func TestCollect_RejectsNonAnalyticsKind(t *testing.T) {
	ctx, e := newEnv(t)
	crashKey := "mfk_" + strings.Repeat("C", 32)
	if _, err := e.tdb.Super.Exec(ctx,
		`INSERT INTO telemetry_client (id,business_id,tenant_root_id,kind,name,publishable_key,require_signature,status)
		 VALUES ($1,$2,$2,'crash','app',$3,false,'active')`,
		uuid.New(), e.biz, crashKey); err != nil {
		t.Fatal(err)
	}
	e.collect(t, crashKey, "/", "", humanUA, "203.0.113.1")

	var n int
	if err := e.tdb.Super.QueryRow(ctx, "SELECT count(*) FROM analytics_event").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a crash key wrote %d analytics events", n)
	}
}

// The read API must refuse another business's site.
func TestSummary_RefusesForeignSite(t *testing.T) {
	ctx, e := newEnv(t)
	other := uuid.New()
	if _, err := e.tdb.Super.Exec(ctx,
		`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'Other','active',now(),now())`,
		other); err != nil {
		t.Fatal(err)
	}
	foreignSite := uuid.New()
	if _, err := e.tdb.Super.Exec(ctx,
		`INSERT INTO telemetry_client (id,business_id,tenant_root_id,kind,name,publishable_key,require_signature,status)
		 VALUES ($1,$2,$2,'analytics','theirs',$3,false,'active')`,
		foreignSite, other, "mfk_"+strings.Repeat("D", 32)); err != nil {
		t.Fatal(err)
	}

	code, _ := e.summary(t, foreignSite)
	if code != http.StatusNotFound {
		t.Fatalf("foreign site: got %d, want 404", code)
	}
	// An unknown id gets the identical answer — no site-existence oracle.
	unknownCode, _ := e.summary(t, uuid.New())
	if unknownCode != code {
		t.Fatalf("SITE-EXISTENCE ORACLE: foreign=%d unknown=%d", code, unknownCode)
	}
}

// The visitor hash must differ across days even for the same visitor, or the salt is not rotating
// and cross-day tracking would be possible.
func TestVisitorHash_RotatesDaily(t *testing.T) {
	ctx, e := newEnv(t)
	e.collect(t, e.key, "/", "", humanUA, "203.0.113.1")

	var todayHash []byte
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT visitor_hash FROM analytics_event WHERE client_id=$1", e.site).Scan(&todayHash); err != nil {
		t.Fatal(err)
	}

	// Simulate tomorrow by installing a different salt for the next day and hashing the same
	// inputs through the same expression the collect function uses.
	var tomorrowHash []byte
	if err := e.tdb.Super.QueryRow(ctx,
		`WITH s AS (
		   INSERT INTO analytics_salt (day, salt)
		   VALUES (((now() AT TIME ZONE 'UTC')::date + 1), gen_random_bytes(32))
		   RETURNING salt)
		 SELECT substring(sha256(s.salt || convert_to($1::text || $2 || $3, 'UTF8')) from 1 for 16) FROM s`,
		e.site, "203.0.113.1", humanUA).Scan(&tomorrowHash); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(todayHash, tomorrowHash) {
		t.Fatal("visitor hash did not change with the daily salt — cross-day tracking would be possible")
	}
}

// Salts past retention are purged, which is what makes an aged-out hash un-derivable.
func TestSaltPurge_DropsExpiredSalts(t *testing.T) {
	ctx, e := newEnv(t)
	if _, err := e.tdb.Super.Exec(ctx,
		`INSERT INTO analytics_salt (day, salt) VALUES (((now() AT TIME ZONE 'UTC')::date - 200), gen_random_bytes(32))`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&timeseries.MaintenanceWorker{DB: e.tdb.App}).SweepOnce(ctx); err != nil {
		t.Fatalf("maintenance: %v", err)
	}
	var n int
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM analytics_salt WHERE day < ((now() AT TIME ZONE 'UTC')::date - 100)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d expired salts survived; aged-out visitor hashes remain re-derivable", n)
	}
}

func TestSalt_NotReadableByAppRole(t *testing.T) {
	ctx, e := newEnv(t)
	e.collect(t, e.key, "/", "", humanUA, "203.0.113.1") // ensure a salt exists

	err := e.tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, "SELECT count(*) FROM analytics_salt").Scan(&n)
	})
	if err == nil {
		t.Fatal("SECURITY REGRESSION: manyforge_app can read analytics_salt; a read-only SQL " +
			"injection anywhere in the app would be enough to start re-deriving visitor hashes")
	}
}

// ---------------------------------------------------------------------------
// Cross-origin embedding
//
// These pin the single most important property of the embed: it is loaded on OTHER PEOPLE'S
// origins. Every other test here is same-origin and cannot see a CORS failure — the original
// version of this endpoint shipped with no CORS headers and a JSON beacon, which meant a real
// embedding site collected exactly nothing while every Go test passed.
// ---------------------------------------------------------------------------

func TestCollect_AnswersCORSPreflight(t *testing.T) {
	_, e := newEnv(t)
	req, _ := http.NewRequest(http.MethodOptions, e.srv.URL+"/a/e", nil)
	req.Header.Set("Origin", "https://a-tenant-site.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q; without it, no embedding site can collect", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Errorf("Access-Control-Allow-Methods = %q, must allow POST", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(strings.ToLower(got), "content-type") {
		t.Errorf("Access-Control-Allow-Headers = %q, must allow Content-Type for the XHR fallback", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q; collector must never accept credentials", got)
	}
}

func TestCollect_SetsCORSOnThePostItself(t *testing.T) {
	ctx, e := newEnv(t)
	b, _ := json.Marshal(map[string]string{"k": e.key, "p": "/", "r": ""})
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/a/e", bytes.NewReader(b))
	req.Header.Set("Origin", "https://a-tenant-site.example")
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("User-Agent", humanUA)
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("POST response missing Access-Control-Allow-Origin (got %q)", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("POST unexpectedly allows credentials: %q", got)
	}

	// text/plain is what the beacon actually sends, precisely BECAUSE it avoids a preflight. The
	// server must therefore parse the body by shape, never by Content-Type.
	var n int
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM analytics_event WHERE client_id=$1", e.site).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("a text/plain beacon stored %d rows, want 1 — the handler must not require "+
			"Content-Type: application/json", n)
	}
}

// The snippet must use a CORS-"simple" content type. application/json forces a preflight on every
// pageview from every embedding site.
func TestSnippet_BeaconUsesSimpleContentType(t *testing.T) {
	_, e := newEnv(t)
	resp, err := e.srv.Client().Get(e.srv.URL + "/a.js")
	if err != nil {
		t.Fatalf("get snippet: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	js := string(body)

	if !strings.Contains(js, "text/plain") {
		t.Error("the beacon must post text/plain; application/json triggers a CORS preflight on " +
			"every pageview from every embedding site")
	}
	if strings.Contains(js, "sendBeacon(ep,new Blob([b],{type:'application/json'})") {
		t.Error("the beacon still sends application/json")
	}
}

// Direct traffic must be derived from ALL attributed referrers, not from the capped top-N list.
// With more referrers than topN, summing the returned rows misclassifies the entire tail as
// direct — and the error grows precisely for sites with the most diverse traffic.
func TestSummary_DirectExcludesReferrersBeyondTopN(t *testing.T) {
	ctx, e := newEnv(t)
	// 25 distinct referrer hosts (> the topN of 20), one pageview each, plus 2 genuinely direct.
	for i := 0; i < 25; i++ {
		e.collect(t, e.key, "/", fmt.Sprintf("ref%02d.example", i), humanUA, "203.0.113.1")
		// Vary the path so the repeated-path suppression in the snippet is irrelevant here; the
		// endpoint itself has no suppression, but keep hits distinct for clarity.
	}
	e.collect(t, e.key, "/", "", humanUA, "203.0.113.2")
	e.collect(t, e.key, "/", "", humanUA, "203.0.113.3")
	e.rollup(t, ctx)

	_, s := e.summary(t, e.site)
	if s.Pageviews != 27 {
		t.Fatalf("pageviews = %d, want 27", s.Pageviews)
	}
	if len(s.TopReferrers) != 20 {
		t.Fatalf("top referrers = %d, want the topN cap of 20", len(s.TopReferrers))
	}
	if s.DirectPageviews != 2 {
		t.Fatalf("direct = %d, want 2. Summing the capped TopReferrers list would report 7 here, "+
			"silently reclassifying the 5 referrers beyond topN as direct", s.DirectPageviews)
	}
}

// The window is inclusive, so days=N covers N days. The cap must be applied to that inclusive
// count, not to the interval between the endpoints.
func TestSummary_RejectsWindowBeyondTheCap(t *testing.T) {
	_, e := newEnv(t)
	for _, tc := range []struct {
		days int
		want int
	}{
		{7, http.StatusOK},
		{366, http.StatusOK},
		{367, http.StatusBadRequest},
		{100000, http.StatusBadRequest},
		{0, http.StatusBadRequest},
		{-1, http.StatusBadRequest},
	} {
		resp, err := e.srv.Client().Get(fmt.Sprintf("%s/businesses/%s/analytics/summary?client_id=%s&days=%d",
			e.srv.URL, e.biz, e.site, tc.days))
		if err != nil {
			t.Fatalf("days=%d: %v", tc.days, err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("days=%d: got %d, want %d", tc.days, resp.StatusCode, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Enrichment (as0 task 4)
// ---------------------------------------------------------------------------

// collectFull posts a pageview through the simulated trusted ingress.
func (e *env) collectFull(t *testing.T, path, ref, query, ua, ip string) int {
	return e.collectFullWithCountry(t, path, ref, query, ua, ip, "")
}

func (e *env) collectFullWithCountry(t *testing.T, path, ref, query, ua, ip, country string) int {
	return e.collectFullWithSourceAndCountry(t, path, ref, query, ua, ip, ip, country)
}

func (e *env) collectFullWithSourceAndCountry(
	t *testing.T,
	path, ref, query, ua, visitorIP, sourceIP, country string,
) int {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"k": e.key, "p": path, "r": ref, "q": query})
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/a/e", bytes.NewReader(b))
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("CF-Connecting-IP", visitorIP)
	req.Header.Set("X-Forwarded-For", sourceIP)
	if country != "" {
		req.Header.Set("CF-IPCountry", country)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

const iphoneUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 " +
	"Version/17.0 Mobile/15E148 Safari/604.1"

func TestEnrichment_StoresDerivedDimensionsOnly(t *testing.T) {
	ctx, e := newEnv(t)
	// Header trust is disabled in this environment.
	e.collectFullWithCountry(t, "/pricing", "",
		"utm_source=hn&utm_medium=social&utm_campaign=launch&token=SECRET",
		humanUA, "203.0.113.10", "US")

	var src, med, camp, device, browser string
	var country *string
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT coalesce(utm_source,''), coalesce(utm_medium,''), coalesce(utm_campaign,''),
		        coalesce(device_type,''), coalesce(browser,''), country
		   FROM analytics_event WHERE client_id=$1`, e.site,
	).Scan(&src, &med, &camp, &device, &browser, &country); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if src != "hn" || med != "social" || camp != "launch" {
		t.Errorf("utm = %q/%q/%q", src, med, camp)
	}
	if device != "desktop" || browser != "Chrome" {
		t.Errorf("device/browser = %q/%q", device, browser)
	}
	// Header trust is disabled in the default test deployment, so country remains NULL.
	if country != nil {
		t.Errorf("country = %v, want NULL with no trusted edge signal", *country)
	}

	// The whole row must still contain no raw UA and no token from the query string.
	var js string
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT row_to_json(t)::text FROM (SELECT * FROM analytics_event WHERE client_id=$1) t`,
		e.site).Scan(&js); err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"SECRET", "Chrome/120.0.0.0", "203.0.113.10"} {
		if strings.Contains(js, leaked) {
			t.Errorf("PRIVACY VIOLATION: %q is stored in the event row: %s", leaked, js)
		}
	}
}

func TestEnrichment_StoresAndRollsUpTrustedCloudflareCountry(t *testing.T) {
	ctx, e := newEnvWithCloudflareCountryTrust(t, true)
	e.collectFullWithSourceAndCountry(
		t, "/", "", "", humanUA, "198.51.100.10", "203.0.113.10", "ca",
	)

	var country *string
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT country FROM analytics_event WHERE client_id=$1`, e.site,
	).Scan(&country); err != nil {
		t.Fatalf("read country: %v", err)
	}
	if country == nil || *country != "CA" {
		t.Fatalf("country = %v, want CA", country)
	}

	e.rollup(t, ctx)
	code, summary := e.summary(t, e.site)
	if code != http.StatusOK {
		t.Fatalf("summary status %d", code)
	}
	rows := summary.Breakdowns["country"]
	if len(rows) != 1 || rows[0].Value != "CA" || rows[0].Pageviews != 1 {
		t.Fatalf("country breakdown = %+v, want CA with one pageview", rows)
	}
}

func TestEnrichment_RejectsCountryFromUntrustedPeer(t *testing.T) {
	ctx, e := newEnvWithCloudflareCountryTrustAndProxy(t, true, false)
	e.collectFullWithCountry(t, "/", "", "", humanUA, "203.0.113.10", "US")

	var country *string
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT country FROM analytics_event WHERE client_id=$1`, e.site,
	).Scan(&country); err != nil {
		t.Fatalf("read country: %v", err)
	}
	if country != nil {
		t.Fatalf("country = %q, want NULL from an untrusted direct peer", *country)
	}
}

func TestEnrichment_RejectsCountryFromNonCloudflareForwardedSource(t *testing.T) {
	ctx, e := newEnvWithCloudflareCountryTrust(t, true)
	e.collectFullWithSourceAndCountry(
		t, "/", "", "", humanUA, "192.0.2.10", "198.51.100.10", "US",
	)

	var country *string
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT country FROM analytics_event WHERE client_id=$1`, e.site,
	).Scan(&country); err != nil {
		t.Fatalf("read country: %v", err)
	}
	if country != nil {
		t.Fatalf("country = %q, want NULL from a non-Cloudflare forwarded source", *country)
	}
}

func TestEnrichment_RollupProducesBreakdowns(t *testing.T) {
	ctx, e := newEnv(t)
	e.collectFull(t, "/", "", "utm_source=hn&utm_medium=social", humanUA, "203.0.113.1")
	e.collectFull(t, "/", "", "utm_source=hn", humanUA, "203.0.113.1")
	e.collectFull(t, "/", "", "utm_source=twitter", iphoneUA, "203.0.113.2")
	e.rollup(t, ctx)

	code, s := e.summary(t, e.site)
	if code != http.StatusOK {
		t.Fatalf("summary status %d", code)
	}

	src := s.Breakdowns["utm_source"]
	if len(src) != 2 {
		t.Fatalf("utm_source rows = %d, want 2: %+v", len(src), src)
	}
	if src[0].Value != "hn" || src[0].Pageviews != 2 {
		t.Errorf("top source = %+v, want hn with 2", src[0])
	}

	dev := s.Breakdowns["device"]
	if len(dev) != 2 {
		t.Fatalf("device rows = %d, want desktop+mobile: %+v", len(dev), dev)
	}

	br := s.Breakdowns["browser"]
	byName := map[string]int64{}
	for _, v := range br {
		byName[v.Value] = v.Pageviews
	}
	if byName["Chrome"] != 2 || byName["Safari"] != 1 {
		t.Errorf("browser breakdown = %+v, want Chrome:2 Safari:1", byName)
	}

	// A tracked dimension with no data is present but empty, so the UI can tell "nothing
	// collected" apart from "not tracked".
	if _, ok := s.Breakdowns["country"]; !ok {
		t.Error("country key should be present (empty) even with no trusted edge signal")
	}
	if len(s.Breakdowns["country"]) != 0 {
		t.Errorf("country should be empty with no trusted edge signal, got %+v", s.Breakdowns["country"])
	}
}

// Same recompute-not-increment contract as every other rollup in the system.
func TestEnrichment_DimensionRollupIsIdempotent(t *testing.T) {
	ctx, e := newEnv(t)
	for i := 0; i < 4; i++ {
		e.collectFull(t, "/", "", "utm_source=hn", humanUA, "203.0.113.1")
	}
	e.rollup(t, ctx)
	_, first := e.summary(t, e.site)

	if _, err := e.tdb.Super.Exec(ctx,
		`UPDATE rollup_state SET watermark_ingested_at='-infinity' WHERE rollup_name='analytics_dimensions'`); err != nil {
		t.Fatal(err)
	}
	e.rollup(t, ctx)
	_, second := e.summary(t, e.site)

	if len(first.Breakdowns["utm_source"]) == 0 {
		t.Fatal("no utm_source rows after the first sweep")
	}
	if first.Breakdowns["utm_source"][0].Pageviews != second.Breakdowns["utm_source"][0].Pageviews {
		t.Fatalf("dimension rollup is not idempotent: %d then %d",
			first.Breakdowns["utm_source"][0].Pageviews,
			second.Breakdowns["utm_source"][0].Pageviews)
	}
	if got := first.Breakdowns["utm_source"][0].Pageviews; got != 4 {
		t.Fatalf("expected 4 pageviews, got %d", got)
	}
}

// Bots must be excluded from the breakdowns exactly as they are from the headline numbers.
func TestEnrichment_BotsExcludedFromBreakdowns(t *testing.T) {
	ctx, e := newEnv(t)
	e.collectFull(t, "/", "", "utm_source=hn", humanUA, "203.0.113.1")
	e.collectFull(t, "/", "", "utm_source=spam", "Googlebot/2.1", "203.0.113.9")
	e.rollup(t, ctx)

	_, s := e.summary(t, e.site)
	for _, v := range s.Breakdowns["utm_source"] {
		if v.Value == "spam" {
			t.Fatalf("bot traffic appears in the utm_source breakdown: %+v", s.Breakdowns["utm_source"])
		}
	}
}

// utm_* values come from a public endpoint and are attacker-chosen. Without a cap, a unique
// campaign per pageview yields roughly one rollup row per event — the table then grows with
// TRAFFIC rather than with distinct values, which defeats the point of a rollup entirely.
func TestEnrichment_DimensionCardinalityIsBounded(t *testing.T) {
	ctx, e := newEnv(t)
	const unique = 40 // deliberately above the test cap set below
	for i := 0; i < unique; i++ {
		e.collectFull(t, "/", "", fmt.Sprintf("utm_campaign=c%03d", i), humanUA, "203.0.113.1")
	}
	// Run the rollup with a small explicit cap so the test does not need 200+ events.
	if _, err := e.tdb.Super.Exec(ctx,
		"SELECT rollup_analytics_dimensions(interval '0', interval '5 minutes', 5)"); err != nil {
		t.Fatalf("rollup: %v", err)
	}

	var rows int
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM analytics_dimension_daily
		  WHERE client_id=$1 AND dimension='utm_campaign'`, e.site).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	// 5 kept values + one '(other)' bucket, never 40.
	if rows != 6 {
		t.Fatalf("dimension rows = %d, want 6 (5 kept + '(other)'). Unbounded cardinality means "+
			"one rollup row per event", rows)
	}

	// Nothing may be lost in the fold: kept + (other) must still account for every pageview.
	var total int64
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT coalesce(sum(pageviews),0) FROM analytics_dimension_daily
		  WHERE client_id=$1 AND dimension='utm_campaign'`, e.site).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != unique {
		t.Fatalf("folded rollup lost pageviews: sum=%d, want %d", total, unique)
	}

	var other int64
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT pageviews FROM analytics_dimension_daily
		  WHERE client_id=$1 AND dimension='utm_campaign' AND value='(other)'`, e.site).Scan(&other); err != nil {
		t.Fatalf("no '(other)' bucket: %v", err)
	}
	if other != unique-5 {
		t.Fatalf("'(other)' = %d, want %d", other, unique-5)
	}
}

// A value that drops out of the kept set between sweeps must not leave a stale row behind while
// also being counted inside '(other)' — that would double-count it.
func TestEnrichment_CappedRollupHasNoStaleRows(t *testing.T) {
	ctx, e := newEnv(t)
	// First: "alpha" is the only campaign, so it is kept.
	for i := 0; i < 3; i++ {
		e.collectFull(t, "/", "", "utm_campaign=alpha", humanUA, "203.0.113.1")
	}
	if _, err := e.tdb.Super.Exec(ctx,
		"SELECT rollup_analytics_dimensions(interval '0', interval '5 minutes', 1)"); err != nil {
		t.Fatal(err)
	}

	// Then "beta" overtakes it, so with a cap of 1 "alpha" must fold into '(other)'.
	for i := 0; i < 10; i++ {
		e.collectFull(t, "/", "", "utm_campaign=beta", humanUA, "203.0.113.1")
	}
	if _, err := e.tdb.Super.Exec(ctx,
		`UPDATE rollup_state SET watermark_ingested_at='-infinity' WHERE rollup_name='analytics_dimensions'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.tdb.Super.Exec(ctx,
		"SELECT rollup_analytics_dimensions(interval '0', interval '5 minutes', 1)"); err != nil {
		t.Fatal(err)
	}

	var total int64
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT coalesce(sum(pageviews),0) FROM analytics_dimension_daily
		  WHERE client_id=$1 AND dimension='utm_campaign'`, e.site).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 13 {
		t.Fatalf("sum = %d, want 13. A stale 'alpha' row left behind while alpha is ALSO inside "+
			"'(other)' would report 16", total)
	}
}

// ---------------------------------------------------------------------------
// Custom events
// ---------------------------------------------------------------------------

func (e *env) collectEvent(t *testing.T, name string, props map[string]any, ua, ip string) int {
	t.Helper()
	body := map[string]any{"k": e.key, "p": "/game", "n": name}
	if props != nil {
		body["d"] = props
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/a/e", bytes.NewReader(b))
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("X-Forwarded-For", ip)
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("collect event: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestCustomEvent_StoredWithNameAndProps(t *testing.T) {
	ctx, e := newEnv(t)
	svc := analytics.NewService(e.tdb.App)
	if _, err := svc.ReplacePropertyRules(ctx, e.prin, e.biz, e.site, []analytics.PropertyRuleInput{
		{EventName: "grow_start", PropertyKey: "level", Label: "Level"},
		{EventName: "grow_start", PropertyKey: "mode", Label: "Mode"},
	}); err != nil {
		t.Fatalf("configure properties: %v", err)
	}
	e.collectEvent(t, "grow_start", map[string]any{
		"level": 3, "mode": "classic", "unknown": "discard me", "email": "private@example.test",
		"nested": map[string]any{"discard": true},
	}, humanUA, "203.0.113.1")

	var name, props string
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT name, props::text FROM analytics_event WHERE client_id=$1`, e.site).Scan(&name, &props); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if name != "grow_start" {
		t.Errorf("name = %q, want grow_start", name)
	}
	if !strings.Contains(props, `"level": "3"`) && !strings.Contains(props, `"level":"3"`) {
		t.Errorf("props missing level: %s", props)
	}
	if !strings.Contains(props, "classic") {
		t.Errorf("props missing mode: %s", props)
	}
	for _, forbidden := range []string{"unknown", "private@example.test", "nested"} {
		if strings.Contains(props, forbidden) {
			t.Errorf("unconfigured/non-scalar property %q retained: %s", forbidden, props)
		}
	}
}

func TestCustomEvent_NoConfigurationStoresNoProperties(t *testing.T) {
	ctx, e := newEnv(t)
	e.collectEvent(t, "grow_start", map[string]any{
		"mode": "classic", "customer_id": "persistent-123",
	}, humanUA, "203.0.113.1")
	var props string
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT props::text FROM analytics_event WHERE client_id=$1`, e.site).Scan(&props); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if props != "{}" {
		t.Fatalf("props without configuration = %s, want {}", props)
	}
}

func TestPropertyAggregates_NonRetroactiveIdempotentAndRollupOnly(t *testing.T) {
	ctx, e := newEnv(t)

	// This raw property predates the rule. Configuring a panel must not make old JSON eligible.
	if _, err := e.tdb.Super.Exec(ctx,
		`INSERT INTO analytics_event (
		    tenant_root_id, business_id, client_id, occurred_at, name, props, path, visitor_hash
		 ) VALUES ($1,$1,$2,now() - interval '1 hour','grow_start',
		           '{"mode":"legacy"}','/game',decode('01','hex'))`,
		e.biz, e.site); err != nil {
		t.Fatalf("seed pre-activation event: %v", err)
	}

	svc := analytics.NewService(e.tdb.App)
	rules, err := svc.ReplacePropertyRules(ctx, e.prin, e.biz, e.site, []analytics.PropertyRuleInput{
		{EventName: "grow_exit", PropertyKey: "reason", Label: "Exit reason"},
		{EventName: "grow_start", PropertyKey: "mode", Label: "Game mode"},
	})
	if err != nil || len(rules) != 2 {
		t.Fatalf("configure property rules = %+v, %v", rules, err)
	}
	// A late-ingested event is still ineligible when its occurrence predates activation. This
	// exercises the boundary independently of the event seeded before configuration existed.
	if _, err := e.tdb.Super.Exec(ctx,
		`INSERT INTO analytics_event (
		    tenant_root_id, business_id, client_id, occurred_at, name, props, path, visitor_hash
		 ) VALUES ($1,$1,$2,now() - interval '30 minutes','grow_start',
		           '{"mode":"late"}','/game',decode('03','hex'))`,
		e.biz, e.site); err != nil {
		t.Fatalf("seed late pre-activation event: %v", err)
	}
	for _, event := range []struct {
		value any
		ip    string
	}{
		{value: "classic", ip: "203.0.113.1"},
		{value: "classic", ip: "203.0.113.2"},
		{value: "timed", ip: "203.0.113.3"},
		{value: 3, ip: "203.0.113.4"},
		{value: true, ip: "203.0.113.5"},
	} {
		if code := e.collectEvent(t, "grow_start", map[string]any{"mode": event.value}, humanUA, event.ip); code != http.StatusNoContent {
			t.Fatalf("collect %v status = %d", event.value, code)
		}
	}
	e.collectEvent(t, "grow_start", map[string]any{"mode": "bot"}, "Googlebot/2.1", "203.0.113.9")
	// The public collector drops nested values, but the SQL rollup also rejects them as defense in
	// depth if malformed historical data exists.
	if _, err := e.tdb.Super.Exec(ctx,
		`INSERT INTO analytics_event (
		    tenant_root_id, business_id, client_id, occurred_at, name, props, path, visitor_hash
		 ) VALUES ($1,$1,$2,now(),'grow_start',
		           '{"mode":{"nested":true}}','/game',decode('02','hex'))`,
		e.biz, e.site); err != nil {
		t.Fatalf("seed non-primitive property: %v", err)
	}
	e.rollup(t, ctx)

	_, first := e.summary(t, e.site)
	if len(first.PropertyBreakdowns) != 2 {
		t.Fatalf("property panels = %+v, want configured non-empty and empty panels", first.PropertyBreakdowns)
	}
	var game, empty *analytics.PropertyBreakdown
	for i := range first.PropertyBreakdowns {
		switch first.PropertyBreakdowns[i].Label {
		case "Game mode":
			game = &first.PropertyBreakdowns[i]
		case "Exit reason":
			empty = &first.PropertyBreakdowns[i]
		}
	}
	if game == nil || empty == nil || len(empty.Values) != 0 {
		t.Fatalf("structured property panels = %+v", first.PropertyBreakdowns)
	}
	values := make(map[string]analytics.PropertyValueCount, len(game.Values))
	for _, value := range game.Values {
		values[value.Value] = value
	}
	if len(values) != 4 || values["classic"].Events != 2 || values["classic"].Visitors != 2 ||
		values["timed"].Events != 1 || values["timed"].Visitors != 1 ||
		values["3"].Events != 1 || values["true"].Events != 1 {
		t.Fatalf("game mode values = %+v, want governed string/number/boolean values", game.Values)
	}
	for _, value := range game.Values {
		if value.Value == "legacy" || value.Value == "late" || value.Value == "bot" {
			t.Fatalf("ineligible value was aggregated: %+v", game.Values)
		}
	}
	if len(first.Breakdowns["event"]) != 1 || first.Breakdowns["event"][0].Pageviews != 8 {
		t.Fatalf("standard event breakdown changed by property rollup: %+v", first.Breakdowns["event"])
	}

	// A normal consecutive sweep overlaps the same UTC bucket after one more event arrives. The
	// whole bucket must be replaced with the new exact count rather than incremented or duplicated.
	e.collectEvent(t, "grow_start", map[string]any{"mode": "classic"}, humanUA, "203.0.113.6")
	e.rollup(t, ctx)
	_, overlapped := e.summary(t, e.site)
	var classicAfterOverlap int64
	for _, panel := range overlapped.PropertyBreakdowns {
		if panel.Label != "Game mode" {
			continue
		}
		for _, value := range panel.Values {
			if value.Value == "classic" {
				classicAfterOverlap = value.Events
			}
		}
	}
	if classicAfterOverlap != 3 {
		t.Fatalf("classic after overlapping sweep = %d, want exact recomputed count 3",
			classicAfterOverlap)
	}

	// Rewinding and recomputing the bucket must replace, not increment, the aggregate.
	if _, err := e.tdb.Super.Exec(ctx,
		`UPDATE rollup_state SET watermark_ingested_at='-infinity'
		  WHERE rollup_name='analytics_properties'`); err != nil {
		t.Fatalf("rewind property rollup: %v", err)
	}
	e.rollup(t, ctx)
	_, second := e.summary(t, e.site)
	if !reflect.DeepEqual(second.PropertyBreakdowns, overlapped.PropertyBreakdowns) {
		t.Fatalf("property rollup is not idempotent: first=%+v second=%+v",
			overlapped.PropertyBreakdowns, second.PropertyBreakdowns)
	}

	// Dashboard reads are independent of raw-event retention after aggregation.
	if _, err := e.tdb.Super.Exec(ctx, `DELETE FROM analytics_event WHERE client_id=$1`, e.site); err != nil {
		t.Fatalf("delete raw events: %v", err)
	}
	_, afterRetention := e.summary(t, e.site)
	if !reflect.DeepEqual(afterRetention.PropertyBreakdowns, overlapped.PropertyBreakdowns) {
		t.Fatalf("summary changed after raw retention: before=%+v after=%+v",
			overlapped.PropertyBreakdowns, afterRetention.PropertyBreakdowns)
	}
}

func TestPropertyAggregates_CardinalityCapAndReactivation(t *testing.T) {
	ctx, e := newEnv(t)
	svc := analytics.NewService(e.tdb.App)
	configured, err := svc.ReplacePropertyRules(ctx, e.prin, e.biz, e.site,
		[]analytics.PropertyRuleInput{{EventName: "grow_start", PropertyKey: "mode", Label: "Mode"}})
	if err != nil || len(configured) != 1 {
		t.Fatalf("configure = %+v, %v", configured, err)
	}
	for value, count := range map[string]int{"alpha": 3, "beta": 2, "gamma": 1} {
		for i := 0; i < count; i++ {
			e.collectEvent(t, "grow_start", map[string]any{"mode": value}, humanUA,
				fmt.Sprintf("203.0.113.%d", 20+i))
		}
	}
	if _, err := e.tdb.Super.Exec(ctx,
		"SELECT rollup_analytics_properties(interval '0', interval '5 minutes', 2)"); err != nil {
		t.Fatalf("bounded property rollup: %v", err)
	}
	dimension := "property:" + configured[0].ID.String()
	var rows int
	var total, other int64
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT count(*), coalesce(sum(pageviews),0),
		        coalesce(sum(pageviews) FILTER (WHERE value='(other)'),0)
		   FROM analytics_dimension_daily WHERE client_id=$1 AND dimension=$2`,
		e.site, dimension).Scan(&rows, &total, &other); err != nil {
		t.Fatalf("read bounded property rows: %v", err)
	}
	if rows != 3 || total != 6 || other != 1 {
		t.Fatalf("bounded rows/counts = %d/%d/%d, want 3/6/1", rows, total, other)
	}

	if _, err := svc.ReplacePropertyRules(ctx, e.prin, e.biz, e.site, nil); err != nil {
		t.Fatalf("remove rule: %v", err)
	}
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM analytics_dimension_daily WHERE client_id=$1 AND dimension=$2`,
		e.site, dimension).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("removed rule rows = %d, err %v", rows, err)
	}
	reactivated, err := svc.ReplacePropertyRules(ctx, e.prin, e.biz, e.site,
		[]analytics.PropertyRuleInput{{EventName: "grow_start", PropertyKey: "mode", Label: "Mode"}})
	if err != nil || len(reactivated) != 1 {
		t.Fatalf("reactivate = %+v, %v", reactivated, err)
	}
	if reactivated[0].ID == configured[0].ID || !reactivated[0].EnabledAt.After(configured[0].EnabledAt) {
		t.Fatalf("reactivation did not establish a new boundary: before=%+v after=%+v",
			configured[0], reactivated[0])
	}
	e.collectEvent(t, "grow_start", map[string]any{"mode": "delta"}, humanUA, "203.0.113.40")
	if _, err := e.tdb.Super.Exec(ctx,
		"SELECT rollup_analytics_properties(interval '0', interval '5 minutes', 2)"); err != nil {
		t.Fatalf("reactivated property rollup: %v", err)
	}
	_, summary := e.summary(t, e.site)
	if len(summary.PropertyBreakdowns) != 1 || len(summary.PropertyBreakdowns[0].Values) != 1 ||
		summary.PropertyBreakdowns[0].Values[0].Value != "delta" ||
		summary.PropertyBreakdowns[0].Values[0].Events != 1 {
		t.Fatalf("reactivated panel included pre-boundary values: %+v", summary.PropertyBreakdowns)
	}
}

func TestPropertyRules_APIStableIdentityValidationAndClear(t *testing.T) {
	ctx, e := newEnv(t)
	path := "/businesses/" + e.biz.String() + "/analytics/sites/" + e.site.String() + "/property-rules"

	put := func(body string) (*http.Response, map[string][]analytics.PropertyRule) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut,
			e.srv.URL+path,
			strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := e.srv.Client().Do(req)
		if err != nil {
			t.Fatalf("PUT property rules: %v", err)
		}
		var decoded map[string][]analytics.PropertyRule
		if resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
				resp.Body.Close()
				t.Fatalf("decode rules: %v", err)
			}
		}
		resp.Body.Close()
		return resp, decoded
	}

	firstResponse, first := put(`{"rules":[{"event_name":"checkout_completed","property_key":"plan","label":"Plan"}]}`)
	if firstResponse.StatusCode != http.StatusOK || len(first["rules"]) != 1 {
		t.Fatalf("first replace = status %d body %+v", firstResponse.StatusCode, first)
	}
	firstRule := first["rules"][0]
	getResponse, err := e.srv.Client().Get(e.srv.URL + path)
	if err != nil {
		t.Fatalf("GET property rules: %v", err)
	}
	var listed map[string][]analytics.PropertyRule
	if err := json.NewDecoder(getResponse.Body).Decode(&listed); err != nil {
		getResponse.Body.Close()
		t.Fatalf("decode listed rules: %v", err)
	}
	getResponse.Body.Close()
	if getResponse.StatusCode != http.StatusOK || len(listed["rules"]) != 1 ||
		listed["rules"][0].ID != firstRule.ID {
		t.Fatalf("GET rules = status %d body %+v", getResponse.StatusCode, listed)
	}

	secondResponse, second := put(`{"rules":[{"event_name":"checkout_completed","property_key":"plan","label":"Subscription plan"}]}`)
	if secondResponse.StatusCode != http.StatusOK || len(second["rules"]) != 1 {
		t.Fatalf("label replace = status %d body %+v", secondResponse.StatusCode, second)
	}
	if second["rules"][0].ID != firstRule.ID || !second["rules"][0].EnabledAt.Equal(firstRule.EnabledAt) {
		t.Fatalf("label-only update changed stable boundary: first=%+v second=%+v",
			firstRule, second["rules"][0])
	}

	badResponse, _ := put(`{"rules":[{"event_name":"checkout_completed","property_key":"customer_id","label":"Customer"}]}`)
	if badResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("sensitive property status = %d, want 400", badResponse.StatusCode)
	}
	for _, body := range []string{
		`{}`,
		`{"rules":null}`,
		`{"rules":[{"event_name":"checkout_completed","property_key":"plan","label":"Plan","typo":true}]}`,
	} {
		response, _ := put(body)
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("invalid body %s status = %d, want 400", body, response.StatusCode)
		}
	}

	getStatus := func(requestPath string) int {
		t.Helper()
		response, err := e.srv.Client().Get(e.srv.URL + requestPath)
		if err != nil {
			t.Fatalf("GET %s: %v", requestPath, err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	if status := getStatus("/businesses/not-a-uuid/analytics/sites/" + e.site.String() + "/property-rules"); status != http.StatusBadRequest {
		t.Errorf("malformed business status = %d, want 400", status)
	}
	if status := getStatus("/businesses/" + e.biz.String() + "/analytics/sites/not-a-uuid/property-rules"); status != http.StatusNotFound {
		t.Errorf("malformed client status = %d, want 404", status)
	}

	unauthenticatedRouter := chi.NewRouter()
	handler := analytics.NewHandler(analytics.NewService(e.tdb.App))
	handler.ReadRoutes(unauthenticatedRouter)
	handler.WriteRoutes(unauthenticatedRouter)
	unauthenticatedServer := httptest.NewServer(unauthenticatedRouter)
	defer unauthenticatedServer.Close()
	unauthenticated, err := unauthenticatedServer.Client().Get(unauthenticatedServer.URL + path)
	if err != nil {
		t.Fatalf("unauthenticated GET: %v", err)
	}
	unauthenticated.Body.Close()
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET status = %d, want 401", unauthenticated.StatusCode)
	}

	clearResponse, cleared := put(`{"rules":[]}`)
	if clearResponse.StatusCode != http.StatusOK || len(cleared["rules"]) != 0 {
		t.Fatalf("clear = status %d body %+v", clearResponse.StatusCode, cleared)
	}
	var count int
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM analytics_property_rule WHERE client_id=$1`, e.site).Scan(&count); err != nil {
		t.Fatalf("count rules: %v", err)
	}
	if count != 0 {
		t.Fatalf("rules after clear = %d, want 0", count)
	}
}

func TestPropertyRules_SQLBoundaryAndTenantIsolation(t *testing.T) {
	ctx, e := newEnv(t)
	svc := analytics.NewService(e.tdb.App)
	if _, err := svc.ReplacePropertyRules(ctx, e.prin, e.biz, e.site, []analytics.PropertyRuleInput{
		{EventName: "grow_start", PropertyKey: "mode", Label: "Mode"},
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}

	if _, err := svc.ListPropertyRules(ctx, e.prin, uuid.New(), e.site); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("wrong business = %v, want not found", err)
	}
	if _, err := svc.ListPropertyRules(ctx, e.prin, e.biz, uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("unknown client = %v, want not found", err)
	}

	if err := e.tdb.App.WithPrincipal(ctx, e.prin, func(tx pgx.Tx) error {
		for _, payload := range []string{
			`[{"event_name":"grow_start","property_key":"email","label":"Email"}]`,
			`[{"event_name":"grow_start","property_key":"bad/key","label":"Bad"}]`,
			`[{"event_name":"bad@event","property_key":"mode","label":"Bad"}]`,
			`{"event_name":"grow_start","property_key":"mode","label":"Mode"}`,
			`[null]`,
		} {
			var outcome string
			if err := tx.QueryRow(ctx,
				`SELECT analytics_replace_property_rules($1,$2,$3::jsonb)`, e.biz, e.site, payload,
			).Scan(&outcome); err != nil {
				return err
			}
			if outcome != "invalid" {
				t.Fatalf("direct SQL validation for %s = %q, want invalid", payload, outcome)
			}
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO analytics_property_rule
			 (tenant_root_id,business_id,client_id,event_name,property_key,label)
			 VALUES ($1,$1,$2,'grow_start','mode2','Mode 2')`, e.biz, e.site)
		return err
	}); err == nil {
		t.Fatal("manyforge_app could mutate analytics_property_rule directly")
	}

	tel := telemetry.NewService(e.tdb.App, nil)
	if _, err := tel.RevokeClient(ctx, e.prin, e.biz, e.site); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.ListPropertyRules(ctx, e.prin, e.biz, e.site); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("list revoked = %v, want not found", err)
	}
	if _, err := svc.ReplacePropertyRules(ctx, e.prin, e.biz, e.site, nil); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("replace revoked = %v, want not found", err)
	}
}

func TestPropertyRules_FollowSiteAcrossTenantRootMove(t *testing.T) {
	ctx, e := newEnv(t)
	analyticsSvc := analytics.NewService(e.tdb.App)
	configured, err := analyticsSvc.ReplacePropertyRules(
		ctx, e.prin, e.biz, e.site,
		[]analytics.PropertyRuleInput{{EventName: "grow_start", PropertyKey: "mode", Label: "Mode"}},
	)
	if err != nil || len(configured) != 1 {
		t.Fatalf("configure = %+v, %v", configured, err)
	}
	e.collectEvent(t, "grow_start", map[string]any{"mode": "classic"}, humanUA, "203.0.113.1")
	e.rollup(t, ctx)

	target := uuid.New()
	var ownerRole uuid.UUID
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'`).Scan(&ownerRole); err != nil {
		t.Fatalf("owner role: %v", err)
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at)
		  VALUES ($1,NULL,$1,'Property target','active',now(),now())`, []any{target}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id)
		  VALUES ($1,$1,0,$1)`, []any{target}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at)
		  VALUES ($1,$2,$2,$3,now())`, []any{e.prin, target, ownerRole}},
	} {
		if _, err := e.tdb.Super.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed move target: %v\n%s", err, statement.sql)
		}
	}

	if _, err := telemetry.NewService(e.tdb.App, nil).MoveClient(
		ctx, e.prin, e.biz, e.site, target,
	); err != nil {
		t.Fatalf("move site: %v", err)
	}
	moved, err := analyticsSvc.ListPropertyRules(ctx, e.prin, target, e.site)
	if err != nil || len(moved) != 1 {
		t.Fatalf("list moved rules = %+v, %v", moved, err)
	}
	if moved[0].ID != configured[0].ID || !moved[0].EnabledAt.Equal(configured[0].EnabledAt) {
		t.Fatalf("move changed stable rule boundary: before=%+v after=%+v", configured[0], moved[0])
	}
	var businessID, tenantRootID uuid.UUID
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT business_id, tenant_root_id FROM analytics_property_rule WHERE id=$1`, moved[0].ID,
	).Scan(&businessID, &tenantRootID); err != nil {
		t.Fatalf("read moved scope: %v", err)
	}
	if businessID != target || tenantRootID != target {
		t.Fatalf("moved rule scope = %s/%s, want %s/%s", businessID, tenantRootID, target, target)
	}
	var dimensionBusinessID, dimensionTenantRootID uuid.UUID
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT business_id, tenant_root_id FROM analytics_dimension_daily
		  WHERE client_id=$1 AND dimension=$2`,
		e.site, "property:"+moved[0].ID.String(),
	).Scan(&dimensionBusinessID, &dimensionTenantRootID); err != nil {
		t.Fatalf("read moved property aggregate: %v", err)
	}
	if dimensionBusinessID != target || dimensionTenantRootID != target {
		t.Fatalf("moved aggregate scope = %s/%s, want %s/%s",
			dimensionBusinessID, dimensionTenantRootID, target, target)
	}
	movedSummary, err := analyticsSvc.Summary(
		ctx, e.prin, target, e.site, timeNow().AddDate(0, 0, -6), timeNow(),
	)
	if err != nil || len(movedSummary.PropertyBreakdowns) != 1 ||
		len(movedSummary.PropertyBreakdowns[0].Values) != 1 ||
		movedSummary.PropertyBreakdowns[0].Values[0].Value != "classic" {
		t.Fatalf("moved property summary = %+v, %v", movedSummary.PropertyBreakdowns, err)
	}
}

func TestPropertyRules_ReplacementSerializesWithCollection(t *testing.T) {
	ctx, e := newEnv(t)
	tx, err := e.tdb.App.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin replacement: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`SELECT set_config('manyforge.principal_id', $1, true)`, e.prin.String()); err != nil {
		t.Fatalf("set principal: %v", err)
	}
	var outcome string
	if err := tx.QueryRow(ctx,
		`SELECT analytics_replace_property_rules($1,$2,$3::jsonb)`, e.biz, e.site,
		`[{"event_name":"grow_start","property_key":"mode","label":"Mode"}]`,
	).Scan(&outcome); err != nil || outcome != "updated" {
		t.Fatalf("replace inside held transaction = %q, %v", outcome, err)
	}

	done := make(chan error, 1)
	go func() {
		body, _ := json.Marshal(map[string]any{
			"k": e.key, "p": "/game", "n": "grow_start", "d": map[string]any{"mode": "classic"},
		})
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/a/e", bytes.NewReader(body))
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
		resp, requestErr := e.srv.Client().Do(req)
		if requestErr == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				requestErr = fmt.Errorf("collector status %d", resp.StatusCode)
			}
		}
		done <- requestErr
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var waitingOnLock bool
		if err := e.tdb.Super.QueryRow(ctx,
			`SELECT EXISTS (
			    SELECT 1
			      FROM pg_stat_activity
			     WHERE datname = current_database()
			       AND query LIKE '%analytics_collect(%'
			       AND wait_event_type = 'Lock'
			)`,
		).Scan(&waitingOnLock); err != nil {
			t.Fatalf("inspect collector lock wait: %v", err)
		}
		if waitingOnLock {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("collector completed before reaching the rule-set lock: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("collector never reached the expected client-row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit replacement: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("collector after commit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("collector remained blocked after property-rule commit")
	}
	var props string
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT props::text FROM analytics_event WHERE client_id=$1`, e.site).Scan(&props); err != nil {
		t.Fatalf("read collected props: %v", err)
	}
	if !strings.Contains(props, "classic") {
		t.Fatalf("collector did not use committed rule set: %s", props)
	}
}

// THE invariant: a custom event must never inflate pageview or visitor counts.
func TestCustomEvent_DoesNotCountAsPageview(t *testing.T) {
	ctx, e := newEnv(t)
	e.collectFull(t, "/", "", "", humanUA, "203.0.113.1")        // 1 real pageview
	e.collectEvent(t, "grow_start", nil, humanUA, "203.0.113.1") // events, not pageviews
	e.collectEvent(t, "grow_exit", nil, humanUA, "203.0.113.1")
	e.rollup(t, ctx)

	code, s := e.summary(t, e.site)
	if code != http.StatusOK {
		t.Fatalf("summary status %d", code)
	}
	if s.Pageviews != 1 {
		t.Fatalf("pageviews = %d, want 1 — custom events must not inflate the headline number", s.Pageviews)
	}
	// Top pages likewise counts only pageviews.
	total := int64(0)
	for _, p := range s.TopPages {
		total += p.Pageviews
	}
	if total != 1 {
		t.Fatalf("top_pages total = %d, want 1", total)
	}
}

func TestCustomEvent_AppearsAsAnEventBreakdown(t *testing.T) {
	ctx, e := newEnv(t)
	e.collectEvent(t, "grow_start", nil, humanUA, "203.0.113.1")
	e.collectEvent(t, "grow_start", nil, humanUA, "203.0.113.2")
	e.collectEvent(t, "grow_exit", nil, humanUA, "203.0.113.1")
	e.rollup(t, ctx)

	_, s := e.summary(t, e.site)
	ev := s.Breakdowns["event"]
	if len(ev) != 2 {
		t.Fatalf("event rows = %d, want 2: %+v", len(ev), ev)
	}
	if ev[0].Value != "grow_start" || ev[0].Pageviews != 2 {
		t.Errorf("top event = %+v, want grow_start with 2", ev[0])
	}
	if ev[0].Visitors != 2 {
		t.Errorf("grow_start visitors = %d, want 2 (two distinct IPs)", ev[0].Visitors)
	}
}

// A bucket containing ONLY custom events must still get its breakdown rolled up — if the touched
// set were pageview-only, an events-only day would be invisible.
func TestCustomEvent_EventsOnlyBucketIsRolledUp(t *testing.T) {
	ctx, e := newEnv(t)
	e.collectEvent(t, "grow_start", nil, humanUA, "203.0.113.1")
	e.rollup(t, ctx)

	_, s := e.summary(t, e.site)
	if len(s.Breakdowns["event"]) != 1 {
		t.Fatalf("an events-only bucket was not rolled up: %+v", s.Breakdowns["event"])
	}
	if s.Pageviews != 0 {
		t.Fatalf("pageviews = %d, want 0", s.Pageviews)
	}
}

func TestCustomEvent_RejectsReservedAndMalformedNames(t *testing.T) {
	ctx, e := newEnv(t)
	for _, bad := range []string{"pageview", "has space", "emoji🎉", strings.Repeat("x", 100)} {
		if code := e.collectEvent(t, bad, nil, humanUA, "203.0.113.1"); code != http.StatusNoContent {
			t.Errorf("name %.20q: got %d, want 204 (uniform)", bad, code)
		}
	}
	var n int
	if err := e.tdb.Super.QueryRow(ctx, "SELECT count(*) FROM analytics_event").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d rows stored for rejected event names — 'pageview' via the event API would "+
			"let a site forge its own pageview count", n)
	}
}

func TestCustomEvent_BotsExcluded(t *testing.T) {
	ctx, e := newEnv(t)
	e.collectEvent(t, "grow_start", nil, humanUA, "203.0.113.1")
	e.collectEvent(t, "grow_start", nil, "Googlebot/2.1", "203.0.113.9")
	e.rollup(t, ctx)

	_, s := e.summary(t, e.site)
	ev := s.Breakdowns["event"]
	if len(ev) != 1 || ev[0].Pageviews != 1 {
		t.Fatalf("bot event counted: %+v", ev)
	}
}

// The migration runs as a pre-upgrade hook, BEFORE new pods roll — so during the rollout window
// the OLD handler is still issuing the 12-argument call. If that stopped resolving, every event in
// the window would be lost silently (collect answers 204 regardless).
func TestCustomEvent_OldTwelveArgCallStillResolves(t *testing.T) {
	ctx, e := newEnv(t)
	var n int
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT analytics_collect($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		e.key, "/legacy", "", "203.0.113.1", humanUA, false, "", "", "", "desktop", "Chrome", "",
	).Scan(&n); err != nil {
		t.Fatalf("the pre-0109 12-argument call no longer resolves — a rolling deploy would lose "+
			"every event until the old pods drain: %v", err)
	}
	if n != 1 {
		t.Fatalf("12-arg call stored %d rows, want 1", n)
	}
	var name string
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT name FROM analytics_event WHERE client_id=$1", e.site).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "pageview" {
		t.Errorf("legacy call stored name=%q, want the pageview default", name)
	}
}

// A bucket touched only by custom events must have its 'event' breakdown recomputed WITHOUT
// destroying the pageview breakdowns already computed for that same day.
func TestCustomEvent_EventOnlySweepPreservesPageviewBreakdowns(t *testing.T) {
	ctx, e := newEnv(t)
	// Day one: a pageview, rolled up. device/browser breakdowns now exist.
	e.collectFull(t, "/", "", "utm_source=hn", humanUA, "203.0.113.1")
	e.rollup(t, ctx)
	_, before := e.summary(t, e.site)
	if len(before.Breakdowns["device"]) == 0 || len(before.Breakdowns["utm_source"]) == 0 {
		t.Fatalf("expected pageview breakdowns after the first sweep: %+v", before.Breakdowns)
	}

	// Now ONLY a custom event arrives and the rollup runs again.
	e.collectEvent(t, "grow_start", nil, humanUA, "203.0.113.1")
	e.rollup(t, ctx)

	_, after := e.summary(t, e.site)
	if len(after.Breakdowns["event"]) != 1 {
		t.Fatalf("the event breakdown was not produced: %+v", after.Breakdowns["event"])
	}
	if len(after.Breakdowns["device"]) == 0 {
		t.Fatal("an events-only sweep destroyed the device breakdown — the delete must be scoped " +
			"to the dimensions actually being recomputed")
	}
	if len(after.Breakdowns["utm_source"]) == 0 {
		t.Fatal("an events-only sweep destroyed the utm_source breakdown")
	}
	if after.Pageviews != before.Pageviews {
		t.Fatalf("pageviews changed across an events-only sweep: %d -> %d",
			before.Pageviews, after.Pageviews)
	}
}

// ---------------------------------------------------------------------------
// manyforge-nk50 — cross-business overview
// ---------------------------------------------------------------------------

type overviewResp struct {
	DataAsOf *string `json:"data_as_of"`
	Sites    []struct {
		ClientID             string  `json:"client_id"`
		Name                 string  `json:"name"`
		BusinessID           string  `json:"business_id"`
		BusinessName         string  `json:"business_name"`
		Pageviews            int64   `json:"pageviews"`
		Visitors             int64   `json:"visitors"`
		AverageDailyVisitors float64 `json:"average_daily_visitors"`
		Series               []struct {
			Date      string `json:"date"`
			Pageviews int64  `json:"pageviews"`
		} `json:"series"`
	} `json:"sites"`
}

// addSubBusiness creates a child business under e.biz with its own analytics site, and returns the
// business id, the site id and the site's publishable key.
func (e *env) addSubBusiness(t *testing.T, ctx context.Context, name string) (uuid.UUID, uuid.UUID, string) {
	t.Helper()
	sub, site := uuid.New(), uuid.New()
	key := "mfk_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	for _, st := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at)
		  VALUES ($1,$2,$3,$4,'active',now(),now())`, []any{sub, e.biz, e.biz, name}},
		// Closure rows: self, plus the parent edge. Without the parent edge the child is invisible
		// to authorized_businesses and the whole point of the test evaporates.
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id)
		  VALUES ($1,$1,0,$2), ($2,$1,1,$2)`, []any{sub, e.biz}},
		{`INSERT INTO telemetry_client (id,tenant_root_id,business_id,kind,name,publishable_key,status,created_at)
		  VALUES ($1,$2,$3,'analytics',$4,$5,'active',now())`,
			[]any{site, e.biz, sub, name + " site", key}},
	} {
		if _, err := e.tdb.Super.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("seed sub-business: %v\n%s", err, st.sql)
		}
	}
	return sub, site, key
}

func (e *env) getOverview(t *testing.T, prin uuid.UUID) overviewResp {
	t.Helper()
	// The test server injects e.prin; for other principals, query as them directly through the
	// service so the principal actually varies.
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/analytics/overview?days=30", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("overview status %d: %s", resp.StatusCode, b)
	}
	var out overviewResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	return out
}

// The feature itself: an owner of the tenant root sees sites in BOTH the root and its
// sub-businesses in one response — that is the whole reason this endpoint exists.
func TestOverview_SpansSubBusinesses(t *testing.T) {
	ctx, e := newEnv(t)
	subID, subSite, _ := e.addSubBusiness(t, ctx, "SubCo")

	got := e.getOverview(t, e.prin)
	byID := map[string]string{}
	for _, s := range got.Sites {
		byID[s.ClientID] = s.BusinessName
	}
	if _, ok := byID[e.site.String()]; !ok {
		t.Errorf("root business site missing from overview; got %+v", byID)
	}
	if bn, ok := byID[subSite.String()]; !ok {
		t.Errorf("SUB-BUSINESS site missing — the endpoint is not spanning the business tree; got %+v", byID)
	} else if bn != "SubCo" {
		t.Errorf("sub-business name = %q, want SubCo", bn)
	}
	for _, s := range got.Sites {
		if s.ClientID == subSite.String() && s.BusinessID != subID.String() {
			t.Errorf("sub site reported under business %s, want %s", s.BusinessID, subID)
		}
	}
}

// A site registered a moment ago has no rollup rows. It must still appear, with zeroes — omitting
// it would read as "your tag is broken" at exactly the moment someone is checking whether it works.
func TestOverview_IncludesSitesWithNoTrafficYet(t *testing.T) {
	ctx, e := newEnv(t)
	_, quietSite, _ := e.addSubBusiness(t, ctx, "QuietCo")

	got := e.getOverview(t, e.prin)
	for _, s := range got.Sites {
		if s.ClientID == quietSite.String() {
			if s.Pageviews != 0 || s.Visitors != 0 || s.AverageDailyVisitors != 0 {
				t.Errorf("untrafficked site should report zeroes, got pv=%d peak=%d avg=%f",
					s.Pageviews, s.Visitors, s.AverageDailyVisitors)
			}
			if len(s.Series) != 30 {
				t.Errorf("series has %d points, want 30 zero-filled days", len(s.Series))
			}
			for _, point := range s.Series {
				if point.Pageviews != 0 {
					t.Errorf("untrafficked site has non-zero series point: %+v", point)
				}
			}
			return
		}
	}
	t.Error("a site with no traffic was omitted from the overview; a newly tagged site would look broken")
}

// THE isolation test. A principal who is a MEMBER of the business but whose role does not grant
// telemetry.read must see nothing. RLS alone would show them the sites, because membership is a
// weaker condition than permission — this is what businesses_with_permission() exists to prevent.
func TestOverview_ExcludesBusinessesWhereCallerLacksTelemetryRead(t *testing.T) {
	ctx, e := newEnv(t)
	e.addSubBusiness(t, ctx, "SubCo")

	// A role with SOME permission but not telemetry.read.
	weakRole, weakPrin, acct := uuid.New(), uuid.New(), uuid.New()
	for _, st := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at)
		  VALUES ($1,$2,'viewer','Viewer',false,now())`, []any{weakRole, e.biz}},
		{`INSERT INTO role_permission (role_id,permission_key) VALUES ($1,'feedback.read')`, []any{weakRole}},
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at)
		  VALUES ($1,$2,'W','active',now(),now(),now())`, []any{acct, "weak-" + weakPrin.String() + "@x.test"}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`, []any{weakPrin, acct}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at)
		  VALUES ($1,$2,$2,$3,now())`, []any{weakPrin, e.biz, weakRole}},
	} {
		if _, err := e.tdb.Super.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("seed weak principal: %v\n%s", err, st.sql)
		}
	}

	// Sanity: the weak principal IS a member, so RLS alone would let them see these rows.
	var visible int
	if err := e.tdb.App.WithPrincipal(ctx, weakPrin, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM telemetry_client WHERE kind='analytics'`).Scan(&visible)
	}); err != nil {
		t.Fatalf("rls probe: %v", err)
	}
	if visible == 0 {
		t.Fatal("test is vacuous: RLS already hides these rows, so it cannot demonstrate that the " +
			"PERMISSION filter is what excludes them")
	}

	sites, err := analytics.NewService(e.tdb.App).Overview(ctx, weakPrin,
		timeNow().AddDate(0, 0, -29), timeNow())
	if err != nil {
		t.Fatalf("overview as weak principal: %v", err)
	}
	if len(sites) != 0 {
		t.Errorf("PERMISSION BYPASS: a member without telemetry.read saw %d site(s) on the overview, "+
			"including ones the per-site dashboard would 404. RLS visibility is not permission.",
			len(sites))
	}
}

// Cross-tenant isolation: a principal in an unrelated tenant sees nothing at all.
func TestOverview_ExcludesOtherTenants(t *testing.T) {
	ctx, e := newEnv(t)
	e.addSubBusiness(t, ctx, "SubCo")

	var ownerRole uuid.UUID
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("owner role: %v", err)
	}
	otherBiz, otherPrin, acct := uuid.New(), uuid.New(), uuid.New()
	for _, st := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at)
		  VALUES ($1,$2,'X','active',now(),now(),now())`, []any{acct, "other-" + otherPrin.String() + "@x.test"}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`, []any{otherPrin, acct}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at)
		  VALUES ($1,NULL,$1,'OtherCo','active',now(),now())`, []any{otherBiz}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id)
		  VALUES ($1,$1,0,$1)`, []any{otherBiz}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at)
		  VALUES ($1,$2,$2,$3,now())`, []any{otherPrin, otherBiz, ownerRole}},
	} {
		if _, err := e.tdb.Super.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("seed other tenant: %v\n%s", err, st.sql)
		}
	}

	sites, err := analytics.NewService(e.tdb.App).Overview(ctx, otherPrin,
		timeNow().AddDate(0, 0, -29), timeNow())
	if err != nil {
		t.Fatalf("overview as other tenant: %v", err)
	}
	if len(sites) != 0 {
		t.Errorf("CROSS-TENANT LEAK: an unrelated tenant's owner saw %d site(s)", len(sites))
	}
}

// timeNow keeps the window construction in these tests identical to the handler's.
func timeNow() time.Time { return time.Now().UTC().Truncate(24 * time.Hour) }

// The route is mounted OUTSIDE the telemetryRead middleware group (that middleware resolves the
// permission from a business id in the path, which this route has none of), so the handler's own
// principal check is the only authentication gate. Every other overview test injects a principal,
// which means none of them execute this branch.
func TestOverview_RequiresAuthentication(t *testing.T) {
	ctx, e := newEnv(t)
	_ = ctx

	// A second server WITHOUT the principal-injecting middleware, i.e. an anonymous caller.
	rd := analytics.NewHandler(analytics.NewService(e.tdb.App))
	r := chi.NewRouter()
	rd.OverviewRoutes(r)
	anon := httptest.NewServer(r)
	t.Cleanup(anon.Close)

	resp, err := http.Get(anon.URL + "/analytics/overview?days=30")
	if err != nil {
		t.Fatalf("anonymous overview: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("anonymous request got %d, want 401 — the overview would be readable without "+
			"credentials. Body: %s", resp.StatusCode, b)
	}
	// And it must not leak a site list in the body.
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), "client_id") {
		t.Error("the 401 body contains site data")
	}
}

func TestOverview_RejectsInvalidDays(t *testing.T) {
	_, e := newEnv(t)
	for _, q := range []string{"days=0", "days=-1", "days=abc", "days=367", "days=99999"} {
		resp, err := http.Get(e.srv.URL + "/analytics/overview?" + q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		code := resp.StatusCode
		resp.Body.Close()
		if code != http.StatusBadRequest {
			t.Errorf("%s got %d, want 400 — an unbounded window would let one request scan the "+
				"whole rollup table", q, code)
		}
	}
	// The cap itself must still be accepted, or the documented maximum is a lie.
	resp, err := http.Get(e.srv.URL + "/analytics/overview?days=366")
	if err != nil {
		t.Fatalf("days=366: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("days=366 (the documented cap) got %d, want 200", resp.StatusCode)
	}
}

// Aggregation semantics: pageviews SUM, visitors MAX, and the window actually excludes rows
// outside it. The MAX is the load-bearing one — a SUM here would make every card disagree with
// the dashboard it opens.
func TestOverview_AggregatesAndRespectsWindow(t *testing.T) {
	ctx, e := newEnv(t)

	// Three days inside the window and one well outside it.
	for _, row := range []struct {
		daysAgo, pv, vis int
	}{{1, 10, 4}, {2, 20, 9}, {3, 5, 2}, {200, 999, 999}} {
		if _, err := e.tdb.Super.Exec(ctx,
			`INSERT INTO analytics_daily
			   (tenant_root_id, business_id, client_id, bucket_date, pageviews, visitors, updated_at)
			 VALUES ($1,$2,$3, (now() AT TIME ZONE 'UTC')::date - $4::int, $5, $6, now())`,
			e.biz, e.biz, e.site, row.daysAgo, row.pv, row.vis); err != nil {
			t.Fatalf("seed daily: %v", err)
		}
	}

	sites, err := analytics.NewService(e.tdb.App).Overview(ctx, e.prin,
		timeNow().AddDate(0, 0, -29), timeNow())
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	var got *analytics.OverviewSite
	for i := range sites {
		if sites[i].ClientID == e.site.String() {
			got = &sites[i]
		}
	}
	if got == nil {
		t.Fatal("seeded site missing from overview")
	}
	if got.Pageviews != 35 {
		t.Errorf("pageviews = %d, want 35 (10+20+5; the 200-days-ago row is outside the window)",
			got.Pageviews)
	}
	if got.Visitors != 9 {
		t.Errorf("visitors = %d, want 9 — the PEAK day, not the sum (15) and not the out-of-window "+
			"999. Summing would make this card disagree with the dashboard it opens.", got.Visitors)
	}
	if math.Abs(got.AverageDailyVisitors-0.5) > 0.0001 {
		t.Errorf("average daily visitors = %f, want 0.5 (15 visitor-days / 30 calendar days)",
			got.AverageDailyVisitors)
	}
	if len(got.Series) != 30 {
		t.Errorf("series has %d points, want 30 — every day in the window must be represented", len(got.Series))
	}
	if got.Series[0].Date != timeNow().AddDate(0, 0, -29).Format("2006-01-02") ||
		got.Series[29].Date != timeNow().Format("2006-01-02") {
		t.Errorf("series boundaries = %s..%s", got.Series[0].Date, got.Series[29].Date)
	}
	if got.Series[26].Pageviews != 5 || got.Series[27].Pageviews != 20 ||
		got.Series[28].Pageviews != 10 || got.Series[29].Pageviews != 0 {
		t.Errorf("overview series does not preserve calendar gaps: %+v", got.Series[26:30])
	}
}

// Revoked and inactive sites must not appear: a revoked key has stopped collecting, and listing it
// implies it is still live.
func TestOverview_ExcludesRevokedSites(t *testing.T) {
	ctx, e := newEnv(t)
	_, revoked, _ := e.addSubBusiness(t, ctx, "RevokedCo")
	_, halfRevoked, _ := e.addSubBusiness(t, ctx, "HalfRevokedCo")

	if _, err := e.tdb.Super.Exec(ctx,
		`UPDATE telemetry_client SET status='revoked', revoked_at=now() WHERE id=$1`, revoked); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// status still 'active' but revoked_at set. The CHECK constraint permits only 'active' and
	// 'revoked', so this inconsistent pair is the ONLY thing the second predicate can catch — and
	// it is what makes `AND c.revoked_at IS NULL` more than a redundant restatement of the status.
	if _, err := e.tdb.Super.Exec(ctx,
		`UPDATE telemetry_client SET revoked_at=now() WHERE id=$1`, halfRevoked); err != nil {
		t.Fatalf("half-revoke: %v", err)
	}

	sites, err := analytics.NewService(e.tdb.App).Overview(ctx, e.prin,
		timeNow().AddDate(0, 0, -29), timeNow())
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	for _, s := range sites {
		if s.ClientID == revoked.String() {
			t.Error("a REVOKED site is listed on the overview; its tag has stopped collecting")
		}
		if s.ClientID == halfRevoked.String() {
			t.Error("a site with revoked_at set is listed despite status still reading 'active'; " +
				"the revoked_at predicate is not doing its job")
		}
	}
}

// Ordering drives which sites survive the 200-site cap, so it must be the metric the contract and
// the card headline both name: average daily visitors.
func TestOverview_OrdersByAverageDailyVisitors(t *testing.T) {
	ctx, e := newEnv(t)
	_, many, _ := e.addSubBusiness(t, ctx, "ManyPageviews")
	_, few, _ := e.addSubBusiness(t, ctx, "ManyVisitors")

	// `many` wins on pageviews and peak visitors (90 on one day). `few` wins on average daily
	// visitors (50 on two days). Ordering by either old metric puts them the wrong way round.
	for _, row := range []struct {
		client           uuid.UUID
		daysAgo, pv, vis int
	}{{many, 1, 5000, 90}, {few, 1, 5, 50}, {few, 2, 5, 50}} {
		if _, err := e.tdb.Super.Exec(ctx,
			`INSERT INTO analytics_daily
			   (tenant_root_id, business_id, client_id, bucket_date, pageviews, visitors, updated_at)
			 SELECT c.tenant_root_id, c.business_id, c.id,
				        (now() AT TIME ZONE 'UTC')::date - $2::int, $3, $4, now()
			   FROM telemetry_client c WHERE c.id = $1`,
			row.client, row.daysAgo, row.pv, row.vis); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	sites, err := analytics.NewService(e.tdb.App).Overview(ctx, e.prin,
		timeNow().AddDate(0, 0, -29), timeNow())
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	var iMany, iFew = -1, -1
	for i, s := range sites {
		switch s.ClientID {
		case many.String():
			iMany = i
		case few.String():
			iFew = i
		}
	}
	if iFew < 0 || iMany < 0 {
		t.Fatalf("seeded sites missing (few=%d many=%d)", iFew, iMany)
	}
	if iFew > iMany {
		t.Errorf("site with the higher daily average ranked below a bursty site with higher peak and " +
			"pageviews; the 200-site cap would shed the wrong site")
	}
}
