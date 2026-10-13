// Finding: SECREVIEW-M4 — a concurrent redelivery could commit an empty orphan
// ticket (both transactions missed the duplicate check; the loser's ticket stayed).
// Fixed in migrations/0141: ingest_inbound_message takes a transaction-scoped
// advisory lock on (business_id, message_id) BEFORE the duplicate check. Behavioural
// test: internal/inbox/ingest_dedup_scope_integration_test.go
// (TestIngestConcurrentRedeliveryNoOrphanTicket).
package security_regression

import (
	"strings"
	"testing"
)

func TestPin_SECREVIEW_M4_IngestAdvisoryLock(t *testing.T) {
	fn, file := secreviewLatestFunction(t, "ingest_inbound_message")
	const lock = "PERFORM pg_advisory_xact_lock(hashtext('ingest_inbound_message'),\n" +
		"                                      hashtext(p_business_id::text || ':' || p_message_id));"
	li := strings.Index(fn, lock)
	if li < 0 {
		t.Fatalf("%s ingest_inbound_message must take the (business_id, message_id) advisory xact lock", file)
	}
	if !strings.Contains(fn, "IF NULLIF(p_message_id, '') IS NOT NULL THEN\n        "+lock) {
		t.Errorf("%s: the advisory lock must be guarded by a non-empty p_message_id", file)
	}
	// The lock must precede the idempotency check and every write.
	for _, later := range []string{"-- Idempotency (FR-005)", "INSERT INTO requester", "INSERT INTO ticket (", "INSERT INTO ticket_message"} {
		if i := strings.Index(fn, later); i < li {
			t.Errorf("%s: %q must come after the advisory lock", file, later)
		}
	}
	// Session-level locks would leak across pooled connections; only the xact form is allowed.
	if strings.Contains(fn, "pg_advisory_lock(") {
		t.Errorf("%s: use pg_advisory_xact_lock, never a session-level advisory lock", file)
	}
}
