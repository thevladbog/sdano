// Package testdb boots a disposable PostgreSQL container with the full
// schema applied — the fixture for every integration test in the repo.
package testdb

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // pgx5:// driver
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// New starts postgres:18.4, applies db/migrations, and returns a pool.
// The container and pool are cleaned up with the test.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:18.4",
		tcpostgres.WithDatabase("sdano_test"),
		tcpostgres.WithUsername("sdano"),
		tcpostgres.WithPassword("sdano"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("starting postgres container: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = container.Terminate(ctx)
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("container dsn: %v", err)
	}

	m, err := migrate.New("file://"+migrationsDir(t), strings.Replace(dsn, "postgres://", "pgx5://", 1))
	if err != nil {
		t.Fatalf("opening migrations: %v", err)
	}
	if err := m.Up(); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	srcErr, dbErr := m.Close()
	if srcErr != nil || dbErr != nil {
		t.Fatalf("closing migrator: src=%v db=%v", srcErr, dbErr)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// migrationsDir resolves <repo-root>/db/migrations from this file's location,
// so tests work regardless of the working directory.
func migrationsDir(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolving caller path")
	}
	// self = <root>/apps/api/internal/testdb/testdb.go
	return filepath.Join(filepath.Dir(self), "..", "..", "..", "..", "db", "migrations")
}
