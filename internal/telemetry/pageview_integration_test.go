//go:build integration

package telemetry_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/analytics"
	"github.com/manyforge/manyforge/internal/platform/timeseries"
	"github.com/manyforge/manyforge/internal/telemetry"
)

func signedPageviewBody(id uuid.UUID, path string) map[string]any {
	return map[string]any{"analytics": []map[string]any{{
		"occurred_at": time.Now().UTC().Format(time.RFC3339Nano), "name": "pageview",
		"path": path, "session_id": id.String(),
		"props": map[string]any{"session": id.String(), "email": "private@example.test"},
	}}}
}

func postSignedPageviews(t *testing.T, e *env, key, secret string, body any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	target := "/api/v1/telemetry/ingest/" + key
	ts := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.%s.%s.", ts, http.MethodPost, target)
	_, _ = mac.Write(raw)
	return post(t, e.srv, key, body, map[string]string{
		"X-Telemetry-Signature": fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil))),
	})
}

func TestIngest_SignedPageviewsPersistCanonicalReportingData(t *testing.T) {
	ctx, e := newEnv(t)
	seed := seedTenant(t, ctx, e.tdb, "Pageviews")
	client, err := telemetry.NewService(e.tdb.App, e.sealer).CreateClient(
		ctx, seed.principalID, seed.businessID, telemetry.KindAnalytics, "signed", true, testAllowedOrigins,
	)
	if err != nil {
		t.Fatalf("CreateClient: %v", err)
	}
	first, second := uuid.New(), uuid.New()
	body := map[string]any{"analytics": []map[string]any{
		{"occurred_at": time.Now().UTC(), "name": "pageview", "path": "/servers/:id", "session_id": first.String(), "props": map[string]any{"session": first.String(), "email": "private@example.test"}},
		{"occurred_at": time.Now().UTC(), "name": "pageview", "path": "/servers/:id", "session_id": first.String()},
		{"occurred_at": time.Now().UTC(), "name": "pageview", "path": "/support", "session_id": second.String()},
	}}
	if code, out := postSignedPageviews(t, e, client.PublishableKey, client.Secret, body); code != http.StatusAccepted {
		t.Fatalf("ingest: %d %s", code, out)
	}
	rows, err := e.tdb.Super.Query(ctx,
		"SELECT path, visitor_hash, props::text, to_jsonb(ev)::text FROM analytics_event ev WHERE client_id=$1", client.ID)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for rows.Next() {
		var path, props, event string
		var hash []byte
		if err := rows.Scan(&path, &hash, &props, &event); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if (path != "/servers/:id" && path != "/support") || len(hash) != 16 || props != "{}" {
			rows.Close()
			t.Fatalf("invalid stored pageview: path=%q hash length=%d props=%s", path, len(hash), props)
		}
		for _, raw := range []string{first.String(), second.String(), "private@example.test", "Go-http-client", "127.0.0.1"} {
			if strings.Contains(event, raw) {
				rows.Close()
				t.Fatalf("raw browser/session data persisted: %q", raw)
			}
		}
		count++
	}
	err = rows.Err()
	rows.Close()
	if err != nil || count != 3 {
		t.Fatalf("rows=%d error=%v, want 3", count, err)
	}
	if _, err := (&timeseries.RollupWorker{DB: e.tdb.App, Lag: -1}).SweepOnce(ctx); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	summary, err := analytics.NewService(e.tdb.App).Summary(ctx, seed.principalID, seed.businessID, client.ID, today, today)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Pageviews != 3 || summary.Visitors != 2 || len(summary.TopPages) != 2 {
		t.Fatalf("summary=%+v, want 3 pageviews, 2 visitors, 2 canonical pages", summary)
	}
	pages := make(map[string]int64)
	for _, page := range summary.TopPages {
		pages[page.Path] = page.Pageviews
	}
	if pages["/servers/:id"] != 2 || pages["/support"] != 1 {
		t.Fatalf("top pages=%v, want canonical routes rather than '/'", pages)
	}
}

