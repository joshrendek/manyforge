//go:build integration

package telemetry_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/timeseries"
	"github.com/manyforge/manyforge/internal/telemetry"
)

type env struct {
	tdb    *testdb.TestDB
	srv    *httptest.Server
	sealer *crypto.Sealer
}

// newEnv brings up a migrated DB, creates today's partitions, and mounts the public ingest handler
// under /api/v1 so signed requests verify against a realistic request-target.
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

	sealer, err := crypto.NewSealer(bytes.Repeat([]byte{0x2a}, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	h := &telemetry.PublicHandler{
		DB:     tdb.App,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sealer: sealer,
	}
	r := chi.NewRouter()
	r.Route("/api/v1", func(api chi.Router) { h.PublicRoutes(api) })
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	return ctx, &env{tdb: tdb, srv: srv, sealer: sealer}
}

// insertClient seeds a telemetry_client directly (RLS-exempt) and returns its publishable key.
func (e *env) insertClient(t *testing.T, ctx context.Context, kind, status string, sealed *string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	key := "mfk_" + randKeyBody(t)
	tenant, business := uuid.New(), uuid.New()
	var revokedAt any
	if status == "revoked" {
		revokedAt = time.Now().UTC()
	}
	if _, err := e.tdb.Super.Exec(ctx,
		`INSERT INTO telemetry_client
		   (id, business_id, tenant_root_id, kind, name, publishable_key, sealed_secret, status, revoked_at)
		 VALUES ($1,$2,$3,$4,'test',$5,$6,$7,$8)`,
		id, business, tenant, kind, key, sealed, status, revokedAt); err != nil {
		t.Fatalf("insert client: %v", err)
	}
	return id, key
}

func randKeyBody(t *testing.T) string {
	t.Helper()
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func post(t *testing.T, srv *httptest.Server, key string, body any, hdrs map[string]string) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/telemetry/ingest/"+key, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func analyticsBody(n int) map[string]any {
	events := make([]map[string]any, n)
	for i := range events {
		events[i] = map[string]any{
			"occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
			"name":        "pageview",
		}
	}
	return map[string]any{"analytics": events}
}

// ---------------------------------------------------------------------------

func TestIngest_HappyPath(t *testing.T) {
	ctx, e := newEnv(t)
	clientID, key := e.insertClient(t, ctx, "analytics", "active", nil)

	code, body := post(t, e.srv, key, analyticsBody(3), nil)
	if code != http.StatusAccepted {
		t.Fatalf("status %d body %s", code, body)
	}

	var n int
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM analytics_event WHERE client_id=$1", clientID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 rows persisted, got %d", n)
	}
}

// The central oracle property: nothing distinguishes "never existed" from "revoked".
func TestIngest_UnknownAndRevokedKeysAreIndistinguishable(t *testing.T) {
	ctx, e := newEnv(t)
	_, revokedKey := e.insertClient(t, ctx, "analytics", "revoked", nil)
	unknownKey := "mfk_" + randKeyBody(t)

	unkCode, unkBody := post(t, e.srv, unknownKey, analyticsBody(1), nil)
	revCode, revBody := post(t, e.srv, revokedKey, analyticsBody(1), nil)

	if unkCode != http.StatusUnauthorized {
		t.Fatalf("unknown key: expected 401, got %d (%s)", unkCode, unkBody)
	}
	if revCode != unkCode || revBody != unkBody {
		t.Fatalf("CLIENT-EXISTENCE ORACLE: unknown=(%d,%s) revoked=(%d,%s)",
			unkCode, unkBody, revCode, revBody)
	}

	// A malformed key must also be indistinguishable.
	malCode, malBody := post(t, e.srv, "mfk_short", analyticsBody(1), nil)
	if malCode != unkCode || malBody != unkBody {
		t.Fatalf("KEY-SHAPE ORACLE: malformed=(%d,%s) unknown=(%d,%s)",
			malCode, malBody, unkCode, unkBody)
	}
}

// A hostile occurred_at must not steer which partition a row lands in — partitions are keyed on
// ingested_at, a server clock the client cannot reach.
func TestIngest_ClientTimeCannotSteerPartition(t *testing.T) {
	ctx, e := newEnv(t)
	clientID, key := e.insertClient(t, ctx, "analytics", "active", nil)

	body := map[string]any{"analytics": []map[string]any{{
		// Inside the accepted window, but far from now.
		"occurred_at": time.Now().UTC().Add(-6 * 24 * time.Hour).Format(time.RFC3339Nano),
		"name":        "pageview",
	}}}
	if code, b := post(t, e.srv, key, body, nil); code != http.StatusAccepted {
		t.Fatalf("status %d body %s", code, b)
	}

	var ingested, occurred time.Time
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT ingested_at, occurred_at FROM analytics_event WHERE client_id=$1", clientID,
	).Scan(&ingested, &occurred); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if time.Since(ingested) > time.Minute {
		t.Fatalf("ingested_at was influenced by the client: %v", ingested)
	}
	// The row must live in today's partition regardless of its occurred_at.
	today := fmt.Sprintf("analytics_event_%s", time.Now().UTC().Format("20060102"))
	var inToday int
	if err := e.tdb.Super.QueryRow(ctx,
		fmt.Sprintf("SELECT count(*) FROM %s WHERE client_id=$1", today), clientID).Scan(&inToday); err != nil {
		t.Fatalf("read partition: %v", err)
	}
	if inToday != 1 {
		t.Fatalf("row did not land in today's partition (%s)", today)
	}
}

