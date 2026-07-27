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
	"errors"
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

	"github.com/manyforge/manyforge/internal/analytics"
	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
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
		   (id, business_id, tenant_root_id, kind, name, publishable_key,
		    require_signature, sealed_secret, status, revoked_at)
		 VALUES ($1,$2,$3,$4,'test',$5,$6,$7,$8,$9)`,
		// A seeded client that carries a secret is, by construction, a signing client.
		id, business, tenant, kind, key, sealed != nil, sealed, status, revokedAt); err != nil {
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
	clientID, key := e.insertClient(t, ctx, "analytics", "active", nil)

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
	// A regression that wrote before detecting the body-limit error would still return 413, so
	// assert the absence of side effects rather than just the status.
	var n int
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM analytics_event WHERE client_id=$1", clientID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("over-cap request persisted %d events", n)
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

// ---------------------------------------------------------------------------
// Authenticated client lifecycle
//
// The public-ingest tests above seed telemetry_client rows directly, so they never exercise the
// permission-gated minting/revocation path: write-once secret exposure, tenant isolation, and the
// sibling-business revoke defense all live here.
// ---------------------------------------------------------------------------

type tenantSeed struct {
	businessID  uuid.UUID
	principalID uuid.UUID
}

// seedTenant creates account → principal → tenant-root business → closure self-row → owner
// membership, so authorized_businesses(current_principal()) returns this business and
// db.WithPrincipal authorizes it. Runs as the RLS-exempt superuser.
func seedTenant(t *testing.T, ctx context.Context, tdb *testdb.TestDB, name string) tenantSeed {
	t.Helper()
	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("preset owner role: %v", err)
	}
	s := tenantSeed{businessID: uuid.New(), principalID: uuid.New()}
	acctID := uuid.New()
	email := "tel-owner-" + s.businessID.String() + "@x.test"

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Owner','active',now(),now(),now())`,
			[]any{acctID, email}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`,
			[]any{s.principalID, acctID}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,$2,'active',now(),now())`,
			[]any{s.businessID, name}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`,
			[]any{s.businessID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{s.principalID, s.businessID, ownerRole}},
	}
	for _, st := range stmts {
		if _, err := tdb.Super.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("seed exec: %v\nSQL: %s", err, st.sql)
		}
	}
	return s
}

func seedChildBusiness(t *testing.T, ctx context.Context, tdb *testdb.TestDB, parent, tenantRoot uuid.UUID, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at)
		 VALUES ($1,$2,$3,$4,'active',now(),now())`,
		id, parent, tenantRoot, name); err != nil {
		t.Fatalf("insert child business: %v", err)
	}
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id)
		 VALUES ($1,$1,0,$2), ($3,$1,1,$2)`,
		id, tenantRoot, parent); err != nil {
		t.Fatalf("insert child closure: %v", err)
	}
	return id
}

func TestClientLifecycle_CreateListRevoke(t *testing.T) {
	ctx, e := newEnv(t)
	seed := seedTenant(t, ctx, e.tdb, "TelCo")
	svc := telemetry.NewService(e.tdb.App, e.sealer)

	created, err := svc.CreateClient(ctx, seed.principalID, seed.businessID, "analytics", "web", false)
	if err != nil {
		t.Fatalf("CreateClient: %v", err)
	}
	if !strings.HasPrefix(created.PublishableKey, "mfk_") {
		t.Fatalf("bad publishable key: %q", created.PublishableKey)
	}
	// The DEFAULT client is the embeddable-SDK shape: no secret, no signature demanded. Minting a
	// secret for every client would force embeddable keys into signed mode.
	if created.Secret != "" || created.HasSecret || created.RequireSignature {
		t.Fatalf("default client should carry no signing secret: secret=%q hasSecret=%v require=%v",
			created.Secret, created.HasSecret, created.RequireSignature)
	}

	// Write-once: the plaintext secret must never appear again on any other path.
	list, err := svc.ListClients(ctx, seed.principalID, seed.businessID, 50)
	if err != nil {
		t.Fatalf("ListClients: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 client, got %d", len(list))
	}
	if list[0].Secret != "" {
		t.Fatal("SECRET LEAK: ListClients returned the plaintext signing secret")
	}

	revoked, err := svc.RevokeClient(ctx, seed.principalID, seed.businessID, created.ID)
	if err != nil {
		t.Fatalf("RevokeClient: %v", err)
	}
	if revoked.Status != "revoked" || revoked.RevokedAt == nil {
		t.Fatalf("client not marked revoked: status=%q revokedAt=%v", revoked.Status, revoked.RevokedAt)
	}
	if revoked.Secret != "" {
		t.Fatal("SECRET LEAK: RevokeClient returned the plaintext signing secret")
	}

	// A revoked key must stop ingesting immediately.
	if code, _ := post(t, e.srv, created.PublishableKey, analyticsBody(1), nil); code != http.StatusUnauthorized {
		t.Fatalf("revoked key still ingesting: got %d", code)
	}
}

func TestClientLifecycle_RevokeIsIdempotentAndNotAnOracle(t *testing.T) {
	ctx, e := newEnv(t)
	seed := seedTenant(t, ctx, e.tdb, "TelCo")
	svc := telemetry.NewService(e.tdb.App, e.sealer)

	created, err := svc.CreateClient(ctx, seed.principalID, seed.businessID, "analytics", "web", false)
	if err != nil {
		t.Fatalf("CreateClient: %v", err)
	}
	if _, err := svc.RevokeClient(ctx, seed.principalID, seed.businessID, created.ID); err != nil {
		t.Fatalf("first revoke: %v", err)
	}

	// Already-revoked and never-existed must be the same error — otherwise the endpoint confirms
	// which client UUIDs are real.
	_, secondErr := svc.RevokeClient(ctx, seed.principalID, seed.businessID, created.ID)
	_, unknownErr := svc.RevokeClient(ctx, seed.principalID, seed.businessID, uuid.New())
	if !errors.Is(secondErr, errs.ErrNotFound) {
		t.Fatalf("re-revoke should be ErrNotFound, got %v", secondErr)
	}
	if !errors.Is(unknownErr, errs.ErrNotFound) {
		t.Fatalf("unknown id should be ErrNotFound, got %v", unknownErr)
	}
}

// A principal must not see, or be able to revoke, another business's clients.
func TestClientLifecycle_TenantIsolation(t *testing.T) {
	ctx, e := newEnv(t)
	a := seedTenant(t, ctx, e.tdb, "AlphaCo")
	b := seedTenant(t, ctx, e.tdb, "BetaCo")
	svc := telemetry.NewService(e.tdb.App, e.sealer)

	aClient, err := svc.CreateClient(ctx, a.principalID, a.businessID, "analytics", "alpha-web", false)
	if err != nil {
		t.Fatalf("CreateClient(alpha): %v", err)
	}

	// B lists its own business: must not see A's client.
	bList, err := svc.ListClients(ctx, b.principalID, b.businessID, 50)
	if err != nil {
		t.Fatalf("ListClients(beta): %v", err)
	}
	if len(bList) != 0 {
		t.Fatalf("CROSS-BUSINESS LEAK: beta sees %d of alpha's clients", len(bList))
	}

	// B tries to list A's business directly: RLS denies, so this must not succeed with rows.
	crossList, err := svc.ListClients(ctx, b.principalID, a.businessID, 50)
	if err == nil && len(crossList) > 0 {
		t.Fatalf("CROSS-BUSINESS LEAK: beta listed %d clients under alpha's business", len(crossList))
	}

	// B tries to revoke A's client under B's own business id — the sibling-business defense.
	if _, err := svc.RevokeClient(ctx, b.principalID, b.businessID, aClient.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("beta revoking alpha's client should be ErrNotFound, got %v", err)
	}

	// A's key still works.
	if code, _ := post(t, e.srv, aClient.PublishableKey, analyticsBody(1), nil); code != http.StatusAccepted {
		t.Fatalf("alpha's client was damaged by beta's attempts: got %d", code)
	}
}

func TestClientLifecycle_MoveAnalyticsSitePreservesIdentityAndHistory(t *testing.T) {
	ctx, e := newEnv(t)
	seed := seedTenant(t, ctx, e.tdb, "MoveCo")
	targetID := seedChildBusiness(t, ctx, e.tdb, seed.businessID, seed.businessID, "MoveCo Labs")
	svc := telemetry.NewService(e.tdb.App, e.sealer)

	created, err := svc.CreateClient(
		ctx, seed.principalID, seed.businessID, "analytics", "move.example", false,
	)
	if err != nil {
		t.Fatalf("CreateClient: %v", err)
	}
	today := time.Now().UTC().Format("2006-01-02")
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO analytics_event
		    (id,tenant_root_id,business_id,client_id,occurred_at,name,path,visitor_hash)
		  VALUES ($1,$2,$3,$4,now(),'pageview','/',decode('01','hex'))`,
			[]any{uuid.New(), seed.businessID, seed.businessID, created.ID}},
		{`INSERT INTO analytics_event_daily
		    (tenant_root_id,business_id,client_id,bucket_date,event_count)
		  VALUES ($1,$2,$3,$4::date,7)`,
			[]any{seed.businessID, seed.businessID, created.ID, today}},
		{`INSERT INTO analytics_daily
		    (tenant_root_id,business_id,client_id,bucket_date,pageviews,visitors)
		  VALUES ($1,$2,$3,$4::date,7,3)`,
			[]any{seed.businessID, seed.businessID, created.ID, today}},
		{`INSERT INTO analytics_page_daily
		    (tenant_root_id,business_id,client_id,bucket_date,path,pageviews,visitors)
		  VALUES ($1,$2,$3,$4::date,'/',7,3)`,
			[]any{seed.businessID, seed.businessID, created.ID, today}},
		{`INSERT INTO analytics_referrer_daily
		    (tenant_root_id,business_id,client_id,bucket_date,referrer_host,pageviews,visitors)
		  VALUES ($1,$2,$3,$4::date,'example.test',2,1)`,
			[]any{seed.businessID, seed.businessID, created.ID, today}},
		{`INSERT INTO analytics_dimension_daily
		    (tenant_root_id,business_id,client_id,bucket_date,dimension,value,pageviews,visitors)
		  VALUES ($1,$2,$3,$4::date,'device','desktop',7,3)`,
			[]any{seed.businessID, seed.businessID, created.ID, today}},
	}
	for _, statement := range statements {
		if _, err := e.tdb.Super.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed analytics history: %v\nSQL: %s", err, statement.sql)
		}
	}

	targets, err := svc.MoveTargets(ctx, seed.principalID, seed.businessID, created.ID)
	if err != nil {
		t.Fatalf("MoveTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].ID != targetID {
		t.Fatalf("MoveTargets = %#v, want only %s", targets, targetID)
	}

	moved, err := svc.MoveClient(
		ctx, seed.principalID, seed.businessID, created.ID, targetID,
	)
	if err != nil {
		t.Fatalf("MoveClient: %v", err)
	}
	if moved.ID != created.ID || moved.PublishableKey != created.PublishableKey {
		t.Fatalf("site identity changed: before=%#v after=%#v", created, moved)
	}
	if moved.BusinessID != targetID || moved.TenantRootID != seed.businessID {
		t.Fatalf("site scope = business %s tenant %s, want %s/%s",
			moved.BusinessID, moved.TenantRootID, targetID, seed.businessID)
	}
	sourceList, err := svc.ListClients(ctx, seed.principalID, seed.businessID, 50)
	if err != nil {
		t.Fatalf("source ListClients: %v", err)
	}
	targetList, err := svc.ListClients(ctx, seed.principalID, targetID, 50)
	if err != nil {
		t.Fatalf("target ListClients: %v", err)
	}
	if len(sourceList) != 0 || len(targetList) != 1 || targetList[0].ID != created.ID {
		t.Fatalf("lists after move: source=%#v target=%#v", sourceList, targetList)
	}

	for _, table := range []string{
		"analytics_event", "analytics_event_daily", "analytics_daily", "analytics_page_daily",
		"analytics_referrer_daily", "analytics_dimension_daily",
	} {
		var sourceRows, targetRows int
		if err := e.tdb.Super.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE business_id=$2),
			        count(*) FILTER (WHERE business_id=$3)
			   FROM `+table+` WHERE client_id=$1`,
			created.ID, seed.businessID, targetID).Scan(&sourceRows, &targetRows); err != nil {
			t.Fatalf("%s ownership: %v", table, err)
		}
		if sourceRows != 0 || targetRows != 1 {
			t.Errorf("%s ownership source=%d target=%d, want 0/1", table, sourceRows, targetRows)
		}
	}
	var sourceClients, targetClients int
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE business_id=$2),
		        count(*) FILTER (WHERE business_id=$3)
		   FROM telemetry_client WHERE id=$1`,
		created.ID, seed.businessID, targetID).Scan(&sourceClients, &targetClients); err != nil {
		t.Fatalf("telemetry_client ownership: %v", err)
	}
	if sourceClients != 0 || targetClients != 1 {
		t.Errorf("telemetry_client ownership source=%d target=%d, want 0/1",
			sourceClients, targetClients)
	}

	analyticsSvc := analytics.NewService(e.tdb.App)
	from := time.Now().UTC().Truncate(24 * time.Hour)
	summary, err := analyticsSvc.Summary(
		ctx, seed.principalID, targetID, created.ID, from, from,
	)
	if err != nil {
		t.Fatalf("target Summary: %v", err)
	}
	if summary.Pageviews != 7 || summary.Visitors != 3 || len(summary.TopPages) != 1 {
		t.Fatalf("target history was not preserved: %#v", summary)
	}
	if _, err := analyticsSvc.Summary(
		ctx, seed.principalID, seed.businessID, created.ID, from, from,
	); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("source Summary after move = %v, want ErrNotFound", err)
	}
	overview, err := analyticsSvc.Overview(ctx, seed.principalID, from, from)
	if err != nil {
		t.Fatalf("Overview after move: %v", err)
	}
	if len(overview) != 1 || overview[0].BusinessID != targetID.String() ||
		overview[0].ClientID != created.ID.String() {
		t.Fatalf("cross-business overview after move = %#v", overview)
	}

	// The deployed snippet keeps working with the original key and writes to the destination.
	if code, body := post(t, e.srv, created.PublishableKey, analyticsBody(1), nil); code != http.StatusAccepted {
		t.Fatalf("post-move ingest: status %d body %s", code, body)
	}
	var staleRows int
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM analytics_event
		  WHERE client_id=$1 AND business_id<>$2`,
		created.ID, targetID).Scan(&staleRows); err != nil {
		t.Fatalf("post-move event ownership: %v", err)
	}
	if staleRows != 0 {
		t.Fatalf("post-move ingest left %d events outside destination", staleRows)
	}
}

func TestClientLifecycle_MoveRefusesOraclesNoOpsAndCrossTenant(t *testing.T) {
	ctx, e := newEnv(t)
	source := seedTenant(t, ctx, e.tdb, "MoveSource")
	targetID := seedChildBusiness(
		t, ctx, e.tdb, source.businessID, source.businessID, "MoveTarget",
	)
	foreignTenant := seedTenant(t, ctx, e.tdb, "ForeignAuthorized")
	unauthorizedTenant := seedTenant(t, ctx, e.tdb, "ForeignHidden")
	svc := telemetry.NewService(e.tdb.App, e.sealer)

	client, err := svc.CreateClient(
		ctx, source.principalID, source.businessID, "analytics", "site", false,
	)
	if err != nil {
		t.Fatalf("CreateClient: %v", err)
	}
	crashClient, err := svc.CreateClient(
		ctx, source.principalID, source.businessID, "crash", "app", false,
	)
	if err != nil {
		t.Fatalf("CreateClient(crash): %v", err)
	}

	if _, err := svc.MoveClient(
		ctx, source.principalID, source.businessID, client.ID, source.businessID,
	); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("same-business move = %v, want ErrConflict", err)
	}

	var ownerRole uuid.UUID
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("owner role: %v", err)
	}
	if _, err := e.tdb.Super.Exec(ctx,
		`INSERT INTO membership
		    (principal_id,business_id,tenant_root_id,role_id,granted_at)
		  VALUES ($1,$2,$2,$3,now())`,
		source.principalID, foreignTenant.businessID, ownerRole); err != nil {
		t.Fatalf("grant foreign target permission: %v", err)
	}
	if _, err := svc.MoveClient(
		ctx, source.principalID, source.businessID, client.ID, foreignTenant.businessID,
	); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("cross-tenant move = %v, want ErrConflict", err)
	}

	unknownErr := func() error {
		_, err := svc.MoveClient(
			ctx, source.principalID, source.businessID, uuid.New(), targetID,
		)
		return err
	}()
	hiddenTargetErr := func() error {
		_, err := svc.MoveClient(
			ctx, source.principalID, source.businessID, client.ID, unauthorizedTenant.businessID,
		)
		return err
	}()
	nonAnalyticsErr := func() error {
		_, err := svc.MoveClient(
			ctx, source.principalID, source.businessID, crashClient.ID, targetID,
		)
		return err
	}()
	for label, err := range map[string]error{
		"unknown client":      unknownErr,
		"unauthorized target": hiddenTargetErr,
		"non-analytics":       nonAnalyticsErr,
	} {
		if !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("%s = %v, want ErrNotFound", label, err)
		}
	}

	var businessID uuid.UUID
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT business_id FROM telemetry_client WHERE id=$1", client.ID).Scan(&businessID); err != nil {
		t.Fatalf("client after rejected moves: %v", err)
	}
	if businessID != source.businessID {
		t.Fatalf("rejected move partially changed client to %s", businessID)
	}
}

func TestClientLifecycle_MoveWaitsForConcurrentIngestAndRewritesIt(t *testing.T) {
	ctx, e := newEnv(t)
	seed := seedTenant(t, ctx, e.tdb, "MoveRace")
	targetID := seedChildBusiness(t, ctx, e.tdb, seed.businessID, seed.businessID, "MoveRace Target")
	svc := telemetry.NewService(e.tdb.App, e.sealer)
	client, err := svc.CreateClient(
		ctx, seed.principalID, seed.businessID, "analytics", "race", false,
	)
	if err != nil {
		t.Fatalf("CreateClient: %v", err)
	}

	// Keep the ingest transaction open after it has inserted. Its FOR SHARE lock must prevent the
	// move from acquiring FOR UPDATE until this event commits.
	ingestTx, err := e.tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin ingest: %v", err)
	}
	var accepted int
	if err := ingestTx.QueryRow(ctx,
		`SELECT telemetry_ingest_analytics($1, $2::jsonb)`,
		client.PublishableKey,
		`[{"occurred_at":"`+time.Now().UTC().Format(time.RFC3339Nano)+`","name":"pageview"}]`,
	).Scan(&accepted); err != nil {
		_ = ingestTx.Rollback(ctx)
		t.Fatalf("concurrent ingest: %v", err)
	}
	if accepted != 1 {
		_ = ingestTx.Rollback(ctx)
		t.Fatalf("concurrent ingest accepted %d, want 1", accepted)
	}

	moveDone := make(chan error, 1)
	go func() {
		_, err := svc.MoveClient(
			context.Background(), seed.principalID, seed.businessID, client.ID, targetID,
		)
		moveDone <- err
	}()
	select {
	case err := <-moveDone:
		_ = ingestTx.Rollback(ctx)
		t.Fatalf("move completed while ingest still held FOR SHARE: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := ingestTx.Commit(ctx); err != nil {
		t.Fatalf("commit ingest: %v", err)
	}
	select {
	case err := <-moveDone:
		if err != nil {
			t.Fatalf("MoveClient after ingest commit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("move remained blocked after ingest committed")
	}

	var sourceRows, targetRows int
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE business_id=$2),
		        count(*) FILTER (WHERE business_id=$3)
		   FROM analytics_event WHERE client_id=$1`,
		client.ID, seed.businessID, targetID).Scan(&sourceRows, &targetRows); err != nil {
		t.Fatalf("event ownership: %v", err)
	}
	if sourceRows != 0 || targetRows != 1 {
		t.Fatalf("concurrent event ownership source=%d target=%d, want 0/1",
			sourceRows, targetRows)
	}
}

