// Finding: SECREVIEW-L2 — send_as could go stale after the sending profile's
// from_email domain changed. Fixed in migrations/0142: get_resend_support_identity
// uses send_as only while the profile is verified and its from_email domain still
// equals send_as's domain; otherwise From falls back to the inbound address.
// Behavioural test: internal/ticketing/identity_send_as_integration_test.go
// (TestResendIdentityRevalidatesSendAsDomain).
package security_regression

import (
	"regexp"
	"strings"
	"testing"
)

func TestPin_SECREVIEW_L2_SendAsRevalidated(t *testing.T) {
	fn, file := secreviewLatestFunction(t, "get_resend_support_identity")
	for _, pin := range []string{
		"SECURITY DEFINER",
		"SET search_path = public",
		"WHEN ia.send_as IS NOT NULL",
		"AND p.status = 'verified'",
		"AND lower(split_part(p.from_email::text, '@', 2)) = lower(split_part(ia.send_as::text, '@', 2))",
		"THEN ia.send_as",
		"ELSE ia.address",
		"ia.address::text",
	} {
		if !strings.Contains(fn, pin) {
			t.Errorf("%s get_resend_support_identity missing %q", file, pin)
		}
	}
	if strings.Contains(fn, "COALESCE(ia.send_as") {
		t.Errorf("%s get_resend_support_identity must not use send_as unconditionally", file)
	}
	// The status check belongs to the send_as CASE only: a non-verified profile row
	// must still be returned so the sender refuses instead of falling back.
	wi := strings.Index(fn, "WHERE p.business_id")
	if wi < 0 {
		t.Fatalf("%s get_resend_support_identity: WHERE clause not found", file)
	}
	if strings.Contains(fn[wi:], "p.status") {
		t.Errorf("%s get_resend_support_identity must not filter profiles by status", file)
	}

	mig := mustRead(t, "../../migrations/"+file)
	if !regexp.MustCompile(`(?m)^REVOKE ALL ON FUNCTION get_resend_support_identity\(uuid, uuid\) FROM PUBLIC;$`).MatchString(mig) {
		t.Errorf("%s must REVOKE ALL ... FROM PUBLIC on get_resend_support_identity", file)
	}
	if !regexp.MustCompile(`(?m)^GRANT EXECUTE ON FUNCTION get_resend_support_identity\(uuid, uuid\) TO manyforge_app;$`).MatchString(mig) {
		t.Errorf("%s must GRANT EXECUTE ... TO manyforge_app on get_resend_support_identity", file)
	}
}