func TestIngest_DropsStaleEventsButKeepsTheBatch(t *testing.T) {
	ctx, e := newEnv(t)
	clientID, key := e.insertClient(t, ctx, "analytics", "active", nil)

	body := map[string]any{"analytics": []map[string]any{
		{"occurred_at": time.Now().UTC().Format(time.RFC3339Nano), "name": "good"},
		{"occurred_at": time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano), "name": "ancient"},
	}}
	code, respBody := post(t, e.srv, key, body, nil)
	if code != http.StatusAccepted {
		t.Fatalf("status %d body %s", code, respBody)
	}
	var resp struct{ Accepted, Dropped int }
	if err := json.Unmarshal([]byte(respBody), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Accepted != 1 || resp.Dropped != 1 {
		t.Fatalf("expected 1 accepted / 1 dropped, got %d/%d", resp.Accepted, resp.Dropped)
	}
	var n int
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM analytics_event WHERE client_id=$1", clientID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 persisted, got %d", n)
	}
}

func TestIngest_BodyCapRejectsOversizedBatch(t *testing.T) {
	ctx, e := newEnv(t)
	_, key := e.insertClient(t, ctx, "analytics", "active", nil)

	// ~1 MiB of props, well past the 256 KiB cap.
	big := map[string]any{"analytics": []map[string]any{{
		"occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
		"name":        "pageview",
		"props":       map[string]string{"blob": strings.Repeat("x", 1<<20)},
	}}}
	code, _ := post(t, e.srv, key, big, nil)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", code)
	}
}

// A client that carries a signing secret must fail closed without a valid signature.
func TestIngest_SignedClientRequiresValidSignature(t *testing.T) {
	ctx, e := newEnv(t)
	const secret = "mfs_integrationsecret"
	sealed, err := e.sealer.Seal([]byte(secret))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	_, key := e.insertClient(t, ctx, "analytics", "active", &sealed)

	body := analyticsBody(1)
	raw, _ := json.Marshal(body)
	target := "/api/v1/telemetry/ingest/" + key

	// Unsigned → 401.
	if code, _ := post(t, e.srv, key, body, nil); code != http.StatusUnauthorized {
		t.Fatalf("unsigned request to a signed client: expected 401, got %d", code)
	}

	// Correctly signed → 202.
	ts := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.%s.%s.", ts, http.MethodPost, target)))
	mac.Write(raw)
	sig := fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))

	code, respBody := post(t, e.srv, key, body, map[string]string{"X-Telemetry-Signature": sig})
	if code != http.StatusAccepted {
		t.Fatalf("signed request: expected 202, got %d (%s)", code, respBody)
	}
}

func TestIngest_CrashKindWritesCrashTable(t *testing.T) {
	ctx, e := newEnv(t)
	clientID, key := e.insertClient(t, ctx, "crash", "active", nil)

	body := map[string]any{"crash": []map[string]any{{
		"occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
		"platform":    "ios",
		"signature":   "SIGSEGV@main",
		"app_version": "1.2.3",
	}}}
	if code, b := post(t, e.srv, key, body, nil); code != http.StatusAccepted {
		t.Fatalf("status %d body %s", code, b)
	}
	var n int
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM crash_event WHERE client_id=$1", clientID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 crash row, got %d", n)
	}
}

// An analytics key must not be usable to write crash rows, and vice versa: the target table is
// chosen by the SERVER from the registered kind, never by the request body.
func TestIngest_KindIsServerChosenNotBodyChosen(t *testing.T) {
	ctx, e := newEnv(t)
	clientID, key := e.insertClient(t, ctx, "analytics", "active", nil)

	// Body claims crash events; the client is registered as analytics.
	body := map[string]any{"crash": []map[string]any{{
		"occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
		"platform":    "ios",
		"signature":   "SIGSEGV@main",
	}}}
	if code, b := post(t, e.srv, key, body, nil); code != http.StatusAccepted {
		t.Fatalf("status %d body %s", code, b)
	}
	var crashRows int
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM crash_event WHERE client_id=$1", clientID).Scan(&crashRows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if crashRows != 0 {
		t.Fatalf("analytics key wrote %d crash rows; kind is body-steerable", crashRows)
	}
}

// Tenant scope must come from the resolved key, not from anything the caller can set.
func TestIngest_TenantScopeComesFromKey(t *testing.T) {
	ctx, e := newEnv(t)
	clientID, key := e.insertClient(t, ctx, "analytics", "active", nil)

	var wantTenant, wantBusiness uuid.UUID
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT tenant_root_id, business_id FROM telemetry_client WHERE id=$1", clientID,
	).Scan(&wantTenant, &wantBusiness); err != nil {
		t.Fatalf("read client: %v", err)
	}

	// Body attempts to declare a different tenant/business.
	body := map[string]any{"analytics": []map[string]any{{
		"occurred_at":    time.Now().UTC().Format(time.RFC3339Nano),
		"name":           "pageview",
		"tenant_root_id": uuid.New().String(),
		"business_id":    uuid.New().String(),
	}}}
	if code, b := post(t, e.srv, key, body, nil); code != http.StatusAccepted {
		t.Fatalf("status %d body %s", code, b)
	}

	var gotTenant, gotBusiness uuid.UUID
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT tenant_root_id, business_id FROM analytics_event WHERE client_id=$1", clientID,
	).Scan(&gotTenant, &gotBusiness); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if gotTenant != wantTenant || gotBusiness != wantBusiness {
		t.Fatalf("tenant scope was body-steerable: got (%s,%s) want (%s,%s)",
			gotTenant, gotBusiness, wantTenant, wantBusiness)
	}
}
