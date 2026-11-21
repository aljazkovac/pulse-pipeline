package store_test

import (
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/testdb"
)

// Shared across the package: one PostgreSQL 17 container, migrated once.
// Tests isolate themselves by creating fresh users rather than truncating.
var (
	testPool  *pgxpool.Pool
	testStore *store.Store
	adminURL  string
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Main(m, func(url string, pool *pgxpool.Pool) {
		adminURL, testPool, testStore = url, pool, store.New(pool)
	}))
}
