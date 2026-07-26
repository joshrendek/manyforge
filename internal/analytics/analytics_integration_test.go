//go:build integration

package analytics_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// collectFull posts a pageview including the campaign parameters the snippet extracts.
func (e *env) collectFull(t *testing.T, path, ref, query, ua, ip string) int {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"k": e.key, "p": path, "r": ref, "q": query})
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/a/e", bytes.NewReader(b))
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("X-Forwarded-For", ip)
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
	e.collectFull(t, "/pricing", "", "utm_source=hn&utm_medium=social&utm_campaign=launch&token=SECRET",
		humanUA, "203.0.113.10")

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
	// No geo database is configured in tests, so country must be NULL rather than a guess.
	if country != nil {
		t.Errorf("country = %v, want NULL with no geo db configured", *country)
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
		t.Error("country key should be present (empty) even with no geo database")
	}
	if len(s.Breakdowns["country"]) != 0 {
		t.Errorf("country should be empty with no geo db, got %+v", s.Breakdowns["country"])
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
	e.collectEvent(t, "grow_start", map[string]any{"level": 3, "mode": "classic"}, humanUA, "203.0.113.1")

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
	Sites []struct {
		ClientID     string `json:"client_id"`
		Name         string `json:"name"`
		BusinessID   string `json:"business_id"`
		BusinessName string `json:"business_name"`
		Pageviews    int64  `json:"pageviews"`
		Visitors     int64  `json:"visitors"`
		Series       []struct {
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
			if s.Pageviews != 0 || s.Visitors != 0 {
				t.Errorf("untrafficked site should report zeroes, got pv=%d v=%d", s.Pageviews, s.Visitors)
			}
			if s.Series == nil {
				t.Error("series must be an empty array, not null — the UI iterates it")
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
