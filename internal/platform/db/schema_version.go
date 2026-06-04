package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SchemaVersion returns the applied golang-migrate version and its dirty flag. A missing
// schema_migrations table or no row (a brand-new, never-migrated database) reports
// version 0, dirty=false — i.e. "behind any code that has migrations".
func (d *DB) SchemaVersion(ctx context.Context) (version int, dirty bool, err error) {
	e := d.pool.QueryRow(ctx, "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty)
	if e != nil {
		var pgErr *pgconn.PgError
		if errors.As(e, &pgErr) && pgErr.Code == "42P01" { // undefined_table: never migrated
			return 0, false, nil
		}
		if errors.Is(e, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read schema version: %w", e)
	}
	return version, dirty, nil
}

// VerifySchemaCurrent fails when the database is behind the code (applied < expected) or
// mid-migration (dirty), so the server refuses to serve rather than 500 at query time on a
// column/table a pending migration adds. A database AHEAD of the code is allowed: queries
// use explicit column lists, so extra columns are harmless (and this is normal mid-deploy).
func (d *DB) VerifySchemaCurrent(ctx context.Context, expected int) error {
	applied, dirty, err := d.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("database schema is dirty at version %d (a migration failed mid-apply); resolve it before serving", applied)
	}
	if applied < expected {
		return fmt.Errorf("database schema is behind the code: applied version %d, code expects %d — run `manyforge migrate`", applied, expected)
	}
	return nil
}
