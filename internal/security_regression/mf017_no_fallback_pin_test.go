// Finding MF-017-NO-FALLBACK (doc_support017.go / spec 017 R11).
package security_regression

import (
	"strings"
	"testing"
)

func TestPin_MF017NoSharedTransportFallback(t *testing.T) {
	src := mustRead(t, "../platform/notify/sender_subscriber.go")
	if !strings.Contains(src, "return s.sendViaSupportProfile(ctx, tx, e, p, mail, identity)") {
		t.Error("Handle must RETURN the support-profile path (no fall-through to Sender)")
	}
	body := mf017Slice(t, src, "func (s SendSubscriber) sendViaSupportProfile(", "func (s SendSubscriber) markFinalFailure(")
	if strings.Contains(body, "s.Sender") {
		t.Error("sendViaSupportProfile must never use the shared Sender")
	}
	mig := mustRead(t, "../../migrations/0139_support_email_resend.up.sql")
	if strings.Contains(mf017SQLFunction(t, mig, "get_resend_support_identity"), "p.status = 'verified'") {
		t.Error("get_resend_support_identity must return non-verified profiles so the sender refuses instead of falling back")
	}
}
