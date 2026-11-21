// Package testdb starts a throwaway, migrated PostgreSQL 17 container for a
// package's integration tests. Use it from TestMain only.
package testdb

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
)

// Main starts PostgreSQL, applies the embedded migrations, calls ready with
// the database URL and a pool, then runs the tests and tears everything down.
// It returns the exit code for os.Exit.
//
// Locally database-dependent tests use Require to skip when Docker is
// unavailable. In CI a failure to start PostgreSQL fails the suite.
func Main(m *testing.M, ready func(databaseURL string, pool *pgxpool.Pool)) int {
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("pulse_test"),
		postgres.WithUsername("pulse"),
		postgres.WithPassword("pulse"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		if os.Getenv("CI") != "" {
			log.Printf("starting postgres testcontainer: %v", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "SKIP integration tests: Docker unavailable (%v)\n", err)
		return m.Run()
	}
	defer func() {
		if err := container.Terminate(context.Background()); err != nil {
			log.Printf("terminating postgres testcontainer: %v", err)
		}
	}()

	url, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("getting connection string: %v", err)
		return 1
	}
	if err := store.MigrateUp(url); err != nil {
		log.Printf("migrating test database: %v", err)
		return 1
	}

	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(connectCtx, url)
	if err != nil {
		log.Printf("connecting to test database: %v", err)
		return 1
	}
	defer pool.Close()

	ready(url, pool)
	return m.Run()
}

// Require skips a test when Main could not start the shared database.
func Require(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if pool == nil {
		t.Skip("PostgreSQL test container unavailable")
	}
}
