//go:build integration

// Spec 017 success criteria: against a fake Resend API, a signed email.received
// creates a ticket; the agent reply leaves through the business's profile with the
// right From and Reply-To; the customer's reply threads onto the same ticket both
// by headers and by token alone.
package inbox_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/inbox"
	"github.com/manyforge/manyforge/internal/mailing"
	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
	mfcrypto "github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/internal/platform/notify"
	"github.com/manyforge/manyforge/internal/ticketing"
)

var loopReplyKey = []byte("loop-reply-token-key-0123456789abcdef")

type loopResend struct {
	mu      sync.Mutex
	server  *httptest.Server
	raws    map[string][]byte
	sent    []map[string]any
	fetches int
}

func newLoopResend(t *testing.T) *loopResend {
	t.Helper()
	f := &loopResend{raws: map[string][]byte{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/emails/receiving/"):
			f.fetches++
			id := strings.TrimPrefix(r.URL.Path, "/emails/receiving/")
			if _, ok := f.raws[id]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "to": []string{}, "received_for": []string{},
				"raw": map[string]any{"download_url": f.server.URL + "/raw/" + id}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/raw/"):
			if r.Header.Get("Authorization") != "" {
				t.Errorf("raw download carried Authorization")
			}
			_, _ = w.Write(f.raws[strings.TrimPrefix(r.URL.Path, "/raw/")])
		case r.Method == http.MethodPost && r.URL.Path == "/emails":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.sent = append(f.sent, body)
			_, _ = io.WriteString(w, fmt.Sprintf(`{"id":"sent-%d"}`, len(f.sent)))
		default:
			t.Errorf("unexpected Resend request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

// sends returns a copy of the captured POST /emails bodies, read under the lock
// the server goroutine appends with.
func (f *loopResend) sends() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sent...)
}

// fetchCount returns how many Receiving API metadata fetches were served.
func (f *loopResend) fetchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches
}

type loopFixture struct {
	ctx        context.Context
	tdb        *testdb.TestDB
	business   uuid.UUID
	owner      uuid.UUID
	profileID  uuid.UUID
	domain     string
	address    string
	webhookKey []byte
	resend     *loopResend
	router     chi.Router
	tickets    *ticketing.Service
	sendSub    notify.SendSubscriber
	shared     *countingSender
}

type countingSender struct{ calls int }

func (c *countingSender) Send(context.Context, notify.Mail) error { c.calls++; return nil }

func newLoopFixture(t *testing.T) *loopFixture {
	t.Helper()
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	sealer, err := mfcrypto.NewSealer(bytes.Repeat([]byte{0x72}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f := &loopFixture{ctx: ctx, tdb: tdb, business: uuid.New(), owner: uuid.New(), profileID: uuid.New(),
		webhookKey: bytes.Repeat([]byte{0x51}, 32), resend: newLoopResend(t), shared: &countingSender{}}
	f.domain = "support-" + f.business.String()[:8] + ".example.com"
	f.address = "support@" + f.domain
	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatal(err)
	}
	accountID, domainID, secretID := uuid.New(), uuid.New(), uuid.New()
	sealed, err := sealer.Seal([]byte(fmt.Sprintf(`{"version":2,"api_key":"re_loop","webhook_id":"wh_loop","webhook_secret":%q}`,
		"whsec_"+base64.StdEncoding.EncodeToString(f.webhookKey))))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Owner','active',now(),now(),now())`, []any{accountID, "owner-" + f.owner.String() + "@x.test"}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`, []any{f.owner, accountID}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'Garden','active',now(),now())`, []any{f.business}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`, []any{f.business}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`, []any{f.owner, f.business, ownerRole}},
		{`INSERT INTO inbound_address (id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at) VALUES ($1,$2,$2,$3,'system',NULL,now(),now())`, []any{uuid.New(), f.business, "b-" + f.business.String()[:8] + "@inbound.localhost"}},
		{`INSERT INTO email_domain (id,business_id,tenant_root_id,domain,mode,verify_token,verified_at,spf_state,created_at,updated_at) VALUES ($1,$2,$2,$3,'provider_route','mf-verify=x',now(),'unknown',now(),now())`, []any{domainID, f.business, f.domain}},
		{`INSERT INTO inbound_address (id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at) VALUES ($1,$2,$2,$3,'custom',$4,now(),now())`, []any{uuid.New(), f.business, f.address, domainID}},
		{`INSERT INTO secret (id,business_id,tenant_root_id,scope,sealed_value,created_at,updated_at) VALUES ($1,$2,$2,'mailing',$3,now(),now())`, []any{secretID, f.business, sealed}},
		{`INSERT INTO mailing_sending_profile (id,business_id,tenant_root_id,mode,from_email,from_name,secret_ref,status,feedback_status,feedback_confirmed_at) VALUES ($1,$2,$2,'resend',$3,'Garden Support',$4,'verified','ready',now())`, []any{f.profileID, f.business, f.address, secretID}},
	} {
		if _, err := tdb.Super.Exec(ctx, s.q, s.args...); err != nil {
			t.Fatalf("seed: %v\nSQL: %s", err, s.q)
		}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	inboxSvc := inbox.NewService(tdb.App, nil, inbox.Config{ReplyTokenKey: loopReplyKey, InboundSystemDomain: "inbound.localhost"}, logger)
	factory := &mailprovider.Factory{DB: tdb.App, ResendBaseURL: f.resend.server.URL, HTTPClient: f.resend.server.Client()}
	mailingSvc := &mailing.Service{DB: tdb.App, Sealer: sealer, Providers: mailprovider.NewCache(factory.Build, time.Minute)}
	wh := mailing.NewWebhookHandler(tdb.App, sealer, logger)
	wh.Inbound = inboxSvc
	wh.ResendBaseURL = f.resend.server.URL
	wh.ResendHTTPClient = f.resend.server.Client()
	wh.RawDownloadClient = f.resend.server.Client()
	wh.AllowInsecureRawURL = true
	wh.MaxReceivedBytes = 1 << 20
	f.router = chi.NewRouter()
	wh.PublicRoutes(f.router)
	f.tickets = &ticketing.Service{DB: tdb.App, ReplyTokenKey: loopReplyKey, SystemDomain: "inbound.localhost"}
	f.sendSub = notify.SendSubscriber{Sender: f.shared, ProfileSender: mailingSvc, Suppression: notify.DBSuppression{DB: tdb.App}}
	return f
}

func (f *loopFixture) receive(t *testing.T, eventID, emailID string, raw []byte) {
	t.Helper()
	f.resend.mu.Lock()
	f.resend.raws[emailID] = raw
	f.resend.mu.Unlock()
	body := []byte(fmt.Sprintf(`{"type":"email.received","created_at":"2026-10-10T12:00:00Z","data":{"email_id":%q,"to":[%q]}}`, emailID, f.address))
	ts := fmt.Sprint(time.Now().Unix())
	mac := hmac.New(sha256.New, f.webhookKey)
	mac.Write([]byte(eventID + "." + ts + "."))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/inbound/mailing/"+f.profileID.String()+"/resend", bytes.NewReader(body))
	req.Header.Set("svix-id", eventID)
	req.Header.Set("svix-timestamp", ts)
	req.Header.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook %s status = %d", eventID, w.Code)
	}
}

func (f *loopFixture) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.tdb.Super.QueryRow(f.ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func customerMail(from, to, messageID, inReplyTo, body string) []byte {
	m := "From: " + from + "\r\nTo: " + to + "\r\nSubject: Seeds never arrived\r\nDate: Mon, 02 Jan 2006 15:04:05 -0700\r\nMessage-ID: <" + messageID + ">\r\n"
	if inReplyTo != "" {
		m += "In-Reply-To: <" + inReplyTo + ">\r\nReferences: <" + inReplyTo + ">\r\n"
	}
	return []byte(m + "MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n")
}

func TestResendSupportLoop(t *testing.T) {
	f := newLoopFixture(t)

	// 1. Customer writes in.
	f.receive(t, "evt-1", "em_1", customerMail("Ada <ada@customer.test>", "Garden Support <"+f.address+">", "c1@customer.test", "", "Where are my seeds?"))
	if n := f.count(t, "SELECT count(*) FROM ticket WHERE business_id=$1", f.business); n != 1 {
		t.Fatalf("tickets = %d, want 1", n)
	}
	var ticketID uuid.UUID
	var token string
	if err := f.tdb.Super.QueryRow(f.ctx, "SELECT id, reply_token FROM ticket WHERE business_id=$1", f.business).Scan(&ticketID, &token); err != nil {
		t.Fatal(err)
	}

	// 2. Agent replies; the outbox event drains through the subscriber.
	reply, err := f.tickets.Reply(f.ctx, f.owner, f.business, ticketID, ticketing.ReplyInput{BodyText: "Shipping today."})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	var ev events.Event
	if err := f.tdb.Super.QueryRow(f.ctx, `SELECT id, tenant_root_id, topic, payload, attempts FROM outbox
		WHERE tenant_root_id=$1 AND topic='ticket.replied' AND processed_at IS NULL
		ORDER BY created_at DESC LIMIT 1`, f.business).Scan(&ev.ID, &ev.TenantRootID, &ev.Topic, &ev.Payload, &ev.Attempts); err != nil {
		t.Fatal(err)
	}
	ev.MaxAttempts = 10
	if err := f.tdb.App.WithTx(f.ctx, func(tx pgx.Tx) error { return f.sendSub.Handle(f.ctx, tx, ev) }); err != nil {
		t.Fatalf("send: %v", err)
	}
	sends := f.resend.sends()
	if f.shared.calls != 0 || len(sends) != 1 {
		t.Fatalf("shared calls=%d resend sends=%d, want 0/1", f.shared.calls, len(sends))
	}
	sent := sends[0]
	wantReplyTo := "support+" + token + "@" + f.domain
	if sent["from"] != f.address || sent["reply_to"] != wantReplyTo {
		t.Fatalf("sent from=%v reply_to=%v, want %s / %s", sent["from"], sent["reply_to"], f.address, wantReplyTo)
	}
	// The reply must thread under the customer's first message.
	headers, _ := sent["headers"].(map[string]any)
	if headers["In-Reply-To"] != "<c1@customer.test>" {
		t.Fatalf("sent In-Reply-To = %v, want <c1@customer.test>", headers["In-Reply-To"])
	}
	if refs, _ := headers["References"].(string); !strings.Contains(refs, "<c1@customer.test>") {
		t.Fatalf("sent References = %q, want it to contain <c1@customer.test>", refs)
	}
	var rfcID string
	if err := f.tdb.Super.QueryRow(f.ctx, "SELECT message_id FROM ticket_message WHERE id=$1", reply.ID).Scan(&rfcID); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "SELECT count(*) FROM ticket_message WHERE id=$1 AND delivery_state='sent'", reply.ID) != 1 {
		t.Fatal("reply not marked sent")
	}

	// 3a. Customer replies with threading headers.
	f.receive(t, "evt-2", "em_2", customerMail("ada@customer.test", f.address, "c2@customer.test", rfcID, "Thanks!"))
	// 3b. Customer replies to the token address with every header stripped.
	f.receive(t, "evt-3", "em_3", customerMail("ada@customer.test", "support+"+token+"@"+f.domain, "c3@customer.test", "", "One more thing"))

	if n := f.count(t, "SELECT count(*) FROM ticket WHERE business_id=$1", f.business); n != 1 {
		t.Fatalf("tickets after replies = %d, want 1", n)
	}
	if n := f.count(t, "SELECT count(*) FROM ticket_message WHERE ticket_id=$1", ticketID); n != 4 {
		t.Fatalf("messages = %d, want 4 (in, out, in by headers, in by token)", n)
	}
}

// Review Focus 3: ingest committed but the delivery row was lost (crash between
// ingest and record) — Resend's redelivery must not duplicate the ticket/message.
func TestResendLoopRedeliveryAfterPartialFailureCreatesNoDuplicate(t *testing.T) {
	f := newLoopFixture(t)
	raw := customerMail("ada@customer.test", f.address, "p1@customer.test", "", "hello")
	f.receive(t, "evt-p", "em_p", raw)
	if _, err := f.tdb.Super.Exec(f.ctx, "DELETE FROM mailing_provider_webhook_delivery WHERE external_event_id='evt-p'"); err != nil {
		t.Fatal(err)
	}
	f.receive(t, "evt-p", "em_p", raw)
	if n := f.count(t, "SELECT count(*) FROM ticket WHERE business_id=$1", f.business); n != 1 {
		t.Fatalf("tickets = %d, want 1", n)
	}
	if n := f.count(t, "SELECT count(*) FROM ticket_message WHERE message_id='p1@customer.test'"); n != 1 {
		t.Fatalf("messages = %d, want 1", n)
	}
	if n := f.count(t, "SELECT count(*) FROM mailing_provider_webhook_delivery WHERE external_event_id='evt-p'"); n != 1 {
		t.Fatalf("delivery rows = %d, want 1 (re-recorded)", n)
	}
}

// I1 / R12: a business whose Resend profile receives mail but which has no custom
// address on a verified provider_route domain (campaign-only) is never fetched
// from Resend and never audited; the event is still recorded and acknowledged.
func TestResendReceivedForCampaignOnlyBusinessFetchesNothing(t *testing.T) {
	for name, strip := range map[string]string{
		"no custom address": "DELETE FROM inbound_address WHERE business_id=$1 AND kind='custom'",
		"unverified domain": "UPDATE email_domain SET verified_at=NULL WHERE business_id=$1",
	} {
		t.Run(name, func(t *testing.T) {
			f := newLoopFixture(t)
			if _, err := f.tdb.Super.Exec(f.ctx, strip, f.business); err != nil {
				t.Fatal(err)
			}
			f.receive(t, "evt-c", "em_c", customerMail("ada@customer.test", f.address, "c@customer.test", "", "hello"))
			if n := f.resend.fetchCount(); n != 0 {
				t.Fatalf("Receiving API fetches = %d, want 0", n)
			}
			if n := f.count(t, "SELECT count(*) FROM audit_entry WHERE business_id=$1 AND action='ticket.inbound_dropped'", f.business); n != 0 {
				t.Fatalf("inbound_dropped audit rows = %d, want 0", n)
			}
			if n := f.count(t, "SELECT count(*) FROM ticket WHERE business_id=$1", f.business); n != 0 {
				t.Fatalf("tickets = %d, want 0", n)
			}
			if n := f.count(t, "SELECT count(*) FROM mailing_provider_webhook_delivery WHERE profile_id=$1 AND external_event_id='evt-c'", f.profileID); n != 1 {
				t.Fatalf("delivery rows = %d, want 1 (recorded as processed)", n)
			}
		})
	}
}
