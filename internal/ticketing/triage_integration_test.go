//go:build integration

package ticketing

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// ---------------------------------------------------------------------------
// T045 — [US3] triage integration RED-GATE.
//
// These tests pin the AGREED interface contract for (*Service).Triage and its
// audit/lifecycle semantics. The method does NOT exist yet: this file is the
// expected RED ("undefined: ...Triage"). T047/T048 implement to match.
//
// What is asserted here (no tautologies — every assertion reads real DB state
// via countSuper / the RLS-exempt Super pool, or the returned Ticket):
//   - each triage field persists (status, priority, tags, assignee)
//   - one audit_entry per CHANGED field, with pinned action + populated
//     old_value/new_value; a no-op writes NO audit row
//   - partial update preserves omitted fields
//   - last_message_at is NOT touched by a triage call
//   - the full manual lifecycle (new→open→pending→solved→closed→open) succeeds
//   - invalid status/priority → ErrValidation
//   - tags full-replace / clear / preserve
//   - eligible assign persists; ineligible-existing AND nonexistent assignee
//     BOTH → ErrConflict uniformly (no oracle); unassign succeeds
//   - unknown/unauthorized ticket → ErrNotFound
// ---------------------------------------------------------------------------

// seedEligibleAssignee inserts a NEW human principal that IS a member of rt.master
// (so it is an eligible assignee), and returns its principal id. It mirrors the
// account/principal/membership inserts in seedReadTenant, via the RLS-exempt pool.
func seedEligibleAssignee(ctx context.Context, t *testing.T, tdb *testdb.TestDB, rt readTenant) uuid.UUID {
	t.Helper()
	memberRole := presetRole(ctx, t, tdb, "member") // any in-tenant role grants membership
	acct := uuid.New()
	prin := uuid.New()
	email := "elig-" + prin.String() + "@x.test"

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed eligible: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at)
		 VALUES ($1,$2,'Elig','active',now(),now(),now())`, acct, email); err != nil {
		t.Fatalf("seed eligible account: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`, prin, acct); err != nil {
		t.Fatalf("seed eligible principal: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at)
		 VALUES ($1,$2,$2,$3,now())`, prin, rt.master, memberRole); err != nil {
		t.Fatalf("seed eligible membership: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed eligible: %v", err)
	}
	return prin
}

// seedIneligiblePrincipal inserts a NEW human principal (a REAL principal row)
// that has NO membership in rt.master (or any ancestor). It is therefore an
// ineligible assignee that nonetheless exists — used to prove the
// ineligible-existing vs nonexistent-uuid responses are identical (no oracle).
func seedIneligiblePrincipal(ctx context.Context, t *testing.T, tdb *testdb.TestDB) uuid.UUID {
	t.Helper()
	acct := uuid.New()
	prin := uuid.New()
	email := "inelig-" + prin.String() + "@x.test"

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed ineligible: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at)
		 VALUES ($1,$2,'Inelig','active',now(),now(),now())`, acct, email); err != nil {
		t.Fatalf("seed ineligible account: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`, prin, acct); err != nil {
		t.Fatalf("seed ineligible principal: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed ineligible: %v", err)
	}
	return prin
}

// ticketTagSet reads the exact tag set persisted for a ticket via the Super pool,
// sorted for deterministic comparison.
func ticketTagSet(ctx context.Context, t *testing.T, tdb *testdb.TestDB, ticketID uuid.UUID) []string {
	t.Helper()
	rows, err := tdb.Super.Query(ctx, `SELECT tag FROM ticket_tag WHERE ticket_id=$1`, ticketID)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	defer rows.Close()
	var tags []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			t.Fatalf("scan tag: %v", err)
		}
		tags = append(tags, tag)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("tag rows: %v", err)
	}
	sort.Strings(tags)
	return tags
}

// readTicketRow reads the raw persisted status/priority/assignee/last_message_at
// for a ticket via the RLS-exempt pool (ground truth, independent of the service).
func readTicketRow(ctx context.Context, t *testing.T, tdb *testdb.TestDB, ticketID uuid.UUID) (status, priority string, assignee *uuid.UUID, lastMsgAt time.Time) {
	t.Helper()
	var a *uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		`SELECT status::text, priority::text, assignee_principal_id, last_message_at FROM ticket WHERE id=$1`,
		ticketID).Scan(&status, &priority, &a, &lastMsgAt); err != nil {
		t.Fatalf("read ticket row: %v", err)
	}
	return status, priority, a, lastMsgAt
}

