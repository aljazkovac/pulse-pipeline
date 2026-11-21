// Command pulse-worker ingests health data from providers (ADR-0003).
//
//	pulse-worker [run]                       poll for due connections and sync them (default)
//	pulse-worker sync -user ID -provider P   sync one connection now (manual)
//	    [-from YYYY-MM-DD -to YYYY-MM-DD]    ...or backfill a date range
//	pulse-worker seed-dev [-email E] [-tz Z] create a local user with a mock connection
//
// Environment: DATABASE_URL (required), PULSE_SYNC_POLL_INTERVAL (default 1m),
// PULSE_ENABLE_MOCK_PROVIDER (default false), ENV (seed-dev needs "development").
// Every command applies pending migrations first.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/provider/mock"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/sync"
)

// errUsage marks command-line mistakes; main prints usage and exits 2.
var errUsage = errors.New("usage")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, logger)
	switch {
	case errors.Is(err, errUsage):
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	case err != nil:
		logger.Error("pulse-worker failed", "error", err)
		os.Exit(1)
	}
}

const usage = `usage:
  pulse-worker [run]
  pulse-worker sync -user ID -provider PROVIDER [-from YYYY-MM-DD -to YYYY-MM-DD]
  pulse-worker seed-dev [-email EMAIL] [-tz IANA_ZONE]`

func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer, logger *slog.Logger) error {
	cmd := "run"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	cfg, err := loadConfig(getenv)
	if err != nil {
		return err
	}

	switch cmd {
	case "run":
		if len(args) > 0 {
			return fmt.Errorf("%w: run takes no arguments", errUsage)
		}
		return withSyncer(ctx, cfg, logger, func(_ *store.Store, s *sync.Syncer) error {
			return poll(ctx, s, cfg.pollInterval, logger)
		})
	case "sync":
		req, err := parseSyncArgs(args)
		if err != nil {
			return err
		}
		return withSyncer(ctx, cfg, logger, func(_ *store.Store, s *sync.Syncer) error {
			r, err := s.SyncConnection(ctx, req)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "run %s: %s, %s %s..%s, %d records%s\n",
				r.ID, r.Status, r.Trigger, r.Range.Start, r.Range.End, r.RecordsIngested, suffix(r.ErrorMessage))
			if err == nil && r.Status != domain.SyncSucceeded {
				err = fmt.Errorf("sync run %s finished %s", r.ID, r.Status)
			}
			return err
		})
	case "seed-dev":
		email, tz, err := parseSeedArgs(args)
		if err != nil {
			return err
		}
		if cfg.env != "development" {
			return errors.New("seed-dev only runs with ENV=development")
		}
		return withSyncer(ctx, cfg, logger, func(st *store.Store, _ *sync.Syncer) error {
			u, err := st.CreateUser(ctx, email, tz)
			if err != nil {
				return err
			}
			if _, err := st.CreateConnection(ctx, u.ID, domain.ProviderMock, nil); err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "created user %s (%s, %s) with a mock connection; "+
				"run the worker with PULSE_ENABLE_MOCK_PROVIDER=true to sync it\n", u.ID, email, tz)
			return err
		})
	default:
		return fmt.Errorf("%w: unknown command %q", errUsage, cmd)
	}
}

type config struct {
	databaseURL  string
	pollInterval time.Duration
	enableMock   bool
	env          string
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{databaseURL: getenv("DATABASE_URL"), pollInterval: time.Minute, env: getenv("ENV")}
	if cfg.databaseURL == "" {
		return config{}, errors.New("DATABASE_URL is required")
	}
	if v := getenv("PULSE_SYNC_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return config{}, fmt.Errorf("PULSE_SYNC_POLL_INTERVAL %q: want a positive duration such as 1m", v)
		}
		cfg.pollInterval = d
	}
	if v := getenv("PULSE_ENABLE_MOCK_PROVIDER"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return config{}, fmt.Errorf("PULSE_ENABLE_MOCK_PROVIDER %q: want true or false", v)
		}
		cfg.enableMock = b
	}
	return cfg, nil
}

func parseSyncArgs(args []string) (sync.Request, error) {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	user := fs.String("user", "", "user ID")
	provider := fs.String("provider", "", "provider, e.g. mock or google_health")
	from := fs.String("from", "", "backfill start date (YYYY-MM-DD)")
	to := fs.String("to", "", "backfill end date (YYYY-MM-DD)")
	if err := fs.Parse(args); err != nil {
		return sync.Request{}, fmt.Errorf("%w: sync: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		return sync.Request{}, fmt.Errorf("%w: sync: unexpected argument %q", errUsage, fs.Arg(0))
	}
	id, err := uuid.Parse(*user)
	if err != nil {
		return sync.Request{}, fmt.Errorf("%w: sync: -user must be a UUID", errUsage)
	}
	if *provider == "" {
		return sync.Request{}, fmt.Errorf("%w: sync: -provider is required", errUsage)
	}
	req := sync.Request{UserID: id, Provider: domain.Provider(*provider), Trigger: domain.SyncTriggerManual}
	if *from == "" && *to == "" {
		return req, nil
	}
	if *from == "" || *to == "" {
		return sync.Request{}, fmt.Errorf("%w: sync: -from and -to go together", errUsage)
	}
	start, err := domain.ParseDate(*from)
	if err != nil {
		return sync.Request{}, fmt.Errorf("%w: sync: %v", errUsage, err)
	}
	end, err := domain.ParseDate(*to)
	if err != nil {
		return sync.Request{}, fmt.Errorf("%w: sync: %v", errUsage, err)
	}
	req.Trigger, req.Range = domain.SyncTriggerBackfill, &domain.DateRange{Start: start, End: end}
	return req, nil
}

func parseSeedArgs(args []string) (email, tz string, err error) {
	fs := flag.NewFlagSet("seed-dev", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&email, "email", "dev@example.com", "email of the local user")
	fs.StringVar(&tz, "tz", "Europe/Stockholm", "IANA timezone of the local user")
	if err := fs.Parse(args); err != nil {
		return "", "", fmt.Errorf("%w: seed-dev: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		return "", "", fmt.Errorf("%w: seed-dev: unexpected argument %q", errUsage, fs.Arg(0))
	}
	return email, tz, nil
}

// withSyncer migrates the database, connects, and builds the Syncer.
func withSyncer(ctx context.Context, cfg config, logger *slog.Logger, fn func(*store.Store, *sync.Syncer) error) error {
	if err := store.MigrateUp(cfg.databaseURL); err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return errors.New("connecting to database: invalid DATABASE_URL") // pgx errors can echo the URL
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}

	var providers []domain.HealthProvider
	if cfg.enableMock {
		providers = append(providers, mock.New(mock.Options{Seed: 1}))
	}
	syncCfg := sync.DefaultConfig()
	syncCfg.Logger = logger
	st := store.New(pool)
	s, err := sync.New(st, providers, syncCfg)
	if err != nil {
		return err
	}
	return fn(st, s)
}

// poll runs due syncs immediately and then every interval until ctx ends.
func poll(ctx context.Context, s *sync.Syncer, interval time.Duration, logger *slog.Logger) error {
	logger.Info("pulse-worker started", "poll_interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := s.RunDue(ctx); err != nil && ctx.Err() == nil {
			logger.Error("scheduled sync pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			logger.Info("pulse-worker stopping")
			return nil
		case <-ticker.C:
		}
	}
}

func suffix(msg string) string {
	if msg == "" {
		return ""
	}
	return " (" + msg + ")"
}
