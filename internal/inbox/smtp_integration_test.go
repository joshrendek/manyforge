//go:build integration

package inbox

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	smtp "github.com/emersion/go-smtp"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// startSMTPAdapter starts the real SMTPAdapter on an ephemeral loopback port backed
// by the given inbox.Service, returning the dial address and a cleanup func. It uses
// the production listener path (ListenAndServe) on a 127.0.0.1:0-style address.
func startSMTPAdapter(ctx context.Context, t *testing.T, svc *Service) string {
	t.Helper()

	// Reserve an ephemeral port, then hand the resolved address to the adapter. We
	// close our probe listener first so the adapter can bind it (tiny race, fine for
	// a test) — simpler than threading a net.Listener into the adapter.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	adapter := NewSMTPAdapter(addr, svc, svc, 1<<20, nil, discardLogger())
	errCh := make(chan error, 1)
	go func() { errCh <- adapter.ListenAndServe() }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = adapter.Shutdown(shutdownCtx)
		if err := <-errCh; err != nil && !errors.Is(err, smtp.ErrServerClosed) {
			t.Logf("smtp serve returned: %v", err)
		}
	})

	// Wait for the listener to accept connections.
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if derr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("smtp adapter did not come up on %s: %v", addr, derr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return addr
}

// discardLogger returns a slog.Logger that writes to the test nopWriter (defined in
// ingest_integration_test.go), keeping SMTP server-internal noise out of test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(nopWriter{}, nil))
}

// sendSMTP connects to addr and sends one message to the given recipients. It
// returns the protocol error (if any) from the transaction so RCPT rejections can
// be asserted.
func sendSMTP(t *testing.T, addr, from string, to []string, raw []byte) error {
	t.Helper()
	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatalf("dial smtp %s: %v", addr, err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Hello("test.localhost"); err != nil {
		t.Fatalf("HELO: %v", err)
	}
	return c.SendMail(from, to, strings.NewReader(string(raw)))
}

// TestSMTPIngestSameTicketShape (T029/T018) — a message delivered over the REAL
// in-process SMTP adapter to a seeded system address produces the SAME ticket +
// requester + inbound ticket_message shape the webhook/direct path produces. This
// completes the T018 "SMTP ingest → same ticket shape" assertion.
func TestSMTPIngestSameTicketShape(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb)
	addr := startSMTPAdapter(ctx, t, svc)

	raw := rfc822("Ada Lovelace <ada@example.com>", ten.address, "my order is late", "smtp-1@example.com", "", "where is my order")
	if err := sendSMTP(t, addr, "ada@example.com", []string{ten.address}, raw); err != nil {
		t.Fatalf("send to seeded address must be accepted, got: %v", err)
	}

	// Give the goroutine handling the session a brief moment to commit (SendMail
	// returns after the 250 on DATA, which is written AFTER Ingest commits, so this
	// should already be durable; poll defensively to avoid flakes).
	waitForCount(ctx, t, tdb, "SELECT count(*) FROM ticket WHERE business_id=$1", ten.master, 1)

	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE business_id=$1", ten.master); n != 1 {
		t.Errorf("ticket count = %d, want 1 (same shape as webhook path)", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM requester WHERE tenant_root_id=$1 AND email='ada@example.com'", ten.tenantRootID); n != 1 {
		t.Errorf("requester count = %d, want 1", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket_message WHERE business_id=$1 AND direction='inbound'", ten.master); n != 1 {
		t.Errorf("inbound ticket_message count = %d, want 1", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket_message WHERE tenant_root_id=$1 AND message_id='smtp-1@example.com'", ten.tenantRootID); n != 1 {
		t.Errorf("ticket_message with the delivered Message-ID = %d, want 1", n)
	}
}

// TestSMTPUnroutedAddressGenericReject (T029) — delivering to an address that does
// not resolve to a business is rejected with the generic 550 and writes NOTHING
// (no oracle: the reject is identical to any other unrouted address).
func TestSMTPUnroutedAddressGenericReject(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb)
	addr := startSMTPAdapter(ctx, t, svc)

	unrouted := "nobody@" + systemDomain
	raw := rfc822("ada@example.com", unrouted, "hello", "smtp-unrouted-1@example.com", "", "anyone there")
	err = sendSMTP(t, addr, "ada@example.com", []string{unrouted}, raw)
	var se *smtp.SMTPError
	if !errors.As(err, &se) {
		t.Fatalf("unrouted recipient must yield an SMTP error, got: %v", err)
	}
	if se.Code != 550 {
		t.Errorf("unrouted reject code = %d, want 550", se.Code)
	}
	if !strings.Contains(strings.ToLower(se.Message), "recipient rejected") {
		t.Errorf("reject message = %q, want the generic 'recipient rejected'", se.Message)
	}

	// A second unrouted address must produce a BYTE-IDENTICAL rejection (no oracle).
	other := "stranger@" + systemDomain
	raw2 := rfc822("ada@example.com", other, "hi", "smtp-unrouted-2@example.com", "", "hi")
	err2 := sendSMTP(t, addr, "ada@example.com", []string{other}, raw2)
	var se2 *smtp.SMTPError
	if !errors.As(err2, &se2) {
		t.Fatalf("second unrouted recipient must yield an SMTP error, got: %v", err2)
	}
	if se.Code != se2.Code || se.EnhancedCode != se2.EnhancedCode || se.Message != se2.Message {
		t.Errorf("550 reply is an oracle: %q vs %q must be identical", se.Error(), se2.Error())
	}

	// Nothing was written for the rejected deliveries.
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE tenant_root_id=$1", ten.tenantRootID); n != 0 {
		t.Errorf("ticket count after unrouted delivery = %d, want 0", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket_message WHERE tenant_root_id=$1", ten.tenantRootID); n != 0 {
		t.Errorf("ticket_message count after unrouted delivery = %d, want 0", n)
	}
}

// waitForCount polls a COUNT query (via the RLS-exempt Super pool) until it reaches
// want or a short deadline elapses, smoothing over commit-visibility timing.
func waitForCount(ctx context.Context, t *testing.T, tdb *testdb.TestDB, query string, arg any, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if countSuper(ctx, t, tdb.Super, query, arg) >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for count >= %d: %s", want, query)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
