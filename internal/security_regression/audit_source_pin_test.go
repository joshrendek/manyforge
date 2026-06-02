// No build tag: this source-level pin runs in `make test` and `make sec-test` with
// no infrastructure (T065 / SC-005 / FR-014). The behavioral "one audit_entry per
// mutation" proof lives in internal/ticketing/audit_integration_test.go (Docker-gated).
// This pin guards two things that a refactor could silently drop even while the
// behavioral matrix is weakened or skipped:
//   1. principal-less INGEST records its source label and a NULL actor (the
//      ingest_inbound_message DEFINER, migration 0024); and
//   2. every principal-driven mutation service method still calls audit.Write with
//      its frozen action string.
// FR-014 requires 100% of mutations audited with actor/source — dropping any of
// these must fail CI loudly regardless of Docker availability.

package security_regression

import (
	"strings"
	"testing"
)

// TestIngestAuditSourcePinned asserts the principal-less ingestion DEFINER records
// the ingestion source in inputs and a NULL actor for every audit it writes.
func TestIngestAuditSourcePinned(t *testing.T) {
	mig := mustRead(t, "../../migrations/0024_loop_guard.up.sql")
	cases := []struct{ name, fragment string }{
		// Every audit the DEFINER writes is principal-less (actor_principal_id = NULL).
		// This exact VALUES prefix is shared by the create/append, reopen, and
		// loop-suppression audits — all four ingest-path actions.
		{"principal-less actor", "VALUES (gen_random_uuid(), p_business_id, p_tenant_root_id, NULL,"},
		// The ingestion source label is captured in inputs (FR-014).
		{"source label in inputs", "'source', p_source"},
		// The create-vs-append action is recorded.
		{"create/append action", "CASE WHEN v_created THEN 'ticket.created' ELSE 'ticket.message.received' END"},
		// The reopen flip is audited (principal-less), same tx.
		{"reopen audited", "'ticket.status_changed', 'ticket', v_ticket_id"},
	}
	for _, c := range cases {
		if !strings.Contains(mig, c.fragment) {
			t.Errorf("%s: ingest-source pin %q missing — was the principal-less ingest audit/source dropped (FR-014)?", c.name, c.fragment)
		}
	}
}

// TestEveryMutationAuditsPinned asserts each principal-driven support mutation still
// emits its in-tx audit. We check the action literal is present alongside an
// audit.Write call in the file that owns that mutation.
func TestEveryMutationAuditsPinned(t *testing.T) {
	svc := mustRead(t, "../ticketing/service.go")
	identity := mustRead(t, "../ticketing/identity.go")

	if !strings.Contains(svc, "audit.Write(") {
		t.Error("ticketing/service.go no longer calls audit.Write — mutation auditing dropped (FR-014/SC-005)")
	}
	if !strings.Contains(identity, "audit.Write(") {
		t.Error("ticketing/identity.go no longer calls audit.Write — identity-config auditing dropped (FR-014/SC-005)")
	}

	// service.go: ticket reply/note + the four triage facets.
	for _, action := range []string{
		`"ticket.replied"`,
		`"ticket.noted"`,
		`"ticket.status_changed"`,
		`"ticket.priority_changed"`,
		`"ticket.tags_changed"`,
		`"ticket.assigned"`,
	} {
		if !strings.Contains(svc, action) {
			t.Errorf("ticketing/service.go missing audited action %s — mutation went unaudited (SC-005)", action)
		}
	}
	// identity.go: domain + inbound-address config.
	for _, action := range []string{
		`"email_domain.created"`,
		`"email_domain.verified"`,
		`"inbound_address.created"`,
	} {
		if !strings.Contains(identity, action) {
			t.Errorf("ticketing/identity.go missing audited action %s — identity mutation went unaudited (SC-005)", action)
		}
	}
}
