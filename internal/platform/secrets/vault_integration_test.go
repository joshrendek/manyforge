//go:build integration

package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestVaultPutOpenRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	seed := seedVaultTenant(ctx, t, tdb)

	v := NewVault(newTestSealer(t))
	secret := []byte(`{"email":"a@b.com","api_token":"super-secret-token"}`)

	var secretID uuid.UUID
	if err := tdb.App.WithPrincipal(ctx, seed.principalID, func(tx pgx.Tx) error {
		id, perr := v.Put(ctx, tx, seed.businessID, "connector", secret)
		secretID = id
		return perr
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	var sealed string
	if err := tdb.Super.QueryRow(ctx, "SELECT sealed_value FROM secret WHERE id=$1", secretID).Scan(&sealed); err != nil {
		t.Fatalf("read sealed: %v", err)
	}
	if strings.Contains(sealed, "super-secret-token") {
		t.Fatalf("plaintext token found in sealed_value: %q", sealed)
	}

	var got []byte
	if err := tdb.App.WithPrincipal(ctx, seed.principalID, func(tx pgx.Tx) error {
		b, oerr := v.Open(ctx, tx, seed.businessID, secretID)
		got = b
		return oerr
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(got) != string(secret) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, secret)
	}
}

func TestVaultOpenWrongBusinessNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	a := seedVaultTenant(ctx, t, tdb)
	b := seedVaultTenant(ctx, t, tdb)

	v := NewVault(newTestSealer(t))
	var secretID uuid.UUID
	if err := tdb.App.WithPrincipal(ctx, a.principalID, func(tx pgx.Tx) error {
		id, perr := v.Put(ctx, tx, a.businessID, "connector", []byte("x"))
		secretID = id
		return perr
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	err = tdb.App.WithPrincipal(ctx, b.principalID, func(tx pgx.Tx) error {
		_, oerr := v.Open(ctx, tx, b.businessID, secretID)
		return oerr
	})
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("want ErrNotFound for cross-tenant open, got %v", err)
	}
}

func TestVaultDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	seed := seedVaultTenant(ctx, t, tdb)
	v := NewVault(newTestSealer(t))

	var secretID uuid.UUID
	if err := tdb.App.WithPrincipal(ctx, seed.principalID, func(tx pgx.Tx) error {
		id, perr := v.Put(ctx, tx, seed.businessID, "connector", []byte("to-delete"))
		secretID = id
		return perr
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := tdb.App.WithPrincipal(ctx, seed.principalID, func(tx pgx.Tx) error {
		return v.Delete(ctx, tx, seed.businessID, secretID)
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	err = tdb.App.WithPrincipal(ctx, seed.principalID, func(tx pgx.Tx) error {
		_, oerr := v.Open(ctx, tx, seed.businessID, secretID)
		return oerr
	})
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
}
