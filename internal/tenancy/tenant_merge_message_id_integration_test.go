//go:build integration

package tenancy_test

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/tenancy"
)

const mergeAttachmentContent = "synthetic attachment"

// seedSupportMessage seeds an archived ticket, message, and attachment in a
// business. It returns the native message ID.
func seedSupportMessage(
	ctx context.Context,
	t *testing.T,
	tdb *testdb.TestDB,
	store blob.Store,
	business uuid.UUID,
	messageID, historicalID string,
) uuid.UUID {
	t.Helper()
	requester, ticket, message, attachment := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	blobKey := blob.Key(business, business, ticket, attachment)
	execTenantMergeSeed(ctx, t, tdb, "seed support message",
		tenantMergeSeedStatement{sql: `
		INSERT INTO requester (id, business_id, tenant_root_id, email)
		VALUES ($1, $2, $2, $3)`,
			args: []any{requester, business, "requester-" + uuid.NewString() + "@x.test"}},
		tenantMergeSeedStatement{sql: `
		INSERT INTO ticket (id, business_id, tenant_root_id, requester_id, subject, reply_token,
			historical_source, historical_id, historical_hash, historical_metadata)
		VALUES ($1, $2, $2, $3, 'Shared Message-ID', $4, 'merge-fixture', $5, decode(repeat('00', 32), 'hex'), '{}'::jsonb)`,
			args: []any{ticket, business, requester, "merge-mid-" + uuid.NewString(), historicalID}},
		tenantMergeSeedStatement{sql: `
		INSERT INTO ticket_message (id, ticket_id, business_id, tenant_root_id, direction, message_id, body_text,
			historical_id, historical_metadata)
		VALUES ($1, $2, $3, $3, 'inbound', $4, 'shared message-id fixture', $5, '{}'::jsonb)`,
			args: []any{message, ticket, business, messageID, historicalID}},
		tenantMergeSeedStatement{sql: `
		INSERT INTO attachment (id, ticket_message_id, business_id, tenant_root_id, blob_key,
			filename, content_type, size, staff_only, historical_id)
		VALUES ($1, $2, $3, $3, $4, 'example.txt', 'text/plain', $5, true, $6)`,
			args: []any{attachment, message, business, blobKey, len(mergeAttachmentContent), historicalID}},
	)
	if err := store.Put(ctx, blobKey, []byte(mergeAttachmentContent), "text/plain"); err != nil {
		t.Fatalf("seed attachment bytes: %v", err)
	}
	return message
}

