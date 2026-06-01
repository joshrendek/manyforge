//go:build integration

package inbox

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// systemDomain is the platform-hosted domain the seeded system inbound address
// lives on; it matches Config.InboundSystemDomain so resolution succeeds.
const systemDomain = "inbound.localhost"

// ingestTenant is a minimally-seeded tenant: one master business and a system
// inbound address (kind='system', email_domain_id NULL) on inbound.localhost.
type ingestTenant struct {
	master       uuid.UUID // business id == tenant_root_id for a master
	tenantRootID uuid.UUID
	address      string // e.g. b-<id>@inbound.localhost
}

// seedIngestTenant seeds a master business + a system inbound address via the
// RLS-exempt Super pool (production auto-provisions this in tenancy.CreateMasterBusiness;
// here we insert directly, the same way the spec-001 regression seeds do). System
// addresses always route (resolve_inbound_address ignores domain verification for
// email_domain_id NULL), so no email_domain row is required.
func seedIngestTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) ingestTenant {
	t.Helper()
	master := uuid.New()
	// A short, unique local part so several tenants can share one system domain.
	addr := fmt.Sprintf("b-%s@%s", master.String()[:8], systemDomain)

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'IngestCo','active',now(),now())`, []any{master}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`, []any{master}},
		{`INSERT INTO inbound_address (id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at) VALUES ($1,$2,$2,$3,'system',NULL,now(),now())`, []any{uuid.New(), master, addr}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return ingestTenant{master: master, tenantRootID: master, address: addr}
}

// newIngestService wires the real RLS-subject DB and a throwaway in-memory blob
// store (file:// under the test tempdir) into the ingestion Service under test.
func newIngestService(ctx context.Context, t *testing.T, tdb *testdb.TestDB) *Service {
	t.Helper()
	store, err := blob.Open(ctx, "file://"+t.TempDir())
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := Config{
		ReplyTokenKey:       []byte("test-reply-token-key-0123456789ab"),
		AttachmentMaxBytes:  25 << 20,
		InboundSystemDomain: systemDomain,
	}
	return NewService(tdb.App, store, cfg, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// rfc822 builds a minimal well-formed text/plain message with the given headers.
func rfc822(from, to, subject, messageID, inReplyTo, body string) []byte {
	msg := "From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\n" +
		"Message-ID: <" + messageID + ">\r\n"
	if inReplyTo != "" {
		msg += "In-Reply-To: <" + inReplyTo + ">\r\n" +
			"References: <" + inReplyTo + ">\r\n"
	}
	msg += "MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		body + "\r\n"
	return []byte(msg)
}

func rawTo(addr, from, subject, messageID, inReplyTo, body string) RawMessage {
	return RawMessage{
		Provider:          "webhook:test",
		EnvelopeRecipient: addr,
		EnvelopeSender:    from,
		ReceivedAt:        time.Now(),
		Raw:               rfc822(from, addr, subject, messageID, inReplyTo, body),
	}
}

// countSuper runs a single-int COUNT via the RLS-exempt Super pool (ground truth).
func countSuper(ctx context.Context, t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// TestIngestCreatesTicketAndRequester (T018) — a well-formed inbound message to a
// seeded system address opens exactly one ticket, dedups one requester, writes one
// inbound ticket_message, and enqueues an outbox event. result.Created is true.
func TestIngestCreatesTicketAndRequester(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb)

	res, err := svc.Ingest(ctx, rawTo(ten.address, "Ada Lovelace <ada@example.com>", "my order is late", "msg-1@example.com", "", "where is my order"))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !res.Created {
		t.Errorf("result.Created = false, want true (a new ticket should have been opened)")
	}

	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE business_id=$1", ten.master); n != 1 {
		t.Errorf("ticket count = %d, want 1", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM requester WHERE tenant_root_id=$1 AND email='ada@example.com'", ten.tenantRootID); n != 1 {
		t.Errorf("requester count for ada@example.com = %d, want 1", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket_message WHERE business_id=$1 AND direction='inbound'", ten.master); n != 1 {
		t.Errorf("inbound ticket_message count = %d, want 1", n)
	}
	// An outbox event must have been enqueued in the same tx (ticket.created and/or
	// message.received) so the worker can fan out notifications.
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM outbox WHERE tenant_root_id=$1 AND topic IN ('ticket.created','message.received')", ten.tenantRootID); n < 1 {
		t.Errorf("outbox event count = %d, want >= 1 (ticket.created/message.received)", n)
	}
}

// TestIngestReplayIdempotent (T018/SC-002) — ingesting the SAME Message-ID twice
// yields exactly one ticket_message and one ticket; the second call reports
// Duplicate.
func TestIngestReplayIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb)

	msg := rawTo(ten.address, "Ada Lovelace <ada@example.com>", "my order is late", "dup-1@example.com", "", "where is my order")

	if _, err := svc.Ingest(ctx, msg); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	res2, err := svc.Ingest(ctx, msg)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if !res2.Duplicate {
		t.Errorf("second ingest Duplicate = false, want true (replay must be a no-op)")
	}

	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket_message WHERE tenant_root_id=$1 AND message_id='dup-1@example.com'", ten.tenantRootID); n != 1 {
		t.Errorf("ticket_message count for replayed message_id = %d, want 1", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE business_id=$1", ten.master); n != 1 {
		t.Errorf("ticket count after replay = %d, want 1 (no duplicate ticket)", n)
	}
}

