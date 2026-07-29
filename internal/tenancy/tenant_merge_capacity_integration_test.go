//go:build integration

package tenancy_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/tenancy"
)

func TestTenantMergeCapacityPolicyPublishesAndRejectsEveryOversizeDimension(
	t *testing.T,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	svc := &tenancy.Service{DB: tdb.App}

	var limitsRaw []byte
	if err := tdb.Super.QueryRow(ctx,
		"SELECT tenant_merge_capacity_limits()",
	).Scan(&limitsRaw); err != nil {
		t.Fatalf("read published capacity limits: %v", err)
	}
	var limits map[string]int64
	if err := json.Unmarshal(limitsRaw, &limits); err != nil {
		t.Fatalf("decode published capacity limits: %v", err)
	}
	wantLimits := map[string]int64{
		"max_source_businesses":    1000,
		"max_resulting_depth":      10,
		"max_relational_rows":      250000,
		"max_relational_bytes":     1073741824,
		"max_attachment_objects":   10000,
		"max_attachment_bytes":     1073741824,
		"max_lock_wait_ms":         10000,
		"max_cutover_statement_ms": 60000,
		"release_gate_p95_ms":      30000,
	}
	for key, want := range wantLimits {
		if got := limits[key]; got != want {
			t.Errorf("published %s = %d, want %d", key, got, want)
		}
	}

	var findingsRaw []byte
	if err := tdb.Super.QueryRow(ctx, `
		SELECT tenant_merge_capacity_findings(
		    1001, 11, 250001, 1073741825, 10001, 1073741825
		)`,
	).Scan(&findingsRaw); err != nil {
		t.Fatalf("evaluate oversize capacity dimensions: %v", err)
	}
	var findings []tenancy.TenantMergeFinding
	if err := json.Unmarshal(findingsRaw, &findings); err != nil {
		t.Fatalf("decode capacity findings: %v", err)
	}
	for _, code := range []string{
		"capacity_businesses_exceeded",
		"resulting_depth_exceeded",
		"capacity_rows_exceeded",
		"capacity_bytes_exceeded",
		"capacity_attachments_exceeded",
		"capacity_attachment_bytes_exceeded",
	} {
		if !hasFinding(findings, code) {
			t.Errorf("oversize policy omitted %q: %+v", code, findings)
		}
	}

	actor, sourceRoot := seedFounder(
		ctx, t, tdb, "capacity-source-owner@x.test",
	)
	_, destinationRoot := seedFounder(
		ctx, t, tdb, "capacity-destination-owner@x.test",
	)
	addDirectOwner(ctx, t, tdb, actor, destinationRoot)
	destinationParent, err := svc.CreateSubBusiness(
		ctx, actor, destinationRoot, "Capacity destination parent",
	)
	if err != nil {
		t.Fatalf("create destination parent: %v", err)
	}

	// The root plus 999 direct children is exactly the supported 1,000
	// businesses. One more child must turn the same durable preflight into an
	// explicit blocker rather than attempting cutover.
	if _, err := tdb.Super.Exec(ctx, `
		WITH children AS (
		    INSERT INTO business (id, parent_id, tenant_root_id, name)
		    SELECT gen_random_uuid(), $1, $1,
		           'Capacity child ' || ordinal::text
		    FROM generate_series(1, 999) AS ordinal
		    RETURNING id
		)
		INSERT INTO business_closure (
		    ancestor_id, descendant_id, depth, tenant_root_id
		)
		SELECT id, id, 0, $1 FROM children
		UNION ALL
		SELECT $1, id, 1, $1 FROM children`,
		sourceRoot,
	); err != nil {
		t.Fatalf("seed supported business envelope: %v", err)
	}
	operation, err := svc.CreateTenantMergeOperation(
		ctx, actor, sourceRoot, destinationParent.ID, "capacity-boundary",
	)
	if err != nil {
		t.Fatalf("create capacity operation: %v", err)
	}
	atLimit, err := svc.PreflightTenantMerge(ctx, actor, operation.ID)
	if err != nil || atLimit.Status != "ready" {
		t.Fatalf("preflight at business limit: status=%q err=%v conflicts=%+v",
			atLimit.Status, err, atLimit.Conflicts)
	}
	if atLimit.SourceBusinesses == nil || *atLimit.SourceBusinesses != 1000 {
		t.Fatalf("business count at limit = %v, want 1000",
			atLimit.SourceBusinesses)
	}

	if _, err := tdb.Super.Exec(ctx, `
		WITH child AS (
		    INSERT INTO business (id, parent_id, tenant_root_id, name)
		    VALUES (gen_random_uuid(), $1, $1, 'Capacity child 1000')
		    RETURNING id
		)
		INSERT INTO business_closure (
		    ancestor_id, descendant_id, depth, tenant_root_id
		)
		SELECT id, id, 0, $1 FROM child
		UNION ALL
		SELECT $1, id, 1, $1 FROM child`,
		sourceRoot,
	); err != nil {
		t.Fatalf("seed first oversized business: %v", err)
	}
	oversized, err := svc.PreflightTenantMerge(ctx, actor, operation.ID)
	if err != nil {
		t.Fatalf("oversized business preflight: %v", err)
	}
	if oversized.Status != "preflight_required" ||
		!hasFinding(oversized.Conflicts, "capacity_businesses_exceeded") {
		t.Fatalf("oversized preflight = status %q conflicts=%+v",
			oversized.Status, oversized.Conflicts)
	}

	// The persisted-preflight backstop applies the same policy to every
	// dimension. This simulates a future preflight implementation attempting
	// to persist an over-limit ready result and proves the database fails it
	// closed.
	if _, err := tdb.Super.Exec(ctx, `
		UPDATE tenant_merge_operation
		SET conflicts = '[]'::jsonb,
		    status = 'ready',
		    ready_at = now(),
		    source_businesses = 1001,
		    resulting_depth = 11,
		    affected_rows = 250001,
		    estimated_bytes = 1073741825,
		    attachment_count = 10001,
		    attachment_bytes = 1073741825
		WHERE id = $1`,
		operation.ID,
	); err != nil {
		t.Fatalf("exercise persisted capacity backstop: %v", err)
	}
	var persistedStatus string
	var persistedFindingsRaw []byte
	if err := tdb.Super.QueryRow(ctx, `
		SELECT status, conflicts
		FROM tenant_merge_operation
		WHERE id=$1`,
		operation.ID,
	).Scan(&persistedStatus, &persistedFindingsRaw); err != nil {
		t.Fatalf("read persisted capacity backstop: %v", err)
	}
	var persistedFindings []tenancy.TenantMergeFinding
	if err := json.Unmarshal(
		persistedFindingsRaw, &persistedFindings,
	); err != nil {
		t.Fatalf("decode persisted capacity findings: %v", err)
	}
	if persistedStatus != "preflight_required" {
		t.Errorf("persisted oversize status = %q, want preflight_required",
			persistedStatus)
	}
	for _, code := range []string{
		"capacity_businesses_exceeded",
		"resulting_depth_exceeded",
		"capacity_rows_exceeded",
		"capacity_bytes_exceeded",
		"capacity_attachments_exceeded",
		"capacity_attachment_bytes_exceeded",
	} {
		if !hasFinding(persistedFindings, code) {
			t.Errorf("persisted capacity backstop omitted %q: %+v",
				code, persistedFindings)
		}
	}
}

