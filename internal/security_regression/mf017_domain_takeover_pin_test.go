// MF-017 domain takeover-on-stale-verification.
//
// Verified email domains are globally unique (migration 0139). Without a
// takeover path, a business that lost DNS control would hold the domain forever.
// These source-level pins keep the takeover narrowly scoped: a SECURITY DEFINER
// that decides server-side from the observed TXT values, never trusting the caller.
package security_regression

import (
	"os"
	"strings"
	"testing"
)

func TestMF017DomainTakeoverIsScopedDefiner(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/0139_support_email_resend.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	i := strings.Index(sql, "CREATE FUNCTION email_domain_takeover_stale(")
	if i < 0 {
		t.Fatal("takeover DEFINER missing from migration 0139")
	}
	body := sql[i:]
	body = body[:strings.Index(body, "$$;\n")+4]
	for _, want := range []string{
		"SECURITY DEFINER SET search_path = public",
		"v_claimant.verify_token = ANY",        // claimant must prove control now
		"v_incumbent.verify_token = ANY",       // incumbent still proving control blocks takeover
		"business_id = p_claimant_business_id", // claimant ownership predicate in SQL
		"DELETE FROM inbound_address",          // incumbent's custom rows must not lock out the claimant
		"'removed_addresses'",                  // and the removal is audited
		"'email_domain.verification_lost'",     // demotion is audited on the incumbent
	} {
		if !strings.Contains(body, want) {
			t.Errorf("takeover DEFINER lost %q", want)
		}
	}
	for _, want := range []string{
		"REVOKE ALL ON FUNCTION email_domain_takeover_stale(uuid, uuid, text[], uuid) FROM PUBLIC;",
		"GRANT EXECUTE ON FUNCTION email_domain_takeover_stale(uuid, uuid, text[], uuid) TO manyforge_app;",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("migration 0139 lost %q", want)
		}
	}
}

func TestMF017VerifyCallsTakeoverBeforeMarking(t *testing.T) {
	raw, err := os.ReadFile("../ticketing/identity.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	a := strings.Index(src, "email_domain_takeover_stale(")
	b := strings.Index(src, "q.MarkEmailDomainVerified(")
	if a < 0 || b < 0 || a > b {
		t.Fatal("VerifyEmailDomain must call email_domain_takeover_stale before MarkEmailDomainVerified")
	}
}
