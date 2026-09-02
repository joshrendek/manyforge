//go:build integration

package security_regression

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func TestAutomationDefinersLeaseReplayPauseAndScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	businessID := uuid.New()
	listID := uuid.New()
	subscriberID := uuid.New()
	automationID := uuid.New()
	versionID := uuid.New()
	seedTx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer seedTx.Rollback(ctx)
	seedStatements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at)
		  VALUES ($1,NULL,$1,'Automation Co','active',now(),now())`, []any{businessID}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id)
		  VALUES ($1,$1,0,$1)`, []any{businessID}},
		{`INSERT INTO mailing_list (id,business_id,tenant_root_id,slug,name,double_opt_in,status)
		  VALUES ($2,$1,$1,'automation','Automation',false,'active')`, []any{businessID, listID}},
		{`INSERT INTO list_subscriber (
			id,business_id,tenant_root_id,list_id,email,status,consent_source,consent_attested_by
		  ) VALUES ($3,$1,$1,$2,'person@example.test','active','manual',$4)`,
			[]any{businessID, listID, subscriberID, uuid.New()}},
		{`INSERT INTO automation (id,business_id,tenant_root_id,name,status,allow_reenroll)
		  VALUES ($2,$1,$1,'Welcome','draft',false)`, []any{businessID, automationID}},
		{`INSERT INTO automation_version (
			id,business_id,tenant_root_id,automation_id,number,status,graph,
			trigger_kind,trigger_ref,activated_at
		  ) VALUES (
			$3,$1,$1,$2,1,'active',
			jsonb_build_object(
				'nodes', jsonb_build_array(
					jsonb_build_object('id','n_trigger','kind','trigger','config',jsonb_build_object('list_id',$4::text)),
					jsonb_build_object('id','n_exit','kind','exit','config','{}'::jsonb)
				),
				'edges', '[{"id":"e1","from":"n_trigger","to":"n_exit","branch":null}]'::jsonb
			),
			'list_joined',$4::text,now()
		  )`, []any{businessID, automationID, versionID, listID.String()}},
		{`UPDATE automation SET status='active',active_version_id=$2,updated_at=now()
		  WHERE id=$1`, []any{automationID, versionID}},
	}
	for _, statement := range seedStatements {
		if _, err := seedTx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("seed automation: %v", err)
		}
	}
	if err := seedTx.Commit(ctx); err != nil {
		t.Fatalf("commit automation seed: %v", err)
	}

	eventID := uuid.New()
	var inserted int
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT automation_enroll_for_trigger($1,$2,$3,$4,$5,$6,$7)`,
			businessID, businessID, "list_joined", listID.String(), subscriberID, eventID,
			time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)).Scan(&inserted)
	}); err != nil || inserted != 1 {
		t.Fatalf("first trigger enrollment = %d, err=%v", inserted, err)
	}
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT automation_enroll_for_trigger($1,$2,$3,$4,$5,$6,$7)`,
			businessID, businessID, "list_joined", listID.String(), subscriberID, eventID,
			time.Date(2026, 9, 2, 12, 0, 1, 0, time.UTC)).Scan(&inserted)
	}); err != nil || inserted != 0 {
		t.Fatalf("replayed trigger enrollment = %d, err=%v", inserted, err)
	}

	var enrollmentID uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id FROM automation_enrollment WHERE automation_id=$1", automationID,
	).Scan(&enrollmentID); err != nil {
		t.Fatal(err)
	}

	claimAt := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	claim := func(at time.Time) (uuid.UUID, int, error) {
		var id uuid.UUID
		var generation int
		err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT enrollment_id,claim_generation
				FROM automation_claim_due($1,1,interval '1 minute')`, at,
			).Scan(&id, &generation)
		})
		return id, generation, err
	}
	claimedID, generation1, err := claim(claimAt)
	if err != nil || claimedID != enrollmentID || generation1 != 1 {
		t.Fatalf("first claim = %s generation %d, err=%v", claimedID, generation1, err)
	}
	if _, _, err := claim(claimAt.Add(30 * time.Second)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim inside lease error = %v, want no rows", err)
	}
	claimedID, generation2, err := claim(claimAt.Add(2 * time.Minute))
	if err != nil || claimedID != enrollmentID || generation2 != 2 {
		t.Fatalf("reclaimed = %s generation %d, err=%v", claimedID, generation2, err)
	}

	record := func(generation int) (bool, error) {
		var changed bool
		err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT automation_record_step(
					$1,$2,'n_trigger','trigger','advanced','n_exit',$3,'active',NULL,'{}'::jsonb,$3
				)`, enrollmentID, generation, claimAt.Add(2*time.Minute)).Scan(&changed)
		})
		return changed, err
	}
	if changed, err := record(generation1); err != nil || changed {
		t.Fatalf("stale generation record = %t, err=%v", changed, err)
	}
	if changed, err := record(generation2); err != nil || !changed {
		t.Fatalf("current generation record = %t, err=%v", changed, err)
	}
	var secondNodeChanged bool
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT automation_record_step(
				$1,$2,'n_exit','exit','entered','n_exit',$3,'active',NULL,'{}'::jsonb,$3
			)`, enrollmentID, generation2, claimAt.Add(2*time.Minute)).Scan(&secondNodeChanged)
	}); err != nil || !secondNodeChanged {
		t.Fatalf("second node in one claim = %t, err=%v", secondNodeChanged, err)
	}

	if _, err := tdb.Super.Exec(ctx, "UPDATE automation SET status='paused' WHERE id=$1", automationID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := claim(claimAt.Add(3 * time.Minute)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("paused automation claim error = %v, want no rows", err)
	}
	if _, err := tdb.Super.Exec(ctx, "UPDATE automation SET status='active' WHERE id=$1", automationID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := claim(claimAt.Add(3 * time.Minute)); err != nil {
		t.Fatalf("resumed automation was not claimable: %v", err)
	}

	var exited int
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT automation_exit_for_subscriber($1,$2,'unsubscribed')",
			subscriberID, businessID).Scan(&exited)
	}); err != nil || exited != 1 {
		t.Fatalf("exit subscriber = %d, err=%v", exited, err)
	}

	var pgErr *pgconn.PgError
	err = tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT automation_enroll_for_trigger($1,$2,$3,$4,$5,$6,now())`,
			businessID, businessID, "list_joined", listID.String(), uuid.New(), uuid.New()).Scan(&inserted)
	})
	if !errors.As(err, &pgErr) || pgErr.Code != "22023" {
		t.Fatalf("foreign subscriber error = %v, want SQLSTATE 22023", err)
	}
}
