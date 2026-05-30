// Package db provides the pgx connection pool and the RLS principal context.
// The application connects as the non-superuser manyforge_app role; every
// tenant-scoped unit of work runs through WithPrincipal so Row-Level Security
// (Constitution Principle I) is in force for the transaction.
package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct{ pool *pgxpool.Pool }

// Open connects a pool using dsn (which should authenticate as manyforge_app).
func Open(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	// Fail-safe: never reuse a connection that still carries a principal GUC.
	// SET LOCAL is transaction-scoped so this should always be empty; if it is
	// not, discard the connection rather than risk a cross-request leak.
	cfg.AfterRelease = func(c *pgx.Conn) bool {
		var v string
		if err := c.QueryRow(context.Background(),
			"SELECT current_setting('manyforge.principal_id', true)").Scan(&v); err != nil {
			return false
		}
		return v == ""
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &DB{pool: pool}, nil
}

func (d *DB) Pool() *pgxpool.Pool { return d.pool }
func (d *DB) Close()              { d.pool.Close() }

// WithTx runs fn in a transaction WITHOUT a principal context — for system and
// auth operations on non-RLS tables (account, principal, refresh_token,
// one_time_token). Use WithPrincipal for tenant-scoped work.
func (d *DB) WithTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// WithPrincipal runs fn inside a transaction whose RLS context is scoped to
// principalID. A uuid.Nil principal sets an empty GUC, so RLS fails closed
// (no rows). The GUC is transaction-local via set_config(..., true).
func (d *DB) WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful Commit

	val := ""
	if principalID != uuid.Nil {
		val = principalID.String()
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('manyforge.principal_id', $1, true)", val); err != nil {
		return fmt.Errorf("set principal context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
