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
	metaHits   int
	auth       []string
	events     []string
	secret     string
	patches    int
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
	if code := rh.post(t, "evt-r1", rh.key); code != http.StatusOK || rh.inbound.count() != 1 || rh.fake.metaHits != 1 {
		t.Fatalf("redelivery status=%d ingests=%d fetches=%d, want 200/1/1", code, rh.inbound.count(), rh.fake.metaHits)
	}
}

// R5: a fetch failure answers 5xx so Resend retries, and is NOT recorded, so the
// retry is processed instead of discarded as a duplicate.
func TestResendReceivedFetchFailureIsRetriedNotRecorded(t *testing.T) {
	rh := newReceivedHarness(t)
	rh.fake.metaStatus = http.StatusInternalServerError
	if code := rh.post(t, "evt-r2", rh.key); code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if rh.recorded(t, "evt-r2") != 0 || rh.inbound.count() != 0 {
		t.Fatal("failed fetch was recorded or ingested")
	}
	rh.fake.metaStatus = 0
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
	if got := rh.fake.auth[len(rh.fake.auth)-1]; got != "Bearer re_rotated" {
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
	if code := rh.post(t, "evt-r7", rh.key); code != http.StatusOK || rh.fake.metaHits != 0 || rh.inbound.count() != 0 {
		t.Fatalf("status=%d fetches=%d ingests=%d, want 200/0/0", code, rh.fake.metaHits, rh.inbound.count())
	}
}

// R1: the boot sweep adds email.received to a legacy webhook in place and keeps
// its signing secret; a second run is read-only.
func TestUpgradeReceivingEventsPatchesLegacyWebhook(t *testing.T) {
	rh := newReceivedHarness(t)
	if err := rh.h.UpgradeReceivingEvents(rh.ctx); err != nil {
		t.Fatal(err)
	}
	if rh.fake.patches != 1 || !strings.Contains(strings.Join(rh.fake.events, ","), "email.received") {
		t.Fatalf("patches=%d events=%v", rh.fake.patches, rh.fake.events)
	}
	if err := rh.h.UpgradeReceivingEvents(rh.ctx); err != nil || rh.fake.patches != 1 {
		t.Fatalf("second sweep patches=%d err=%v, want read-only", rh.fake.patches, err)
	}
}
