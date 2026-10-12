//go:build integration

package migrations_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func TestSupportSendAsMigrationDownUp(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ctx)

	columns := func() int {
		t.Helper()
		var n int
		if err := database.Super.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM information_schema.columns WHERE table_name='inbound_address' AND column_name='send_as') +
			(SELECT count(*) FROM information_schema.routines r JOIN information_schema.parameters p
			   ON p.specific_name = r.specific_name
			 WHERE r.routine_name='get_resend_support_identity' AND p.parameter_mode='OUT' AND p.parameter_name='inbound_address')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := columns(); got != 2 {
		t.Fatalf("0140 objects = %d, want 2 (send_as column + inbound_address output)", got)
	}
	if _, err := database.Super.Exec(ctx, readMigration(t, "0140_support_inbound_send_as.down.sql")); err != nil {
		t.Fatalf("0140 down: %v", err)
	}
	if got := columns(); got != 0 {
		t.Fatalf("0140 objects after down = %d, want 0", got)
	}
	if _, err := database.Super.Exec(ctx, readMigration(t, "0140_support_inbound_send_as.up.sql")); err != nil {
		t.Fatalf("0140 up: %v", err)
	}
	if got := columns(); got != 2 {
		t.Fatalf("0140 objects after re-up = %d, want 2", got)
	}
}

func TestSupportSendAsCheckConstraint(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ctx)

	biz := seedResendBusiness(ctx, t, database)
	domainID := seedResendDomain(ctx, t, database, biz, "support.check.example", "provider_route", true)
	custom := seedResendAddress(ctx, t, database, biz, domainID, "support@support.check.example")
	system := uuid.New()
	if _, err := database.Super.Exec(ctx, `INSERT INTO inbound_address (id,business_id,tenant_root_id,address,kind,created_at,updated_at)
		VALUES ($1,$2,$2,'sys@inbound.localhost','system',now(),now())`, system, biz); err != nil {
		t.Fatal(err)
	}

	set := func(id uuid.UUID, v string) error {
		_, err := database.Super.Exec(ctx, "UPDATE inbound_address SET send_as=$1 WHERE id=$2", v, id)
		return err
	}
	if err := set(custom, "support@check.example"); err != nil {
		t.Fatalf("valid send_as rejected: %v", err)
	}
	long := "a@" + strings.Repeat("b", 250) + ".com"
	for name, c := range map[string]struct {
		id uuid.UUID
		v  string
	}{
		"system address": {system, "support@check.example"},
		"no at sign":     {custom, "support.check.example"},
		"no dot":         {custom, "support@check"},
		"whitespace":     {custom, "sup port@check.example"},
		"too long":       {custom, long},
	} {
		var pgErr *pgconn.PgError
		if err := set(c.id, c.v); !errors.As(err, &pgErr) || pgErr.ConstraintName != "inbound_address_send_as_chk" {
			t.Errorf("%s: err = %v, want inbound_address_send_as_chk violation", name, err)
		}
	}
}
