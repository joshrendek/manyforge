//go:build integration

package ticketing

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// ---------------------------------------------------------------------------
// T064 — consolidated AUDIT MATRIX (SC-005 "100% of mutations audited" / FR-014).
//
// One place that drives EVERY support mutation through its real service method
// and proves each writes EXACTLY ONE in-tx audit_entry with the correct action,
// target_type, actor_principal_id, and populated old_value/new_value where the
// mutation carries a before/after. This characterization test is EXPECTED TO
// PASS on first run (the behavior already exists); it exists so a dropped or
// mislabeled audit on any mutation fails loudly in ONE file.
//
// Scope of THIS file (package ticketing): the nine principal-bearing mutations —
//   Reply, AddNote, Triage{status,priority,tags,assignee}, and
//   IdentityService{CreateEmailDomain, VerifyEmailDomain, CreateInboundAddress}.
//
// The three PRINCIPAL-LESS ingest audits (ticket.created, ticket.message.received,
// the reopen ticket.status_changed) are emitted by the migration-0024
// ingest_inbound_message SECURITY DEFINER with actor_principal_id = NULL and an
// inputs->>'source' label — they live in package inbox, NOT here. Driving Ingest
// is cheap there (newIngestService + seedIngestTenant), so their audit coverage
// lives in internal/inbox/audit_integration_test.go (created alongside this file)
// plus the T065 DEFINER source-level pin.
//
// Construction patterns mirror the existing passing per-mutation tests EXACTLY:
//   - Service:         reply_integration_test.go / note_integration_test.go / triage_integration_test.go
//   - IdentityService: identity_integration_test.go (b64Sealer, stubResolver, newIdentityService)
// Seed harness reused from read_integration_test.go (startReadDB/seedReadTenant/
// seedTicket/presetRole). countSuper is the read-harness helper.
//
// IMPORTANT seeding note: Reply/AddNote on a `new` ticket ALSO advance it to
// `open`, emitting a SECOND audit row (ticket.status_changed via advanceNewToOpen).
// To assert "exactly one" audit for the reply/note mutation itself, every ticket
// here is seeded in `open` so that lifecycle side-effect does not fire.
// ---------------------------------------------------------------------------

// auditRow is the projected newest audit_entry for an action, read RLS-exempt.
type auditRow struct {
	action        string
	targetType    *string
	targetID      *uuid.UUID
	actor         *uuid.UUID
	hasOldValue   bool
	hasNewValue   bool
}

// latestAudit reads the single newest audit_entry matching action (RLS-exempt via
// Super) and reports whether old_value/new_value are populated. It is the ground
// truth for the matrix: the caller asserts target_type/actor/before-after against it.
func latestAudit(ctx context.Context, t *testing.T, tdb *testdb.TestDB, action string) auditRow {
	t.Helper()
	var r auditRow
	err := tdb.Super.QueryRow(ctx,
		`SELECT action, target_type, target_id, actor_principal_id,
		        old_value IS NOT NULL, new_value IS NOT NULL
		   FROM audit_entry
		  WHERE action = $1
		  ORDER BY created_at DESC, id DESC
		  LIMIT 1`, action).
		Scan(&r.action, &r.targetType, &r.targetID, &r.actor, &r.hasOldValue, &r.hasNewValue)
	if err != nil {
		t.Fatalf("latestAudit(%q): %v", action, err)
	}
	return r
}

// countAuditAction counts audit_entry rows for an action (RLS-exempt) — used to
// assert a single mutation wrote EXACTLY ONE audit row for that action.
func countAuditAction(ctx context.Context, t *testing.T, tdb *testdb.TestDB, action string) int {
	t.Helper()
	return countSuper(ctx, t, tdb.Super, `SELECT count(*) FROM audit_entry WHERE action=$1`, action)
}

