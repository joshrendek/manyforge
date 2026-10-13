//go:build integration

package inbox

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// SECREVIEW-M6: ingest stores only the verdicts of the trusted receiving hop. A
// sender-forged "everything passes" header with an untrusted authserv-id must not
// reach the stored auth_results the agent UI renders.
func TestIngestStoresOnlyTrustedAuthResults(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	ten := seedIngestTenant(ctx, t, tdb)
	svc := newIngestService(ctx, t, tdb) // trusts amazonses.com

	stored := func(provider, messageID, authHeaders string) map[string]string {
		t.Helper()
		raw := []byte("From: CEO <ceo@victim.example>\r\nTo: " + ten.address + "\r\n" +
			"Subject: urgent\r\nMessage-ID: <" + messageID + ">\r\n" + authHeaders + "\r\nbody\r\n")
		res, err := svc.Ingest(ctx, RawMessage{
			Provider: provider, EnvelopeRecipient: ten.address, EnvelopeSender: "ceo@victim.example",
			ReceivedAt: time.Now(), Raw: raw,
		})
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
		var b []byte
		if err := tdb.Super.QueryRow(ctx, "SELECT auth_results FROM ticket_message WHERE id=$1", res.MessageID).Scan(&b); err != nil {
			t.Fatal(err)
		}
		var m map[string]string
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	forged := stored(resendSource, "forged@victim.example",
		"Authentication-Results: attacker.invalid; spf=pass; dkim=pass; dmarc=pass\r\n")
	if forged["spf"] != "" || forged["dkim"] != "" || forged["dmarc"] != "" {
		t.Fatalf("forged-only auth_results = %v, want unknown (empty) verdicts", forged)
	}

	real := stored(resendSource, "real@victim.example",
		"Authentication-Results: attacker.invalid; spf=pass; dkim=pass; dmarc=pass\r\n"+
			"Authentication-Results: amazonses.com; spf=fail smtp.mailfrom=victim.example; dkim=none; dmarc=fail header.from=victim.example\r\n")
	if real["spf"] != "fail" || real["dkim"] != "none" || real["dmarc"] != "fail" {
		t.Fatalf("auth_results = %v, want the trusted amazonses.com verdicts", real)
	}

	// Off the Resend path there is no SES hop above the sender's headers, so even
	// a header naming the trusted authserv-id is sender-written: nothing is trusted.
	webhook := stored("webhook:test", "webhook@victim.example",
		"Authentication-Results: amazonses.com; spf=pass; dkim=pass; dmarc=pass\r\n")
	if webhook["spf"] != "" || webhook["dkim"] != "" || webhook["dmarc"] != "" {
		t.Fatalf("webhook auth_results = %v, want unknown (empty) verdicts", webhook)
	}
}
