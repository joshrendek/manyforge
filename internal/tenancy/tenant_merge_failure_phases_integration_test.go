//go:build integration

package tenancy_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/tenancy"
)

type tenantMergeFailurePhase struct {
	name             string
	wantStage        string
	manifestTable    string
	statusTransition string
	constraint       bool
	sourceResidue    bool
	attachment       bool
}

func TestTenantMergeCutoverRollsBackEveryFailurePhase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	svc := &tenancy.Service{DB: tdb.App}

	if _, err := tdb.Super.Exec(ctx, `
		CREATE FUNCTION tenant_merge_test_fail_statement() RETURNS trigger
		LANGUAGE plpgsql AS $fn$
		BEGIN
		    RAISE EXCEPTION 'injected statement failure';
		END;
		$fn$;

		CREATE FUNCTION tenant_merge_test_fail_status() RETURNS trigger
		LANGUAGE plpgsql AS $fn$
		BEGIN
		    IF NEW.status = TG_ARGV[0] THEN
		        RAISE EXCEPTION 'injected status failure';
		    END IF;
		    RETURN NEW;
		END;
		$fn$;

		CREATE FUNCTION tenant_merge_test_fail_deferred() RETURNS trigger
		LANGUAGE plpgsql AS $fn$
		BEGIN
		    RAISE EXCEPTION 'injected deferred failure';
		END;
		$fn$;

		CREATE FUNCTION tenant_merge_test_insert_source_residue()
		RETURNS trigger
		LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $fn$
		DECLARE
		    operation tenant_merge_operation%ROWTYPE;
		BEGIN
		    SELECT * INTO STRICT operation
		    FROM tenant_merge_operation
		    WHERE id = current_setting(
		        'manyforge.tenant_merge_operation'
		    )::uuid;
		    INSERT INTO notification (
		        tenant_root_id, principal_id, kind, ref
		    ) VALUES (
		        operation.source_root_id,
		        operation.actor_principal_id,
		        'tenant_merge_test_residue',
		        '{}'::jsonb
		    );
		    RETURN NULL;
		END;
		$fn$`,
	); err != nil {
		t.Fatalf("install failure-injection functions: %v", err)
	}

	phases := []tenantMergeFailurePhase{
		{
			name:             "mark_running",
			wantStage:        "mark_running",
			statusTransition: "running",
		},
		{
			name:       "attachment_preconditions",
			wantStage:  "attachment_preconditions",
			attachment: true,
		},
	}
	rows, err := tdb.Super.Query(ctx, `
		SELECT table_name::text
		FROM tenant_merge_manifest
		ORDER BY
		    CASE table_name::text
		        WHEN 'business' THEN 0
		        WHEN 'business_closure' THEN 1
		        WHEN 'role' THEN 2
		        WHEN 'principal' THEN 3
		        WHEN 'membership' THEN 4
		        ELSE 5
		    END,
		    table_name`)
	if err != nil {
		t.Fatalf("list cutover manifest phases: %v", err)
	}
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("scan cutover manifest phase: %v", err)
		}
		phases = append(phases, tenantMergeFailurePhase{
			name:          "rewrite_" + table,
			wantStage:     table,
			manifestTable: table,
		})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate cutover manifest phases: %v", err)
	}
	rows.Close()
	phases = append(phases,
		tenantMergeFailurePhase{
			name:          "source_residue",
			wantStage:     "source_residue",
			sourceResidue: true,
		},
		tenantMergeFailurePhase{
			name:       "constraint_validation",
			wantStage:  "constraint_validation",
			constraint: true,
		},
		tenantMergeFailurePhase{
			name:             "mark_succeeded",
			wantStage:        "mark_succeeded",
			statusTransition: "succeeded",
		},
	)

	for phaseIndex, phase := range phases {
		phaseIndex := phaseIndex
		phase := phase
		t.Run(phase.name, func(t *testing.T) {
			actor, sourceRoot := seedFounder(
				ctx, t, tdb,
				"failure-source-"+uuid.NewString()+"@x.test",
			)
			_, destinationRoot := seedFounder(
				ctx, t, tdb,
				"failure-destination-"+uuid.NewString()+"@x.test",
			)
			addDirectOwner(ctx, t, tdb, actor, destinationRoot)
			destinationParent, err := svc.CreateSubBusiness(
				ctx, actor, destinationRoot,
				"Failure destination",
			)
			if err != nil {
				t.Fatalf("create destination parent: %v", err)
			}
			if _, err := svc.CreateSubBusiness(
				ctx, actor, sourceRoot, "Failure source child",
			); err != nil {
				t.Fatalf("create source child: %v", err)
			}

			cleanup := installTenantMergeFailurePhase(
				ctx, t, tdb, phase,
			)
			defer cleanup()

			if phase.attachment {
				seedMalformedTenantMergeAttachment(
					ctx, t, tdb, actor, sourceRoot,
				)
			}

			sourceBefore := rootSnapshot(ctx, t, tdb, sourceRoot)
			destinationBefore := rootSnapshot(
				ctx, t, tdb, destinationRoot,
			)
			operation, err := svc.CreateTenantMergeOperation(
				ctx, actor, sourceRoot, destinationParent.ID,
				"failure-phase-"+phase.name,
			)
			if err != nil {
				t.Fatalf("create operation: %v", err)
			}
			ready, err := svc.PreflightTenantMerge(
				ctx, actor, operation.ID,
			)
			if err != nil || ready.Status != "ready" {
				t.Fatalf("preflight: status=%q err=%v conflicts=%+v",
					ready.Status, err, ready.Conflicts)
			}
			authorizeTenantMergeCutover(
				ctx, t, tdb, operation.ID,
			)
			if phase.attachment {
				if _, err := tdb.Super.Exec(ctx, `
					UPDATE tenant_merge_operation
					SET attachments_staged_at = now(),
					    attachments_staged_generation =
					        preflight_generation,
					    attachments_staged_count = attachment_count,
					    attachments_staged_bytes = attachment_bytes
					WHERE id = $1`,
					operation.ID,
				); err != nil {
					t.Fatalf("mark malformed attachment staged: %v", err)
				}
			}

			failed, err := svc.CutoverTenantMerge(
				ctx, actor, operation.ID,
			)
			if err != nil {
				t.Fatalf("cutover returned database error: %v", err)
			}
			if failed.Status != "failed" || failed.Failure == nil ||
				failed.Failure.Stage != phase.wantStage {
				t.Fatalf("failure %d status/detail = %q/%+v, want stage %q; events=%+v",
					phaseIndex, failed.Status, failed.Failure,
					phase.wantStage, failed.Events)
			}
			if failed.Manifest != nil {
				t.Errorf("failed phase emitted success manifest: %+v",
					failed.Manifest)
			}
			if after := rootSnapshot(
				ctx, t, tdb, sourceRoot,
			); !reflect.DeepEqual(after, sourceBefore) {
				t.Error("failed phase changed source tenant rows")
			}
			if after := rootSnapshot(
				ctx, t, tdb, destinationRoot,
			); !reflect.DeepEqual(after, destinationBefore) {
				t.Error("failed phase changed destination tenant rows")
			}
			var fences, receipts int
			if err := tdb.Super.QueryRow(ctx, `
				SELECT
				    (SELECT count(*) FROM tenant_merge_fence
				     WHERE operation_id=$1),
				    (SELECT count(*) FROM audit_entry
				     WHERE action='tenant.merge.completed'
				       AND correlation_id=$2)`,
				operation.ID, failed.CorrelationID.String(),
			).Scan(&fences, &receipts); err != nil {
				t.Fatalf("inspect failed phase cleanup: %v", err)
			}
			if fences != 0 || receipts != 0 {
				t.Errorf("failed phase fences/receipts = %d/%d, want 0/0",
					fences, receipts)
			}
		})
	}
}

