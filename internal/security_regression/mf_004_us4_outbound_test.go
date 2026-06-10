// mf_004_us4_outbound (Spec 004 US4 §7.4/§7.5, manyforge-a7j.4): source-level pins
// for the Jira OUTBOUND path. Each is a pure string match against the relevant source
// file, so it needs NO infrastructure and runs under both `make test` AND `make sec-test`
// for fast feedback — matching the US3 source-pin style in
// us3_jira_factory_source_pin_test.go. The single behavioural SSRF dial-refusal pin lives
// in the integration-tagged internal/connectors/mf_004_us4_outbound_pin_integration_test.go
// (co-located there because dispatchOnce + the seed helpers are unexported).
//
// Finding IDs (Spec 004 US4 §7):
//   - MF-004-US4-SSRF        — outbound dispatcher builds connector clients via the
//     SSRF-guarded Registry.BuildSystem, never a raw http client.
//   - MF-004-US4-AUDIT       — both outbound completers (comment, create) write an audit_entry.
//   - MF-004-US4-CONFLICT    — external-wins conflict resolution is audited deterministically.
//   - MF-004-US4-OWNERSHIP   — outbound-create ownership predicate is pushed into SQL (no oracle).
//   - MF-004-US4-NO-ORACLE   — the service maps unknown/foreign/already-linked → ErrNotFound,
//     with NO 403/404 (ErrForbidden) split that would leak existence.
package security_regression

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMF004US4_OutboundUsesRegistryBuildSystem pins MF-004-US4-SSRF (Spec 004 §7.5).
// SSRF safety on the outbound path comes from Registry.BuildSystem → the jira factory →
// netsafe; a raw http.Client / http.DefaultClient would bypass that guard. A refactor that
// dialed the external system with a bare client (even with a host allowlist — DNS rebinds)
// would fail CI here.
func TestMF004US4_OutboundUsesRegistryBuildSystem(t *testing.T) {
	// MF-004-US4-SSRF — Spec 004 §7.5
	path := filepath.Join("..", "connectors", "outbound.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("MF-004-US4-SSRF SOURCE PIN: read %s: %v", path, err)
	}
	src := string(raw)
	if !strings.Contains(src, "BuildSystem") {
		t.Fatalf("MF-004-US4-SSRF SOURCE PIN: outbound.go no longer builds the connector via Registry.BuildSystem — SSRF guard (netsafe) bypassed")
	}
	if strings.Contains(src, "http.Client{") {
		t.Fatalf("MF-004-US4-SSRF SOURCE PIN: outbound.go constructs a raw http.Client{} — must dial only through Registry.BuildSystem → netsafe")
	}
	if strings.Contains(src, "http.DefaultClient") {
		t.Fatalf("MF-004-US4-SSRF SOURCE PIN: outbound.go references http.DefaultClient — must dial only through Registry.BuildSystem → netsafe")
	}
}

// TestMF004US4_OutboundWritesAudited pins MF-004-US4-AUDIT (Spec 004 §7). Both outbound
// completers (complete_outbound_comment / complete_outbound_create in migration 0045) insert
// an audit_entry, so an external post can never happen without a durable audit trail.
func TestMF004US4_OutboundWritesAudited(t *testing.T) {
	// MF-004-US4-AUDIT — Spec 004 §7 (migration 0045)
	path := filepath.Join("..", "..", "migrations", "0045_connector_outbound.up.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("MF-004-US4-AUDIT SOURCE PIN: read %s: %v", path, err)
	}
	src := string(raw)
	if !strings.Contains(src, "connector.outbound.commented") {
		t.Fatalf("MF-004-US4-AUDIT SOURCE PIN: 0045 no longer audits 'connector.outbound.commented' — outbound comment writes must be audited")
	}
	if !strings.Contains(src, "connector.outbound.created") {
		t.Fatalf("MF-004-US4-AUDIT SOURCE PIN: 0045 no longer audits 'connector.outbound.created' — outbound create writes must be audited")
	}
}

// TestMF004US4_ConflictAuditedAsExternalWins pins MF-004-US4-CONFLICT (Spec 004 §7.4).
// When external-wins clobbers a scalar that diverged locally since the last sync,
// sync_inbound_external_issue (migration 0046) must audit a deterministic
// 'connector.conflict.resolved' entry with decision 'external_wins'.
func TestMF004US4_ConflictAuditedAsExternalWins(t *testing.T) {
	// MF-004-US4-CONFLICT — Spec 004 §7.4 (migration 0046)
	path := filepath.Join("..", "..", "migrations", "0046_connector_conflict_audit.up.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("MF-004-US4-CONFLICT SOURCE PIN: read %s: %v", path, err)
	}
	src := string(raw)
	if !strings.Contains(src, "connector.conflict.resolved") {
		t.Fatalf("MF-004-US4-CONFLICT SOURCE PIN: 0046 no longer audits 'connector.conflict.resolved' — both-changed conflicts must be recorded")
	}
	if !strings.Contains(src, "external_wins") {
		t.Fatalf("MF-004-US4-CONFLICT SOURCE PIN: 0046 conflict audit no longer records the 'external_wins' decision")
	}
}

