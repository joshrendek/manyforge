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

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/tenancy"
)

// seedSupportMessage seeds requester -> ticket -> inbound ticket_message in
// business (a tenant root here) carrying the given RFC Message-ID, and returns the
// ticket_message id.
func seedSupportMessage(
	ctx context.Context,
	t *testing.T,
	tdb *testdb.TestDB,
	business uuid.UUID,
	messageID string,
) uuid.UUID {
	t.Helper()
	requester, ticket, message := uuid.New(), uuid.New(), uuid.New()
	execTenantMergeSeed(ctx, t, tdb, "seed support message",
		tenantMergeSeedStatement{sql: `
		INSERT INTO requester (id, business_id, tenant_root_id, email)
		VALUES ($1, $2, $2, $3)`,
			args: []any{requester, business, "requester-" + uuid.NewString() + "@x.test"}},
		tenantMergeSeedStatement{sql: `
		INSERT INTO ticket (id, business_id, tenant_root_id, requester_id, subject, reply_token)
		VALUES ($1, $2, $2, $3, 'Shared Message-ID', $4)`,
			args: []any{ticket, business, requester, "merge-mid-" + uuid.NewString()}},
		tenantMergeSeedStatement{sql: `
		INSERT INTO ticket_message (id, ticket_id, business_id, tenant_root_id, direction, message_id, body_text)
		VALUES ($1, $2, $3, $3, 'inbound', $4, 'shared message-id fixture')`,
			args: []any{message, ticket, business, messageID}},
	)
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
	svc := &tenancy.Service{DB: tdb.App}

	// Dropping the blocker rests on ticket_message having no unique key that a
	// tenant_root_id rewrite could make collide. Pin the full set so a new one forces
	// a review of the merge preflight.
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
	}
	sort.Strings(uniques)
	if !reflect.DeepEqual(uniques, wantUniques) {
		t.Fatalf("ticket_message unique keys = %q, want %q; re-check the merge preflight's Message-ID policy", uniques, wantUniques)
	}

	actor, sourceRoot := seedFounder(ctx, t, tdb, "merge-mid-source@x.test")
	_, destinationRoot := seedFounder(ctx, t, tdb, "merge-mid-destination@x.test")
	addDirectOwner(ctx, t, tdb, actor, destinationRoot)

	const sharedMessageID = "same-mail-to-both-tenants@example.com"
	sourceMessage := seedSupportMessage(ctx, t, tdb, sourceRoot, sharedMessageID)
	destinationMessage := seedSupportMessage(ctx, t, tdb, destinationRoot, sharedMessageID)

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

	authorizeTenantMergeCutover(ctx, t, tdb, operation.ID)
	succeeded, err := svc.CutoverTenantMerge(ctx, actor, operation.ID)
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
		if err := tdb.Super.QueryRow(ctx,
			"SELECT tenant_root_id, business_id, message_id FROM ticket_message WHERE id=$1",
			c.message,
		).Scan(&root, &business, &messageID); err != nil {
			t.Fatalf("read merged message %s: %v", c.message, err)
		}
		if root != destinationRoot || business != c.business || messageID != sharedMessageID {
			t.Errorf("merged message %s root/business/message_id = %s/%s/%q, want %s/%s/%q",
				c.message, root, business, messageID, destinationRoot, c.business, sharedMessageID)
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
