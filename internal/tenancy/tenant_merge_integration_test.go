//go:build integration

package tenancy_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/tenancy"
)

func addDirectOwner(
	ctx context.Context,
	t *testing.T,
	tdb *testdb.TestDB,
	principal, root uuid.UUID,
) {
	t.Helper()
	ownerRole := presetRole(ctx, t, tdb, "owner")
	if _, err := tdb.Super.Exec(ctx, `
		INSERT INTO membership
		    (principal_id, business_id, tenant_root_id, role_id, granted_at)
		VALUES ($1, $2, $2, $3, now())`,
		principal, root, ownerRole,
	); err != nil {
		t.Fatalf("add direct owner: %v", err)
	}
}

func rootSnapshot(
	ctx context.Context,
	t *testing.T,
	tdb *testdb.TestDB,
	root uuid.UUID,
) map[string]any {
	t.Helper()
	var raw []byte
	if err := tdb.Super.QueryRow(ctx,
		"SELECT tenant_merge_root_snapshot($1)", root,
	).Scan(&raw); err != nil {
		t.Fatalf("root snapshot: %v", err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("decode root snapshot: %v", err)
	}
	return snapshot
}

func hasFinding(findings []tenancy.TenantMergeFinding, code string) bool {
	for _, finding := range findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func TestTenantMergeOperationAndPreflightContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	svc := &tenancy.Service{DB: tdb.App}

	actor, sourceRoot := seedFounder(ctx, t, tdb, "merge-source-owner@x.test")
	destinationFounder, destinationRoot := seedFounder(ctx, t, tdb, "merge-destination-owner@x.test")
	addDirectOwner(ctx, t, tdb, actor, destinationRoot)

	parent, err := svc.CreateSubBusiness(ctx, actor, destinationRoot, "Destination parent")
	if err != nil {
		t.Fatalf("create destination parent: %v", err)
	}

	t.Run("operation creation is actor-bound, idempotent, and non-oracular", func(t *testing.T) {
		operation, err := svc.CreateTenantMergeOperation(
			ctx, actor, sourceRoot, parent.ID, "merge-idempotency-1",
		)
		if err != nil {
			t.Fatalf("create operation: %v", err)
		}
		if operation.Status != "preflight_required" {
			t.Errorf("initial status = %q, want preflight_required", operation.Status)
		}
		if operation.SourceRootID != sourceRoot ||
			operation.DestinationParentID != parent.ID ||
			operation.DestinationRootID != destinationRoot ||
			operation.ActorPrincipalID != actor {
			t.Errorf("operation binding is wrong: %+v", operation)
		}
		if len(operation.Events) != 1 || operation.Events[0].Event != "operation.created" {
			t.Errorf("creation audit events = %+v", operation.Events)
		}

		replay, err := svc.CreateTenantMergeOperation(
			ctx, actor, sourceRoot, parent.ID, "merge-idempotency-1",
		)
		if err != nil {
			t.Fatalf("replay operation: %v", err)
		}
		if replay.ID != operation.ID {
			t.Errorf("idempotent replay id = %s, want %s", replay.ID, operation.ID)
		}
		got, err := svc.GetTenantMergeOperation(ctx, actor, operation.ID)
		if err != nil || got.ID != operation.ID {
			t.Fatalf("get durable operation: id=%s err=%v", got.ID, err)
		}
		_, otherActorErr := svc.GetTenantMergeOperation(
			ctx, destinationFounder, operation.ID,
		)
		if !errors.Is(otherActorErr, errs.ErrNotFound) {
			t.Fatalf("other actor get: want ErrNotFound, got %v", otherActorErr)
		}

		_, err = svc.CreateTenantMergeOperation(
			ctx, actor, sourceRoot, destinationRoot, "merge-idempotency-1",
		)
		if !errors.Is(err, errs.ErrConflict) {
			t.Fatalf("incompatible key reuse: want ErrConflict, got %v", err)
		}

		_, unknownErr := svc.CreateTenantMergeOperation(
			ctx, actor, uuid.New(), parent.ID, "merge-unknown",
		)
		_, unauthorizedErr := svc.CreateTenantMergeOperation(
			ctx, destinationFounder, sourceRoot, parent.ID, "merge-unauthorized",
		)
		if !errors.Is(unknownErr, errs.ErrNotFound) ||
			!errors.Is(unauthorizedErr, errs.ErrNotFound) {
			t.Fatalf("unknown/unauthorized must both be ErrNotFound: unknown=%v unauthorized=%v",
				unknownErr, unauthorizedErr)
		}
	})

	operation, err := svc.CreateTenantMergeOperation(
		ctx, actor, sourceRoot, parent.ID, "merge-preflight",
	)
	if err != nil {
		t.Fatalf("create preflight operation: %v", err)
	}

	t.Run("preflight is deterministic, complete, ready, and tenant-read-only", func(t *testing.T) {
		sourceBefore := rootSnapshot(ctx, t, tdb, sourceRoot)
		destinationBefore := rootSnapshot(ctx, t, tdb, destinationRoot)

		first, err := svc.PreflightTenantMerge(ctx, actor, operation.ID)
		if err != nil {
			t.Fatalf("first preflight: %v", err)
		}
		if first.Status != "ready" {
			t.Fatalf("status = %q, want ready; conflicts=%+v", first.Status, first.Conflicts)
		}
		if first.PreflightGeneration == nil ||
			first.SourceGeneration == nil ||
			first.DestinationGeneration == nil ||
			first.SchemaHash == nil ||
			first.SchemaVersion == nil ||
			first.InventoryVersion == nil {
			t.Fatalf("preflight generations/schema were not persisted: %+v", first)
		}
		if *first.InventoryVersion != 1 || *first.SchemaVersion != 114 {
			t.Errorf("manifest/schema versions = %d/%d, want 1/114",
				*first.InventoryVersion, *first.SchemaVersion)
		}
		if len(first.TableMetrics) != 51 {
			t.Errorf("table metrics = %d, want 51", len(first.TableMetrics))
		}
		if metric := first.TableMetrics["business"]; metric.ContentDigest == "" ||
			metric.StableIDDigest == "" {
			t.Errorf("business fingerprints are incomplete: %+v", metric)
		}
		if first.SourceBusinesses == nil || *first.SourceBusinesses != 1 {
			t.Errorf("source business count = %v, want 1", first.SourceBusinesses)
		}
		if first.ResultingDepth == nil || *first.ResultingDepth != 2 {
			t.Errorf("resulting depth = %v, want 2", first.ResultingDepth)
		}
		if len(first.Conflicts) != 0 {
			t.Errorf("unexpected blockers: %+v", first.Conflicts)
		}
		if _, ok := first.ModuleCounts["tenancy"]; !ok {
			t.Errorf("deterministic module counts omit tenancy: %+v", first.ModuleCounts)
		}

		second, err := svc.PreflightTenantMerge(ctx, actor, operation.ID)
		if err != nil {
			t.Fatalf("second preflight: %v", err)
		}
		if *second.PreflightGeneration != *first.PreflightGeneration ||
			!reflect.DeepEqual(second.ModuleCounts, first.ModuleCounts) ||
			!reflect.DeepEqual(second.TableMetrics, first.TableMetrics) {
			t.Errorf("unchanged preflight is not deterministic")
		}

		if after := rootSnapshot(ctx, t, tdb, sourceRoot); !reflect.DeepEqual(after, sourceBefore) {
			t.Error("preflight changed source tenant rows")
		}
		if after := rootSnapshot(ctx, t, tdb, destinationRoot); !reflect.DeepEqual(after, destinationBefore) {
			t.Error("preflight changed destination tenant rows")
		}
	})

	t.Run("root-wide collisions and lifecycle failures are machine blockers", func(t *testing.T) {
		sourceRole := uuid.New()
		destinationRole := uuid.New()
		for _, args := range [][]any{
			{sourceRole, sourceRoot},
			{destinationRole, destinationRoot},
		} {
			if _, err := tdb.Super.Exec(ctx, `
				INSERT INTO role (id, tenant_root_id, key, name, is_locked, created_at)
				VALUES ($1, $2, 'merge-collision', 'Collision', false, now())`,
				args...,
			); err != nil {
				t.Fatalf("seed role collision: %v", err)
			}
		}

		blocked, err := svc.PreflightTenantMerge(ctx, actor, operation.ID)
		if err != nil {
			t.Fatalf("collision preflight: %v", err)
		}
		if blocked.Status != "preflight_required" ||
			!hasFinding(blocked.Conflicts, "custom_role_key_collision") {
			t.Errorf("custom-role collision did not block: status=%s conflicts=%+v",
				blocked.Status, blocked.Conflicts)
		}

		if _, err := tdb.Super.Exec(ctx,
			"DELETE FROM role WHERE id IN ($1, $2)", sourceRole, destinationRole,
		); err != nil {
			t.Fatalf("remove role collision: %v", err)
		}
		if _, err := tdb.Super.Exec(ctx,
			"UPDATE business SET status='archived' WHERE id=$1", sourceRoot,
		); err != nil {
			t.Fatalf("archive source: %v", err)
		}
		archived, err := svc.PreflightTenantMerge(ctx, actor, operation.ID)
		if err != nil {
			t.Fatalf("archived preflight: %v", err)
		}
		if archived.Status != "preflight_required" ||
			!hasFinding(archived.Conflicts, "source_not_active") {
			t.Errorf("archived source did not block: status=%s conflicts=%+v",
				archived.Status, archived.Conflicts)
		}
		if _, err := tdb.Super.Exec(ctx,
			"UPDATE business SET status='active' WHERE id=$1", sourceRoot,
		); err != nil {
			t.Fatalf("restore source: %v", err)
		}

		outboxID := uuid.New()
		if _, err := tdb.Super.Exec(ctx, `
			INSERT INTO outbox (id, tenant_root_id, topic, payload)
			VALUES ($1, $2::uuid, 'ticket.created',
			        jsonb_build_object('tenant_root_id', ($2::uuid)::text))`,
			outboxID, sourceRoot,
		); err != nil {
			t.Fatalf("seed unknown root-bearing payload: %v", err)
		}
		unknownScope, err := svc.PreflightTenantMerge(ctx, actor, operation.ID)
		if err != nil {
			t.Fatalf("unknown payload scope preflight: %v", err)
		}
		if !hasFinding(unknownScope.Conflicts, "unknown_outbox_tenant_scope") {
			t.Errorf("unknown root-bearing payload did not block: %+v", unknownScope.Conflicts)
		}
		if _, err := tdb.Super.Exec(ctx, "DELETE FROM outbox WHERE id=$1", outboxID); err != nil {
			t.Fatalf("remove unknown payload: %v", err)
		}

		if _, err := tdb.Super.Exec(ctx, `
			CREATE TABLE tenant_merge_unclassified_test (
				id uuid PRIMARY KEY,
				tenant_root_id uuid NOT NULL
			)`); err != nil {
			t.Fatalf("create unclassified tenant table: %v", err)
		}
		schemaBlocked, err := svc.PreflightTenantMerge(ctx, actor, operation.ID)
		if err != nil {
			t.Fatalf("schema coverage preflight: %v", err)
		}
		if !hasFinding(schemaBlocked.Conflicts, "schema_manifest_mismatch") {
			t.Errorf("unclassified tenant table did not block: %+v", schemaBlocked.Conflicts)
		}
		if _, err := tdb.Super.Exec(ctx, "DROP TABLE tenant_merge_unclassified_test"); err != nil {
			t.Fatalf("drop unclassified tenant table: %v", err)
		}
	})

	t.Run("source, destination, and schema changes each invalidate ready", func(t *testing.T) {
		ready, err := svc.PreflightTenantMerge(ctx, actor, operation.ID)
		if err != nil || ready.Status != "ready" {
			t.Fatalf("ready before staleness checks: status=%s err=%v conflicts=%+v",
				ready.Status, err, ready.Conflicts)
		}

		if _, err := tdb.Super.Exec(ctx,
			"UPDATE business SET name='Source changed' WHERE id=$1", sourceRoot,
		); err != nil {
			t.Fatalf("mutate source: %v", err)
		}
		sourceStale, err := svc.ValidateTenantMergePreflight(ctx, actor, operation.ID)
		if err != nil || sourceStale.Current ||
			sourceStale.Operation.Status != "preflight_required" {
			t.Fatalf("source mutation did not stale: result=%+v err=%v", sourceStale, err)
		}

		ready, err = svc.PreflightTenantMerge(ctx, actor, operation.ID)
		if err != nil || ready.Status != "ready" {
			t.Fatalf("re-preflight after source change: status=%s err=%v", ready.Status, err)
		}
		if _, err := tdb.Super.Exec(ctx,
			"UPDATE business SET name='Destination changed' WHERE id=$1", destinationRoot,
		); err != nil {
			t.Fatalf("mutate destination: %v", err)
		}
		destinationStale, err := svc.ValidateTenantMergePreflight(ctx, actor, operation.ID)
		if err != nil || destinationStale.Current {
			t.Fatalf("destination mutation did not stale: result=%+v err=%v", destinationStale, err)
		}

		ready, err = svc.PreflightTenantMerge(ctx, actor, operation.ID)
		if err != nil || ready.Status != "ready" {
			t.Fatalf("re-preflight after destination change: status=%s err=%v", ready.Status, err)
		}
		if _, err := tdb.Super.Exec(ctx,
			"UPDATE schema_migrations SET version=999", // restored below before test cleanup
		); err != nil {
			t.Fatalf("mutate schema generation: %v", err)
		}
		schemaStale, err := svc.ValidateTenantMergePreflight(ctx, actor, operation.ID)
		if err != nil || schemaStale.Current {
			t.Fatalf("schema mutation did not stale: result=%+v err=%v", schemaStale, err)
		}
		if _, err := tdb.Super.Exec(ctx,
			"UPDATE schema_migrations SET version=114",
		); err != nil {
			t.Fatalf("restore schema version: %v", err)
		}

		var staleEvents int
		if err := tdb.Super.QueryRow(ctx, `
			SELECT count(*) FROM tenant_merge_operation_event
			WHERE operation_id=$1 AND event='preflight.stale'`,
			operation.ID,
		).Scan(&staleEvents); err != nil {
			t.Fatalf("count stale events: %v", err)
		}
		if staleEvents != 3 {
			t.Errorf("stale transition audit events = %d, want 3", staleEvents)
		}
	})

	t.Run("control-plane tables have no direct app-role read surface", func(t *testing.T) {
		directErr := tdb.App.WithPrincipal(ctx, actor, func(tx pgx.Tx) error {
			var count int
			return tx.QueryRow(ctx,
				"SELECT count(*) FROM tenant_merge_operation",
			).Scan(&count)
		})
		var pgErr *pgconn.PgError
		if !errors.As(directErr, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("direct control-plane read error = %v, want SQLSTATE 42501", directErr)
		}

		var tableGrants int
		if err := tdb.Super.QueryRow(ctx, `
			SELECT count(*)
			FROM information_schema.role_table_grants
			WHERE grantee = 'manyforge_app'
			  AND table_name LIKE 'tenant_merge_%'`,
		).Scan(&tableGrants); err != nil {
			t.Fatalf("inspect control-plane grants: %v", err)
		}
		if tableGrants != 0 {
			t.Errorf("manyforge_app has %d direct tenant-merge table grants, want 0", tableGrants)
		}

		for _, signature := range []string{
			"tenant_merge_authorized(uuid,uuid,uuid)",
			"tenant_merge_schema_state()",
			"tenant_merge_root_snapshot(uuid)",
			"tenant_merge_operation_json(uuid)",
		} {
			var executable bool
			if err := tdb.Super.QueryRow(ctx,
				"SELECT has_function_privilege('manyforge_app', $1, 'EXECUTE')",
				signature,
			).Scan(&executable); err != nil {
				t.Fatalf("inspect helper grant %s: %v", signature, err)
			}
			if executable {
				t.Errorf("manyforge_app can execute internal helper %s", signature)
			}
		}
	})
}

func TestTenantMergeCutoverMovesCompleteHierarchyAndIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	svc := &tenancy.Service{DB: tdb.App}

	actor, sourceRoot := seedFounder(ctx, t, tdb, "cutover-source-owner@x.test")
	destinationFounder, destinationRoot := seedFounder(
		ctx, t, tdb, "cutover-destination-owner@x.test",
	)
	addDirectOwner(ctx, t, tdb, actor, destinationRoot)

	destinationParent, err := svc.CreateSubBusiness(
		ctx, actor, destinationRoot, "Destination parent",
	)
	if err != nil {
		t.Fatalf("create destination parent: %v", err)
	}
	sourceChild, err := svc.CreateSubBusiness(ctx, actor, sourceRoot, "Source child")
	if err != nil {
		t.Fatalf("create source child: %v", err)
	}

	customRole := uuid.New()
	if _, err := tdb.Super.Exec(ctx, `
		INSERT INTO role (id, tenant_root_id, key, name, is_locked, created_at)
		VALUES ($1, $2, 'source-specialist', 'Source specialist', false, now())`,
		customRole, sourceRoot,
	); err != nil {
		t.Fatalf("seed source custom role: %v", err)
	}

	operation, err := svc.CreateTenantMergeOperation(
		ctx, actor, sourceRoot, destinationParent.ID, "cutover-success",
	)
	if err != nil {
		t.Fatalf("create operation: %v", err)
	}
	ready, err := svc.PreflightTenantMerge(ctx, actor, operation.ID)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if ready.Status != "ready" {
		t.Fatalf("preflight status = %q; conflicts=%+v", ready.Status, ready.Conflicts)
	}
	reversed, err := svc.CreateTenantMergeOperation(
		ctx, actor, destinationRoot, sourceRoot, "cutover-reversed",
	)
	if err != nil {
		t.Fatalf("create reversed operation: %v", err)
	}
	reversedReady, err := svc.PreflightTenantMerge(ctx, actor, reversed.ID)
	if err != nil || reversedReady.Status != "ready" {
		t.Fatalf("reversed preflight: status=%q err=%v conflicts=%+v",
			reversedReady.Status, err, reversedReady.Conflicts)
	}

	_, err = svc.CutoverTenantMerge(ctx, destinationFounder, operation.ID)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("non-actor cutover: want ErrNotFound, got %v", err)
	}

	directErr := tdb.App.WithPrincipal(ctx, actor, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE business SET tenant_root_id=$2 WHERE id=$1`,
			sourceChild.ID, destinationRoot,
		)
		return err
	})
	if directErr == nil ||
		(!strings.Contains(directErr.Error(), "tenant_root_id is immutable") &&
			!strings.Contains(directErr.Error(), "permission denied")) {
		t.Fatalf("direct app root rewrite error = %v, want guarded rejection", directErr)
	}

	succeeded, err := svc.CutoverTenantMerge(ctx, actor, operation.ID)
	if err != nil {
		t.Fatalf("cutover: %v", err)
	}
	if succeeded.Status != "succeeded" {
		t.Fatalf("cutover status = %q; events=%+v", succeeded.Status, succeeded.Events)
	}
	if len(succeeded.Events) < 4 ||
		succeeded.Events[len(succeeded.Events)-2].Event != "cutover.started" ||
		succeeded.Events[len(succeeded.Events)-1].Event != "cutover.succeeded" {
		t.Fatalf("cutover events = %+v", succeeded.Events)
	}
	successMetadata := succeeded.Events[len(succeeded.Events)-1].Metadata
	tableCounts, ok := successMetadata["table_counts"].(map[string]any)
	if !ok || len(tableCounts) != len(ready.TableMetrics) {
		t.Errorf("cutover table counts = %#v, want %d manifest entries",
			successMetadata["table_counts"], len(ready.TableMetrics))
	}
	if got, ok := successMetadata["rewritten_rows"].(float64); !ok ||
		int64(got) != ready.AffectedRows {
		t.Errorf("cutover rewritten_rows = %#v, want %d",
			successMetadata["rewritten_rows"], ready.AffectedRows)
	}

	var sourceParent, sourceTenant, childParent, childTenant uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `
		SELECT source.parent_id, source.tenant_root_id,
		       child.parent_id, child.tenant_root_id
		FROM business source
		JOIN business child ON child.id=$2
		WHERE source.id=$1`,
		sourceRoot, sourceChild.ID,
	).Scan(&sourceParent, &sourceTenant, &childParent, &childTenant); err != nil {
		t.Fatalf("read moved hierarchy: %v", err)
	}
	if sourceParent != destinationParent.ID || sourceTenant != destinationRoot {
		t.Errorf("source root moved to parent/root %s/%s, want %s/%s",
			sourceParent, sourceTenant, destinationParent.ID, destinationRoot)
	}
	if childParent != sourceRoot || childTenant != destinationRoot {
		t.Errorf("source child parent/root = %s/%s, want %s/%s",
			childParent, childTenant, sourceRoot, destinationRoot)
	}

	ancestors := ancestorIDs(ctx, t, tdb, sourceChild.ID)
	wantDepths := map[string]int32{
		sourceChild.ID.String():       0,
		sourceRoot.String():           1,
		destinationParent.ID.String(): 2,
		destinationRoot.String():      3,
	}
	if !reflect.DeepEqual(ancestors, wantDepths) {
		t.Errorf("moved child ancestors = %v, want %v", ancestors, wantDepths)
	}

	var movedRoleRoot uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT tenant_root_id FROM role WHERE id=$1", customRole,
	).Scan(&movedRoleRoot); err != nil {
		t.Fatalf("read moved custom role: %v", err)
	}
	if movedRoleRoot != destinationRoot {
		t.Errorf("custom role root = %s, want %s", movedRoleRoot, destinationRoot)
	}
	var staleOutboxPayloads int
	if err := tdb.Super.QueryRow(ctx, `
		SELECT count(*)
		FROM outbox
		WHERE tenant_root_id=$1
		  AND topic IN ('business.created', 'agent.action.approved')
		  AND payload->>'tenant_root_id'=$2`,
		destinationRoot, sourceRoot.String(),
	).Scan(&staleOutboxPayloads); err != nil {
		t.Fatalf("inspect rewritten outbox payloads: %v", err)
	}
	if staleOutboxPayloads != 0 {
		t.Errorf("%d moved outbox payloads retain the source root", staleOutboxPayloads)
	}

	rows, err := tdb.Super.Query(ctx,
		"SELECT table_name::text FROM tenant_merge_manifest ORDER BY table_name")
	if err != nil {
		t.Fatalf("list manifest: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("scan manifest: %v", err)
		}
		var residue int64
		if err := tdb.Super.QueryRow(ctx,
			"SELECT count(*) FROM "+
				pgx.Identifier{table}.Sanitize()+
				" WHERE tenant_root_id=$1",
			sourceRoot,
		).Scan(&residue); err != nil {
			t.Fatalf("count source residue in %s: %v", table, err)
		}
		if residue != 0 {
			t.Errorf("%s retains %d source-root rows", table, residue)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate manifest: %v", err)
	}

	eventCount := len(succeeded.Events)
	replay, err := svc.CutoverTenantMerge(ctx, actor, operation.ID)
	if err != nil {
		t.Fatalf("replay cutover: %v", err)
	}
	if replay.Status != "succeeded" || len(replay.Events) != eventCount {
		t.Errorf("replay = status %q, %d events; want succeeded, %d events",
			replay.Status, len(replay.Events), eventCount)
	}

	_, err = svc.CutoverTenantMerge(ctx, actor, reversed.ID)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("reversed cutover after success: want ErrNotFound, got %v", err)
	}
	reversedAfter, err := svc.GetTenantMergeOperation(ctx, actor, reversed.ID)
	if err != nil || reversedAfter.Status != "ready" {
		t.Fatalf("reversed operation after conflict: status=%q err=%v",
			reversedAfter.Status, err)
	}
}

func TestTenantMergeCutoverRollsBackInjectedFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	svc := &tenancy.Service{DB: tdb.App}

	actor, sourceRoot := seedFounder(ctx, t, tdb, "rollback-source-owner@x.test")
	_, destinationRoot := seedFounder(ctx, t, tdb, "rollback-destination-owner@x.test")
	addDirectOwner(ctx, t, tdb, actor, destinationRoot)
	destinationParent, err := svc.CreateSubBusiness(
		ctx, actor, destinationRoot, "Rollback destination parent",
	)
	if err != nil {
		t.Fatalf("create destination parent: %v", err)
	}
	sourceChild, err := svc.CreateSubBusiness(ctx, actor, sourceRoot, "Rollback source child")
	if err != nil {
		t.Fatalf("create source child: %v", err)
	}

	customRole := uuid.New()
	if _, err := tdb.Super.Exec(ctx, `
		INSERT INTO role (id, tenant_root_id, key, name, is_locked, created_at)
		VALUES ($1, $2, 'rollback-role', 'Rollback role', false, now())`,
		customRole, sourceRoot,
	); err != nil {
		t.Fatalf("seed failure role: %v", err)
	}
	if _, err := tdb.Super.Exec(ctx, `
		CREATE FUNCTION tenant_merge_test_fail_role() RETURNS trigger
		LANGUAGE plpgsql AS $fn$
		BEGIN
		    IF NEW.tenant_root_id IS DISTINCT FROM OLD.tenant_root_id THEN
		        RAISE EXCEPTION 'injected tenant merge failure';
		    END IF;
		    RETURN NEW;
		END;
		$fn$;
		CREATE TRIGGER tenant_merge_test_fail_role_trg
		BEFORE UPDATE ON role
		FOR EACH ROW EXECUTE FUNCTION tenant_merge_test_fail_role()`,
	); err != nil {
		t.Fatalf("install failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = tdb.Super.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS tenant_merge_test_fail_role_trg ON role;
			DROP FUNCTION IF EXISTS tenant_merge_test_fail_role()`)
	})

	sourceBefore := rootSnapshot(ctx, t, tdb, sourceRoot)
	destinationBefore := rootSnapshot(ctx, t, tdb, destinationRoot)
	operation, err := svc.CreateTenantMergeOperation(
		ctx, actor, sourceRoot, destinationParent.ID, "cutover-rollback",
	)
	if err != nil {
		t.Fatalf("create operation: %v", err)
	}
	ready, err := svc.PreflightTenantMerge(ctx, actor, operation.ID)
	if err != nil || ready.Status != "ready" {
		t.Fatalf("preflight: status=%q err=%v conflicts=%+v",
			ready.Status, err, ready.Conflicts)
	}

	failed, err := svc.CutoverTenantMerge(ctx, actor, operation.ID)
	if err != nil {
		t.Fatalf("cutover returned database error: %v", err)
	}
	if failed.Status != "failed" ||
		failed.Events[len(failed.Events)-1].Event != "cutover.failed" {
		t.Fatalf("failed cutover = status %q events=%+v", failed.Status, failed.Events)
	}
	if got := failed.Events[len(failed.Events)-1].Metadata["stage"]; got != "role" {
		t.Errorf("failure stage = %v, want role", got)
	}

	if after := rootSnapshot(ctx, t, tdb, sourceRoot); !reflect.DeepEqual(after, sourceBefore) {
		t.Error("failed cutover changed source tenant rows")
	}
	if after := rootSnapshot(ctx, t, tdb, destinationRoot); !reflect.DeepEqual(after, destinationBefore) {
		t.Error("failed cutover changed destination tenant rows")
	}

	var parent *uuid.UUID
	var root uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT parent_id, tenant_root_id FROM business WHERE id=$1",
		sourceRoot,
	).Scan(&parent, &root); err != nil {
		t.Fatalf("read source after rollback: %v", err)
	}
	if parent != nil || root != sourceRoot {
		t.Errorf("source after rollback parent/root = %v/%s, want nil/%s",
			parent, root, sourceRoot)
	}
	ancestors := ancestorIDs(ctx, t, tdb, sourceChild.ID)
	if len(ancestors) != 2 ||
		ancestors[sourceChild.ID.String()] != 0 ||
		ancestors[sourceRoot.String()] != 1 {
		t.Errorf("source closure after rollback = %v", ancestors)
	}
}
