//go:build integration

package inbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

type resendTenant struct {
	business uuid.UUID
	domain   string
	address  string
}

// seedResendTenant: a business (with its system address) plus a provider_route
// domain and a custom support@ address on it.
func seedResendTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB, verified bool) resendTenant {
	t.Helper()
	ten := seedIngestTenant(ctx, t, tdb)
	domain := "support-" + ten.master.String()[:8] + ".example.com"
	domainID := uuid.New()
	var verifiedAt any
	if verified {
		verifiedAt = time.Now().UTC()
	}
	if _, err := tdb.Super.Exec(ctx, `INSERT INTO email_domain
		(id,business_id,tenant_root_id,domain,mode,verify_token,verified_at,dkim_selector,dkim_public_key,dkim_private_key_ref,spf_state,created_at,updated_at)
		VALUES ($1,$2,$2,$3,'provider_route','mf-verify=seed',$4,'mfseed','cHVi','cmVm','unknown',now(),now())`,
		domainID, ten.master, domain, verifiedAt); err != nil {
		t.Fatalf("seed email_domain: %v", err)
	}
	addr := "support@" + domain
	if _, err := tdb.Super.Exec(ctx, `INSERT INTO inbound_address
		(id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at)
		VALUES ($1,$2,$2,$3,'custom',$4,now(),now())`, uuid.New(), ten.master, addr, domainID); err != nil {
		t.Fatalf("seed inbound_address: %v", err)
	}
	return resendTenant{business: ten.master, domain: domain, address: addr}
}

func resendRaw(from, to, cc, subject, messageID, inReplyTo, contentType, body string) []byte {
	msg := "From: " + from + "\r\nTo: " + to + "\r\n"
	if cc != "" {
		msg += "Cc: " + cc + "\r\n"
	}
	msg += "Subject: " + subject + "\r\nDate: Mon, 02 Jan 2006 15:04:05 -0700\r\nMessage-ID: <" + messageID + ">\r\n"
	if inReplyTo != "" {
		msg += "In-Reply-To: <" + inReplyTo + ">\r\nReferences: <" + inReplyTo + ">\r\n"
	}
	msg += "MIME-Version: 1.0\r\nContent-Type: " + contentType + "\r\n\r\n" + body + "\r\n"
	return []byte(msg)
}

func ticketCount(ctx context.Context, t *testing.T, tdb *testdb.TestDB, business uuid.UUID) int {
	t.Helper()
	return countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket WHERE business_id=$1", business)
}

func startResendDB(t *testing.T) (context.Context, *testdb.TestDB, *Service) {
	t.Helper()
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	return ctx, tdb, newIngestService(ctx, t, tdb)
}