// TestIngestRequesterDedup (T018/FR-006) — two distinct messages (distinct
// Message-IDs, distinct subjects, no In-Reply-To → two separate tickets) from the
// SAME sender share exactly ONE requester row in the tenant.
func TestIngestRequesterDedup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb)

	if _, err := svc.Ingest(ctx, rawTo(ten.address, "Grace Hopper <grace@example.com>", "first issue", "ded-1@example.com", "", "issue one")); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if _, err := svc.Ingest(ctx, rawTo(ten.address, "Grace Hopper <grace@example.com>", "second issue", "ded-2@example.com", "", "issue two")); err != nil {
		t.Fatalf("second ingest: %v", err)
	}

	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM requester WHERE tenant_root_id=$1 AND email='grace@example.com'", ten.tenantRootID); n != 1 {
		t.Errorf("requester count for grace@example.com = %d, want 1 (deduped within tenant)", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE business_id=$1", ten.master); n != 2 {
		t.Errorf("ticket count = %d, want 2 (distinct subjects, no threading header)", n)
	}
}

// TestIngestRepliesThreadViaReplyToken (manyforge-btv) — an inbound reply carrying
// a ticket's HMAC reply token in the VERP plus-address (support+{token}@domain)
// threads onto that SAME ticket even with NO In-Reply-To/References header. This is
// the reply-token fallback (R4 step 2); it was dead because Ingest signed the token
// over a throwaway uuid (≠ ticket.id) AND normalizeRecipient lowercased the
// case-sensitive token. Both must be fixed for this to thread.
func TestIngestRepliesThreadViaReplyToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb)

	// 1. A first inbound message opens a new ticket; the DEFINER persists its
	//    reply_token (what an outbound reply's Reply-To/VERP address carries).
	first, err := svc.Ingest(ctx, rawTo(ten.address, "Ada Lovelace <ada@example.com>", "login broken", "rt-1@example.com", "", "cannot sign in"))
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if !first.Created {
		t.Fatalf("first ingest Created = false, want true (a new ticket should open)")
	}

	var token string
	if err := tdb.Super.QueryRow(ctx, "SELECT reply_token FROM ticket WHERE id=$1", first.TicketID).Scan(&token); err != nil {
		t.Fatalf("load reply_token: %v", err)
	}

	// 2. A reply lands on the VERP plus-address with NO threading header, so the
	//    reply-token fallback is the ONLY signal that can thread it.
	at := strings.LastIndexByte(ten.address, '@')
	verp := ten.address[:at] + "+" + token + ten.address[at:]
	second, err := svc.Ingest(ctx, rawTo(verp, "Ada Lovelace <ada@example.com>", "Re: login broken", "rt-2@example.com", "", "still cannot sign in"))
	if err != nil {
		t.Fatalf("reply ingest: %v", err)
	}

	// 3. The reply must thread onto the SAME ticket — not open a second one.
	if second.Created {
		t.Errorf("reply ingest Created = true, want false (must thread via reply token)")
	}
	if second.TicketID != first.TicketID {
		t.Errorf("reply threaded to ticket %s, want %s (same ticket)", second.TicketID, first.TicketID)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE business_id=$1", ten.master); n != 1 {
		t.Errorf("ticket count = %d, want 1 (reply must not open a new ticket)", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND direction='inbound'", first.TicketID); n != 2 {
		t.Errorf("inbound message count on ticket = %d, want 2 (original + reply)", n)
	}
}