func TestIngest_EnhancedPageviewRequiresVerifiedSignature(t *testing.T) {
	ctx, e := newEnv(t)
	clientID, key := e.insertClient(t, ctx, telemetry.KindAnalytics, "active", nil)
	body := signedPageviewBody(uuid.New(), "/support")
	code, out := post(t, e.srv, key, body, nil)
	var result struct{ Accepted, Dropped int }
	if code != http.StatusAccepted || json.Unmarshal([]byte(out), &result) != nil || result.Accepted != 0 || result.Dropped != 1 {
		t.Fatalf("unsigned enhanced request status=%d body=%s, want accepted=0 dropped=1", code, out)
	}
	var n int
	if err := e.tdb.Super.QueryRow(ctx, "SELECT count(*) FROM analytics_event WHERE client_id=$1", clientID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unsigned enhanced rows=%d error=%v", n, err)
	}
	if code, out := post(t, e.srv, key, analyticsBody(1), nil); code != http.StatusAccepted {
		t.Fatalf("legacy request status=%d body=%s", code, out)
	}
	if err := e.tdb.Super.QueryRow(ctx, "SELECT count(*) FROM analytics_event WHERE client_id=$1", clientID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("legacy row count=%d error=%v, want 1", n, err)
	}
	sealed, err := e.sealer.Seal([]byte("mfs_test-signing-secret"))
	if err != nil {
		t.Fatal(err)
	}
	signedID, signedKey := e.insertClient(t, ctx, telemetry.KindAnalytics, "active", &sealed)
	if code, out := postSignedPageviews(t, e, signedKey, "mfs_wrong-secret", body); code != http.StatusUnauthorized {
		t.Fatalf("bad signature status=%d body=%s", code, out)
	}
	if err := e.tdb.Super.QueryRow(ctx, "SELECT count(*) FROM analytics_event WHERE client_id=$1", signedID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("bad signature rows=%d error=%v", n, err)
	}
}

func TestIngest_SignedSessionHashIsScopedAndDailySecretBound(t *testing.T) {
	ctx, e := newEnv(t)
	seed := seedTenant(t, ctx, e.tdb, "Session scope")
	svc := telemetry.NewService(e.tdb.App, e.sealer)
	first, err := svc.CreateClient(ctx, seed.principalID, seed.businessID, telemetry.KindAnalytics, "first", true, testAllowedOrigins)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CreateClient(ctx, seed.principalID, seed.businessID, telemetry.KindAnalytics, "second", true, testAllowedOrigins)
	if err != nil {
		t.Fatal(err)
	}
	session := uuid.New()
	body := signedPageviewBody(session, "/support")
	for _, client := range []telemetry.Client{first, second} {
		if code, out := postSignedPageviews(t, e, client.PublishableKey, client.Secret, body); code != http.StatusAccepted {
			t.Fatalf("signed request: %d %s", code, out)
		}
	}
	var firstHash, secondHash, salt []byte
	if err := e.tdb.Super.QueryRow(ctx, "SELECT visitor_hash FROM analytics_event WHERE client_id=$1", first.ID).Scan(&firstHash); err != nil {
		t.Fatal(err)
	}
	if err := e.tdb.Super.QueryRow(ctx, "SELECT visitor_hash FROM analytics_event WHERE client_id=$1", second.ID).Scan(&secondHash); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstHash, secondHash) {
		t.Fatal("same tab session is correlatable across clients")
	}
	var day string
	if err := e.tdb.Super.QueryRow(ctx, "SELECT day::text,salt FROM analytics_salt WHERE day=(now() AT TIME ZONE 'UTC')::date").Scan(&day, &salt); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, salt)
	_, _ = fmt.Fprintf(mac, "manyforge:analytics:tab-session:v1:%s:%s:%s:%s:%s", first.TenantRootID, first.BusinessID, first.ID, day, session)
	if want := mac.Sum(nil)[:16]; !bytes.Equal(firstHash, want) {
		t.Fatal("visitor hash does not bind tenant, business, client, day and transient session with domain-separated HMAC")
	}
	if _, err := e.tdb.Super.Exec(ctx, "UPDATE analytics_salt SET salt=gen_random_bytes(32) WHERE day=(now() AT TIME ZONE 'UTC')::date"); err != nil {
		t.Fatal(err)
	}
	if code, out := postSignedPageviews(t, e, first.PublishableKey, first.Secret, body); code != http.StatusAccepted {
		t.Fatalf("rotated-salt request: %d %s", code, out)
	}
	var distinct int
	if err := e.tdb.Super.QueryRow(ctx, "SELECT count(DISTINCT visitor_hash) FROM analytics_event WHERE client_id=$1", first.ID).Scan(&distinct); err != nil || distinct != 2 {
		t.Fatalf("distinct visitor hashes=%d error=%v, want 2 after daily secret change", distinct, err)
	}
}
