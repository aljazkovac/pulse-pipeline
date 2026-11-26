package main

// Integration tests for the pulse-worker commands against a real PostgreSQL 17
// (Testcontainers). They drive run() exactly as main does, so the wiring in
// withDeps — migrations, pool, providers, vault, Syncer — is exercised end to
// end with the mock provider. No test reaches Google.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/testdb"
)

const testTimezone = "Europe/Stockholm"

var (
	testURL  string
	testPool *pgxpool.Pool
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Main(m, func(url string, pool *pgxpool.Pool) { testURL, testPool = url, pool }))
}

// devEnv is a local-development environment with the mock provider enabled.
func devEnv(extra map[string]string) func(string) string {
	m := map[string]string{"DATABASE_URL": testURL, "ENV": "development", "PULSE_ENABLE_MOCK_PROVIDER": "true"}
	for k, v := range extra {
		m[k] = v
	}
	return env(m)
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	testdb.Require(t, testPool)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var createdUser = regexp.MustCompile(`created user ([0-9a-f-]{36}) `)

// seedDev runs `seed-dev -connection empty` for a fresh, obviously fictional
// user and returns its ID.
func seedDev(t *testing.T, ctx context.Context, extraFlags ...string) uuid.UUID {
	t.Helper()
	var out bytes.Buffer
	email := fmt.Sprintf("worker-test-%s@example.com", uuid.NewString())
	args := append([]string{"seed-dev", "-email", email, "-tz", testTimezone, "-connection", "empty"}, extraFlags...)
	if err := run(ctx, args, devEnv(nil), &out, quietLogger()); err != nil {
		t.Fatalf("seed-dev: %v", err)
	}
	m := createdUser.FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("seed-dev output has no user ID: %q", out.String())
	}
	return uuid.MustParse(m[1])
}

