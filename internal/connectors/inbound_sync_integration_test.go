//go:build integration

package connectors

// TestInboundSyncSubscriber exercises the InboundSyncSubscriber end-to-end
// against a real Postgres instance. It verifies:
//   - The subscriber correctly upserts ticket + requester + messages + sync_state
//     by calling the SECURITY DEFINER functions (principal-less, no RLS context).
//   - Two Handle calls are idempotent: exactly ONE ticket, comments are NOT duplicated.
//   - A status change on the second Handle call (external-wins) updates the ticket.
//
// Sealer-sharing: the Service and InboundSyncSubscriber share the SAME *crypto.Sealer
// so the credential sealed by Service.Create can be successfully opened by the subscriber.

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/internal/platform/secrets"
)

// buildInboundSyncEvent builds a synthetic connector.inbound.sync outbox event.
func buildInboundSyncEvent(t *testing.T, connectorID uuid.UUID, externalID string, businessID uuid.UUID) events.Event {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"connector_id": connectorID,
		"external_id":  externalID,
		"business_id":  businessID,
	})
	if err != nil {
		t.Fatalf("marshal event payload: %v", err)
	}
	id, _ := uuid.NewV7()
	return events.Event{
		ID:      id,
		Topic:   TopicConnectorInboundSync,
		Payload: payload,
	}
}