// Review Focus 2: display names, several recipients, foreign and other-business
// addresses in To/Cc — routes once, into the receiving business only.
func TestReceiveResendRoutesOwnedRecipientAmongDisplayNamesAndForeignAddresses(t *testing.T) {
	ctx, tdb, svc := startResendDB(t)
	own := seedResendTenant(ctx, t, tdb, true)
	other := seedResendTenant(ctx, t, tdb, true)
	raw := resendRaw("Ada Customer <ada@customer.test>",
		`"Someone Else" <help@foreign.example>, "Garden Support" <SUPPORT@`+own.domain+`>`,
		`"Other Biz" <`+other.address+`>`, "Need help", "rs-1@customer.test", "", "text/plain; charset=utf-8", "hello")
	if err := svc.ReceiveResend(ctx, own.business, "evt-1", nil, raw); err != nil {
		t.Fatalf("ReceiveResend: %v", err)
	}
	if n := ticketCount(ctx, t, tdb, own.business); n != 1 {
		t.Fatalf("own tickets = %d, want 1", n)
	}
	if n := ticketCount(ctx, t, tdb, other.business); n != 0 {
		t.Fatalf("other business tickets = %d, want 0 (same-business rule)", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM audit_entry WHERE business_id=$1 AND inputs->>'source'='inbox:resend'", own.business); n == 0 {
		t.Fatalf("no ingest audit with source inbox:resend")
	}
}

// R4: mail addressed only to another business is dropped with an audit entry
// and the uniform nil (same response as an unknown recipient).
func TestReceiveResendDropsMailForAnotherBusiness(t *testing.T) {
	ctx, tdb, svc := startResendDB(t)
	own := seedResendTenant(ctx, t, tdb, true)
	other := seedResendTenant(ctx, t, tdb, true)
	raw := resendRaw("ada@customer.test", other.address, "", "wrong", "rs-2@customer.test", "", "text/plain", "x")
	if err := svc.ReceiveResend(ctx, own.business, "evt-cross", []string{other.address}, raw); err != nil {
		t.Fatalf("ReceiveResend: %v", err)
	}
	if ticketCount(ctx, t, tdb, own.business) != 0 || ticketCount(ctx, t, tdb, other.business) != 0 {
		t.Fatal("cross-business mail created a ticket")
	}
	if n := countSuper(ctx, t, tdb.Super, `SELECT count(*) FROM audit_entry
		WHERE business_id=$1 AND action='ticket.inbound_dropped' AND inputs->>'event_id'='evt-cross'`, own.business); n != 1 {
		t.Fatalf("drop audits = %d, want 1", n)
	}
}

func TestReceiveResendDropsUnverifiedDomain(t *testing.T) {
	ctx, tdb, svc := startResendDB(t)
	own := seedResendTenant(ctx, t, tdb, false)
	raw := resendRaw("ada@customer.test", own.address, "", "early", "rs-3@customer.test", "", "text/plain", "x")
	if err := svc.ReceiveResend(ctx, own.business, "evt-unverified", nil, raw); err != nil {
		t.Fatalf("ReceiveResend: %v", err)
	}
	if n := ticketCount(ctx, t, tdb, own.business); n != 0 {
		t.Fatalf("tickets = %d, want 0 for an unverified domain", n)
	}
}

// Scenario 4: a reply that drops In-Reply-To/References still threads by the
// +token on the chosen To address.
func TestReceiveResendThreadsByReplyTokenWithoutHeaders(t *testing.T) {
	ctx, tdb, svc := startResendDB(t)
	own := seedResendTenant(ctx, t, tdb, true)
	first := resendRaw("ada@customer.test", own.address, "", "Order 12", "rs-4@customer.test", "", "text/plain", "first")
	if err := svc.ReceiveResend(ctx, own.business, "evt-4a", nil, first); err != nil {
		t.Fatal(err)
	}
	var ticketID uuid.UUID
	var token string
	if err := tdb.Super.QueryRow(ctx, "SELECT id, reply_token FROM ticket WHERE business_id=$1", own.business).Scan(&ticketID, &token); err != nil {
		t.Fatal(err)
	}
	reply := resendRaw("ada@customer.test", "Garden Support <support+"+token+"@"+own.domain+">", "",
		"totally different subject", "rs-5@customer.test", "", "text/plain", "second")
	if err := svc.ReceiveResend(ctx, own.business, "evt-4b", nil, reply); err != nil {
		t.Fatal(err)
	}
	if n := ticketCount(ctx, t, tdb, own.business); n != 1 {
		t.Fatalf("tickets = %d, want 1 (token threading)", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket_message WHERE ticket_id=$1", ticketID); n != 2 {
		t.Fatalf("messages = %d, want 2", n)
	}
}

// Review Focus 1: HTML-only mail must produce a readable body.
func TestReceiveResendHTMLOnlyMessageHasReadableBody(t *testing.T) {
	ctx, tdb, svc := startResendDB(t)
	own := seedResendTenant(ctx, t, tdb, true)
	raw := resendRaw("ada@customer.test", own.address, "", "html", "rs-6@customer.test", "",
		"text/html; charset=utf-8", "<html><body><p>Hello <b>there</b>, my order is late.</p></body></html>")
	if err := svc.ReceiveResend(ctx, own.business, "evt-html", nil, raw); err != nil {
		t.Fatal(err)
	}
	var bodyText string
	var bodyHTML *string
	if err := tdb.Super.QueryRow(ctx, `SELECT COALESCE(m.body_text,''), m.body_html FROM ticket_message m
		JOIN ticket t ON t.id = m.ticket_id WHERE t.business_id=$1`, own.business).Scan(&bodyText, &bodyHTML); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodyText, "my order is late") || bodyHTML == nil {
		t.Fatalf("body_text = %q, body_html nil = %v; want readable text and the html kept", bodyText, bodyHTML == nil)
	}
}

// Review Focus 2: a Bcc'd support address (not in To/Cc) routes through the
// authenticated envelope list; another business's envelope address does not.
func TestReceiveResendFallsBackToEnvelopeForBcc(t *testing.T) {
	ctx, tdb, svc := startResendDB(t)
	own := seedResendTenant(ctx, t, tdb, true)
	other := seedResendTenant(ctx, t, tdb, true)
	raw := resendRaw("ada@customer.test", "friend@foreign.example", "", "bcc", "rs-7@customer.test", "", "text/plain", "x")
	if err := svc.ReceiveResend(ctx, own.business, "evt-bcc", []string{own.address}, raw); err != nil {
		t.Fatal(err)
	}
	if n := ticketCount(ctx, t, tdb, own.business); n != 1 {
		t.Fatalf("own tickets = %d, want 1 via envelope fallback", n)
	}
	raw2 := resendRaw("ada@customer.test", "friend@foreign.example", "", "bcc2", "rs-8@customer.test", "", "text/plain", "x")
	if err := svc.ReceiveResend(ctx, own.business, "evt-bcc2", []string{other.address}, raw2); err != nil {
		t.Fatal(err)
	}
	if ticketCount(ctx, t, tdb, own.business) != 1 || ticketCount(ctx, t, tdb, other.business) != 0 {
		t.Fatal("envelope fallback routed to another business")
	}
}

func TestReceiveResendDuplicateRawIsIdempotent(t *testing.T) {
	ctx, tdb, svc := startResendDB(t)
	own := seedResendTenant(ctx, t, tdb, true)
	raw := resendRaw("ada@customer.test", own.address, "", "dup", "rs-9@customer.test", "", "text/plain", "x")
	for i := 0; i < 2; i++ {
		if err := svc.ReceiveResend(ctx, own.business, "evt-dup", nil, raw); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if n := countSuper(ctx, t, tdb.Super, `SELECT count(*) FROM ticket_message m JOIN ticket t ON t.id=m.ticket_id
		WHERE t.business_id=$1`, own.business); n != 1 {
		t.Fatalf("messages = %d, want 1", n)
	}
}