func installTenantMergeFailurePhase(
	ctx context.Context,
	t *testing.T,
	tdb *testdb.TestDB,
	phase tenantMergeFailurePhase,
) func() {
	t.Helper()
	triggerName := "tenant_merge_test_failure"
	switch {
	case phase.manifestTable != "":
		table := pgx.Identifier{phase.manifestTable}.Sanitize()
		if _, err := tdb.Super.Exec(ctx,
			"CREATE TRIGGER "+triggerName+
				" BEFORE UPDATE ON "+table+
				" FOR EACH STATEMENT EXECUTE FUNCTION "+
				"tenant_merge_test_fail_statement()",
		); err != nil {
			t.Fatalf("install table failure trigger on %s: %v",
				phase.manifestTable, err)
		}
		return func() {
			_, _ = tdb.Super.Exec(context.Background(),
				"DROP TRIGGER IF EXISTS "+triggerName+" ON "+table)
		}
	case phase.statusTransition != "":
		if phase.statusTransition != "running" &&
			phase.statusTransition != "succeeded" {
			t.Fatalf("unreviewed failure transition %q",
				phase.statusTransition)
		}
		if _, err := tdb.Super.Exec(ctx,
			"CREATE TRIGGER "+triggerName+
				" BEFORE UPDATE OF status ON tenant_merge_operation"+
				" FOR EACH ROW EXECUTE FUNCTION "+
				"tenant_merge_test_fail_status('"+
				phase.statusTransition+"')",
		); err != nil {
			t.Fatalf("install %s transition failure trigger: %v",
				phase.statusTransition, err)
		}
		return func() {
			_, _ = tdb.Super.Exec(context.Background(), `
				DROP TRIGGER IF EXISTS tenant_merge_test_failure
				ON tenant_merge_operation`)
		}
	case phase.constraint:
		if _, err := tdb.Super.Exec(ctx, `
			CREATE CONSTRAINT TRIGGER tenant_merge_test_failure
			AFTER UPDATE ON business
			DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW
			EXECUTE FUNCTION tenant_merge_test_fail_deferred()`,
		); err != nil {
			t.Fatalf("install deferred failure trigger: %v", err)
		}
		return func() {
			_, _ = tdb.Super.Exec(context.Background(), `
				DROP TRIGGER IF EXISTS tenant_merge_test_failure
				ON business`)
		}
	case phase.sourceResidue:
		if _, err := tdb.Super.Exec(ctx, `
			ALTER TABLE notification
			    DISABLE TRIGGER tenant_merge_write_fence;
			CREATE TRIGGER tenant_merge_test_failure
			AFTER UPDATE ON ticket_tag
			FOR EACH STATEMENT
			EXECUTE FUNCTION tenant_merge_test_insert_source_residue()`,
		); err != nil {
			t.Fatalf("install source-residue injection: %v", err)
		}
		return func() {
			_, _ = tdb.Super.Exec(context.Background(), `
				DROP TRIGGER IF EXISTS tenant_merge_test_failure
				    ON ticket_tag;
				ALTER TABLE notification
				    ENABLE TRIGGER tenant_merge_write_fence`)
		}
	default:
		return func() {}
	}
}

