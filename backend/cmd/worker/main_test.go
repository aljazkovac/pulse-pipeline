package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadConfig(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"DATABASE_URL": "postgres://localhost/x", "PULSE_SYNC_POLL_INTERVAL": "30s", "PULSE_ENABLE_MOCK_PROVIDER": "true", "ENV": "development",
	}))
	if err != nil || cfg.pollInterval != 30*time.Second || !cfg.enableMock || cfg.env != "development" {
		t.Fatalf("loadConfig = %+v, %v", cfg, err)
	}
	defaults, err := loadConfig(env(map[string]string{"DATABASE_URL": "postgres://localhost/x"}))
	if err != nil || defaults.pollInterval != time.Minute || defaults.enableMock {
		t.Fatalf("defaults = %+v, %v", defaults, err)
	}
	for name, e := range map[string]map[string]string{
		"missing url":   {},
		"bad interval":  {"DATABASE_URL": "x", "PULSE_SYNC_POLL_INTERVAL": "soon"},
		"zero interval": {"DATABASE_URL": "x", "PULSE_SYNC_POLL_INTERVAL": "0s"},
		"bad bool":      {"DATABASE_URL": "x", "PULSE_ENABLE_MOCK_PROVIDER": "maybe"},
	} {
		if _, err := loadConfig(env(e)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestParseSyncArgs(t *testing.T) {
	id := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	manual, err := parseSyncArgs([]string{"-user", id.String(), "-provider", "mock"})
	if err != nil || manual.UserID != id || manual.Provider != domain.ProviderMock || manual.Trigger != domain.SyncTriggerManual || manual.Range != nil {
		t.Fatalf("manual = %+v, %v", manual, err)
	}
	backfill, err := parseSyncArgs([]string{"-user", id.String(), "-provider", "mock", "-from", "2026-01-01", "-to", "2026-01-31"})
	if err != nil || backfill.Trigger != domain.SyncTriggerBackfill || backfill.Range == nil ||
		backfill.Range.Start.String() != "2026-01-01" || backfill.Range.End.String() != "2026-01-31" {
		t.Fatalf("backfill = %+v, %v", backfill, err)
	}
	for name, args := range map[string][]string{
		"no user":       {"-provider", "mock"},
		"bad user":      {"-user", "me", "-provider", "mock"},
		"no provider":   {"-user", id.String()},
		"only from":     {"-user", id.String(), "-provider", "mock", "-from", "2026-01-01"},
		"bad date":      {"-user", id.String(), "-provider", "mock", "-from", "2026-02-30", "-to", "2026-03-01"},
		"unknown flag":  {"-user", id.String(), "-provider", "mock", "-force"},
		"extra operand": {"-user", id.String(), "-provider", "mock", "now"},
	} {
		if _, err := parseSyncArgs(args); !errors.Is(err, errUsage) {
			t.Errorf("%s: want usage error, got %v", name, err)
		}
	}
}

func TestParseSeedArgs(t *testing.T) {
	email, tz, err := parseSeedArgs(nil)
	if err != nil || email != "dev@example.com" || tz != "Europe/Stockholm" {
		t.Fatalf("defaults = %q %q %v", email, tz, err)
	}
	if _, _, err := parseSeedArgs([]string{"-tz", "UTC", "extra"}); !errors.Is(err, errUsage) {
		t.Errorf("extra operand: want usage error, got %v", err)
	}
}

func TestRunRejectsBadInvocationsBeforeTouchingTheDatabase(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// An unroutable URL: reaching the database would hang or fail differently.
	e := env(map[string]string{"DATABASE_URL": "postgres://invalid.invalid/x", "ENV": "production"})
	for name, tc := range map[string]struct {
		args  []string
		usage bool
		want  string
	}{
		"unknown command":   {[]string{"frobnicate"}, true, "unknown command"},
		"run with operands": {[]string{"run", "now"}, true, "no arguments"},
		"sync without user": {[]string{"sync", "-provider", "mock"}, true, "-user"},
		"seed outside dev":  {[]string{"seed-dev"}, false, "ENV=development"},
	} {
		err := run(context.Background(), tc.args, e, io.Discard, logger)
		if err == nil || errors.Is(err, errUsage) != tc.usage || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
