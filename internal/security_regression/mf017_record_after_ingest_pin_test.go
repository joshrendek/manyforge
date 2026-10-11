// Finding MF-017-RECORD-AFTER-INGEST (doc_support017.go / spec 017 R5).
//
// The ingest-before-record order and the single recordReceived call site are
// pinned in resend_received_netsafe_pin_test.go. This adds the parts it does not
// cover: a seen/ignored event short-circuits before any Resend fetch, and the
// handler never records directly.
package security_regression

import (
	"strings"
	"testing"
)

func TestPin_MF017SeenEventShortCircuitsBeforeFetch(t *testing.T) {
	src := mustRead(t, "../mailing/resend_received.go")
	body := mf017Slice(t, src, "func (h *WebhookHandler) handleResendReceived(", "func (h *WebhookHandler) skipOrRetry(")
	guard := strings.Index(body, `if state != "new" {`)
	fetch := strings.Index(body, "client.GetReceivedEmail(")
	if guard < 0 || fetch < 0 || guard > fetch {
		t.Error("a seen/ignored event must short-circuit before any fetch")
	}
	if strings.Contains(body, "mailing_record_received_webhook") {
		t.Error("handleResendReceived must record only through finishReceived")
	}
}
