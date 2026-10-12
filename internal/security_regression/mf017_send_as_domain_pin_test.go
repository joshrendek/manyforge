// Finding MF-017-SEND-AS-DOMAIN (doc_support017.go / spec 017 addendum-send-as.md).
package security_regression

import (
	"regexp"
	"strings"
	"testing"
)

func TestPin_MF017SendAsDomain(t *testing.T) {
	// The domain rule and the ownership predicate live in ONE UPDATE (no TOCTOU).
	q := mustRead(t, "../../db/query/identity.sql")
	upd := mf017Slice(t, q, "-- name: SetInboundAddressSendAs :one", "RETURNING ia.*;")
	for _, pin := range []string{
		"ia.business_id = sqlc.arg('business_id')::uuid",
		"ia.tenant_root_id = sqlc.arg('tenant_root_id')::uuid",
		"ia.kind = 'custom'",
		"p.business_id = ia.business_id",
		"p.tenant_root_id = ia.tenant_root_id",
		"p.mode = 'resend'",
		"p.status = 'verified'",
		"lower(split_part(p.from_email::text, '@', 2)) = lower(split_part(sqlc.narg('send_as')::text, '@', 2))",
	} {
		if !strings.Contains(upd, pin) {
			t.Errorf("SetInboundAddressSendAs missing %q", pin)
		}
	}

	// The service audits through that UPDATE in the same principal-bound tx.
	svc := mustRead(t, "../ticketing/identity.go")
	body := mf017Slice(t, svc, "func (s *IdentityService) SetInboundAddressSendAs(", "// ListInboundAddresses returns")
	for _, pin := range []string{"s.DB.WithPrincipal(ctx, principalID,", "q.GetInboundAddressForUpdate(",
		"q.SetInboundAddressSendAs(", `Action:           "inbound_address.send_as_updated"`, "OldValue:"} {
		if !strings.Contains(body, pin) {
			t.Errorf("SetInboundAddressSendAs service missing %q", pin)
		}
	}

	// The replaced identity DEFINER keeps 0139's hardening and exposes the inbound address.
	mig := mustRead(t, "../../migrations/0140_support_inbound_send_as.up.sql")
	fn := mf017SQLFunction(t, mig, "get_resend_support_identity")
	for _, pin := range []string{"SECURITY DEFINER", "SET search_path = public",
		"COALESCE(ia.send_as, ia.address)::text", "ia.address::text", "inbound_address text"} {
		if !strings.Contains(fn, pin) {
			t.Errorf("0140 get_resend_support_identity missing %q", pin)
		}
	}
	if strings.Contains(fn, "p.status = 'verified'") {
		t.Error("get_resend_support_identity must return non-verified profiles so the sender refuses instead of falling back")
	}
	if !regexp.MustCompile(`(?m)^REVOKE ALL ON FUNCTION get_resend_support_identity\(uuid, uuid\) FROM PUBLIC;$`).MatchString(mig) {
		t.Error("0140 must REVOKE ALL ... FROM PUBLIC on get_resend_support_identity")
	}
	if !regexp.MustCompile(`(?m)^GRANT EXECUTE ON FUNCTION get_resend_support_identity\(uuid, uuid\) TO manyforge_app;$`).MatchString(mig) {
		t.Error("0140 must GRANT EXECUTE ... TO manyforge_app on get_resend_support_identity")
	}

	// Reply-To is always built from the Resend-routed inbound address, never send_as.
	sender := mustRead(t, "../platform/notify/sender_subscriber.go")
	send := mf017Slice(t, sender, "func (s SendSubscriber) sendViaSupportProfile(", "func (s SendSubscriber) markFinalFailure(")
	if !strings.Contains(send, "mail.ReplyTo = verpAddress(id.inboundAddress, p.ReplyToken)") {
		t.Error("sendViaSupportProfile must build Reply-To from id.inboundAddress")
	}
	if strings.Contains(send, "verpAddress(id.fromAddress") {
		t.Error("sendViaSupportProfile must not derive Reply-To from the send_as From")
	}
}
