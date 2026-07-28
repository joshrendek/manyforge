//go:build integration

package tenancy_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
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
		if *first.InventoryVersion != 1 || *first.SchemaVersion != 113 {
			t.Errorf("manifest/schema versions = %d/%d, want 1/113",
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
			"UPDATE schema_migrations SET version=113",
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