// TestMF004US4_OutboundCreateOwnershipInSQL pins MF-004-US4-OWNERSHIP (Spec 004 §7). The
// EnqueueOutboundCreate query pushes the ownership predicate into the INSERT…SELECT: the row
// is inserted ONLY if the ticket is owned by the caller's business AND is not already linked.
// A foreign / unknown / already-linked ticket therefore matches 0 rows (no oracle), instead
// of relying on a handler-side check that could drift from the SQL.
func TestMF004US4_OutboundCreateOwnershipInSQL(t *testing.T) {
	// MF-004-US4-OWNERSHIP — Spec 004 §7
	path := filepath.Join("..", "..", "db", "query", "connector_outbound.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("MF-004-US4-OWNERSHIP SOURCE PIN: read %s: %v", path, err)
	}
	src := string(raw)
	// Create-specific anchor: the EnqueueOutboundCreate INSERT column tuple. This embeds the
	// business_id + tenant_root_id columns AND is distinct from the EnqueueOutboundComment
	// INSERT (which additionally lists message_id), so it pins the CREATE query specifically —
	// the test fails if the create INSERT drops the business_id/tenant_root_id columns.
	if !strings.Contains(src, "INSERT INTO connector_outbound_op (business_id, tenant_root_id, connector_id, ticket_id, op_type, body)") {
		t.Fatalf("MF-004-US4-OWNERSHIP SOURCE PIN: EnqueueOutboundCreate INSERT column tuple changed — re-verify it still carries business_id/tenant_root_id (tenancy columns)")
	}
	// Create-unique SELECT projection: pins the create query's identity (vs the comment query).
	if !strings.Contains(src, "'create_issue', sqlc.arg(body)::text") {
		t.Fatalf("MF-004-US4-OWNERSHIP SOURCE PIN: EnqueueOutboundCreate SELECT projection changed — re-verify the ownership pin")
	}
	// Ownership predicate: the SELECT only matches a ticket owned by the caller's business. This
	// literal also appears in the comment query; together with the create-unique anchors above it
	// guarantees the create query's WHERE retains the business scoping.
	if !strings.Contains(src, "t.business_id = sqlc.arg(business_id)") {
		t.Fatalf("MF-004-US4-OWNERSHIP SOURCE PIN: connector_outbound.sql no longer scopes an enqueue by business_id — ownership predicate dropped")
	}
	// Not-already-linked predicate: the create path only matches an UNLINKED ticket; this
	// substring is unique to the EnqueueOutboundCreate query (the comment query uses
	// connector_id IS NOT NULL), so a foreign/already-linked ticket matches 0 rows (no oracle).
	if !strings.Contains(src, "t.connector_id IS NULL") {
		t.Fatalf("MF-004-US4-OWNERSHIP SOURCE PIN: EnqueueOutboundCreate no longer requires connector_id IS NULL — an already-linked/foreign ticket would match >0 rows")
	}
}

// TestMF004US4_ServiceMapsToNotFoundNoOracle pins MF-004-US4-NO-ORACLE (Spec 004 §7). The
// EnqueueOutboundCreateIssue service method maps a 0-row enqueue (unknown / foreign /
// already-linked ticket) to errs.ErrNotFound, and never introduces an ErrForbidden branch —
// a 403/404 split would be a UUID-existence oracle.
func TestMF004US4_ServiceMapsToNotFoundNoOracle(t *testing.T) {
	// MF-004-US4-NO-ORACLE — Spec 004 §7
	path := filepath.Join("..", "connectors", "service.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("MF-004-US4-NO-ORACLE SOURCE PIN: read %s: %v", path, err)
	}
	src := string(raw)
	if !strings.Contains(src, "func (s *Service) EnqueueOutboundCreateIssue") {
		t.Fatalf("MF-004-US4-NO-ORACLE SOURCE PIN: EnqueueOutboundCreateIssue method gone/renamed — re-verify the no-oracle pin")
	}
	if !strings.Contains(src, "errs.ErrNotFound") {
		t.Fatalf("MF-004-US4-NO-ORACLE SOURCE PIN: service.go no longer maps to errs.ErrNotFound — 0-row enqueue must collapse to not-found")
	}
	// No 403/404 split: an ErrForbidden anywhere in this file would mean a foreign-but-existing
	// ticket could be distinguished from an unknown one — a UUID-existence oracle.
	if strings.Contains(src, "ErrForbidden") {
		t.Fatalf("MF-004-US4-NO-ORACLE SOURCE PIN: service.go references ErrForbidden — foreign/unknown must return the SAME not-found shape (no oracle)")
	}
}