// TestTenantMergeAllowsSharedMessageIDAcrossTenants (manyforge-jkgj) — since 0141
// Message-ID idempotency is UNIQUE (business_id, message_id). A merge rewrites only
// tenant_root_id and every business keeps its id, so two tenants that each received
// the same email can no longer collide on any unique index. The preflight used to
// block such a merge with ticket_message_id_collision (a tenant-wide check left from
// the old (tenant_root_id, message_id) key); it now succeeds and both messages
// survive under the destination root.
func TestTenantMergeAllowsSharedMessageIDAcrossTenants(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	store := newTenantMergeMemoryBlob()
	svc := &tenancy.Service{DB: tdb.App, Blob: store}

	// Dropping the blocker rests on ticket_message having no unique key that a
	// tenant_root_id rewrite could make collide. Pin the full set so a new one forces
	// a review of the merge preflight. Historical IDs are scoped to the ticket, and
	// cutover preserves each ticket's ID.
	rows, err := tdb.Super.Query(ctx, `
		SELECT pg_get_indexdef(i.indexrelid)
		FROM pg_index i
		WHERE i.indrelid = 'ticket_message'::regclass AND i.indisunique
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var uniques []string
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			t.Fatal(err)
		}
		uniques = append(uniques, def[strings.Index(def, " USING "):])
	}
	rows.Close()
	wantUniques := []string{
		" USING btree (business_id, message_id)",
		" USING btree (connector_id, external_id) WHERE (connector_id IS NOT NULL)",
		" USING btree (id)",
		" USING btree (id, tenant_root_id)",
		" USING btree (source_approval_item_id) WHERE (source_approval_item_id IS NOT NULL)",
		" USING btree (ticket_id, historical_id) WHERE (historical_id IS NOT NULL)",
	}
	sort.Strings(uniques)
	if !reflect.DeepEqual(uniques, wantUniques) {
		t.Fatalf("ticket_message unique keys = %q, want %q; re-check the merge preflight's Message-ID policy", uniques, wantUniques)
	}

	actor, sourceRoot := seedFounder(ctx, t, tdb, "merge-mid-source@x.test")
	_, destinationRoot := seedFounder(ctx, t, tdb, "merge-mid-destination@x.test")
	addDirectOwner(ctx, t, tdb, actor, destinationRoot)

	const sharedMessageID = "same-mail-to-both-tenants@example.com"
	const sharedHistoricalID = "synthetic-archive-1"
	sourceMessage := seedSupportMessage(ctx, t, tdb, store, sourceRoot, sharedMessageID, sharedHistoricalID)
	destinationMessage := seedSupportMessage(ctx, t, tdb, store, destinationRoot, sharedMessageID, sharedHistoricalID)

	operation, err := svc.CreateTenantMergeOperation(
		ctx, actor, sourceRoot, destinationRoot, "shared-message-id",
	)
	if err != nil {
		t.Fatalf("create operation: %v", err)
	}
	ready, err := svc.PreflightTenantMerge(ctx, actor, operation.ID)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if hasFinding(ready.Conflicts, "ticket_message_id_collision") ||
		(ready.ReconciliationPlan != nil &&
			hasFinding(ready.ReconciliationPlan.Conflicts, "ticket_message_id_collision")) {
		t.Fatalf("preflight still reports ticket_message_id_collision: %+v", ready.Conflicts)
	}
	if ready.Status != "ready" {
		t.Fatalf("preflight status = %q; conflicts=%+v", ready.Status, ready.Conflicts)
	}

	passwordHash, err := auth.HashPassword(tenantMergeTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Super.Exec(ctx, `
		UPDATE account SET password_hash=$2 WHERE id=(SELECT account_id FROM principal WHERE id=$1)`,
		actor, passwordHash,
	); err != nil {
		t.Fatalf("seed merge confirmation password: %v", err)
	}
	succeeded, err := svc.ConfirmTenantMerge(ctx, actor, operation.ID, "Root", "Root", tenantMergeTestPassword)
	if err != nil {
		t.Fatalf("cutover: %v", err)
	}
	if succeeded.Status != "succeeded" {
		t.Fatalf("cutover status = %q; events=%+v", succeeded.Status, succeeded.Events)
	}

	for _, c := range []struct {
		message, business uuid.UUID
	}{{sourceMessage, sourceRoot}, {destinationMessage, destinationRoot}} {
		var root, business uuid.UUID
		var messageID string
		var ticketHistory, messageHistory, attachmentHistory, blobKey string
		var staffOnly bool
		if err := tdb.Super.QueryRow(ctx, `
			SELECT m.tenant_root_id, m.business_id, m.message_id, t.historical_id,
				m.historical_id, a.historical_id, a.staff_only, a.blob_key
			FROM ticket_message m
			JOIN ticket t ON t.id=m.ticket_id AND t.tenant_root_id=m.tenant_root_id AND t.business_id=m.business_id
			JOIN attachment a ON a.ticket_message_id=m.id AND a.tenant_root_id=m.tenant_root_id AND a.business_id=m.business_id
			WHERE m.id=$1`,
			c.message,
		).Scan(&root, &business, &messageID, &ticketHistory, &messageHistory, &attachmentHistory, &staffOnly, &blobKey); err != nil {
			t.Fatalf("read merged message %s: %v", c.message, err)
		}
		if root != destinationRoot || business != c.business || messageID != sharedMessageID {
			t.Errorf("merged message %s root/business/message_id = %s/%s/%q, want %s/%s/%q",
				c.message, root, business, messageID, destinationRoot, c.business, sharedMessageID)
		}
		if ticketHistory != sharedHistoricalID || messageHistory != sharedHistoricalID || attachmentHistory != sharedHistoricalID || !staffOnly {
			t.Errorf("merge changed archive IDs or attachment visibility for message %s", c.message)
		}
		content, err := store.Get(ctx, blobKey)
		if err != nil || string(content) != mergeAttachmentContent || !strings.HasPrefix(blobKey, destinationRoot.String()+"/") {
			t.Errorf("merged message %s references an invalid attachment object: %v", c.message, err)
		}
	}
	var shared int
	if err := tdb.Super.QueryRow(ctx,
		"SELECT count(*) FROM ticket_message WHERE tenant_root_id=$1 AND message_id=$2",
		destinationRoot, sharedMessageID,
	).Scan(&shared); err != nil {
		t.Fatal(err)
	}
	if shared != 2 {
		t.Errorf("messages with the shared Message-ID under the merged root = %d, want 2", shared)
	}
}
