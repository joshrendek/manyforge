// Finding MF-017-DEFINERS (doc_support017.go / spec 017 R7 + data model).
package security_regression

import (
	"regexp"
	"strings"
	"testing"
)

func TestPin_MF017DefinersAndGlobalUniqueness(t *testing.T) {
	mig := mustRead(t, "../../migrations/0139_support_email_resend.up.sql")
	for _, fn := range []string{
		"resolve_inbound_address_for_business", "get_resend_support_identity",
		"set_message_dropped_attachments", "support_audit_inbound_dropped",
		"mailing_received_webhook_state", "mailing_record_received_webhook",
		"mailing_list_verified_resend_profiles", "support_business_receives_via_resend",
	} {
		body := mf017SQLFunction(t, mig, fn)
		if !strings.Contains(body, "SECURITY DEFINER") || !strings.Contains(body, "SET search_path = ") {
			t.Errorf("%s is not a search-path-pinned SECURITY DEFINER", fn)
		}
		// Whole statements, so `FROM nobody` / `TO someone_else` cannot satisfy the pin.
		revoke := regexp.MustCompile(`(?m)^REVOKE ALL ON FUNCTION ` + regexp.QuoteMeta(fn) + `\([^)]*\) FROM PUBLIC;$`)
		grant := regexp.MustCompile(`(?m)^GRANT EXECUTE ON FUNCTION ` + regexp.QuoteMeta(fn) + `\([^)]*\) TO manyforge_app;$`)
		if !revoke.MatchString(mig) {
			t.Errorf("%s must REVOKE ALL ... FROM PUBLIC", fn)
		}
		if !grant.MatchString(mig) {
			t.Errorf("%s must GRANT EXECUTE ... TO manyforge_app", fn)
		}
	}
	for _, pin := range []string{
		"CREATE UNIQUE INDEX email_domain_verified_global_uq",
		"WHERE verified_at IS NOT NULL",
		"CREATE UNIQUE INDEX inbound_address_custom_global_uq",
		"RAISE EXCEPTION '0139: a verified email domain is shared",
	} {
		if !strings.Contains(mig, pin) {
			t.Errorf("0139 missing %q", pin)
		}
	}
}