func count(t *testing.T, ctx context.Context, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// fingerprint hashes the user's settled data from before the cutoff.
func fingerprint(t *testing.T, ctx context.Context, userID uuid.UUID, before time.Time) string {
	t.Helper()
	const q = `
SELECT
  (SELECT count(*) || ':' || coalesce(md5(string_agg(metric_type || recorded_at::text || value::text, ',' ORDER BY metric_type, recorded_at)), '')
     FROM intraday_metrics WHERE user_id = $1 AND recorded_at < $2)
  || '|' ||
  (SELECT count(*) || ':' || coalesce(md5(string_agg(summary_date::text || coalesce(steps, -1)::text || coalesce(resting_heart_rate, -1)::text, ',' ORDER BY summary_date)), '')
     FROM daily_summaries WHERE user_id = $1 AND summary_date < ($2 AT TIME ZONE $3)::date)
  || '|' ||
  (SELECT count(*) || ':' || coalesce(md5(string_agg(provider_session_id || start_at::text || end_at::text, ',' ORDER BY start_at)), '')
     FROM sleep_sessions WHERE user_id = $1 AND end_at < $2)`
	var fp string
	if err := testPool.QueryRow(ctx, q, userID, before, testTimezone).Scan(&fp); err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return fp
}

func TestWorker_SeedDevThenManualSyncIngestsIdempotently(t *testing.T) {
	ctx := ctxT(t)
	userID := seedDev(t, ctx)
	loc, err := time.LoadLocation(testTimezone)
	if err != nil {
		t.Fatalf("load test timezone: %v", err)
	}
	// An overnight session can publish yesterday's HRV and SpO₂ samples
	// when it ends today. Compare only data from before yesterday began.
	cutoff := domain.DateOf(time.Now().In(loc)).AddDays(-1).StartIn(loc)

	args := []string{"sync", "-user", userID.String(), "-provider", "mock"}
	var out bytes.Buffer
	if err := run(ctx, args, devEnv(nil), &out, quietLogger()); err != nil {
		t.Fatalf("first sync: %v (output %q)", err, out.String())
	}
	if !strings.Contains(out.String(), ": succeeded, manual ") {
		t.Fatalf("first sync output = %q", out.String())
	}
	if n := count(t, ctx, `SELECT count(*) FROM sync_runs WHERE user_id = $1
		AND trigger = 'manual' AND status = 'succeeded' AND range_end - range_start = 29`, userID); n != 1 {
		t.Errorf("succeeded manual runs covering 30 days = %d, want 1", n)
	}
	first := fingerprint(t, ctx, userID, cutoff)
	if strings.HasPrefix(first, "0:") {
		t.Fatalf("first sync ingested no intraday data: %s", first)
	}
	for _, table := range []string{"daily_summaries", "sleep_sessions"} {
		if n := count(t, ctx, "SELECT count(*) FROM "+table+" WHERE user_id = $1", userID); n == 0 {
			t.Errorf("%s: no rows after the initial sync", table)
		}
	}

	out.Reset()
	if err := run(ctx, args, devEnv(nil), &out, quietLogger()); err != nil {
		t.Fatalf("second sync: %v (output %q)", err, out.String())
	}
	if second := fingerprint(t, ctx, userID, cutoff); second != first {
		t.Fatalf("re-sync changed settled data:\n first  %s\n second %s", first, second)
	}
	if n := count(t, ctx, "SELECT count(*) FROM sync_runs WHERE user_id = $1 AND trigger = 'manual' AND status = 'succeeded'", userID); n != 2 {
		t.Errorf("succeeded manual runs = %d, want 2", n)
	}
}

func TestWorker_SeedDevConnectionStatesAndPinnedClock(t *testing.T) {
	ctx := ctxT(t)
	const pinnedNow = "2026-11-15T14:30:00Z"
	wantNow := time.Date(2026, 11, 15, 14, 30, 0, 0, time.UTC)
	pinnedEnv := devEnv(map[string]string{"PULSE_DEV_NOW": pinnedNow, "PULSE_ENABLE_MOCK_PROVIDER": "false"})

	syncedID := uuid.MustParse("00000000-0000-4000-8000-000000000901")
	emptyID := uuid.MustParse("00000000-0000-4000-8000-000000000902")
	revokedID := uuid.MustParse("00000000-0000-4000-8000-000000000903")
	noneID := uuid.MustParse("00000000-0000-4000-8000-000000000904")

	// Default -connection is synced; PULSE_DEV_NOW + -user-id pins every row.
	var out bytes.Buffer
	if err := run(ctx, []string{"seed-dev", "-user-id", syncedID.String(), "-email", "seed-synced@example.com", "-tz", testTimezone}, pinnedEnv, &out, quietLogger()); err != nil {
		t.Fatalf("seed-dev synced: %v", err)
	}
	if !strings.Contains(out.String(), syncedID.String()) || !strings.Contains(out.String(), "synced mock connection") {
		t.Fatalf("synced output = %q", out.String())
	}
	firstFP := fingerprint(t, ctx, syncedID, wantNow.Add(24*time.Hour))
	if strings.HasPrefix(firstFP, "0:") {
		t.Fatalf("synced fixture has no intraday rows: %s", firstFP)
	}
	if future := count(t, ctx, "SELECT count(*) FROM intraday_metrics WHERE user_id = $1 AND recorded_at >= $2", syncedID, wantNow); future != 0 {
		t.Errorf("intraday rows at or after PULSE_DEV_NOW = %d, want 0", future)
	}
	if n := count(t, ctx, `SELECT count(*) FROM provider_connections WHERE user_id = $1
		AND status = 'active' AND last_synced_at = $2 AND next_sync_at > $2`, syncedID, wantNow); n != 1 {
		t.Errorf("synced connection not scheduled after PULSE_DEV_NOW")
	}

	// Re-seeding the same -user-id resets mutations and reproduces the exact fingerprint.
	if _, err := testPool.Exec(ctx, "UPDATE users SET timezone = 'UTC' WHERE id = $1", syncedID); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, []string{"seed-dev", "-user-id", syncedID.String(), "-email", "seed-synced@example.com", "-tz", testTimezone}, pinnedEnv, io.Discard, quietLogger()); err != nil {
		t.Fatalf("re-seed synced: %v", err)
	}
	if secondFP := fingerprint(t, ctx, syncedID, wantNow.Add(24*time.Hour)); secondFP != firstFP {
		t.Fatalf("re-seed changed pinned data:\n first  %s\n second %s", firstFP, secondFP)
	}
	if n := count(t, ctx, "SELECT count(*) FROM sync_runs WHERE user_id = $1", syncedID); n != 1 {
		t.Errorf("sync_runs after re-seed = %d, want 1", n)
	}

	// Seed the remaining three states: empty, revoked, none.
	for _, tc := range []struct {
		id    uuid.UUID
		email string
		state string
	}{
		{emptyID, "seed-empty@example.com", "empty"},
		{revokedID, "seed-revoked@example.com", "revoked"},
		{noneID, "seed-none@example.com", "none"},
	} {
		if err := run(ctx, []string{"seed-dev", "-user-id", tc.id.String(), "-email", tc.email, "-tz", testTimezone, "-connection", tc.state}, pinnedEnv, io.Discard, quietLogger()); err != nil {
			t.Fatalf("seed-dev %s: %v", tc.state, err)
		}
	}
	if n := count(t, ctx, `SELECT count(*) FROM provider_connections WHERE user_id = $1
		AND status = 'active' AND last_synced_at IS NULL AND next_sync_at > $2`, emptyID, wantNow); n != 1 {
		t.Errorf("empty connection state mismatch")
	}
	if n := count(t, ctx, "SELECT count(*) FROM daily_summaries WHERE user_id = $1", emptyID); n != 0 {
		t.Errorf("empty user has %d summaries, want 0", n)
	}
	if n := count(t, ctx, "SELECT count(*) FROM provider_connections WHERE user_id = $1 AND status = 'revoked'", revokedID); n != 1 {
		t.Errorf("revoked connection state mismatch")
	}
	if n := count(t, ctx, "SELECT count(*) FROM provider_connections WHERE user_id = $1", noneID); n != 0 {
		t.Errorf("none user has %d connections, want 0", n)
	}

	// None of the four fixtures is due at PULSE_DEV_NOW, so a polling worker leaves them alone.
	due, err := store.New(testPool).ListConnectionsDueForSync(ctx, wantNow, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range due {
		for _, id := range []uuid.UUID{syncedID, emptyID, revokedID, noneID} {
			if c.UserID == id {
				t.Errorf("fixture %s appeared in ListConnectionsDueForSync at PULSE_DEV_NOW", id)
			}
		}
	}
}

func TestWorker_BackfillSyncsOnlyTheRequestedRange(t *testing.T) {
	ctx := ctxT(t)
	userID := seedDev(t, ctx)
	var out bytes.Buffer
	err := run(ctx, []string{"sync", "-user", userID.String(), "-provider", "mock", "-from", "2026-01-01", "-to", "2026-01-07"}, devEnv(nil), &out, quietLogger())
	if err != nil {
		t.Fatalf("backfill: %v (output %q)", err, out.String())
	}
	if !strings.Contains(out.String(), "succeeded, backfill 2026-01-01..2026-01-07") {
		t.Errorf("backfill output = %q", out.String())
	}
	// The mock leaves ~3% of days off-wrist (no data), so a week can be short.
	if n := count(t, ctx, "SELECT count(*) FROM daily_summaries WHERE user_id = $1", userID); n < 1 || n > 7 {
		t.Errorf("daily summaries = %d, want 1..7", n)
	}
	if n := count(t, ctx, `SELECT count(*) FROM sync_runs WHERE user_id = $1 AND trigger = 'backfill'
		AND range_start = '2026-01-01' AND range_end = '2026-01-07' AND status = 'succeeded'`, userID); n != 1 {
		t.Errorf("backfill runs over the requested range = %d, want 1", n)
	}
	outside := count(t, ctx, `SELECT count(*) FROM daily_summaries WHERE user_id = $1
		AND summary_date NOT BETWEEN '2026-01-01' AND '2026-01-07'`, userID)
	if outside != 0 {
		t.Errorf("%d summaries outside the backfill range", outside)
	}
	// A backfill leaves the schedule alone: last_synced_at stays NULL.
	if n := count(t, ctx, "SELECT count(*) FROM provider_connections WHERE user_id = $1 AND last_synced_at IS NULL", userID); n != 1 {
		t.Errorf("backfill touched the connection's schedule")
	}
}

// cancelOnSync cancels the worker once it logs a finished sync for one user.
// The Syncer attaches user_id with Logger.With, so it arrives via WithAttrs.
type cancelOnSync struct {
	slog.Handler
	userID string
	attrs  []slog.Attr
	cancel context.CancelFunc
}

func (h *cancelOnSync) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "sync finished" {
		for _, a := range h.attrs {
			if a.Key == "user_id" && fmt.Sprint(a.Value.Any()) == h.userID {
				h.cancel()
			}
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h *cancelOnSync) WithAttrs(as []slog.Attr) slog.Handler {
	return &cancelOnSync{Handler: h.Handler.WithAttrs(as), userID: h.userID, attrs: append(append([]slog.Attr{}, h.attrs...), as...), cancel: h.cancel}
}

func TestWorker_RunPollsDueConnectionsUntilStopped(t *testing.T) {
	ctx := ctxT(t)
	userID := seedDev(t, ctx)
	if _, err := testPool.Exec(ctx, "UPDATE provider_connections SET next_sync_at = now() WHERE user_id = $1", userID); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	logger := slog.New(&cancelOnSync{Handler: slog.NewTextHandler(io.Discard, nil), userID: userID.String(), cancel: cancel})

	done := make(chan error, 1)
	go func() {
		done <- run(runCtx, nil, devEnv(map[string]string{"PULSE_SYNC_POLL_INTERVAL": "1h"}), io.Discard, logger)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("worker did not sync the seeded connection and stop")
	}

	if n := count(t, ctx, "SELECT count(*) FROM sync_runs WHERE user_id = $1 AND trigger = 'scheduled' AND status = 'succeeded'", userID); n != 1 {
		t.Errorf("scheduled runs = %d, want 1", n)
	}
	// The first sync schedules the next one for a future night.
	if n := count(t, ctx, "SELECT count(*) FROM provider_connections WHERE user_id = $1 AND next_sync_at > now() AND last_synced_at IS NOT NULL", userID); n != 1 {
		t.Error("connection was not rescheduled after the scheduled sync")
	}
}

func TestWorker_SyncFailsForUnknownConnection(t *testing.T) {
	ctx := ctxT(t)
	err := run(ctx, []string{"sync", "-user", uuid.NewString(), "-provider", "mock"}, devEnv(nil), io.Discard, quietLogger())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestWorker_ConnectGoogleChecksTheUserBeforeAnyOAuth(t *testing.T) {
	ctx := ctxT(t)
	// Fictional OAuth client and key: the command must fail on the unknown
	// user before it prints an authorization URL or contacts Google.
	e := devEnv(map[string]string{
		"GOOGLE_OAUTH_CLIENT_ID": "fake-id.apps.example.com", "GOOGLE_OAUTH_CLIENT_SECRET": fakeSecret, "TOKEN_ENCRYPTION_KEY": fakeKey,
	})
	var out bytes.Buffer
	err := run(ctx, []string{"connect-google", "-user", uuid.NewString()}, e, &out, quietLogger())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if out.Len() != 0 {
		t.Errorf("printed before checking the user: %q", out.String())
	}
}

func TestWorker_DatabaseErrorsDoNotLeakThePassword(t *testing.T) {
	// Nothing listens on port 1, so migrating fails without a container.
	const password = "fake-db-password"
	e := env(map[string]string{"DATABASE_URL": "postgres://pulse:" + password + "@127.0.0.1:1/pulse?sslmode=disable&connect_timeout=2"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := run(ctx, []string{"sync", "-user", uuid.NewString(), "-provider", "mock"}, e, io.Discard, quietLogger())
	if err == nil {
		t.Fatal("want a connection error")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("error leaks the database password: %v", err)
	}
}

// A Phase 1 user keeps their history: the operator binds the owner's Google
// identity to the existing user, and the sign-in lookup then resolves it.
func TestWorker_BindGoogleIdentityToAnExistingUser(t *testing.T) {
	ctx := ctxT(t)
	userID := seedDev(t, ctx)
	st := store.New(testPool)
	u, err := st.GetUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	claims := googleClaims(u.Email, "", true) // seed-dev's example.com address
	claims.Subject = "fake-sub-" + uuid.NewString()
	req := bindRequest{userID: userID}

	// example.com is not a Google-authoritative address.
	if err := bindGoogleIdentity(ctx, st, fakeLoopback{claims: claims}, req, io.Discard, time.Now); err == nil {
		t.Fatal("bound a third-party address without -mailbox-verified")
	}
	req.mailboxVerified = true
	var out bytes.Buffer
	if err := bindGoogleIdentity(ctx, st, fakeLoopback{claims: claims}, req, &out, time.Now); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetUserIDByIdentity(ctx, claims.Issuer, claims.Subject)
	if err != nil || got != userID {
		t.Fatalf("identity resolves to %s, %v; want %s", got, err, userID)
	}

	// Replacing it requires -replace and signs the user out everywhere.
	if err := st.CreateSession(ctx, userID, bytes.Repeat([]byte{9}, 32), time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	other := claims
	other.Subject = "fake-sub-" + uuid.NewString()
	if err := bindGoogleIdentity(ctx, st, fakeLoopback{claims: other}, req, io.Discard, time.Now); !errors.Is(err, store.ErrIdentityBound) {
		t.Fatalf("replacing without -replace = %v, want ErrIdentityBound", err)
	}
	req.replace = true
	out.Reset()
	if err := bindGoogleIdentity(ctx, st, fakeLoopback{claims: other}, req, &out, time.Now); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "signed out 1 sessions") {
		t.Errorf("output = %q", out.String())
	}
	if n := count(t, ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, userID); n != 0 {
		t.Errorf("%d sessions survived the replacement", n)
	}
}

func TestWorker_RevokeSessionsSignsOneUserOutEverywhere(t *testing.T) {
	ctx := ctxT(t)
	target, other := seedDev(t, ctx), seedDev(t, ctx)
	st := store.New(testPool)
	for _, id := range []uuid.UUID{target, target, other} {
		hash := sha256.Sum256([]byte(uuid.NewString()))
		if err := st.CreateSession(ctx, id, hash[:], time.Now(), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	if err := run(ctx, []string{"revoke-sessions", "-user", target.String()}, devEnv(nil), &out, quietLogger()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "out of 2 sessions") {
		t.Errorf("output = %q", out.String())
	}
	if n := count(t, ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, target); n != 0 {
		t.Errorf("%d sessions survived", n)
	}
	if n := count(t, ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, other); n != 1 {
		t.Errorf("another user's sessions = %d, want 1 untouched", n)
	}
	if err := run(ctx, []string{"revoke-sessions", "-user", uuid.NewString()}, devEnv(nil), io.Discard, quietLogger()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown user = %v, want ErrNotFound", err)
	}
}