func ptrStr(s string) *string { return &s }

// TestTriageEachFieldPersistsAndAudits — changing each of the four triage fields
// persists the new value AND writes exactly one audit_entry with the pinned action
// and populated old_value/new_value. A no-op (submitting the current value) writes
// no audit row.
func TestTriageEachFieldPersistsAndAudits(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := &Service{DB: tdb.App}

	// --- status ---
	stID := uuid.New()
	seedTicket(ctx, t, tdb, rt, stID, "open", "normal", "status-change", nil, nil, -1*time.Hour)
	tk, err := svc.Triage(ctx, rt.reader, rt.master, stID, TriageInput{Status: ptrStr("pending")})
	if err != nil {
		t.Fatalf("triage status: %v", err)
	}
	if tk.Status != "pending" {
		t.Errorf("returned status = %q, want pending", tk.Status)
	}
	if gotStatus, _, _, _ := readTicketRow(ctx, t, tdb, stID); gotStatus != "pending" {
		t.Errorf("persisted status = %q, want pending", gotStatus)
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND target_type='ticket' AND action='ticket.status_changed'
		   AND old_value=$2::jsonb AND new_value=$3::jsonb`,
		stID, `{"status":"open"}`, `{"status":"pending"}`); n != 1 {
		t.Errorf("status audit (pinned old/new) = %d, want 1", n)
	}

	// --- priority ---
	prID := uuid.New()
	seedTicket(ctx, t, tdb, rt, prID, "open", "normal", "priority-change", nil, nil, -1*time.Hour)
	tk, err = svc.Triage(ctx, rt.reader, rt.master, prID, TriageInput{Priority: ptrStr("high")})
	if err != nil {
		t.Fatalf("triage priority: %v", err)
	}
	if tk.Priority != "high" {
		t.Errorf("returned priority = %q, want high", tk.Priority)
	}
	if _, gotPrio, _, _ := readTicketRow(ctx, t, tdb, prID); gotPrio != "high" {
		t.Errorf("persisted priority = %q, want high", gotPrio)
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND target_type='ticket' AND action='ticket.priority_changed'
		   AND old_value IS NOT NULL AND new_value IS NOT NULL`, prID); n != 1 {
		t.Errorf("priority audit = %d, want 1 (with populated old/new)", n)
	}

	// --- tags ---
	tgID := uuid.New()
	seedTicket(ctx, t, tdb, rt, tgID, "open", "normal", "tags-change", nil, []string{"a", "b"}, -1*time.Hour)
	if _, err := svc.Triage(ctx, rt.reader, rt.master, tgID, TriageInput{Tags: &[]string{"b", "c"}}); err != nil {
		t.Fatalf("triage tags: %v", err)
	}
	if got := ticketTagSet(ctx, t, tdb, tgID); len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Errorf("persisted tags = %v, want [b c]", got)
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND target_type='ticket' AND action='ticket.tags_changed'
		   AND old_value IS NOT NULL AND new_value IS NOT NULL`, tgID); n != 1 {
		t.Errorf("tags audit = %d, want 1 (with populated old/new)", n)
	}

	// --- assignee ---
	asID := uuid.New()
	seedTicket(ctx, t, tdb, rt, asID, "open", "normal", "assignee-change", nil, nil, -1*time.Hour)
	elig := seedEligibleAssignee(ctx, t, tdb, rt)
	tk, err = svc.Triage(ctx, rt.reader, rt.master, asID, TriageInput{AssigneeSet: true, Assignee: &elig})
	if err != nil {
		t.Fatalf("triage assignee: %v", err)
	}
	if tk.AssigneePrincipalID == nil || *tk.AssigneePrincipalID != elig {
		t.Errorf("returned assignee = %v, want %v", tk.AssigneePrincipalID, elig)
	}
	if _, _, gotAssignee, _ := readTicketRow(ctx, t, tdb, asID); gotAssignee == nil || *gotAssignee != elig {
		t.Errorf("persisted assignee = %v, want %v", gotAssignee, elig)
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND target_type='ticket' AND action='ticket.assigned'
		   AND new_value IS NOT NULL`, asID); n != 1 {
		t.Errorf("assigned audit = %d, want 1 (with populated new)", n)
	}

	// --- no-op: submitting the CURRENT status writes no status_changed audit ---
	noopID := uuid.New()
	seedTicket(ctx, t, tdb, rt, noopID, "open", "normal", "noop", nil, nil, -1*time.Hour)
	if _, err := svc.Triage(ctx, rt.reader, rt.master, noopID, TriageInput{Status: ptrStr("open")}); err != nil {
		t.Fatalf("triage no-op status: %v", err)
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND action='ticket.status_changed'`, noopID); n != 0 {
		t.Errorf("no-op status audit = %d, want 0 (submitting current value is a no-op)", n)
	}
}

// TestTriagePartialUpdatePreservesOmittedFields — a triage that changes only one
// field leaves the others (and last_message_at) exactly as they were.
func TestTriagePartialUpdatePreservesOmittedFields(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := &Service{DB: tdb.App}

	id := uuid.New()
	elig := seedEligibleAssignee(ctx, t, tdb, rt)
	seedTicket(ctx, t, tdb, rt, id, "open", "high", "partial", &elig, []string{"keep1", "keep2"}, -2*time.Hour)
	_, _, _, beforeLMA := readTicketRow(ctx, t, tdb, id)

	// Change ONLY status; priority, tags, assignee, last_message_at must be intact.
	tk, err := svc.Triage(ctx, rt.reader, rt.master, id, TriageInput{Status: ptrStr("pending")})
	if err != nil {
		t.Fatalf("triage: %v", err)
	}
	if tk.Status != "pending" {
		t.Errorf("status = %q, want pending", tk.Status)
	}
	if tk.Priority != "high" {
		t.Errorf("priority changed to %q, want preserved high", tk.Priority)
	}
	if tk.AssigneePrincipalID == nil || *tk.AssigneePrincipalID != elig {
		t.Errorf("assignee changed to %v, want preserved %v", tk.AssigneePrincipalID, elig)
	}
	if got := ticketTagSet(ctx, t, tdb, id); len(got) != 2 || got[0] != "keep1" || got[1] != "keep2" {
		t.Errorf("tags changed to %v, want preserved [keep1 keep2]", got)
	}

	gotStatus, gotPrio, gotAssignee, afterLMA := readTicketRow(ctx, t, tdb, id)
	if gotStatus != "pending" || gotPrio != "high" {
		t.Errorf("persisted status/priority = %q/%q, want pending/high", gotStatus, gotPrio)
	}
	if gotAssignee == nil || *gotAssignee != elig {
		t.Errorf("persisted assignee = %v, want preserved %v", gotAssignee, elig)
	}
	// last_message_at MUST NOT be touched by a triage call.
	if !afterLMA.Equal(beforeLMA) {
		t.Errorf("last_message_at changed by triage: before=%v after=%v (must be untouched)", beforeLMA, afterLMA)
	}
	// Exactly one audit row (only status changed); no priority/tags/assignee rows.
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND action IN
		   ('ticket.priority_changed','ticket.tags_changed','ticket.assigned')`, id); n != 0 {
		t.Errorf("unchanged fields wrote %d audit rows, want 0", n)
	}
}

