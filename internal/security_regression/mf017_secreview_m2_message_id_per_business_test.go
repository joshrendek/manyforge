// Finding: SECREVIEW-M2 — tenant-scoped Message-ID deduplication let one business
// suppress another business's mail. Fixed in migrations/0141: idempotency is
// UNIQUE (business_id, message_id) and ingest_inbound_message's duplicate lookups
// and ON CONFLICT target include business_id. Behavioural test:
// internal/inbox/ingest_dedup_scope_integration_test.go (TestIngestMessageIDDedupIsPerBusiness).
package security_regression

import (
	"strings"
	"testing"
)

func TestPin_SECREVIEW_M2_MessageIDPerBusiness(t *testing.T) {
	mig := mustRead(t, "../../migrations/0141_support_message_id_per_business.up.sql")
	for _, pin := range []string{
		"ADD CONSTRAINT ticket_message_business_id_message_id_key UNIQUE (business_id, message_id);",
		"DROP CONSTRAINT ticket_message_tenant_root_id_message_id_key;",
	} {
		if !strings.Contains(mig, pin) {
			t.Errorf("0141 missing %q", pin)
		}
	}

	// No migration after 0141 may restore the tenant-scoped key.
	for _, f := range secreviewMigrationsMentioning(t, "UNIQUE (tenant_root_id, message_id)") {
		if f[:4] > "0141" {
			t.Errorf("%s re-adds UNIQUE (tenant_root_id, message_id)", f)
		}
	}

	fn, file := secreviewLatestFunction(t, "ingest_inbound_message")
	if got := strings.Count(fn, "WHERE tenant_root_id = p_tenant_root_id AND business_id = p_business_id\n"); got != 2 {
		t.Errorf("%s ingest_inbound_message: %d business-scoped duplicate lookups, want 2", file, got)
	}
	if !strings.Contains(fn, "ON CONFLICT (business_id, message_id) DO NOTHING") {
		t.Errorf("%s ingest_inbound_message: ON CONFLICT target must be (business_id, message_id)", file)
	}
	if strings.Contains(fn, "ON CONFLICT (tenant_root_id, message_id)") ||
		strings.Contains(fn, "WHERE tenant_root_id = p_tenant_root_id AND message_id = p_message_id") {
		t.Errorf("%s ingest_inbound_message still dedupes by tenant root only", file)
	}
}
