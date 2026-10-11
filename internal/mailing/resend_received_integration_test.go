//go:build integration

package mailing_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/mailing"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

type fakeInboundCall struct {
	businessID uuid.UUID
	eventID    string
	envelope   []string
	raw        []byte
}

type fakeInbound struct {
	mu    sync.Mutex
	calls []fakeInboundCall
	err   error
}

func (f *fakeInbound) ReceiveResend(_ context.Context, businessID uuid.UUID, eventID string, envelope []string, raw []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeInboundCall{businessID, eventID, envelope, raw})
	return f.err
}

func (f *fakeInbound) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

// fakeResend serves the Receiving API (metadata + pre-signed raw) and the
// webhook detail/patch endpoints the boot sweep uses.
type fakeResend struct {
	mu         sync.Mutex
	server     *httptest.Server
	raw        []byte
	metaStatus int
	rawStatus  int
	metaHits   int
	auth       []string
	events     []string
	secret     string
	patches    int
}

func (f *fakeResend) setMetaStatus(code int) { f.mu.Lock(); defer f.mu.Unlock(); f.metaStatus = code }
func (f *fakeResend) setRawStatus(code int)  { f.mu.Lock(); defer f.mu.Unlock(); f.rawStatus = code }
func (f *fakeResend) hits() int              { f.mu.Lock(); defer f.mu.Unlock(); return f.metaHits }
func (f *fakeResend) patchCount() int        { f.mu.Lock(); defer f.mu.Unlock(); return f.patches }
func (f *fakeResend) lastAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auth[len(f.auth)-1]
}
func (f *fakeResend) eventList() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.events, ",")
}