func TestClientLifecycle_RejectsInvalidKindAndEmptyName(t *testing.T) {
	ctx, e := newEnv(t)
	seed := seedTenant(t, ctx, e.tdb, "TelCo")
	svc := telemetry.NewService(e.tdb.App, e.sealer)

	if _, err := svc.CreateClient(ctx, seed.principalID, seed.businessID, "metrics", "x", false); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("unknown kind should be ErrValidation, got %v", err)
	}
	if _, err := svc.CreateClient(ctx, seed.principalID, seed.businessID, "analytics", "   ", false); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("blank name should be ErrValidation, got %v", err)
	}
}

// Revocation must be atomic with the write, not merely checked at auth time. This drives the
// window the resolve/insert split used to leave open: the client is revoked AFTER a caller would
// have passed the auth lookup, and the insert must still refuse.
func TestIngest_RevocationIsAtomicWithInsert(t *testing.T) {
	ctx, e := newEnv(t)
	seed := seedTenant(t, ctx, e.tdb, "TelCo")
	svc := telemetry.NewService(e.tdb.App, e.sealer)

	created, err := svc.CreateClient(ctx, seed.principalID, seed.businessID, "analytics", "web", false)
	if err != nil {
		t.Fatalf("CreateClient: %v", err)
	}
	// Revoke directly, then call the ingest function the way the handler does. Even with a
	// previously-resolved client in hand, the SQL function must refuse.
	if _, err := svc.RevokeClient(ctx, seed.principalID, seed.businessID, created.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	var n int
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT telemetry_ingest_analytics($1, $2::jsonb)`,
		created.PublishableKey,
		`[{"occurred_at":"`+time.Now().UTC().Format(time.RFC3339)+`","name":"pageview"}]`,
	).Scan(&n); err != nil {
		t.Fatalf("ingest fn: %v", err)
	}
	if n != -1 {
		t.Fatalf("REVOCATION RACE: ingest function accepted %d events for a revoked client", n)
	}

	var rows int
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM analytics_event WHERE client_id=$1", created.ID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Fatalf("revoked client persisted %d events", rows)
	}
}

func TestIngest_OversizeBatchIsRejectedNotTruncated(t *testing.T) {
	ctx, e := newEnv(t)
	clientID, key := e.insertClient(t, ctx, "analytics", "active", nil)

	events := make([]map[string]any, 1001)
	for i := range events {
		events[i] = map[string]any{
			"occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
			"name":        "p",
		}
	}
	code, body := post(t, e.srv, key, map[string]any{"analytics": events}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("oversize batch: expected 400, got %d (%s)", code, body)
	}
	// Nothing may be persisted — a partial write would mean the caller's 400 hid 1000 stored rows.
	var n int
	if err := e.tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM analytics_event WHERE client_id=$1", clientID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("rejected batch still persisted %d events", n)
	}
}

// A signing client is the opt-in, server-to-server shape: it DOES get a secret, returned exactly
// once, and its ingest fails closed without a valid signature.
func TestClientLifecycle_SigningClientIsOptIn(t *testing.T) {
	ctx, e := newEnv(t)
	seed := seedTenant(t, ctx, e.tdb, "TelCo")
	svc := telemetry.NewService(e.tdb.App, e.sealer)

	signed, err := svc.CreateClient(ctx, seed.principalID, seed.businessID, "analytics", "backend", true)
	if err != nil {
		t.Fatalf("CreateClient(signed): %v", err)
	}
	if !signed.RequireSignature || !signed.HasSecret {
		t.Fatalf("signing client not configured: require=%v hasSecret=%v",
			signed.RequireSignature, signed.HasSecret)
	}
	if !strings.HasPrefix(signed.Secret, "mfs_") {
		t.Fatalf("signing secret not returned at creation: %q", signed.Secret)
	}

	// Write-once.
	list, err := svc.ListClients(ctx, seed.principalID, seed.businessID, 50)
	if err != nil {
		t.Fatalf("ListClients: %v", err)
	}
	if list[0].Secret != "" {
		t.Fatal("SECRET LEAK: ListClients returned the plaintext signing secret")
	}

	// Unsigned request to a signing client fails closed.
	if code, _ := post(t, e.srv, signed.PublishableKey, analyticsBody(1), nil); code != http.StatusUnauthorized {
		t.Fatalf("unsigned request to a signing client: expected 401, got %d", code)
	}

	// Correctly signed request is accepted.
	body := analyticsBody(1)
	raw, _ := json.Marshal(body)
	target := "/api/v1/telemetry/ingest/" + signed.PublishableKey
	ts := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(signed.Secret))
	mac.Write([]byte(fmt.Sprintf("%d.%s.%s.", ts, http.MethodPost, target)))
	mac.Write(raw)
	sig := fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
	if code, b := post(t, e.srv, signed.PublishableKey, body, map[string]string{"X-Telemetry-Signature": sig}); code != http.StatusAccepted {
		t.Fatalf("signed request: expected 202, got %d (%s)", code, b)
	}
}

// An embeddable (non-signing) client must keep working with the mfk_ key alone even though the
// deployment has a master key configured. This is the regression that would silently break every
// app SDK if signature handling were keyed off secret presence.
func TestIngest_EmbeddableClientNeedsNoSignature(t *testing.T) {
	ctx, e := newEnv(t)
	seed := seedTenant(t, ctx, e.tdb, "TelCo")
	svc := telemetry.NewService(e.tdb.App, e.sealer) // sealer IS configured

	c, err := svc.CreateClient(ctx, seed.principalID, seed.businessID, "analytics", "mobile-app", false)
	if err != nil {
		t.Fatalf("CreateClient: %v", err)
	}
	if code, b := post(t, e.srv, c.PublishableKey, analyticsBody(2), nil); code != http.StatusAccepted {
		t.Fatalf("embeddable client rejected without a signature: got %d (%s)", code, b)
	}
}
