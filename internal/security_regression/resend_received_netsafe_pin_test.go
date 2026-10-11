// Finding: spec 017 — the pre-signed Resend raw-message URL is caller-influenced,
// so production MUST fetch it with the SSRF-guarded netsafe client, and an
// event is recorded only after a successful ingest (R5).
package security_regression

import (
	"os"
	"strings"
	"testing"
)

func TestResendReceivedRawDownloadUsesNetsafe(t *testing.T) {
	src, err := os.ReadFile("../../cmd/manyforge/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "mailingWebhookH.RawDownloadClient = netsafe.NewClient(") {
		t.Fatal("cmd/manyforge/main.go must set mailingWebhookH.RawDownloadClient to a netsafe client (SSRF guard)")
	}
}

func TestResendReceivedRecordsOnlyAfterIngest(t *testing.T) {
	src, err := os.ReadFile("../mailing/resend_received.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	ingest := strings.Index(s, "h.Inbound.ReceiveResend(")
	record := strings.LastIndex(s, "h.finishReceived(w, r, wc, eventID)")
	if ingest < 0 || record < ingest {
		t.Fatal("handleResendReceived must record the event only after ReceiveResend succeeds")
	}
	// recordReceived is reachable only through finishReceived.
	if strings.Count(s, "h.recordReceived(") != 1 {
		t.Fatal("recordReceived must be called solely from finishReceived")
	}
}
