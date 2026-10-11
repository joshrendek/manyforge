// Finding MF-017-SAME-BUSINESS (doc_support017.go / spec 017 R4).
package security_regression

import (
	"strings"
	"testing"
)

func TestPin_MF017SameBusinessRouting(t *testing.T) {
	mig := mustRead(t, "../../migrations/0139_support_email_resend.up.sql")
	body := mf017SQLFunction(t, mig, "resolve_inbound_address_for_business")
	for _, pin := range []string{"ia.business_id = p_business_id", "ed.business_id = p_business_id",
		"ia.kind = 'custom'", "ed.verified_at IS NOT NULL", "SET search_path = public"} {
		if !strings.Contains(body, pin) {
			t.Errorf("resolve_inbound_address_for_business missing %q", pin)
		}
	}
	src := mustRead(t, "../inbox/resend.go")
	for _, pin := range []string{"resolveRecipientForBusiness(ctx, tx, businessID, normalized)",
		"RestrictToBusiness: businessID", "support_audit_inbound_dropped"} {
		if !strings.Contains(src, pin) {
			t.Errorf("inbox/resend.go missing %q", pin)
		}
	}
	svc := mustRead(t, "../inbox/service.go")
	if !strings.Contains(svc, "if msg.RestrictToBusiness != uuid.Nil {") ||
		!strings.Contains(svc, "resolveRecipientForBusiness(ctx, tx, msg.RestrictToBusiness, normalizedAddr)") {
		t.Error("Ingest must honor RestrictToBusiness before resolving the recipient")
	}
	if !strings.Contains(mustRead(t, "../mailing/resend_received.go"), "h.Inbound.ReceiveResend(ctx, wc.businessID,") {
		t.Error("the Resend webhook must pass the PROFILE's business id, never one derived from the payload")
	}
}