func TestTenantMergeMaximumRowEnvelopeCompletesWithinPublishedP95(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	svc := &tenancy.Service{DB: tdb.App}

	actor, sourceRoot := seedFounder(
		ctx, t, tdb, "capacity-load-source-owner@x.test",
	)
	_, destinationRoot := seedFounder(
		ctx, t, tdb, "capacity-load-destination-owner@x.test",
	)
	addDirectOwner(ctx, t, tdb, actor, destinationRoot)
	destinationParent, err := svc.CreateSubBusiness(
		ctx, actor, destinationRoot, "Capacity load destination",
	)
	if err != nil {
		t.Fatalf("create destination parent: %v", err)
	}

	// A directly-seeded founder owns exactly three manifest rows: business,
	// closure, and membership. Add 249,997 notifications to exercise the
	// published 250,000-row cutover boundary exactly.
	if _, err := tdb.Super.Exec(ctx, `
		INSERT INTO notification (
		    id, tenant_root_id, principal_id, kind, ref
		)
		SELECT gen_random_uuid(), $1, $2, 'capacity_load',
		       jsonb_build_object('ordinal', ordinal)
		FROM generate_series(1, 249997) AS ordinal`,
		sourceRoot, actor,
	); err != nil {
		t.Fatalf("seed maximum row envelope: %v", err)
	}
	operation, err := svc.CreateTenantMergeOperation(
		ctx, actor, sourceRoot, destinationParent.ID,
		"capacity-maximum-row-envelope",
	)
	if err != nil {
		t.Fatalf("create maximum-envelope operation: %v", err)
	}
	ready, err := svc.PreflightTenantMerge(ctx, actor, operation.ID)
	if err != nil || ready.Status != "ready" {
		t.Fatalf("maximum-envelope preflight: status=%q rows=%d err=%v conflicts=%+v",
			ready.Status, ready.AffectedRows, err, ready.Conflicts)
	}
	if ready.AffectedRows != 250000 {
		t.Fatalf("maximum-envelope affected rows = %d, want 250000",
			ready.AffectedRows)
	}
	authorizeTenantMergeCutover(ctx, t, tdb, operation.ID)

	startedAt := time.Now()
	succeeded, err := svc.CutoverTenantMerge(ctx, actor, operation.ID)
	elapsed := time.Since(startedAt)
	if err != nil || succeeded.Status != "succeeded" {
		t.Fatalf("maximum-envelope cutover: status=%q elapsed=%s err=%v events=%+v",
			succeeded.Status, elapsed, err, succeeded.Events)
	}
	t.Logf("250,000-row cutover completed in %s", elapsed)
	if elapsed >= 30*time.Second {
		t.Errorf("maximum-envelope cutover elapsed = %s, want <30s", elapsed)
	}
	var sourceNotifications int64
	if err := tdb.Super.QueryRow(ctx, `
		SELECT count(*) FROM notification WHERE tenant_root_id=$1`,
		sourceRoot,
	).Scan(&sourceNotifications); err != nil {
		t.Fatalf("count maximum-envelope source residue: %v", err)
	}
	if sourceNotifications != 0 {
		t.Errorf("maximum-envelope source notifications = %d, want 0",
			sourceNotifications)
	}
}