// TestTriageFullLifecycleTransitions — every manual transition in the data-model
// state table is allowed (no terminal lock): new→open→pending→solved→closed→open.
func TestTriageFullLifecycleTransitions(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := &Service{DB: tdb.App}

	id := uuid.New()
	seedTicket(ctx, t, tdb, rt, id, "new", "normal", "lifecycle", nil, nil, -1*time.Hour)

	steps := []struct{ from, to string }{
		{"new", "open"},
		{"open", "pending"},
		{"pending", "solved"},
		{"solved", "closed"},
		{"closed", "open"}, // reopen — closed is NOT terminal
	}
	for _, s := range steps {
		tk, err := svc.Triage(ctx, rt.reader, rt.master, id, TriageInput{Status: ptrStr(s.to)})
		if err != nil {
			t.Fatalf("transition %s→%s: %v", s.from, s.to, err)
		}
		if tk.Status != s.to {
			t.Errorf("transition %s→%s: returned status %q", s.from, s.to, tk.Status)
		}
		if got, _, _, _ := readTicketRow(ctx, t, tdb, id); got != s.to {
			t.Errorf("transition %s→%s: persisted status %q", s.from, s.to, got)
		}
		if n := countSuper(ctx, t, tdb.Super,
			`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND action='ticket.status_changed'
			   AND old_value=$2::jsonb AND new_value=$3::jsonb`,
			id, `{"status":"`+s.from+`"}`, `{"status":"`+s.to+`"}`); n != 1 {
			t.Errorf("transition %s→%s: pinned status audit = %d, want 1", s.from, s.to, n)
		}
	}
}

