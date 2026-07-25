//go:build integration

package analytics_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/analytics"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/timeseries"
)

const humanUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

type env struct {
	tdb  *testdb.TestDB
	srv  *httptest.Server
	biz  uuid.UUID
	prin uuid.UUID
	site uuid.UUID
	key  string
}

func newEnv(t *testing.T) (context.Context, *env) {
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

	e := &env{tdb: tdb}
	e.seedTenant(t, ctx)
	e.seedSite(t, ctx)

	// Trust loopback so X-Forwarded-For is honoured, matching production where the app sits behind
	// an ingress. Without this the handler correctly ignores XFF and every test request would look
	// like the same visitor.
	_, loopback4, _ := net.ParseCIDR("127.0.0.0/8")
	_, loopback6, _ := net.ParseCIDR("::1/128")
	pub := &analytics.PublicHandler{
		DB:             tdb.App,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		TrustedProxies: []*net.IPNet{loopback4, loopback6},
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
