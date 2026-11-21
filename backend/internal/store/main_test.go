package store_test

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

// Shared across the package: one PostgreSQL 17 container, migrated once.
// Tests isolate themselves by creating fresh users rather than truncating.
var (
	testPool  *pgxpool.Pool
	testStore *store.Store
	adminURL  string
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("pulse_test"),
		postgres.WithUsername("pulse"),
		postgres.WithPassword("pulse"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		// Locally, skip when Docker is unavailable. In CI a missing database
		// must fail loudly instead of silently skipping the integration suite.
		if os.Getenv("CI") != "" {
			log.Printf("starting postgres testcontainer: %v", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "SKIP store integration tests: Docker unavailable (%v)\n", err)
		return 0
	}
	defer func() {
		if err := container.Terminate(context.Background()); err != nil {
			log.Printf("terminating postgres testcontainer: %v", err)
		}
	}()

	adminURL, err = container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("getting connection string: %v", err)
		return 1
	}
	if err := store.MigrateUp(adminURL); err != nil {
		log.Printf("migrating test database: %v", err)
		return 1
	}

	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	testPool, err = pgxpool.New(connectCtx, adminURL)
	if err != nil {
		log.Printf("connecting to test database: %v", err)
		return 1
	}
	defer testPool.Close()
	testStore = store.New(testPool)

	return m.Run()
}