// TestTriageInvalidEnumIsValidation — an unknown status or priority value is a
// caller-input error (ErrValidation), not a 500 and not a silent no-op.
func TestTriageInvalidEnumIsValidation(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := &Service{DB: tdb.App}

	id := uuid.New()
	seedTicket(ctx, t, tdb, rt, id, "open", "normal", "invalid-enum", nil, nil, -1*time.Hour)

	if _, err := svc.Triage(ctx, rt.reader, rt.master, id, TriageInput{Status: ptrStr("frozen")}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("invalid status: want ErrValidation, got %v", err)
	}
	if _, err := svc.Triage(ctx, rt.reader, rt.master, id, TriageInput{Priority: ptrStr("nuclear")}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("invalid priority: want ErrValidation, got %v", err)
	}
	// A rejected triage must not mutate the row or write an audit entry.
	if got, _, _, _ := readTicketRow(ctx, t, tdb, id); got != "open" {
		t.Errorf("invalid status mutated row to %q, want unchanged open", got)
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1`, id); n != 0 {
		t.Errorf("rejected triage wrote %d audit rows, want 0", n)
	}
}

// TestTriageTagsReplaceClearPreserve — Tags semantics: non-nil = FULL replacement
// (empty clears all); nil = preserve.
func TestTriageTagsReplaceClearPreserve(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := &Service{DB: tdb.App}

	// Replace: {a,b} -> {b,c}.
	repID := uuid.New()
	seedTicket(ctx, t, tdb, rt, repID, "open", "normal", "tags-replace", nil, []string{"a", "b"}, -1*time.Hour)
	if _, err := svc.Triage(ctx, rt.reader, rt.master, repID, TriageInput{Tags: &[]string{"b", "c"}}); err != nil {
		t.Fatalf("tags replace: %v", err)
	}
	if got := ticketTagSet(ctx, t, tdb, repID); len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Errorf("replace: tags = %v, want [b c] (a removed, c added)", got)
	}

	// Clear: {x,y} -> {} via empty (non-nil) slice.
	clrID := uuid.New()
	seedTicket(ctx, t, tdb, rt, clrID, "open", "normal", "tags-clear", nil, []string{"x", "y"}, -1*time.Hour)
	empty := []string{}
	if _, err := svc.Triage(ctx, rt.reader, rt.master, clrID, TriageInput{Tags: &empty}); err != nil {
		t.Fatalf("tags clear: %v", err)
	}
	if got := ticketTagSet(ctx, t, tdb, clrID); len(got) != 0 {
		t.Errorf("clear: tags = %v, want [] (empty non-nil slice clears all)", got)
	}

	// Preserve: nil Tags leaves {p,q} intact (changing status only).
	preID := uuid.New()
	seedTicket(ctx, t, tdb, rt, preID, "open", "normal", "tags-preserve", nil, []string{"p", "q"}, -1*time.Hour)
	if _, err := svc.Triage(ctx, rt.reader, rt.master, preID, TriageInput{Status: ptrStr("pending")}); err != nil {
		t.Fatalf("tags preserve: %v", err)
	}
	if got := ticketTagSet(ctx, t, tdb, preID); len(got) != 2 || got[0] != "p" || got[1] != "q" {
		t.Errorf("preserve: tags = %v, want [p q] (nil Tags preserves)", got)
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND action='ticket.tags_changed'`, preID); n != 0 {
		t.Errorf("nil Tags wrote %d tags audit rows, want 0", n)
	}
}

