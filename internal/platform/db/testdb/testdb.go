// Package testdb spins an ephemeral PostgreSQL via testcontainers, runs the
// migrations, and exposes two handles: a superuser pool (RLS-exempt, for
// seeding) and an *db.DB connected as the real non-superuser manyforge_app role
// (RLS-subject) so tests exercise isolation exactly as production does.
package testdb

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	appdb "github.com/manyforge/manyforge/internal/platform/db"
)

const appPassword = "apppw"

// TestDB bundles the seeded ephemeral database and its two connection handles.
type TestDB struct {
	Super     *pgxpool.Pool // superuser; RLS-exempt; for seeding
	App       *appdb.DB     // manyforge_app; RLS-subject; what production uses
	terminate func(context.Context) error
}

// Start launches Postgres, migrates it, and enables login for manyforge_app.
func Start(ctx context.Context) (*TestDB, error) {
	configureDockerEnv()

	ctr, err := postgres.Run(ctx, "postgres:16",
		postgres.WithDatabase("manyforge"),
		postgres.WithUsername("manyforge"),
		postgres.WithPassword("devpassword"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		return nil, fmt.Errorf("start container: %w", err)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		return nil, err
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return nil, err
	}
	superDSN := fmt.Sprintf("postgres://manyforge:devpassword@%s:%s/manyforge?sslmode=disable", host, port.Port())

	// Colima forwards the mapped port to the host with a short lag after the
	// container reports ready; retry until the forward is live before migrating.
	super, err := connectWithRetry(ctx, superDSN, 30, 500*time.Millisecond)
	if err != nil {
		_ = ctr.Terminate(ctx)
		return nil, err
	}

	if err := runMigrations(host, port.Port()); err != nil {
		super.Close()
		_ = ctr.Terminate(ctx)
		return nil, err
	}

	// Enable login for the app role (migrations create it NOLOGIN).
	if _, err := super.Exec(ctx, fmt.Sprintf("ALTER ROLE manyforge_app LOGIN PASSWORD '%s'", appPassword)); err != nil {
		super.Close()
		_ = ctr.Terminate(ctx)
		return nil, fmt.Errorf("enable app login: %w", err)
	}

	appDSN := fmt.Sprintf("postgres://manyforge_app:%s@%s:%s/manyforge?sslmode=disable", appPassword, host, port.Port())
	app, err := appdb.Open(ctx, appDSN)
	if err != nil {
		super.Close()
		_ = ctr.Terminate(ctx)
		return nil, err
	}

	return &TestDB{
		Super:     super,
		App:       app,
		terminate: func(c context.Context) error { return ctr.Terminate(c) },
	}, nil
}

// Close releases pools and terminates the container.
func (t *TestDB) Close(ctx context.Context) {
	if t.App != nil {
		t.App.Close()
	}
	if t.Super != nil {
		t.Super.Close()
	}
	if t.terminate != nil {
		_ = t.terminate(ctx)
	}
}

// configureDockerEnv makes testcontainers work out-of-the-box on colima/rootless
// setups: derive DOCKER_HOST from the active docker context, and disable Ryuk
// (we terminate the container ourselves via Close).
func configureDockerEnv() {
	if os.Getenv("DOCKER_HOST") == "" {
		out, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
		if err == nil {
			if h := strings.TrimSpace(string(out)); h != "" {
				_ = os.Setenv("DOCKER_HOST", h)
			}
		}
	}
	if os.Getenv("TESTCONTAINERS_RYUK_DISABLED") == "" {
		_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	}
}

func connectWithRetry(ctx context.Context, dsn string, attempts int, delay time.Duration) (*pgxpool.Pool, error) {
	var lastErr error
	for range attempts {
		pool, err := pgxpool.New(ctx, dsn)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, fmt.Errorf("connect after %d attempts: %w", attempts, lastErr)
}

func runMigrations(host, port string) error {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return fmt.Errorf("cannot locate migrations dir")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
	migDir := filepath.Join(root, "migrations")
	dbURL := fmt.Sprintf("pgx5://manyforge:devpassword@%s:%s/manyforge?sslmode=disable", host, port)
	m, err := migrate.New("file://"+migDir, dbURL)
	if err != nil {
		return fmt.Errorf("migrate init: %w", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}
