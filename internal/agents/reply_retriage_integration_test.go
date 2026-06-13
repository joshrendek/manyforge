//go:build integration

package agents

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/events"
)

// startRetriageTDB spins an ephemeral Postgres with the run-loop lifecycle the other
// integration tests use (testdb.Start(ctx) returns (*TestDB, error); cleanup via t.Cleanup).
func startRetriageTDB(t *testing.T) (context.Context, *testdb.TestDB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	return ctx, tdb
}

// seedReplyMessage inserts an additional ticket_message on an existing ticket via the
// RLS-exempt Super pool. direction is 'inbound' (author NULL) | 'outbound'|'note' (author set).
func seedReplyMessage(ctx context.Context, t *testing.T, tdb *testdb.TestDB, s runSeed, ticketID uuid.UUID, direction string, isAuto bool) uuid.UUID {
	t.Helper()
	msgID := uuid.New()
	var author any
	if direction == "inbound" {
		author = nil
	} else {
		author = s.ownerID // outbound/note require a non-null author_principal_id (CHECK)
	}
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO ticket_message (id,ticket_id,business_id,tenant_root_id,direction,author_principal_id,message_id,"references",body_text,is_auto_reply,created_at)
		 VALUES ($1,$2,$3,$3,$4::ticket_message_direction,$5,$6,'{}','reply body',$7,now())`,
		msgID, ticketID, s.businessID, direction, author, "rm-"+msgID.String()+"@example.com", isAuto); err != nil {
		t.Fatalf("seed reply message (%s): %v", direction, err)
	}
	return msgID
}

// createRetriageAgent creates an enabled agent via the real service; optIn toggles retriage_on_reply.
func createRetriageAgent(ctx context.Context, t *testing.T, tdb *testdb.TestDB, s runSeed, name string, optIn bool) Agent {
	t.Helper()
	svc := &AgentService{DB: tdb.App}
	ag, err := svc.Create(ctx, s.ownerID, s.businessID, CreateAgentInput{
		Name: name, Provider: "anthropic", Model: "claude-sonnet-4-5",
		SystemPrompt: "Triage.", AllowedTools: []string{"read_ticket", "draft_reply"},
		AutonomyMode: ModeAssist, Enabled: true, RetriageOnReply: optIn,
	})
	if err != nil {
		t.Fatalf("create agent %q: %v", name, err)
	}
	return ag
}

// fireReply drives the trigger end-to-end for one message.received event.
func fireReply(ctx context.Context, t *testing.T, tdb *testdb.TestDB, s runSeed, ticketID, messageID uuid.UUID, cap int) {
	t.Helper()
	trig := &ReplyRetriageTrigger{Runs: &AgentRunStore{DB: tdb.App}, RetriageCap: cap}
	payload, _ := json.Marshal(map[string]any{
		"ticket_id": ticketID, "business_id": s.businessID, "message_id": messageID,
	})
	ev := events.Event{ID: uuid.New(), TenantRootID: s.tenantRootID, Payload: payload}
	if err := trig.Handle(ctx, nil, ev); err != nil {
		t.Fatalf("trigger Handle: %v", err)
	}
}

func replyRunCount(ctx context.Context, t *testing.T, tdb *testdb.TestDB, agentID, ticketID uuid.UUID) int {
	return countSuperRows(ctx, t, tdb,
		`SELECT count(*) FROM agent_run WHERE agent_id=$1 AND target_id=$2 AND trigger='reply'`,
		agentID, ticketID)
}

// Case 1: opted-in agent + genuine customer reply => one queued run with trigger='reply'.
func TestReplyRetriage_OptedInGenuineReply(t *testing.T) {
	ctx, tdb := startRetriageTDB(t)
	s := seedRunTenant(ctx, t, tdb)
	ag := createRetriageAgent(ctx, t, tdb, s, "Opted In", true)
	ticketID := seedRunTicket(ctx, t, tdb, s, "open")
	mid := seedReplyMessage(ctx, t, tdb, s, ticketID, "inbound", false)

	fireReply(ctx, t, tdb, s, ticketID, mid, 5)

	if got := replyRunCount(ctx, t, tdb, ag.ID, ticketID); got != 1 {
		t.Fatalf("reply runs = %d, want 1", got)
	}
	if n := countSuperRows(ctx, t, tdb,
		`SELECT count(*) FROM agent_run WHERE agent_id=$1 AND target_id=$2 AND trigger='reply' AND status='queued'`,
		ag.ID, ticketID); n != 1 {
		t.Fatalf("queued reply runs = %d, want 1", n)
	}
}

// Case 2: opted-out agent => no run.
func TestReplyRetriage_OptedOutNoRun(t *testing.T) {
	ctx, tdb := startRetriageTDB(t)
	s := seedRunTenant(ctx, t, tdb)
	ag := createRetriageAgent(ctx, t, tdb, s, "Opted Out", false)
	ticketID := seedRunTicket(ctx, t, tdb, s, "open")
	mid := seedReplyMessage(ctx, t, tdb, s, ticketID, "inbound", false)

	fireReply(ctx, t, tdb, s, ticketID, mid, 5)

	if got := replyRunCount(ctx, t, tdb, ag.ID, ticketID); got != 0 {
		t.Fatalf("reply runs = %d, want 0 (opted out)", got)
	}
}

// Case 3: is_auto_reply reply => no run (skipped_auto_reply).
func TestReplyRetriage_AutoReplySkipped(t *testing.T) {
	ctx, tdb := startRetriageTDB(t)
	s := seedRunTenant(ctx, t, tdb)
	ag := createRetriageAgent(ctx, t, tdb, s, "Opted In", true)
	ticketID := seedRunTicket(ctx, t, tdb, s, "open")
	mid := seedReplyMessage(ctx, t, tdb, s, ticketID, "inbound", true) // is_auto_reply

	fireReply(ctx, t, tdb, s, ticketID, mid, 5)

	if got := replyRunCount(ctx, t, tdb, ag.ID, ticketID); got != 0 {
		t.Fatalf("reply runs = %d, want 0 (auto-reply)", got)
	}
}

// Case 4: outbound/note message => no run (skipped_not_inbound).
func TestReplyRetriage_OutboundSkipped(t *testing.T) {
	ctx, tdb := startRetriageTDB(t)
	s := seedRunTenant(ctx, t, tdb)
	ag := createRetriageAgent(ctx, t, tdb, s, "Opted In", true)
	ticketID := seedRunTicket(ctx, t, tdb, s, "open")
	mid := seedReplyMessage(ctx, t, tdb, s, ticketID, "note", false)

	fireReply(ctx, t, tdb, s, ticketID, mid, 5)

	if got := replyRunCount(ctx, t, tdb, ag.ID, ticketID); got != 0 {
		t.Fatalf("reply runs = %d, want 0 (outbound/note)", got)
	}
}

// Case 5: cap — the (cap+1)th reply within an hour is suppressed + audited; earlier ones enqueue.
func TestReplyRetriage_CapSuppressesAndAudits(t *testing.T) {
	ctx, tdb := startRetriageTDB(t)
	s := seedRunTenant(ctx, t, tdb)
	ag := createRetriageAgent(ctx, t, tdb, s, "Opted In", true)
	ticketID := seedRunTicket(ctx, t, tdb, s, "open")

	const cap = 2
	// cap distinct replies all enqueue (distinct message ids => no dedup collision).
	for i := 0; i < cap; i++ {
		mid := seedReplyMessage(ctx, t, tdb, s, ticketID, "inbound", false)
		fireReply(ctx, t, tdb, s, ticketID, mid, cap)
	}
	if got := replyRunCount(ctx, t, tdb, ag.ID, ticketID); got != cap {
		t.Fatalf("reply runs after %d replies = %d, want %d", cap, got, cap)
	}
	// The (cap+1)th is suppressed (count of prior reply runs >= cap) and audited.
	overflow := seedReplyMessage(ctx, t, tdb, s, ticketID, "inbound", false)
	fireReply(ctx, t, tdb, s, ticketID, overflow, cap)

	if got := replyRunCount(ctx, t, tdb, ag.ID, ticketID); got != cap {
		t.Fatalf("reply runs after overflow = %d, want %d (capped)", got, cap)
	}
	if n := countSuperRows(ctx, t, tdb,
		`SELECT count(*) FROM audit_entry WHERE action='agent.retriage_suppressed' AND target_id=$1`,
		ticketID); n != 1 {
		t.Fatalf("retriage_suppressed audit rows = %d, want 1", n)
	}
}

// Case 6: two opted-in agents, one reply => two runs (per-agent cap, not a shared budget).
func TestReplyRetriage_PerAgentNotShared(t *testing.T) {
	ctx, tdb := startRetriageTDB(t)
	s := seedRunTenant(ctx, t, tdb)
	a1 := createRetriageAgent(ctx, t, tdb, s, "Agent One", true)
	a2 := createRetriageAgent(ctx, t, tdb, s, "Agent Two", true)
	ticketID := seedRunTicket(ctx, t, tdb, s, "open")
	mid := seedReplyMessage(ctx, t, tdb, s, ticketID, "inbound", false)

	fireReply(ctx, t, tdb, s, ticketID, mid, 5)

	if got := replyRunCount(ctx, t, tdb, a1.ID, ticketID); got != 1 {
		t.Fatalf("agent1 reply runs = %d, want 1", got)
	}
	if got := replyRunCount(ctx, t, tdb, a2.ID, ticketID); got != 1 {
		t.Fatalf("agent2 reply runs = %d, want 1", got)
	}
}

// Case 7 (dedup loop-guard): two message.received deliveries of the SAME message id => one run.
func TestReplyRetriage_RedeliveryDedups(t *testing.T) {
	ctx, tdb := startRetriageTDB(t)
	s := seedRunTenant(ctx, t, tdb)
	ag := createRetriageAgent(ctx, t, tdb, s, "Opted In", true)
	ticketID := seedRunTicket(ctx, t, tdb, s, "open")
	mid := seedReplyMessage(ctx, t, tdb, s, ticketID, "inbound", false)

	fireReply(ctx, t, tdb, s, ticketID, mid, 5)
	fireReply(ctx, t, tdb, s, ticketID, mid, 5) // at-least-once redelivery

	if got := replyRunCount(ctx, t, tdb, ag.ID, ticketID); got != 1 {
		t.Fatalf("reply runs after redelivery = %d, want 1 (deduped)", got)
	}
}

// Case 8 (claim hardening): a queued run with a missing agent is failed; a valid run drains.
func TestClaim_ToleratesOrphanedRun(t *testing.T) {
	ctx, tdb := startRetriageTDB(t)
	s := seedRunTenant(ctx, t, tdb)
	ag := createRetriageAgent(ctx, t, tdb, s, "Valid", false)
	ticketID := seedRunTicket(ctx, t, tdb, s, "open")

	// Orphan: a queued run pointing at a non-existent agent. The agent_run->agent FK blocks
	// this normally, so disable FK/trigger enforcement for just this seed tx (superuser only).
	orphanRunID := uuid.New()
	orphanAgentID := uuid.New()
	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin orphan seed: %v", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("disable FK triggers: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_run (id,agent_id,business_id,tenant_root_id,trigger,status,correlation_id,created_at,updated_at)
		 VALUES ($1,$2,$3,$3,'manual','queued',$4,now()-interval '5 minutes',now())`,
		orphanRunID, orphanAgentID, s.businessID, "corr-"+orphanRunID.String()); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("seed orphan run: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit orphan seed: %v", err)
	}

	// A valid, newer queued run for the real agent.
	validRunID := uuid.New()
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO agent_run (id,agent_id,business_id,tenant_root_id,trigger,target_type,target_id,status,correlation_id,created_at,updated_at)
		 VALUES ($1,$2,$3,$3,'manual','ticket',$4,'queued',$5,now(),now())`,
		validRunID, ag.ID, s.businessID, ticketID, "corr-"+validRunID.String()); err != nil {
		t.Fatalf("seed valid run: %v", err)
	}

	claimed, err := (&AgentRunStore{DB: tdb.App}).ClaimNextQueuedRun(ctx)
	if err != nil {
		t.Fatalf("ClaimNextQueuedRun: %v", err)
	}
	if claimed == nil || claimed.RunID != validRunID {
		t.Fatalf("claimed = %+v, want the valid run %s (orphan must be skipped)", claimed, validRunID)
	}
	if st := superRunStatus(ctx, t, tdb, orphanRunID); st != "failed" {
		t.Fatalf("orphan run status = %q, want failed", st)
	}
}

func superRunStatus(ctx context.Context, t *testing.T, tdb *testdb.TestDB, runID uuid.UUID) string {
	t.Helper()
	var st string
	if err := tdb.Super.QueryRow(ctx, `SELECT status FROM agent_run WHERE id=$1`, runID).Scan(&st); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	return st
}