// TestTriageTagsCitextCaseFolding — ticket_tag.tag is citext with PK (ticket_id,tag),
// so "Bug" and "bug" collide. The service must case-fold its dedup (so a mixed-case
// submit persists exactly ONE row, no unique-violation 500) and case-fold its no-op
// detection (a case-only-different submit equal to the stored set writes NO update
// and NO ticket.tags_changed audit). First-seen ORIGINAL casing is what gets stored.
func TestTriageTagsCitextCaseFolding(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := &Service{DB: tdb.App}

	// Mixed-case input collapses to one tag (citext-equal "Bug"/"bug"); the first-seen
	// casing "Bug" is what persists — and exactly one row, no 500 from a PK collision.
	dupID := uuid.New()
	seedTicket(ctx, t, tdb, rt, dupID, "open", "normal", "tags-casefold-dup", nil, nil, -1*time.Hour)
	if _, err := svc.Triage(ctx, rt.reader, rt.master, dupID, TriageInput{Tags: &[]string{"Bug", "bug"}}); err != nil {
		t.Fatalf("case-fold dedup triage: %v", err)
	}
	if got := ticketTagSet(ctx, t, tdb, dupID); len(got) != 1 || got[0] != "Bug" {
		t.Errorf("case-fold dedup: tags = %v, want [Bug] (one row, first-seen casing)", got)
	}

	// No-op: stored set is {bug}; submitting {BUG} (citext-equal) must NOT delete+reinsert
	// and must write NO ticket.tags_changed audit.
	noopID := uuid.New()
	seedTicket(ctx, t, tdb, rt, noopID, "open", "normal", "tags-casefold-noop", nil, []string{"bug"}, -1*time.Hour)
	if _, err := svc.Triage(ctx, rt.reader, rt.master, noopID, TriageInput{Tags: &[]string{"BUG"}}); err != nil {
		t.Fatalf("case-fold no-op triage: %v", err)
	}
	if got := ticketTagSet(ctx, t, tdb, noopID); len(got) != 1 || got[0] != "bug" {
		t.Errorf("case-fold no-op: tags = %v, want [bug] (unchanged — no delete+reinsert)", got)
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM audit_entry WHERE target_id=$1 AND action='ticket.tags_changed'`, noopID); n != 0 {
		t.Errorf("case-only-different submit wrote %d tags audit rows, want 0 (no-op)", n)
	}
}

// TestTriageTagsEmptyOrWhitespaceIsValidation — an empty or whitespace-only tag is a
// caller-input error (ErrValidation), never a persisted junk tag.
func TestTriageTagsEmptyOrWhitespaceIsValidation(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := &Service{DB: tdb.App}

	id := uuid.New()
	seedTicket(ctx, t, tdb, rt, id, "open", "normal", "tags-empty", nil, []string{"keep"}, -1*time.Hour)

	if _, err := svc.Triage(ctx, rt.reader, rt.master, id, TriageInput{Tags: &[]string{""}}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("empty tag: want ErrValidation, got %v", err)
	}
	if _, err := svc.Triage(ctx, rt.reader, rt.master, id, TriageInput{Tags: &[]string{"   "}}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("whitespace tag: want ErrValidation, got %v", err)
	}
	// A rejected tag triage must not mutate the existing tag set.
	if got := ticketTagSet(ctx, t, tdb, id); len(got) != 1 || got[0] != "keep" {
		t.Errorf("rejected tag triage mutated tags to %v, want preserved [keep]", got)
	}
}

// TestTriageAssigneeEligibilityAndUnassign — assigning to an eligible member
// persists; assigning to an ineligible-but-existing principal AND to a random
// nonexistent uuid BOTH return ErrConflict uniformly (no oracle); unassign
// (AssigneeSet=true, Assignee=nil) always succeeds and NULLs the column.
func TestTriageAssigneeEligibilityAndUnassign(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := &Service{DB: tdb.App}

	// Eligible assign persists.
	okID := uuid.New()
	seedTicket(ctx, t, tdb, rt, okID, "open", "normal", "assign-ok", nil, nil, -1*time.Hour)
	elig := seedEligibleAssignee(ctx, t, tdb, rt)
	if _, err := svc.Triage(ctx, rt.reader, rt.master, okID, TriageInput{AssigneeSet: true, Assignee: &elig}); err != nil {
		t.Fatalf("eligible assign: %v", err)
	}
	if _, _, got, _ := readTicketRow(ctx, t, tdb, okID); got == nil || *got != elig {
		t.Errorf("eligible assign persisted = %v, want %v", got, elig)
	}

	// Ineligible-existing vs nonexistent uuid: BOTH ErrConflict, identical (no oracle).
	badID := uuid.New()
	seedTicket(ctx, t, tdb, rt, badID, "open", "normal", "assign-bad", nil, nil, -1*time.Hour)
	inelig := seedIneligiblePrincipal(ctx, t, tdb) // real principal, NO membership in rt.master
	nonexistent := uuid.New()                      // no principal row at all

	_, errInelig := svc.Triage(ctx, rt.reader, rt.master, badID, TriageInput{AssigneeSet: true, Assignee: &inelig})
	_, errNonexistent := svc.Triage(ctx, rt.reader, rt.master, badID, TriageInput{AssigneeSet: true, Assignee: &nonexistent})

	if !errors.Is(errInelig, errs.ErrConflict) {
		t.Errorf("ineligible-existing assignee: want ErrConflict, got %v", errInelig)
	}
	if !errors.Is(errNonexistent, errs.ErrConflict) {
		t.Errorf("nonexistent assignee: want ErrConflict, got %v", errNonexistent)
	}
	if (errInelig == nil) != (errNonexistent == nil) || errInelig.Error() != errNonexistent.Error() {
		t.Errorf("assignee oracle: ineligible (%v) and nonexistent (%v) must be identical", errInelig, errNonexistent)
	}
	// The refused assigns must not have mutated the assignee column.
	if _, _, got, _ := readTicketRow(ctx, t, tdb, badID); got != nil {
		t.Errorf("refused assign mutated assignee to %v, want NULL", got)
	}

	// Unassign always succeeds → NULL.
	unID := uuid.New()
	seedTicket(ctx, t, tdb, rt, unID, "open", "normal", "unassign", &elig, nil, -1*time.Hour)
	tk, err := svc.Triage(ctx, rt.reader, rt.master, unID, TriageInput{AssigneeSet: true, Assignee: nil})
	if err != nil {
		t.Fatalf("unassign: %v", err)
	}
	if tk.AssigneePrincipalID != nil {
		t.Errorf("unassign returned assignee %v, want nil", tk.AssigneePrincipalID)
	}
	if _, _, got, _ := readTicketRow(ctx, t, tdb, unID); got != nil {
		t.Errorf("unassign persisted assignee = %v, want NULL", got)
	}
}

// TestTriageUnknownTicketIsNotFound — an unknown / cross-tenant ticket id collapses
// to ErrNotFound (no existence oracle), identical for both.
func TestTriageUnknownTicketIsNotFound(t *testing.T) {
	ctx, tdb := startReadDB(t)
	t1 := seedReadTenant(ctx, t, tdb)
	t2 := seedReadTenant(ctx, t, tdb)
	svc := &Service{DB: tdb.App}

	// A real ticket in t2, addressed under t1's business.
	t2Ticket := uuid.New()
	seedTicket(ctx, t, tdb, t2, t2Ticket, "open", "normal", "t2-secret", nil, nil, -1*time.Hour)

	_, errCross := svc.Triage(ctx, t1.reader, t1.master, t2Ticket, TriageInput{Status: ptrStr("pending")})
	_, errUnknown := svc.Triage(ctx, t1.reader, t1.master, uuid.New(), TriageInput{Status: ptrStr("pending")})

	if !errors.Is(errCross, errs.ErrNotFound) {
		t.Errorf("cross-tenant ticket: want ErrNotFound, got %v", errCross)
	}
	if !errors.Is(errUnknown, errs.ErrNotFound) {
		t.Errorf("unknown ticket: want ErrNotFound, got %v", errUnknown)
	}
	if (errCross == nil) != (errUnknown == nil) || errCross.Error() != errUnknown.Error() {
		t.Errorf("oracle: cross-tenant (%v) and unknown (%v) must be identical", errCross, errUnknown)
	}
}