func seedMalformedTenantMergeAttachment(
	ctx context.Context,
	t *testing.T,
	tdb *testdb.TestDB,
	actor, sourceRoot uuid.UUID,
) {
	t.Helper()
	requesterID := uuid.New()
	ticketID := uuid.New()
	messageID := uuid.New()
	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin malformed attachment seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	statements := []struct {
		sql  string
		args []any
	}{
		{`
			INSERT INTO requester (
			    id, business_id, tenant_root_id, email
			) VALUES ($1, $2, $2, $3)`,
			[]any{
				requesterID, sourceRoot,
				"failure-attachment-" + uuid.NewString() + "@x.test",
			}},
		{`
			INSERT INTO ticket (
			    id, business_id, tenant_root_id, requester_id, subject,
			    reply_token
			) VALUES (
			    $1, $2, $2, $3, 'Malformed attachment', $4
			)`,
			[]any{
				ticketID, sourceRoot, requesterID,
				"failure-attachment-" + uuid.NewString(),
			}},
		{`
			INSERT INTO ticket_message (
			    id, ticket_id, business_id, tenant_root_id, direction,
			    author_principal_id, message_id, body_text
			) VALUES (
			    $1, $2, $3, $3, 'outbound', $4, $5,
			    'malformed attachment fixture'
			)`,
			[]any{
				messageID, ticketID, sourceRoot, actor,
				"<failure-attachment-" + uuid.NewString() + "@x.test>",
			}},
		{`
			INSERT INTO attachment (
			    ticket_message_id, business_id, tenant_root_id,
			    blob_key, content_type, size
			) VALUES (
			    $1, $2, $2, 'not-rooted-at-source/object',
			    'text/plain', 1
			)`,
			[]any{messageID, sourceRoot}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed malformed attachment: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit malformed attachment seed: %v", err)
	}
}
