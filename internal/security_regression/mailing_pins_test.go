// Finding: Spec 013 mailing is a large business-scoped surface with principal-less public and
// worker consumers. These source pins keep both the tenant boundary and public token boundary
// structural as later campaign and automation slices build on them.
package security_regression

import (
	"os"
	"strings"
	"testing"
)

func TestPin_MailingCoreTenantBoundary(t *testing.T) {
	upBytes, err := os.ReadFile("../../migrations/0124_mailing_core.up.sql")
	if err != nil {
		t.Fatalf("read mailing migration: %v", err)
	}
	up := string(upBytes)
	tables := []string{
		"mailing_sending_profile",
		"mailing_list",
		"mailing_list_key",
		"list_subscriber",
		"subscriber_tag",
		"mailing_suppression",
		"mailing_template",
	}
	for _, table := range tables {
		checks := []string{
			"CREATE TABLE " + table,
			"ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY",
			"CREATE POLICY " + table + "_rls ON " + table + " FOR ALL",
			"CREATE TRIGGER " + table + "_troot_immutable",
			"BEFORE INSERT OR UPDATE OR DELETE ON " + table,
			"('" + table + "', 'mailing', 'drain_fence_then_rewrite', 1)",
		}
		for _, check := range checks {
			if !strings.Contains(up, check) {
				t.Errorf("%s missing %q", table, check)
			}
		}
	}
	if got := strings.Count(up, "authorized_businesses(current_principal())"); got < 2*len(tables) {
		t.Errorf("mailing migration has %d authorized_businesses predicates, want at least %d (USING + WITH CHECK per table)", got, 2*len(tables))
	}
	if strings.Contains(up, "authorized_tenants") {
		t.Fatal("mailing tables are business-scoped; authorized_tenants must not appear")
	}
	for _, fk := range []string{
		"mailing_sending_profile_email_domain_fk",
		"mailing_sending_profile_secret_fk",
		"mailing_list_key_list_fk",
		"list_subscriber_list_fk",
		"list_subscriber_contact_fk",
		"subscriber_tag_list_fk",
		"subscriber_tag_subscriber_fk",
	} {
		if !strings.Contains(up, "CONSTRAINT "+fk) {
			t.Errorf("tenant-preserving FK %q is not explicit", fk)
		}
	}
}

func TestPin_MailingQueriesKeepTenantPredicate(t *testing.T) {
	b, err := os.ReadFile("../../db/query/mailing.sql")
	if err != nil {
		t.Fatalf("read mailing queries: %v", err)
	}
	sql := string(b)
	if strings.Contains(sql, "authorized_tenants") {
		t.Fatal("mailing query file must not broaden scope to authorized_tenants")
	}
	for _, name := range []string{
		"GetMailingList", "UpdateMailingList", "ArchiveMailingList",
		"GetListSubscriber", "UpdateListSubscriber", "UnsubscribeListSubscriber",
		"RevokeMailingListKey", "GetMailingTemplate", "UpdateMailingTemplate",
		"DeleteMailingTemplate", "DeleteMailingSuppression",
	} {
		marker := "-- name: " + name + " "
		start := strings.Index(sql, marker)
		if start < 0 {
			t.Errorf("missing query %s", name)
			continue
		}
		rest := sql[start+len(marker):]
		end := strings.Index(rest, "-- name: ")
		if end >= 0 {
			rest = rest[:end]
		}
		if !strings.Contains(rest, "tenant_root_id") {
			t.Errorf("query %s lacks tenant_root_id predicate", name)
		}
	}
}

func TestPin_MailingPublicDefinersAndTokens(t *testing.T) {
	upBytes, err := os.ReadFile("../../migrations/0125_mailing_public_definers.up.sql")
	if err != nil {
		t.Fatalf("read mailing public migration: %v", err)
	}
	up := string(upBytes)
	for _, fn := range []string{
		"mailing_public_list", "mailing_key_subscribe", "mailing_public_subscribe",
		"mailing_s2s_subscribe", "mailing_confirm", "mailing_unsubscribe",
		"mailing_s2s_unsubscribe", "mailing_relay_identity",
	} {
		start := strings.Index(up, "CREATE FUNCTION "+fn+"(")
		if start < 0 {
			t.Errorf("missing public-boundary function %s", fn)
			continue
		}
		rest := up[start:]
		end := strings.Index(rest, "$$;")
		if end < 0 {
			t.Errorf("function %s has no body terminator", fn)
			continue
		}
		body := rest[:end]
		if !strings.Contains(body, "SECURITY DEFINER") || !strings.Contains(body, "SET search_path = public") {
			t.Errorf("function %s is not a search-path-pinned SECURITY DEFINER", fn)
		}
		if !strings.Contains(up, "REVOKE ALL ON FUNCTION "+fn+"(") {
			t.Errorf("function %s retains default PUBLIC execute", fn)
		}
	}
	if strings.Contains(up, "authorized_tenants") {
		t.Fatal("public mailing DEFINERs must derive scope from the resolved list key, not authorized_tenants")
	}

	codecBytes, err := os.ReadFile("../mailing/token/codec.go")
	if err != nil {
		t.Fatalf("read mailing token codec: %v", err)
	}
	codec := string(codecBytes)
	for _, pin := range []string{"mf-mailing/", "hmac.Equal", "sha256.Sum256", "base64.RawURLEncoding"} {
		if !strings.Contains(codec, pin) {
			t.Errorf("mailing token codec missing security pin %q", pin)
		}
	}

	trackBytes, err := os.ReadFile("../mailing/track.go")
	if err != nil {
		t.Fatalf("read mailing tracking handler: %v", err)
	}
	track := string(trackBytes)
	unsubStart := strings.Index(track, "func (s *Service) Unsubscribe")
	if unsubStart < 0 {
		t.Fatal("missing stateless unsubscribe service boundary")
	}
	unsub := track[unsubStart:]
	decode := strings.Index(unsub, "DecodeUnsubscribe(raw)")
	dbUse := strings.Index(unsub, "s.DB.WithTx")
	if decode < 0 || dbUse < 0 || decode > dbUse {
		t.Fatal("unsubscribe token MAC must be verified before the first database access")
	}
}
