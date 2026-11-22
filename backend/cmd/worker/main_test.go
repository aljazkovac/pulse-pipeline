package main

import (
	"context"
	"errors"
	"fmt"
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

// Fictional values; the key is 32 bytes of 0x01, base64-encoded.
const (
	fakeKey    = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	fakeSecret = "fake-google-client-secret"
)

func TestLoadConfig_GoogleAndTokenKey(t *testing.T) {
	base := func(extra map[string]string) map[string]string {
		m := map[string]string{"DATABASE_URL": "postgres://localhost/x"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	cfg, err := loadConfig(env(base(map[string]string{
		"GOOGLE_OAUTH_CLIENT_ID": "fake-id.apps.example.com", "GOOGLE_OAUTH_CLIENT_SECRET": fakeSecret, "TOKEN_ENCRYPTION_KEY": fakeKey,
	})))
	if err != nil || !cfg.google.enabled() || cfg.keyring == nil || cfg.keyring.CurrentVersion() != 1 {
		t.Fatalf("google config = %v, keyring %v, err %v", cfg.google, cfg.keyring != nil, err)
	}
	if s := fmt.Sprintf("%v %+v %#v %#v", cfg.google, cfg, cfg, cfg.google); strings.Contains(s, fakeSecret) || strings.Contains(s, "postgres://") {
		t.Errorf("printing the config leaks the client secret: %s", s)
	}
	keyOnly, err := loadConfig(env(base(map[string]string{"TOKEN_ENCRYPTION_KEY": fakeKey, "TOKEN_ENCRYPTION_KEY_VERSION": "3"})))
	if err != nil || keyOnly.google.enabled() || keyOnly.keyring.CurrentVersion() != 3 {
		t.Fatalf("key only = %+v, %v", keyOnly, err)
	}
	for name, e := range map[string]map[string]string{
		"id without secret":  {"GOOGLE_OAUTH_CLIENT_ID": "fake-id", "TOKEN_ENCRYPTION_KEY": fakeKey},
		"secret without id":  {"GOOGLE_OAUTH_CLIENT_SECRET": fakeSecret, "TOKEN_ENCRYPTION_KEY": fakeKey},
		"google without key": {"GOOGLE_OAUTH_CLIENT_ID": "fake-id", "GOOGLE_OAUTH_CLIENT_SECRET": fakeSecret},
		"bad key":            {"TOKEN_ENCRYPTION_KEY": "too-short"},
		"bad key version":    {"TOKEN_ENCRYPTION_KEY": fakeKey, "TOKEN_ENCRYPTION_KEY_VERSION": "-1"},
	} {
		_, err := loadConfig(env(base(e)))
		if err == nil {
			t.Errorf("%s: want error", name)
			continue
		}
		if strings.Contains(err.Error(), fakeSecret) || strings.Contains(err.Error(), fakeKey) {
			t.Errorf("%s: error leaks a secret: %v", name, err)
		}
	}
}

func TestParseConnectArgs(t *testing.T) {
	id := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	got, err := parseConnectArgs([]string{"-user", id.String()})
	if err != nil || got != id {
		t.Fatalf("parseConnectArgs = %s, %v", got, err)
	}
	if _, err := parseConnectArgs(nil); !errors.Is(err, errUsage) {
		t.Errorf("no user: want usage error, got %v", err)
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
		"connect bad user":  {[]string{"connect-google", "-user", "me"}, true, "-user must be a UUID"},
		"connect extra arg": {[]string{"connect-google", "-user", "00000000-0000-4000-8000-000000000001", "now"}, true, "unexpected argument"},
		"connect no google": {[]string{"connect-google", "-user", "00000000-0000-4000-8000-000000000001"}, false, "GOOGLE_OAUTH_CLIENT_ID"},
	} {
		err := run(context.Background(), tc.args, e, io.Discard, logger)
		if err == nil || errors.Is(err, errUsage) != tc.usage || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
