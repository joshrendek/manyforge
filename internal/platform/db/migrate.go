package db

import (
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

// Migrate applies all up migrations from migrationsDir against dsn. It is
// invoked by `manyforge migrate` and by tests. dsn may use a postgres:// or
// postgresql:// scheme; it is rewritten to golang-migrate's pgx5:// scheme.
func Migrate(dsn, migrationsDir string) error {
	url := dsn
	for _, scheme := range []string{"postgresql://", "postgres://"} {
		if rest, ok := strings.CutPrefix(url, scheme); ok {
			url = "pgx5://" + rest
			break
		}
	}
	m, err := migrate.New("file://"+migrationsDir, url)
	if err != nil {
		return fmt.Errorf("migrate init: %w", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}
