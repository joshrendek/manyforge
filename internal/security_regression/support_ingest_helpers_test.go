//go:build integration

// Shared seeding + ingestion-service wiring for the spec-002 support-desk
// behavioral security regressions (MF-002-THREAD-IDEMPOTENCY, MF-002-INGEST-SCOPE,
// MF-002-MIME-SNIFF). Mirrors the inbox package's integration harness but lives
// here so the regression matrices can assert ground truth via the RLS-exempt
// Super pool while driving the real RLS-subject inbox.Service.

package security_regression

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/manyforge/manyforge/internal/inbox"
	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

const supportSystemDomain = "inbound.localhost"

// supportTenant is one master business + a system inbound address it routes to.
type supportTenant struct {
	business     uuid.UUID
	tenantRootID uuid.UUID
	address      string
}

// seedSupportTenant inserts a master business + a system inbound address
// (kind='system', email_domain_id NULL, always-routing) via the Super pool.
func seedSupportTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) supportTenant {
	t.Helper()
	master := uuid.New()
	addr := fmt.Sprintf("b-%s@%s", master.String()[:8], supportSystemDomain)

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'SupportCo','active',now(),now())`, []any{master}},
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
	return supportTenant{business: master, tenantRootID: master, address: addr}
}

// newSupportIngest wires the real RLS-subject DB + a throwaway file blob store.
func newSupportIngest(ctx context.Context, t *testing.T, tdb *testdb.TestDB) *inbox.Service {
	t.Helper()
	store, err := blob.Open(ctx, "file://"+t.TempDir())
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := inbox.Config{
		ReplyTokenKey:       []byte("test-reply-token-key-0123456789ab"),
		AttachmentMaxBytes:  25 << 20,
		InboundSystemDomain: supportSystemDomain,
	}
	return inbox.NewService(tdb.App, store, cfg, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// supportRFC822 builds a minimal text/plain message; inReplyTo (when non-empty)
// is set as both In-Reply-To and References so header threading can match it.
func supportRFC822(from, to, subject, messageID, inReplyTo, body string) []byte {
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

func supportRaw(addr, from, subject, messageID, inReplyTo, body string) inbox.RawMessage {
	return inbox.RawMessage{
		Provider:          "webhook:test",
		EnvelopeRecipient: addr,
		EnvelopeSender:    from,
		ReceivedAt:        time.Now(),
		Raw:               supportRFC822(from, addr, subject, messageID, inReplyTo, body),
	}
}

func countSuperInt(ctx context.Context, t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}