func newFakeResend(t *testing.T, raw []byte) *fakeResend {
	t.Helper()
	f := &fakeResend{raw: raw, events: []string{"email.bounced", "email.complained"}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/emails/receiving/"):
			f.metaHits++
			f.auth = append(f.auth, r.Header.Get("Authorization"))
			if f.metaStatus != 0 {
				w.WriteHeader(f.metaStatus)
				return
			}
			id := strings.TrimPrefix(r.URL.Path, "/emails/receiving/")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": id, "to": []string{"support@support.example.test"},
				"received_for": []string{"support@support.example.test"}, "message_id": "<m@customer.test>",
				"raw": map[string]any{"download_url": f.server.URL + "/raw/" + id, "expires_at": "2099-01-01T00:00:00Z"},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/raw/"):
			if f.rawStatus != 0 {
				w.WriteHeader(f.rawStatus)
				return
			}
			if r.Header.Get("Authorization") != "" {
				t.Errorf("raw download carried Authorization")
			}
			_, _ = w.Write(f.raw)
		case r.Method == http.MethodGet && r.URL.Path == "/webhooks/wh_fixture":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "wh_fixture", "status": "enabled",
				"events": f.events, "signing_secret": f.secret})
		case r.Method == http.MethodPatch && r.URL.Path == "/webhooks/wh_fixture":
			f.patches++
			var body struct {
				Events []string `json:"events"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.events = body.Events
			_, _ = io.WriteString(w, `{"object":"webhook","id":"wh_fixture"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

type receivedHarness struct {
	ctx     context.Context
	tdb     *testdb.TestDB
	seed    mailingSeed
	fx      webhookFixture
	h       *mailing.WebhookHandler
	router  chi.Router
	inbound *fakeInbound
	fake    *fakeResend
	key     []byte
}

var testRaw = []byte("From: Ada <ada@customer.test>\r\nTo: Support <support@support.example.test>\r\nSubject: Help\r\nMessage-ID: <m@customer.test>\r\n\r\nhello\r\n")

func newReceivedHarness(t *testing.T) *receivedHarness {
	t.Helper()
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	seed := seedMailingTenant(ctx, t, tdb)
	fx := seedWebhookFixture(ctx, t, tdb, seed)
	seedSupportAddress(ctx, t, tdb, seed.businessID)
	inbound := &fakeInbound{}
	fake := newFakeResend(t, testRaw)
	fake.secret = "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x51}, 32))
	h := mailing.NewWebhookHandler(tdb.App, fx.svc.Sealer, nil)
	h.Now = func() time.Time { return time.Unix(1_800_000_000, 0).UTC() }
	h.Inbound = inbound
	h.ResendBaseURL = fake.server.URL
	h.ResendHTTPClient = fake.server.Client()
	h.RawDownloadClient = fake.server.Client()
	h.AllowInsecureRawURL = true
	h.MaxReceivedBytes = 1 << 20
	router := chi.NewRouter()
	h.PublicRoutes(router)
	return &receivedHarness{ctx: ctx, tdb: tdb, seed: seed, fx: fx, h: h, router: router,
		inbound: inbound, fake: fake, key: bytes.Repeat([]byte{0x51}, 32)}
}

// seedSupportAddress gives the business a custom address on a verified
// provider_route domain, so email.received passes the R12 support gate.
func seedSupportAddress(ctx context.Context, t *testing.T, tdb *testdb.TestDB, businessID uuid.UUID) {
	t.Helper()
	domainID := uuid.New()
	if _, err := tdb.Super.Exec(ctx, `INSERT INTO email_domain
		(id,business_id,tenant_root_id,domain,mode,verify_token,verified_at,spf_state,created_at,updated_at)
		VALUES ($1,$2,$2,'support.example.test','provider_route','mf-verify=seed',now(),'unknown',now(),now())`,
		domainID, businessID); err != nil {
		t.Fatalf("seed email_domain: %v", err)
	}
	if _, err := tdb.Super.Exec(ctx, `INSERT INTO inbound_address
		(id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at)
		VALUES ($1,$2,$2,'support@support.example.test','custom',$3,now(),now())`,
		uuid.New(), businessID, domainID); err != nil {
		t.Fatalf("seed inbound_address: %v", err)
	}
}

func (rh *receivedHarness) post(t *testing.T, eventID string, key []byte) int {
	t.Helper()
	body := []byte(`{"type":"email.received","created_at":"2026-10-10T12:00:00Z","data":{"email_id":"em_1","to":["support@support.example.test"]}}`)
	ts := fmt.Sprint(rh.h.Now().Unix())
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(eventID + "." + ts + "."))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/inbound/mailing/"+rh.fx.profileID.String()+"/resend", bytes.NewReader(body))
	req.Header.Set("svix-id", eventID)
	req.Header.Set("svix-timestamp", ts)
	req.Header.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	rh.router.ServeHTTP(w, req)
	return w.Code
}

func (rh *receivedHarness) recorded(t *testing.T, eventID string) int {
	t.Helper()
	var n int
	if err := rh.tdb.Super.QueryRow(rh.ctx, `SELECT count(*) FROM mailing_provider_webhook_delivery
		WHERE profile_id=$1 AND external_event_id=$2 AND processing_status='applied'`, rh.fx.profileID, eventID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (rh *receivedHarness) setCredential(t *testing.T, apiKey string, webhookKey []byte) {
	t.Helper()
	secret := "whsec_" + base64.StdEncoding.EncodeToString(webhookKey)
	sealed, err := rh.fx.svc.Sealer.Seal([]byte(fmt.Sprintf(
		`{"version":2,"api_key":%q,"webhook_id":"wh_fixture","webhook_secret":%q}`, apiKey, secret)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rh.tdb.Super.Exec(rh.ctx, `UPDATE secret SET sealed_value=$1
		WHERE id=(SELECT secret_ref FROM mailing_sending_profile WHERE id=$2)`, sealed, rh.fx.profileID); err != nil {
		t.Fatal(err)
	}
}

func TestResendReceivedIngestsThenRecords(t *testing.T) {
	rh := newReceivedHarness(t)
	if code := rh.post(t, "evt-r1", rh.key); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if rh.inbound.count() != 1 {
		t.Fatalf("ingest calls = %d, want 1", rh.inbound.count())
	}
	call := rh.inbound.calls[0]
	if call.businessID != rh.seed.businessID || call.eventID != "evt-r1" || !bytes.Equal(call.raw, testRaw) ||
		len(call.envelope) == 0 || call.envelope[0] != "support@support.example.test" {
		t.Fatalf("ingest call = %+v", call)
	}
	if rh.recorded(t, "evt-r1") != 1 {
		t.Fatal("delivery row not recorded after a successful ingest")
	}
	if code := rh.post(t, "evt-r1", rh.key); code != http.StatusOK || rh.inbound.count() != 1 || rh.fake.hits() != 1 {
		t.Fatalf("redelivery status=%d ingests=%d fetches=%d, want 200/1/1", code, rh.inbound.count(), rh.fake.hits())
	}
}

// R5: a fetch failure answers 5xx so Resend retries, and is NOT recorded, so the
// retry is processed instead of discarded as a duplicate.
func TestResendReceivedFetchFailureIsRetriedNotRecorded(t *testing.T) {
	rh := newReceivedHarness(t)
	rh.fake.setMetaStatus(http.StatusInternalServerError)
	if code := rh.post(t, "evt-r2", rh.key); code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if rh.recorded(t, "evt-r2") != 0 || rh.inbound.count() != 0 {
		t.Fatal("failed fetch was recorded or ingested")
	}
	rh.fake.setMetaStatus(0)
	if code := rh.post(t, "evt-r2", rh.key); code != http.StatusOK || rh.inbound.count() != 1 || rh.recorded(t, "evt-r2") != 1 {
		t.Fatalf("retry status=%d ingests=%d recorded=%d, want 200/1/1", code, rh.inbound.count(), rh.recorded(t, "evt-r2"))
	}
}

func TestResendReceivedIngestFailureIsRetried(t *testing.T) {
	rh := newReceivedHarness(t)
	rh.inbound.err = errors.New("database unavailable")
	if code := rh.post(t, "evt-r3", rh.key); code != http.StatusServiceUnavailable || rh.recorded(t, "evt-r3") != 0 {
		t.Fatalf("status=%d recorded=%d, want 503/0", code, rh.recorded(t, "evt-r3"))
	}
	rh.inbound.err = nil
	if code := rh.post(t, "evt-r3", rh.key); code != http.StatusOK || rh.recorded(t, "evt-r3") != 1 {
		t.Fatalf("retry status=%d recorded=%d, want 200/1", code, rh.recorded(t, "evt-r3"))
	}
}

// R6 / Review Focus 4: an oversized raw message is acknowledged (and recorded so
// Resend stops retrying) without ingesting.
func TestResendReceivedOversizedRawIsAcknowledged(t *testing.T) {
	rh := newReceivedHarness(t)
	rh.h.MaxReceivedBytes = 16
	if code := rh.post(t, "evt-r4", rh.key); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if rh.inbound.count() != 0 || rh.recorded(t, "evt-r4") != 1 {
		t.Fatalf("ingests=%d recorded=%d, want 0/1", rh.inbound.count(), rh.recorded(t, "evt-r4"))
	}
}

// Review Focus 5: the fetch uses the CURRENT API key after rotation, and an
// event signed with the retired webhook secret is rejected.
func TestResendReceivedUsesCurrentCredentialAfterRotation(t *testing.T) {
	rh := newReceivedHarness(t)
	rh.setCredential(t, "re_rotated", rh.key)
	if code := rh.post(t, "evt-r5", rh.key); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got := rh.fake.lastAuth(); got != "Bearer re_rotated" {
		t.Fatalf("fetch authorization = %q, want the rotated key", got)
	}
	newKey := bytes.Repeat([]byte{0x52}, 32)
	rh.setCredential(t, "re_rotated", newKey)
	if code := rh.post(t, "evt-r6", rh.key); code != http.StatusUnauthorized {
		t.Fatalf("retired-secret status = %d, want 401", code)
	}
	if code := rh.post(t, "evt-r6", newKey); code != http.StatusOK {
		t.Fatalf("new-secret status = %d, want 200", code)
	}
}

func TestResendReceivedIgnoredForUnverifiedProfile(t *testing.T) {
	rh := newReceivedHarness(t)
	if _, err := rh.tdb.Super.Exec(rh.ctx, `UPDATE mailing_sending_profile SET status='unverified' WHERE id=$1`, rh.fx.profileID); err != nil {
		t.Fatal(err)
	}
	if code := rh.post(t, "evt-r7", rh.key); code != http.StatusOK || rh.fake.hits() != 0 || rh.inbound.count() != 0 {
		t.Fatalf("status=%d fetches=%d ingests=%d, want 200/0/0", code, rh.fake.hits(), rh.inbound.count())
	}
}

// R1: the boot sweep adds email.received to a legacy webhook in place and keeps
// its signing secret; a second run is read-only.
func TestUpgradeReceivingEventsPatchesLegacyWebhook(t *testing.T) {
	rh := newReceivedHarness(t)
	if err := rh.h.UpgradeReceivingEvents(rh.ctx); err != nil {
		t.Fatal(err)
	}
	if rh.fake.patchCount() != 1 || !strings.Contains(rh.fake.eventList(), "email.received") {
		t.Fatalf("patches=%d events=%v", rh.fake.patchCount(), rh.fake.eventList())
	}
	if err := rh.h.UpgradeReceivingEvents(rh.ctx); err != nil || rh.fake.patchCount() != 1 {
		t.Fatalf("second sweep patches=%d err=%v, want read-only", rh.fake.patchCount(), err)
	}
}

// Gone (metadata 404) is permanent: acknowledged and recorded, never ingested.
func TestResendReceivedGoneIsAcknowledged(t *testing.T) {
	rh := newReceivedHarness(t)
	rh.fake.setMetaStatus(http.StatusNotFound)
	if code := rh.post(t, "evt-r8", rh.key); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if rh.inbound.count() != 0 || rh.recorded(t, "evt-r8") != 1 {
		t.Fatalf("ingests=%d recorded=%d, want 0/1", rh.inbound.count(), rh.recorded(t, "evt-r8"))
	}
}

// A raw-download 5xx is transient: 503, nothing recorded, the retry succeeds.
func TestResendReceivedDownloadFailureIsRetriedNotRecorded(t *testing.T) {
	rh := newReceivedHarness(t)
	rh.fake.setRawStatus(http.StatusBadGateway)
	if code := rh.post(t, "evt-r9", rh.key); code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if rh.recorded(t, "evt-r9") != 0 || rh.inbound.count() != 0 {
		t.Fatal("failed download was recorded or ingested")
	}
	rh.fake.setRawStatus(0)
	if code := rh.post(t, "evt-r9", rh.key); code != http.StatusOK || rh.inbound.count() != 1 || rh.recorded(t, "evt-r9") != 1 {
		t.Fatalf("retry status=%d ingests=%d recorded=%d, want 200/1/1", code, rh.inbound.count(), rh.recorded(t, "evt-r9"))
	}
}

// A rejected credential (401) still answers 503 so Resend retries.
func TestResendReceivedRejectedCredentialIsRetried(t *testing.T) {
	rh := newReceivedHarness(t)
	rh.fake.setMetaStatus(http.StatusUnauthorized)
	if code := rh.post(t, "evt-r10", rh.key); code != http.StatusServiceUnavailable || rh.recorded(t, "evt-r10") != 0 {
		t.Fatalf("status=%d recorded=%d, want 503/0", code, rh.recorded(t, "evt-r10"))
	}
}

// A bad signature is rejected before any Receiving API call.
func TestResendReceivedBadSignatureFetchesNothing(t *testing.T) {
	rh := newReceivedHarness(t)
	if code := rh.post(t, "evt-r11", bytes.Repeat([]byte{0x99}, 32)); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
	if rh.fake.hits() != 0 || rh.inbound.count() != 0 || rh.recorded(t, "evt-r11") != 0 {
		t.Fatalf("fetches=%d ingests=%d, want 0/0", rh.fake.hits(), rh.inbound.count())
	}
}

// I1 / R12: without a custom address on a verified provider_route domain the
// event is acknowledged and recorded with no Receiving API call and no ingest.
func TestResendReceivedSkipsBusinessWithoutSupportAddress(t *testing.T) {
	rh := newReceivedHarness(t)
	if _, err := rh.tdb.Super.Exec(rh.ctx, `UPDATE email_domain SET mode='forward_in' WHERE business_id=$1`, rh.seed.businessID); err != nil {
		t.Fatal(err)
	}
	if code := rh.post(t, "evt-r12", rh.key); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if rh.fake.hits() != 0 || rh.inbound.count() != 0 || rh.recorded(t, "evt-r12") != 1 {
		t.Fatalf("fetches=%d ingests=%d recorded=%d, want 0/0/1", rh.fake.hits(), rh.inbound.count(), rh.recorded(t, "evt-r12"))
	}
}
