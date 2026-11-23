// Command pulse-api serves the Pulse Pipeline HTTP API (ADR-0005): the
// /api/v1 contract in backend/api/openapi.yaml and the /healthz liveness probe.
//
// Environment: DATABASE_URL (required), API_PORT (default 8080). Pending
// migrations are applied on startup. SIGINT/SIGTERM drain in-flight requests
// for up to shutdownTimeout before exiting.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/api"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
)

// shutdownTimeout bounds how long a stopping API waits for in-flight requests.
const shutdownTimeout = 15 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := run(ctx, os.Getenv, logger, nil); err != nil {
		logger.Error("pulse-api failed", "error", err)
		os.Exit(1)
	}
}

type config struct {
	databaseURL string
	port        int
}

// String omits the database URL, which may hold a password.
func (c config) String() string { return fmt.Sprintf("config{port: %d}", c.port) }

// GoString redacts for %#v as well.
func (c config) GoString() string { return c.String() }

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{databaseURL: getenv("DATABASE_URL"), port: 8080}
	if cfg.databaseURL == "" {
		return config{}, errors.New("DATABASE_URL is required")
	}
	if v := getenv("API_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return config{}, fmt.Errorf("API_PORT %q: want a port number between 1 and 65535", v)
		}
		cfg.port = p
	}
	return cfg, nil
}

// run migrates, connects and serves until ctx is cancelled. listening, when
// non-nil, receives the bound address once the API accepts connections.
func run(ctx context.Context, getenv func(string) string, logger *slog.Logger, listening func(net.Addr)) error {
	cfg, err := loadConfig(getenv)
	if err != nil {
		return err
	}
	if err := store.MigrateUp(cfg.databaseURL); err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return errors.New("connecting to database: invalid DATABASE_URL") // pgx errors can echo the URL
	}
	defer pool.Close()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", ":"+strconv.Itoa(cfg.port))
	if err != nil {
		return fmt.Errorf("listening on port %d: %w", cfg.port, err)
	}
	if listening != nil {
		listening(ln.Addr())
	}
	return serve(ctx, ln, api.NewHandler(api.Options{DB: pool, Logger: logger}), logger, shutdownTimeout)
}

// serve runs an HTTP server on ln until ctx is cancelled, then stops
// accepting connections and waits up to drain for in-flight requests.
func serve(ctx context.Context, ln net.Listener, h http.Handler, logger *slog.Logger, drain time.Duration) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	logger.Info("pulse-api listening", "addr", ln.Addr().String())

	select {
	case err := <-served:
		return fmt.Errorf("serving HTTP: %w", err)
	case <-ctx.Done():
	}

	logger.Info("pulse-api shutting down", "drain", drain)
	// Request contexts are not derived from ctx, so in-flight requests finish
	// normally while Shutdown waits for them.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drain)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close() // drop connections that outlived the drain window
		return fmt.Errorf("shutting down HTTP server: %w", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving HTTP: %w", err)
	}
	logger.Info("pulse-api stopped")
	return nil
}
