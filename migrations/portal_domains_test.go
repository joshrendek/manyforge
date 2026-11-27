//go:build integration

package migrations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func TestPortalDomainsMigration(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ctx)

	var rls bool
	var policies []string
	if err := database.Super.QueryRow(ctx, `SELECT relrowsecurity FROM pg_class WHERE oid = to_regclass('portal_domain')`).Scan(&rls); err != nil {
		t.Fatalf("portal_domain table: %v", err)
	}
	if !rls {
		t.Error("portal_domain row level security is not enabled")
	}
	rows, err := database.Super.Query(ctx, `SELECT policyname FROM pg_policies WHERE tablename = 'portal_domain'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		policies = append(policies, name)
	}
	rows.Close()
	if len(policies) != 1 || policies[0] != "portal_domain_rls" {
		t.Errorf("policies = %v, want exactly [portal_domain_rls]", policies)
	}

	var unique bool
	var pred string
	if err := database.Super.QueryRow(ctx, `SELECT i.indisunique, pg_get_expr(i.indpred, i.indrelid)
		FROM pg_index i WHERE i.indexrelid = to_regclass('portal_domain_verified_uq')`).Scan(&unique, &pred); err != nil {
		t.Fatalf("portal_domain_verified_uq: %v", err)
	}
	if !unique || pred != "(verified_at IS NOT NULL)" {
		t.Errorf("verified index unique=%v predicate=%q, want unique partial on verified_at IS NOT NULL", unique, pred)
	}

	a, b := seedResendBusiness(ctx, t, database), seedResendBusiness(ctx, t, database)
	insert := func(business uuid.UUID, host string, verifiedAt any) error {
		_, err := database.Super.Exec(ctx, `INSERT INTO portal_domain
			(business_id,tenant_root_id,hostname,verify_token,verified_at) VALUES ($1,$1,$2,'mf-verify=seed',$3)`,
			business, host, verifiedAt)
		return err
	}
	host := "portal.example.test"
	if err := insert(a, host, nil); err != nil {
		t.Fatalf("pending insert a: %v", err)
	}
	if err := insert(b, host, nil); err != nil {
		t.Fatalf("pending insert b: %v", err)
	}

	resolve := func() *uuid.UUID {
		t.Helper()
		var got *uuid.UUID
		err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT business_id FROM portal_domain_resolve($1)`, host).Scan(&got)
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return got
	}
	if got := resolve(); got != nil {
		t.Fatalf("pending hostname resolved to %v, want no row", *got)
	}

	if _, err := database.Super.Exec(ctx, `UPDATE portal_domain SET verified_at = now() WHERE business_id = $1`, a); err != nil {
		t.Fatalf("verify a: %v", err)
	}
	if got := resolve(); got == nil || *got != a {
		t.Fatalf("verified hostname resolved to %v, want %v", got, a)
	}
	_, err = database.Super.Exec(ctx, `UPDATE portal_domain SET verified_at = now() WHERE business_id = $1`, b)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("second verified row error = %v, want unique violation 23505", err)
	}

	// primary: earliest verified hostname wins; pending ones are ignored.
	early := time.Now().UTC().Add(-time.Hour)
	if err := insert(a, "early.example.test", early); err != nil {
		t.Fatalf("early insert: %v", err)
	}
	if err := insert(a, "pending.example.test", nil); err != nil {
		t.Fatalf("pending insert: %v", err)
	}
	var primary *string
	err = database.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT hostname FROM portal_domain_primary($1)`, a).Scan(&primary)
	})
	if err != nil {
		t.Fatalf("primary: %v", err)
	}
	if primary == nil || *primary != "early.example.test" {
		t.Fatalf("primary = %v, want early.example.test", primary)
	}
}

// The generated queries must report a miss as pgx.ErrNoRows (never NULL into a
// non-pointer type) so services can treat it as "not found".
func TestPortalDomainLookupsThroughDbgen(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ctx)

	biz, empty := seedResendBusiness(ctx, t, database), seedResendBusiness(ctx, t, database)
	if _, err := database.Super.Exec(ctx, `INSERT INTO portal_domain
		(business_id,tenant_root_id,hostname,verify_token,verified_at) VALUES ($1,$1,'Portal.Example.Test','t',now())`, biz); err != nil {
		t.Fatal(err)
	}

	err = database.App.WithTx(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		got, err := q.ResolvePortalDomain(ctx, "portal.example.test")
		if err != nil || got != biz {
			t.Errorf("resolve hit = %v, %v; want %v", got, err, biz)
		}
		if _, err := q.ResolvePortalDomain(ctx, "other.example.test"); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("resolve miss err = %v, want pgx.ErrNoRows", err)
		}
		host, err := q.PrimaryPortalDomain(ctx, biz)
		if err != nil || host != "Portal.Example.Test" {
			t.Errorf("primary hit = %q, %v", host, err)
		}
		if _, err := q.PrimaryPortalDomain(ctx, empty); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("primary miss err = %v, want pgx.ErrNoRows", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