// TestInboundSyncOrdersMessagesChronologically (manyforge-4d1) pins that the description + each
// comment land with their REAL external timestamps as ticket_message.created_at — not the shared
// reconcile-transaction now() — so the thread (ORDER BY created_at ASC, id ASC) sorts
// chronologically even when comments arrive out of order in the fetch slice.
func TestInboundSyncOrdersMessagesChronologically(t *testing.T) {
	ctx, tdb, seed := startConn(t)

	sharedSealer := newTestSealer(t)
	svc := &Service{DB: tdb.App, Vault: secrets.NewVault(sharedSealer), Verify: nil}
	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	// Distinct, well-separated timestamps. The description is oldest (issue created first); the
	// comments are intentionally listed NEWEST-FIRST in the slice (c_new before c_old) so a test
	// that merely preserved insertion order would still sort them wrong.
	tCreated := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	tOld := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	tNew := time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)

	issue := ExternalIssue{
		ExternalID:    "JIRA-7",
		URL:           "https://acme.atlassian.net/browse/JIRA-7",
		Title:         "Ordering bug",
		Status:        "Open",
		Priority:      "Normal",
		ReporterEmail: "reporter@acme.test",
		ReporterName:  "R",
		Description:   "the original request",
		CreatedAt:     tCreated,
		UpdatedAt:     tNew,
		Comments: []ExternalComment{
			{ExternalID: "c_new", Author: "A", Body: "later reply", CreatedAt: tNew},
			{ExternalID: "c_old", Author: "B", Body: "earlier reply", CreatedAt: tOld},
		},
	}
	reg := NewRegistry(svc)
	reg.Register("jira", func(rc ResolvedConnector) (TicketingConnector, error) {
		return &fakeConnector{issue: issue}, nil
	})
	sub := &InboundSyncSubscriber{DB: tdb.App, Sealer: sharedSealer, Registry: reg, Logger: slog.Default()}

	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return sub.Handle(ctx, tx, buildInboundSyncEvent(t, connID, "JIRA-7", seed.businessID))
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Each inbound message carries its real timestamp (PG stores microseconds; compare on equality
	// after truncating the seed to micros).
	wantAt := map[string]time.Time{
		"JIRA-7:description": tCreated,
		"c_old":              tOld,
		"c_new":              tNew,
	}
	for extID, want := range wantAt {
		var got time.Time
		if err := tdb.Super.QueryRow(ctx,
			`SELECT created_at FROM ticket_message WHERE connector_id=$1 AND external_id=$2`,
			connID, extID).Scan(&got); err != nil {
			t.Fatalf("read created_at for %q: %v", extID, err)
		}
		if !got.UTC().Equal(want.UTC()) {
			t.Fatalf("message %q created_at = %s, want %s (real external timestamp, not now())", extID, got.UTC(), want.UTC())
		}
	}

	// The thread sorts chronologically: description (09:00) → c_old (10:00) → c_new (11:00).
	rows, err := tdb.Super.Query(ctx,
		`SELECT external_id FROM ticket_message WHERE connector_id=$1 AND direction='inbound'
		   ORDER BY created_at ASC, id ASC`, connID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	defer rows.Close()
	var order []string
	for rows.Next() {
		var ext string
		if err := rows.Scan(&ext); err != nil {
			t.Fatalf("scan: %v", err)
		}
		order = append(order, ext)
	}
	want := []string{"JIRA-7:description", "c_old", "c_new"}
	if len(order) != len(want) {
		t.Fatalf("thread order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("thread order = %v, want %v (chronological)", order, want)
		}
	}
}

// TestInboundSyncSubscriber verifies the full subscriber flow: fetch → upsert → idempotent.
func TestInboundSyncSubscriber(t *testing.T) {
	ctx, tdb, seed := startConn(t)

	// Share ONE sealer between Service and Subscriber so unseal succeeds.
	sharedSealer := newTestSealer(t)
	vault := secrets.NewVault(sharedSealer)
	svc := &Service{DB: tdb.App, Vault: vault, Verify: nil}

	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	// Register a fake "jira" factory returning a canned issue with two comments.
	issue := ExternalIssue{
		ExternalID:    "JIRA-1",
		URL:           "https://acme.atlassian.net/browse/JIRA-1",
		Title:         "Login bug",
		Status:        "Done",
		Priority:      "High",
		ReporterEmail: "reporter@acme.test",
		ReporterName:  "R",
		UpdatedAt:     time.Now().UTC().Add(-10 * time.Minute),
		Comments: []ExternalComment{
			{ExternalID: "c1", Author: "A", Body: "first"},
			{ExternalID: "c2", Author: "B", Body: "second"},
		},
	}
	reg := NewRegistry(svc)
	reg.Register("jira", func(rc ResolvedConnector) (TicketingConnector, error) {
		return &fakeConnector{issue: issue}, nil
	})

	sub := &InboundSyncSubscriber{
		DB:       tdb.App,
		Sealer:   sharedSealer,
		Registry: reg,
		Logger:   slog.Default(),
	}

	ev := buildInboundSyncEvent(t, connID, "JIRA-1", seed.businessID)

	// --- First Handle: insert ---
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return sub.Handle(ctx, tx, ev)
	}); err != nil {
		t.Fatalf("first Handle: %v", err)
	}

	// Assert: exactly ONE ticket with correct connector + external_id.
	var ticketCount int
	var ticketStatus string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT COUNT(*), max(status::text)
		   FROM ticket WHERE connector_id=$1 AND external_id='JIRA-1'`,
		connID,
	).Scan(&ticketCount, &ticketStatus); err != nil {
		t.Fatalf("count tickets: %v", err)
	}
	if ticketCount != 1 {
		t.Fatalf("want 1 ticket after first Handle, got %d", ticketCount)
	}
	// Done → closed (external-wins mapping).
	if ticketStatus != "closed" {
		t.Fatalf("want status=closed (Done→closed mapping), got %q", ticketStatus)
	}

	// Assert: requester with reporter email.
	var requesterCount int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT COUNT(*) FROM requester WHERE tenant_root_id=$1 AND email='reporter@acme.test'`,
		seed.businessID,
	).Scan(&requesterCount); err != nil {
		t.Fatalf("count requester: %v", err)
	}
	if requesterCount != 1 {
		t.Fatalf("want 1 requester row, got %d", requesterCount)
	}

	// Assert: two ticket_message rows (direction=inbound).
	var msgCount int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT COUNT(*) FROM ticket_message
		   WHERE connector_id=$1 AND direction='inbound'
		     AND external_id IN ('c1','c2')`,
		connID,
	).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 2 {
		t.Fatalf("want 2 ticket_messages (c1, c2), got %d", msgCount)
	}

	// Assert: connector_sync_state row.
	var syncStateCount int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT COUNT(*) FROM connector_sync_state
		   WHERE connector_id=$1 AND external_id='JIRA-1'`,
		connID,
	).Scan(&syncStateCount); err != nil {
		t.Fatalf("count sync_state: %v", err)
	}
	if syncStateCount != 1 {
		t.Fatalf("want 1 connector_sync_state, got %d", syncStateCount)
	}

	// --- Second Handle (same event): idempotent — still ONE ticket, still TWO messages ---
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return sub.Handle(ctx, tx, ev)
	}); err != nil {
		t.Fatalf("second Handle (idempotent): %v", err)
	}

	if err := tdb.Super.QueryRow(ctx,
		`SELECT COUNT(*), max(status::text) FROM ticket WHERE connector_id=$1 AND external_id='JIRA-1'`,
		connID,
	).Scan(&ticketCount, &ticketStatus); err != nil {
		t.Fatalf("count tickets after 2nd Handle: %v", err)
	}
	if ticketCount != 1 {
		t.Fatalf("idempotent: want 1 ticket, got %d", ticketCount)
	}
	if ticketStatus != "closed" {
		t.Fatalf("idempotent: want status=closed, got %q", ticketStatus)
	}

	if err := tdb.Super.QueryRow(ctx,
		`SELECT COUNT(*) FROM ticket_message
		   WHERE connector_id=$1 AND direction='inbound'
		     AND external_id IN ('c1','c2')`,
		connID,
	).Scan(&msgCount); err != nil {
		t.Fatalf("count messages after 2nd Handle: %v", err)
	}
	if msgCount != 2 {
		t.Fatalf("idempotent: want 2 ticket_messages, got %d (dedupe broken)", msgCount)
	}
}

