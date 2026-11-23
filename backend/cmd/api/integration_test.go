package main

// Integration test for pulse-api against a real PostgreSQL 17 (Testcontainers,
// migrated by testdb): run() connects, serves the contract and shuts down cleanly.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/testdb"
)

var (
	testURL  string
	testPool *pgxpool.Pool
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Main(m, func(url string, pool *pgxpool.Pool) { testURL, testPool = url, pool }))
}

func TestRunServesHealthAgainstPostgres(t *testing.T) {
	testdb.Require(t, testPool)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var lc net.ListenConfig
	listening := make(chan net.Addr, 1)
	listen := func(ctx context.Context, network, _ string) (net.Listener, error) {
		ln, err := lc.Listen(ctx, network, "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("binding test listener: %w", err)
		}
		listening <- ln.Addr()
		return ln, nil
	}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, env(map[string]string{"DATABASE_URL": testURL}), quietLogger(), listen)
	}()

	var base string
	select {
	case addr := <-listening:
		base = "http://" + addr.String()
	case err := <-done:
		t.Fatalf("run exited before listening: %v", err)
	}

	resp, err := get(ctx, base+"/api/v1/health")
	if err != nil {
		t.Fatalf("GET /api/v1/health: %v", err)
	}
	var body struct {
		Status string `json:"status"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || decodeErr != nil || body.Status != "ok" {
		t.Fatalf("health = %d %+v (decode err %v), want 200 ok", resp.StatusCode, body, decodeErr)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run = %v, want a clean shutdown", err)
	}
}
