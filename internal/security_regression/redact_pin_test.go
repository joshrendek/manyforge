// No build tag: this source-level pin runs in `make test` and `make sec-test` with no
// infrastructure (T066 / FR-014 / Principle VI). The behavioral redact contract lives
// in internal/ticketing/redact_integration_test.go (Docker-gated). This pin locks the
// invariants a refactor could silently break even while the behavioral test is skipped:
//   1. redact is a SOFT update (blank + stamp redacted_at) — NEVER a hard DELETE of the
//      ticket row (Principle VI: data is redacted in place, not destroyed);
//   2. get/list exclude redacted tickets (no post-redaction leak);
//   3. the redact service runs under the caller's RLS principal (no DEFINER), audits
//      in-tx, and enqueues the attachment-blob purge; and
//   4. the DELETE route is gated on tickets.delete with the no-oracle 404 collapse.

package security_regression

import (
	"regexp"
	"strings"
	"testing"
)

func TestRedactIsSoftDeletePinned(t *testing.T) {
	sql := mustRead(t, "../../db/query/ticketing.sql")
	svc := mustRead(t, "../ticketing/redact.go")
	mainGo := mustRead(t, "../../cmd/manyforge/main.go")
	handler := mustRead(t, "../ticketing/handler.go")

	// (1) Redact is the soft UPDATE that blanks the subject + stamps redacted_at...
	if !strings.Contains(sql, "UPDATE ticket\nSET subject = '', redacted_at = now()") {
		t.Error("redact pin: RedactTicket is no longer the soft UPDATE (subject='', redacted_at=now()) — Principle VI/FR-014")
	}
	// ...and there is NO hard DELETE of the ticket table anywhere (the \b word boundary
	// excludes the legitimate `DELETE FROM ticket_tag`, since '_' is a word character).
	if regexp.MustCompile(`(?i)DELETE\s+FROM\s+ticket\b`).MatchString(sql) {
		t.Error("redact pin: a hard `DELETE FROM ticket` appeared — redaction/erasure must be a soft UPDATE (Principle VI/FR-014)")
	}

	// (2) get/list exclude redacted tickets — no leak after redact.
	for _, frag := range []string{
		"WHERE id = $1 AND business_id = $2 AND redacted_at IS NULL", // GetTicket
		"t.redacted_at IS NULL", // ListTickets / ListTicketsAfter
	} {
		if !strings.Contains(sql, frag) {
			t.Errorf("redact pin: ticketing.sql missing redacted-exclusion %q — a redacted ticket would leak", frag)
		}
	}

	// (3) The redact service runs under the caller's RLS principal (no SECURITY DEFINER),
	// audits in-tx, and enqueues the attachment purge.
	for _, frag := range []string{
		"s.DB.WithPrincipal(",
		"q.RedactTicket(",
		`"ticket.redacted"`,
		"events.TopicAttachmentPurge",
	} {
		if !strings.Contains(svc, frag) {
			t.Errorf("redact pin: redact.go missing %q — the redact design (principal-scoped, audited, purge-enqueued) changed", frag)
		}
	}

	// (4) The DELETE route exists and is gated on tickets.delete (no-oracle 404 on lacking perm).
	if !strings.Contains(handler, `r.Delete("/businesses/{id}/tickets/{tid}", h.deleteTicket)`) {
		t.Error("redact pin: the DELETE /businesses/{id}/tickets/{tid} route is gone")
	}
	// authz.PermTicketsDelete since manyforge-xxe (constant→SQL key pinned by
	// TestPin_PermConstantsMatchSeededCatalog).
	if !strings.Contains(mainGo, `httpx.RequirePermission(database, permResolve, authz.PermTicketsDelete, businessIDFromPath)`) {
		t.Error("redact pin: the DELETE route is no longer gated on tickets.delete")
	}
}