// strDeref renders an optional string for failure messages.
func strDeref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestSupportAuditMatrix drives every principal-bearing support mutation and asserts
// each writes exactly one in-tx audit_entry with the expected action, target_type,
// actor, and populated before/after values (SC-005/FR-014).
func TestSupportAuditMatrix(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)

	// Service for the ticket mutations (Reply/AddNote/Triage). Mirrors
	// reply_integration_test.go's construction (replyKey + SystemDomain).
	svc := &Service{DB: tdb.App, ReplyTokenKey: replyKey, SystemDomain: "inbound.localhost"}
	// IdentityService for the three identity mutations. Mirrors
	// identity_integration_test.go's newIdentityService (b64Sealer + stub resolver).
	resolver := newStubResolver()
	idSvc := newIdentityService(tdb, resolver)

	// expect asserts the newest audit_entry for action matches the expectations and
	// that the mutation produced exactly `wantCount` rows for that action.
	expect := func(t *testing.T, action, wantTargetType string, wantActor uuid.UUID, wantOld, wantNew bool, gotCount, wantCount int) {
		t.Helper()
		if gotCount != wantCount {
			t.Fatalf("%s: audit row count = %d, want %d (exactly-one-per-mutation)", action, gotCount, wantCount)
		}
		row := latestAudit(ctx, t, tdb, action)
		if row.targetType == nil || *row.targetType != wantTargetType {
			t.Errorf("%s: target_type = %q, want %q", action, strDeref(row.targetType), wantTargetType)
		}
		if row.targetID == nil {
			t.Errorf("%s: target_id is NULL, want a target", action)
		}
		if row.actor == nil || *row.actor != wantActor {
			t.Errorf("%s: actor_principal_id = %v, want %v", action, row.actor, wantActor)
		}
		if row.hasOldValue != wantOld {
			t.Errorf("%s: old_value populated = %v, want %v", action, row.hasOldValue, wantOld)
		}
		if row.hasNewValue != wantNew {
			t.Errorf("%s: new_value populated = %v, want %v", action, row.hasNewValue, wantNew)
		}
	}

	// --- ticket.replied -------------------------------------------------------
	// Actor = the replying member; target_type = ticket_message; only new_value.
	t.Run("ticket.replied", func(t *testing.T) {
		tid := uuid.New()
		seedTicket(ctx, t, tdb, rt, tid, "open", "normal", "replied", nil, nil, -1*time.Hour)
		before := countAuditAction(ctx, t, tdb, "ticket.replied")
		if _, err := svc.Reply(ctx, rt.reader, rt.master, tid, ReplyInput{BodyText: "on it"}); err != nil {
			t.Fatalf("Reply: %v", err)
		}
		got := countAuditAction(ctx, t, tdb, "ticket.replied")
		expect(t, "ticket.replied", "ticket_message", rt.reader, false, true, got-before, 1)
	})

	// --- ticket.noted ---------------------------------------------------------
	// Actor = the noting member; target_type = ticket_message; only new_value.
	t.Run("ticket.noted", func(t *testing.T) {
		tid := uuid.New()
		seedTicket(ctx, t, tdb, rt, tid, "open", "normal", "noted", nil, nil, -1*time.Hour)
		before := countAuditAction(ctx, t, tdb, "ticket.noted")
		if _, err := svc.AddNote(ctx, rt.reader, rt.master, tid, NoteInput{BodyText: "looking"}); err != nil {
			t.Fatalf("AddNote: %v", err)
		}
		got := countAuditAction(ctx, t, tdb, "ticket.noted")
		expect(t, "ticket.noted", "ticket_message", rt.reader, false, true, got-before, 1)
	})

	// --- ticket.status_changed (triage) --------------------------------------
	// Actor = the triaging member; target_type = ticket; before+after status.
	// Seed `open`, flip to `pending` so exactly one facet changes.
	t.Run("ticket.status_changed", func(t *testing.T) {
		tid := uuid.New()
		seedTicket(ctx, t, tdb, rt, tid, "open", "normal", "status", nil, nil, -1*time.Hour)
		before := countAuditAction(ctx, t, tdb, "ticket.status_changed")
		st := "pending"
		if _, err := svc.Triage(ctx, rt.reader, rt.master, tid, TriageInput{Status: &st}); err != nil {
			t.Fatalf("Triage status: %v", err)
		}
		got := countAuditAction(ctx, t, tdb, "ticket.status_changed")
		expect(t, "ticket.status_changed", "ticket", rt.reader, true, true, got-before, 1)
	})

	// --- ticket.priority_changed (triage) ------------------------------------
	t.Run("ticket.priority_changed", func(t *testing.T) {
		tid := uuid.New()
		seedTicket(ctx, t, tdb, rt, tid, "open", "normal", "priority", nil, nil, -1*time.Hour)
		before := countAuditAction(ctx, t, tdb, "ticket.priority_changed")
		pr := "high"
		if _, err := svc.Triage(ctx, rt.reader, rt.master, tid, TriageInput{Priority: &pr}); err != nil {
			t.Fatalf("Triage priority: %v", err)
		}
		got := countAuditAction(ctx, t, tdb, "ticket.priority_changed")
		expect(t, "ticket.priority_changed", "ticket", rt.reader, true, true, got-before, 1)
	})

	// --- ticket.tags_changed (triage) ----------------------------------------
	t.Run("ticket.tags_changed", func(t *testing.T) {
		tid := uuid.New()
		seedTicket(ctx, t, tdb, rt, tid, "open", "normal", "tags", nil, nil, -1*time.Hour)
		before := countAuditAction(ctx, t, tdb, "ticket.tags_changed")
		tags := []string{"billing", "urgent"}
		if _, err := svc.Triage(ctx, rt.reader, rt.master, tid, TriageInput{Tags: &tags}); err != nil {
			t.Fatalf("Triage tags: %v", err)
		}
		got := countAuditAction(ctx, t, tdb, "ticket.tags_changed")
		expect(t, "ticket.tags_changed", "ticket", rt.reader, true, true, got-before, 1)
	})

	// --- ticket.assigned (triage) --------------------------------------------
	// Assign an eligible member; old_value(=null prior assignee) and new_value
	// are both serialized maps, so both columns are populated.
	t.Run("ticket.assigned", func(t *testing.T) {
		tid := uuid.New()
		seedTicket(ctx, t, tdb, rt, tid, "open", "normal", "assigned", nil, nil, -1*time.Hour)
		assignee := seedEligibleAssignee(ctx, t, tdb, rt)
		before := countAuditAction(ctx, t, tdb, "ticket.assigned")
		if _, err := svc.Triage(ctx, rt.reader, rt.master, tid, TriageInput{AssigneeSet: true, Assignee: &assignee}); err != nil {
			t.Fatalf("Triage assignee: %v", err)
		}
		got := countAuditAction(ctx, t, tdb, "ticket.assigned")
		expect(t, "ticket.assigned", "ticket", rt.reader, true, true, got-before, 1)
	})

	// --- email_domain.created -------------------------------------------------
	// Actor = the owner; target_type = email_domain; only new_value.
	t.Run("email_domain.created", func(t *testing.T) {
		before := countAuditAction(ctx, t, tdb, "email_domain.created")
		domain := uniqueDomain("auditcreate")
		if _, err := idSvc.CreateEmailDomain(ctx, rt.owner, rt.master, CreateEmailDomainInput{Domain: domain, Mode: "forward_in"}); err != nil {
			t.Fatalf("CreateEmailDomain: %v", err)
		}
		got := countAuditAction(ctx, t, tdb, "email_domain.created")
		expect(t, "email_domain.created", "email_domain", rt.owner, false, true, got-before, 1)
	})

	// --- email_domain.verified ------------------------------------------------
	// Create, publish the TXT in the stub resolver, then verify. Actor = owner;
	// target_type = email_domain; only new_value.
	t.Run("email_domain.verified", func(t *testing.T) {
		domain := uniqueDomain("auditverify")
		ed, err := idSvc.CreateEmailDomain(ctx, rt.owner, rt.master, CreateEmailDomainInput{Domain: domain, Mode: "forward_in"})
		if err != nil {
			t.Fatalf("CreateEmailDomain: %v", err)
		}
		resolver.records["_manyforge."+domain] = []string{ed.DNSChallenge.VerificationTXT.Value}
		before := countAuditAction(ctx, t, tdb, "email_domain.verified")
		if _, err := idSvc.VerifyEmailDomain(ctx, rt.owner, rt.master, ed.ID); err != nil {
			t.Fatalf("VerifyEmailDomain: %v", err)
		}
		got := countAuditAction(ctx, t, tdb, "email_domain.verified")
		expect(t, "email_domain.verified", "email_domain", rt.owner, false, true, got-before, 1)
	})

	// --- inbound_address.created ----------------------------------------------
	// Needs a verified domain first. Actor = owner; target_type = inbound_address;
	// only new_value.
	t.Run("inbound_address.created", func(t *testing.T) {
		domain := uniqueDomain("auditinbound")
		ed, err := idSvc.CreateEmailDomain(ctx, rt.owner, rt.master, CreateEmailDomainInput{Domain: domain, Mode: "subdomain_mx"})
		if err != nil {
			t.Fatalf("CreateEmailDomain: %v", err)
		}
		resolver.records["_manyforge."+domain] = []string{ed.DNSChallenge.VerificationTXT.Value}
		if _, err := idSvc.VerifyEmailDomain(ctx, rt.owner, rt.master, ed.ID); err != nil {
			t.Fatalf("VerifyEmailDomain: %v", err)
		}
		before := countAuditAction(ctx, t, tdb, "inbound_address.created")
		addr := "support@" + domain
		if _, err := idSvc.CreateInboundAddress(ctx, rt.owner, rt.master, CreateInboundAddressInput{Address: addr, EmailDomainID: ed.ID}); err != nil {
			t.Fatalf("CreateInboundAddress: %v", err)
		}
		got := countAuditAction(ctx, t, tdb, "inbound_address.created")
		expect(t, "inbound_address.created", "inbound_address", rt.owner, false, true, got-before, 1)
	})
}