// TestInboundSyncDescriptionAsFirstMessage verifies the fix for the description bug: an
// issue carrying a Description (the original request body) produces an INBOUND ticket_message
// whose body == the description, keyed by the synthetic "<ExternalID>:description" external_id —
// and a second sync does NOT duplicate it (idempotent via the comment DEFINER's dedupe).
func TestInboundSyncDescriptionAsFirstMessage(t *testing.T) {
	ctx, tdb, seed := startConn(t)

	sharedSealer := newTestSealer(t)
	vault := secrets.NewVault(sharedSealer)
	svc := &Service{DB: tdb.App, Vault: vault, Verify: nil}

	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	const descBody = "The whole bug report lives in the description, with no comments."
	// An issue with a Description and NO comments — the exact case the bug stranded
	// (subject only, no inbound body).
	issue := ExternalIssue{
		ExternalID:    "JIRA-DESC",
		URL:           "https://acme.atlassian.net/browse/JIRA-DESC",
		Title:         "Description-only issue",
		Status:        "In Progress",
		Priority:      "High",
		ReporterEmail: "reporter@acme.test",
		ReporterName:  "R",
		Description:   descBody,
		UpdatedAt:     time.Now().UTC().Add(-10 * time.Minute),
	}
	reg := NewRegistry(svc)
	reg.Register("jira", func(rc ResolvedConnector) (TicketingConnector, error) {
		return &fakeConnector{issue: issue}, nil
	})

	sub := &InboundSyncSubscriber{
		DB:       tdb.App,
		Sealer:   sharedSealer,
		Registry: reg,
		Logger:   slog.Default(),
	}

	ev := buildInboundSyncEvent(t, connID, "JIRA-DESC", seed.businessID)

	// --- First Handle: the description becomes the first inbound message. ---
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return sub.Handle(ctx, tx, ev)
	}); err != nil {
		t.Fatalf("first Handle: %v", err)
	}

	const descExternalID = "JIRA-DESC:description"
	var gotBody string
	var msgCount int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT COUNT(*), max(body_text) FROM ticket_message
		   WHERE connector_id=$1 AND direction='inbound' AND external_id=$2`,
		connID, descExternalID,
	).Scan(&msgCount, &gotBody); err != nil {
		t.Fatalf("count description message: %v", err)
	}
	if msgCount != 1 {
		t.Fatalf("want exactly 1 inbound description message (external_id=%q), got %d", descExternalID, msgCount)
	}
	if gotBody != descBody {
		t.Fatalf("description message body = %q, want %q", gotBody, descBody)
	}

	// --- Second Handle (same event): idempotent — still exactly ONE description message. ---
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return sub.Handle(ctx, tx, ev)
	}); err != nil {
		t.Fatalf("second Handle (idempotent): %v", err)
	}
	if err := tdb.Super.QueryRow(ctx,
		`SELECT COUNT(*) FROM ticket_message
		   WHERE connector_id=$1 AND direction='inbound' AND external_id=$2`,
		connID, descExternalID,
	).Scan(&msgCount); err != nil {
		t.Fatalf("count description message after 2nd Handle: %v", err)
	}
	if msgCount != 1 {
		t.Fatalf("idempotent: want 1 description message, got %d (dedupe broken)", msgCount)
	}
}

// TestInboundSyncStatusChange verifies that when the external issue changes status on a
// subsequent sync, the ticket is updated (external-wins) and the snapshot is updated.
func TestInboundSyncStatusChange(t *testing.T) {
	ctx, tdb, seed := startConn(t)

	sharedSealer := newTestSealer(t)
	vault := secrets.NewVault(sharedSealer)
	svc := &Service{DB: tdb.App, Vault: vault, Verify: nil}

	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	// First sync: issue is "In Progress" → native 'open'.
	firstIssue := ExternalIssue{
		ExternalID:    "JIRA-2",
		URL:           "https://acme.atlassian.net/browse/JIRA-2",
		Title:         "Status test",
		Status:        "In Progress",
		Priority:      "Normal",
		ReporterEmail: "user@acme.test",
		ReporterName:  "User",
		UpdatedAt:     time.Now().UTC().Add(-20 * time.Minute),
	}

	// fakeConnector is pointer so we can swap the issue for the second call.
	fake := &fakeConnector{issue: firstIssue}
	reg := NewRegistry(svc)
	reg.Register("jira", func(rc ResolvedConnector) (TicketingConnector, error) {
		return fake, nil
	})

	sub := &InboundSyncSubscriber{
		DB:       tdb.App,
		Sealer:   sharedSealer,
		Registry: reg,
		Logger:   slog.Default(),
	}

	// We need a fresh context for each Handle call; reuse the parent ctx.
	ev1 := buildInboundSyncEvent(t, connID, "JIRA-2", seed.businessID)
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return sub.Handle(ctx, tx, ev1)
	}); err != nil {
		t.Fatalf("first Handle: %v", err)
	}

	var status1 string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT status::text FROM ticket WHERE connector_id=$1 AND external_id='JIRA-2'`,
		connID,
	).Scan(&status1); err != nil {
		t.Fatalf("read status after first sync: %v", err)
	}
	if status1 != "open" {
		t.Fatalf("want status=open for 'In Progress', got %q", status1)
	}

	// Second sync: issue is now "Done" → native 'closed' (external-wins).
	fake.issue.Status = "Done"
	fake.issue.UpdatedAt = time.Now().UTC()

	ev2 := buildInboundSyncEvent(t, connID, "JIRA-2", seed.businessID)
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return sub.Handle(ctx, tx, ev2)
	}); err != nil {
		t.Fatalf("second Handle: %v", err)
	}

	var ticketCount int
	var status2 string
	if err := tdb.Super.QueryRow(ctx,
		`SELECT COUNT(*), max(t.status::text)
		   FROM ticket t
		  WHERE t.connector_id=$1 AND t.external_id='JIRA-2'`,
		connID,
	).Scan(&ticketCount, &status2); err != nil {
		t.Fatalf("read after 2nd sync: %v", err)
	}
	if ticketCount != 1 {
		t.Fatalf("want exactly 1 ticket after status change, got %d", ticketCount)
	}
	if status2 != "closed" {
		t.Fatalf("want status=closed after Done sync, got %q", status2)
	}

	// Read snapshot separately to avoid max(jsonb) (no aggregate for jsonb).
	var snapshotRaw []byte
	if err := tdb.Super.QueryRow(ctx,
		`SELECT s.snapshot FROM connector_sync_state s
		   JOIN ticket t ON t.id = s.ticket_id
		  WHERE t.connector_id=$1 AND t.external_id='JIRA-2'`,
		connID,
	).Scan(&snapshotRaw); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	// Snapshot should reflect the updated status.
	var snap map[string]any
	if err := json.Unmarshal(snapshotRaw, &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if snap["status"] != "Done" {
		t.Fatalf("want snapshot.status=Done, got %v", snap["status"])
	}
}
