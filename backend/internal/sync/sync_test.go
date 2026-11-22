package sync_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/provider/mock"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/sync"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/testdb"
)

var (
	testPool  *pgxpool.Pool
	testStore *store.Store
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Main(m, func(_ string, pool *pgxpool.Pool) {
		testPool, testStore = pool, store.New(pool)
	}))
}

// 10:30 in Stockholm: today is 2026-11-21 locally, the nightly hour has passed.
var t0 = time.Date(2026, 11, 21, 9, 30, 0, 0, time.UTC)

// harness is one user with one connection, a Syncer and a controllable clock
// shared with the mock provider.
type harness struct {
	t      *testing.T
	ctx    context.Context
	now    time.Time
	user   domain.User
	conn   domain.ConnectionInfo
	mock   *mock.Provider
	syncer *sync.Syncer
	sleeps []time.Duration
	store  sync.Store // defaults to testStore; options may wrap it
}

type option func(*harness, *sync.Config, *[]domain.HealthProvider)

func newHarness(t *testing.T, opts ...option) *harness {
	t.Helper()
	testdb.Require(t, testPool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	h := &harness{t: t, ctx: ctx, now: t0}
	h.mock = mock.New(mock.Options{Seed: 7, Now: h.clock})
	h.user = h.newUser("Europe/Stockholm")
	h.conn = h.connect(h.user, domain.ProviderMock)
	h.conn.NextSyncAt = h.now.Add(-time.Minute)
	if err := testStore.RescheduleConnection(h.ctx, h.user.ID, h.conn.ID, h.conn.NextSyncAt); err != nil {
		t.Fatal(err)
	}

	cfg := sync.DefaultConfig()
	cfg.InitialBackfillDays = 7 // keeps each first sync to ~30k rows; the default is unit-tested
	cfg.Now = h.clock
	cfg.Sleep = func(ctx context.Context, d time.Duration) error {
		h.sleeps = append(h.sleeps, d)
		return ctx.Err()
	}
	providers := []domain.HealthProvider{h.mock}
	h.store = testStore
	for _, o := range opts {
		o(h, &cfg, &providers)
	}
	s, err := sync.New(h.store, providers, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.syncer = s
	return h
}

func (h *harness) clock() time.Time { return h.now }

// withProvider replaces the mock with a wrapper around it.
func withProvider(wrap func(domain.HealthProvider) domain.HealthProvider) option {
	return func(h *harness, _ *sync.Config, ps *[]domain.HealthProvider) {
		*ps = []domain.HealthProvider{wrap(h.mock)}
	}
}

func (h *harness) newUser(tz string) domain.User {
	h.t.Helper()
	u, err := testStore.CreateUser(h.ctx, fmt.Sprintf("sync-%s@example.com", uuid.NewString()), tz)
	if err != nil {
		h.t.Fatal(err)
	}
	return u
}

func (h *harness) connect(u domain.User, p domain.Provider) domain.ConnectionInfo {
	h.t.Helper()
	c, err := testStore.CreateConnection(h.ctx, u.ID, p, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) sync(trigger domain.SyncTrigger, r *domain.DateRange) domain.SyncRun {
	h.t.Helper()
	run, err := h.syncer.SyncConnection(h.ctx, sync.Request{UserID: h.user.ID, Provider: domain.ProviderMock, Trigger: trigger, Range: r})
	if err != nil {
		h.t.Fatalf("SyncConnection(%s): %v", trigger, err)
	}
	return run
}

func (h *harness) connection() domain.ConnectionInfo {
	h.t.Helper()
	c, err := testStore.GetConnection(h.ctx, h.user.ID, domain.ProviderMock)
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) cursor() string {
	h.t.Helper()
	c, err := testStore.GetConnectionSyncCursor(h.ctx, h.user.ID, h.conn.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(c)
}

func (h *harness) count(table string, user uuid.UUID) int {
	h.t.Helper()
	var n int
	if err := testPool.QueryRow(h.ctx, "SELECT count(*) FROM "+table+" WHERE user_id = $1", user).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// fingerprint hashes every health row the user owns, so two syncs can be
// compared for identical values, not just identical counts.
func (h *harness) fingerprint(user uuid.UUID) string {
	h.t.Helper()
	var fp string
	err := testPool.QueryRow(h.ctx, `
		SELECT concat_ws('/',
		  (SELECT md5(coalesce(string_agg(t::text, '|' ORDER BY t::text), '')) FROM
		     (SELECT summary_date, steps, resting_heart_rate, hrv_rmssd_ms, sleep_score, calories_kcal, spo2_avg_pct
		      FROM daily_summaries WHERE user_id = $1) t),
		  (SELECT md5(coalesce(string_agg(t::text, '|' ORDER BY t::text), '')) FROM
		     (SELECT id, provider_session_id, sleep_date, start_at, end_at, is_main_sleep, efficiency_pct
		      FROM sleep_sessions WHERE user_id = $1) t),
		  (SELECT md5(coalesce(string_agg(t::text, '|' ORDER BY t::text), '')) FROM
		     (SELECT session_id, stage, start_at, end_at FROM sleep_stages WHERE user_id = $1) t),
		  (SELECT md5(coalesce(string_agg(t::text, '|' ORDER BY t::text), '')) FROM
		     (SELECT metric_type, recorded_at, value FROM intraday_metrics WHERE user_id = $1) t))`, user).Scan(&fp)
	if err != nil {
		h.t.Fatal(err)
	}
	return fp
}

// isolateDue parks every other connection in the shared test database so
// RunDue only sees the given ones, which become due now. Test-only SQL; safe
// because no test in this package calls t.Parallel().
func (h *harness) isolateDue(conns ...domain.ConnectionInfo) {
	h.t.Helper()
	if _, err := testPool.Exec(h.ctx, `UPDATE provider_connections SET next_sync_at = 'infinity'`); err != nil {
		h.t.Fatal(err)
	}
	for _, c := range conns {
		if err := testStore.RescheduleConnection(h.ctx, c.UserID, c.ID, h.now.Add(-time.Minute)); err != nil {
			h.t.Fatal(err)
		}
	}
}

func date(t *testing.T, s string) domain.Date {
	t.Helper()
	d, err := domain.ParseDate(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func dateRange(t *testing.T, start, end string) domain.DateRange {
	return domain.DateRange{Start: date(t, start), End: date(t, end)}
}

// faulty wraps a provider and injects errors chosen by fail, which sees the
// operation ("daily", "sleep", "refresh" or a metric type) and its call count.
type faulty struct {
	domain.HealthProvider
	fail         func(op string, call int) error
	calls        map[string]int
	rewrite      func([]domain.DailySummary)
	rewriteSleep func([]domain.SleepSession)
}

func newFaulty(fail func(op string, call int) error) func(domain.HealthProvider) domain.HealthProvider {
	return func(p domain.HealthProvider) domain.HealthProvider {
		return &faulty{HealthProvider: p, fail: fail, calls: map[string]int{}}
	}
}

func (f *faulty) inject(op string) error {
	f.calls[op]++
	if f.fail == nil {
		return nil
	}
	return f.fail(op, f.calls[op])
}

func (f *faulty) FetchDailySummaries(ctx context.Context, c domain.Connection, r domain.DateRange) ([]domain.DailySummary, error) {
	if err := f.inject("daily"); err != nil {
		return nil, err
	}
	rows, err := f.HealthProvider.FetchDailySummaries(ctx, c, r)
	if f.rewrite != nil {
		f.rewrite(rows)
	}
	return rows, err
}

func (f *faulty) FetchSleepSessions(ctx context.Context, c domain.Connection, r domain.DateRange) ([]domain.SleepSession, error) {
	if err := f.inject("sleep"); err != nil {
		return nil, err
	}
	rows, err := f.HealthProvider.FetchSleepSessions(ctx, c, r)
	if f.rewriteSleep != nil {
		f.rewriteSleep(rows)
	}
	return rows, err
}

func (f *faulty) FetchIntradayMetrics(ctx context.Context, c domain.Connection, m domain.MetricType, r domain.TimeRange) ([]domain.IntradayPoint, error) {
	if err := f.inject(string(m)); err != nil {
		return nil, err
	}
	return f.HealthProvider.FetchIntradayMetrics(ctx, c, m, r)
}

func (f *faulty) RefreshAuth(ctx context.Context, c domain.Connection) (domain.Credentials, error) {
	if err := f.inject("refresh"); err != nil {
		return domain.Credentials{}, err
	}
	return f.HealthProvider.RefreshAuth(ctx, c)
}

// ---------------------------------------------------------------------------

func TestFirstSync_BackfillsInitialWindowAndSchedulesNightly(t *testing.T) {
	h := newHarness(t)
	run := h.sync(domain.SyncTriggerScheduled, nil)

	if run.Status != domain.SyncSucceeded || run.ErrorMessage != "" || run.FinishedAt == nil {
		t.Fatalf("run = %+v", run)
	}
	if want := dateRange(t, "2026-11-15", "2026-11-21"); run.Range != want {
		t.Errorf("range = %+v, want the initial window ending today %+v", run.Range, want)
	}
	rows := h.count("daily_summaries", h.user.ID) + h.count("sleep_sessions", h.user.ID) + h.count("intraday_metrics", h.user.ID)
	if rows == 0 || int(run.RecordsIngested) != rows {
		t.Errorf("records ingested = %d, rows written = %d", run.RecordsIngested, rows)
	}
	if h.count("sleep_stages", h.user.ID) == 0 {
		t.Error("no sleep stages ingested")
	}

	c := h.connection()
	if c.LastSyncedAt == nil || !c.LastSyncedAt.Equal(t0) {
		t.Errorf("last synced = %v, want %s", c.LastSyncedAt, t0)
	}
	if want := time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC); !c.NextSyncAt.Equal(want) { // 04:00 CET
		t.Errorf("next sync = %s, want %s", c.NextSyncAt, want)
	}
	if !strings.Contains(h.cursor(), `"synced_through": "2026-11-21"`) {
		t.Errorf("cursor = %s", h.cursor())
	}
}

// QA spec: syncing the same mock dataset twice yields identical rows.
func TestResync_IsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.sync(domain.SyncTriggerScheduled, nil)
	before := h.fingerprint(h.user.ID)
	counts := [4]int{h.count("daily_summaries", h.user.ID), h.count("sleep_sessions", h.user.ID),
		h.count("sleep_stages", h.user.ID), h.count("intraday_metrics", h.user.ID)}

	full := dateRange(t, "2026-11-15", "2026-11-21")
	again := h.sync(domain.SyncTriggerBackfill, &full)
	if again.Status != domain.SyncSucceeded {
		t.Fatalf("re-sync = %+v", again)
	}
	if after := h.fingerprint(h.user.ID); after != before {
		t.Error("re-syncing the same range changed stored values")
	}
	if after := [4]int{h.count("daily_summaries", h.user.ID), h.count("sleep_sessions", h.user.ID),
		h.count("sleep_stages", h.user.ID), h.count("intraday_metrics", h.user.ID)}; after != counts {
		t.Errorf("row counts changed: %v → %v", counts, after)
	}
	// Daily and sleep upserts count every row they write; unchanged intraday rows count zero.
	if want := counts[0] + counts[1]; int(again.RecordsIngested) != want {
		t.Errorf("re-sync reported %d records, want %d (no intraday changes)", again.RecordsIngested, want)
	}
}

func TestRunDue_ResumesFromCursorWithOverlap(t *testing.T) {
	h := newHarness(t)
	h.sync(domain.SyncTriggerScheduled, nil)

	h.now = t0.Add(24 * time.Hour) // the next morning; next_sync_at (04:00) has passed
	if _, err := testPool.Exec(h.ctx, `UPDATE provider_connections SET next_sync_at = 'infinity' WHERE id <> $1`, h.conn.ID); err != nil {
		t.Fatal(err)
	}
	done, err := h.syncer.RunDue(h.ctx)
	if err != nil || done != 1 {
		t.Fatalf("RunDue = %d, %v", done, err)
	}
	runs, err := testStore.ListSyncRuns(h.ctx, h.user.ID, domain.ProviderMock, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].Trigger != domain.SyncTriggerScheduled || runs[0].Status != domain.SyncSucceeded {
		t.Fatalf("runs = %+v", runs)
	}
	if want := dateRange(t, "2026-11-18", "2026-11-22"); runs[0].Range != want {
		t.Errorf("incremental range = %+v, want cursor minus 3 overlap days through today %+v", runs[0].Range, want)
	}
	if !strings.Contains(h.cursor(), "2026-11-22") {
		t.Errorf("cursor = %s", h.cursor())
	}
	if done, _ := h.syncer.RunDue(h.ctx); done != 0 {
		t.Errorf("connection synced again before its next nightly slot")
	}
}

type dueSnapshot struct {
	*store.Store
	afterList func()
}

func (s dueSnapshot) ListConnectionsDueForSync(ctx context.Context, now time.Time, limit int32) ([]domain.DueConnection, error) {
	due, err := s.Store.ListConnectionsDueForSync(ctx, now, limit)
	if err == nil {
		s.afterList()
	}
	return due, err
}

func TestRunDue_SkipsConnectionRescheduledSinceListing(t *testing.T) {
	var nextSync time.Time
	h := newHarness(t, func(h *harness, _ *sync.Config, _ *[]domain.HealthProvider) {
		h.store = dueSnapshot{Store: testStore, afterList: func() {
			if run := h.sync(domain.SyncTriggerManual, nil); run.Status != domain.SyncSucceeded {
				t.Fatalf("manual sync: %+v", run)
			}
			nextSync = h.connection().NextSyncAt
		}}
	})
	h.isolateDue(h.conn)

	if done, err := h.syncer.RunDue(h.ctx); err != nil || done != 0 {
		t.Fatalf("RunDue processed a stale due entry: %d, %v", done, err)
	}
	if n := h.count("sync_runs", h.user.ID); n != 1 {
		t.Errorf("recorded %d runs, want only the manual run", n)
	}
	if got := h.connection().NextSyncAt; !got.Equal(nextSync) {
		t.Errorf("next sync = %s, want %s from the manual run", got, nextSync)
	}
}

func TestBackfill_FillsHistoryWithoutTouchingConnectionState(t *testing.T) {
	h := newHarness(t)
	h.sync(domain.SyncTriggerScheduled, nil)
	before, beforeCursor := h.connection(), h.cursor()

	// Two days later a scheduled run would move both the cursor (to 11-23)
	// and the schedule (to 11-24 04:00); a backfill must move neither.
	h.now = t0.Add(48 * time.Hour)
	sept := dateRange(t, "2026-09-01", "2026-09-07")
	run := h.sync(domain.SyncTriggerBackfill, &sept)
	if run.Status != domain.SyncSucceeded || run.Trigger != domain.SyncTriggerBackfill || run.Range != sept {
		t.Fatalf("backfill = %+v", run)
	}
	got, err := testStore.ListDailySummaries(h.ctx, h.user.ID, domain.ProviderMock, sept)
	if err != nil || len(got) < 5 {
		t.Fatalf("September summaries: %d, %v", len(got), err)
	}
	after := h.connection()
	if !after.NextSyncAt.Equal(before.NextSyncAt) || !after.LastSyncedAt.Equal(*before.LastSyncedAt) || h.cursor() != beforeCursor {
		t.Errorf("backfill changed connection state: next %s → %s, last %v → %v, cursor %s → %s",
			before.NextSyncAt, after.NextSyncAt, before.LastSyncedAt, after.LastSyncedAt, beforeCursor, h.cursor())
	}

	future := dateRange(t, "2026-11-20", "2026-12-31")
	if capped := h.sync(domain.SyncTriggerBackfill, &future); capped.Range.End != date(t, "2026-11-23") {
		t.Errorf("backfill past today not capped: %+v", capped.Range)
	}
	tomorrow := dateRange(t, "2026-11-24", "2026-11-25")
	if _, err := h.syncer.SyncConnection(h.ctx, sync.Request{UserID: h.user.ID, Provider: domain.ProviderMock,
		Trigger: domain.SyncTriggerBackfill, Range: &tomorrow}); err == nil {
		t.Error("backfill starting in the future must fail")
	}
}

func TestBackfill_OnNewConnectionLeavesItUnsynced(t *testing.T) {
	h := newHarness(t)
	before := h.connection()
	r := dateRange(t, "2026-11-01", "2026-11-03")
	if run := h.sync(domain.SyncTriggerBackfill, &r); run.Status != domain.SyncSucceeded {
		t.Fatalf("backfill = %+v", run)
	}
	after := h.connection()
	if strings.Contains(h.cursor(), "synced_through") || after.LastSyncedAt != nil || !after.NextSyncAt.Equal(before.NextSyncAt) {
		t.Errorf("backfill marked a new connection synced: cursor %s, last %v, next %s → %s",
			h.cursor(), after.LastSyncedAt, before.NextSyncAt, after.NextSyncAt)
	}
}

func TestTransientErrors_AreRetriedWithExponentialBackoff(t *testing.T) {
	h := newHarness(t, withProvider(newFaulty(func(op string, call int) error {
		if op == "daily" && call <= 2 {
			return fmt.Errorf("upstream 503: %w", domain.ErrTransient)
		}
		return nil
	})))
	run := h.sync(domain.SyncTriggerManual, nil)
	if run.Status != domain.SyncSucceeded {
		t.Fatalf("run = %+v", run)
	}
	if len(h.sleeps) != 2 {
		t.Fatalf("slept %d times, want 2: %v", len(h.sleeps), h.sleeps)
	}
	for i, want := range []time.Duration{time.Second, 2 * time.Second} { // base 1s, doubling, equal jitter
		if h.sleeps[i] < want/2 || h.sleeps[i] > want {
			t.Errorf("retry %d waited %s, want within [%s, %s]", i+1, h.sleeps[i], want/2, want)
		}
	}
}

func TestFailingUnits_MakeRunPartialAndScheduleRetry(t *testing.T) {
	h := newHarness(t, withProvider(newFaulty(func(op string, _ int) error {
		switch op {
		case string(domain.MetricSpO2):
			return errors.New("spo2 scope not granted")
		case "sleep":
			return fmt.Errorf("upstream 429: %w", domain.ErrTransient)
		}
		return nil
	})))
	run := h.sync(domain.SyncTriggerScheduled, nil)

	if run.Status != domain.SyncPartial {
		t.Fatalf("status = %s, want partial", run.Status)
	}
	for _, want := range []string{"sleep sessions 2026-11-15..2026-11-21", "after 4 attempts", "upstream 429", "spo2 scope not granted"} {
		if !strings.Contains(run.ErrorMessage, want) {
			t.Errorf("error message %q lacks %q", run.ErrorMessage, want)
		}
	}
	if len(h.sleeps) != 3 {
		t.Errorf("slept %d times for one exhausted unit, want 3", len(h.sleeps))
	}
	if h.count("daily_summaries", h.user.ID) == 0 || h.count("sleep_sessions", h.user.ID) != 0 {
		t.Error("want the healthy units ingested and the failing ones not")
	}
	c := h.connection()
	if want := t0.Add(time.Hour); !c.NextSyncAt.Equal(want) || c.LastSyncedAt != nil {
		t.Errorf("next sync %s (want %s), last synced %v (want unset)", c.NextSyncAt, want, c.LastSyncedAt)
	}
	if strings.Contains(h.cursor(), "synced_through") {
		t.Errorf("partial run advanced the cursor: %s", h.cursor())
	}
}

func TestUnauthorized_StopsTheRunAndDisablesConnection(t *testing.T) {
	var wrapped *faulty
	h := newHarness(t, withProvider(func(p domain.HealthProvider) domain.HealthProvider {
		wrapped = newFaulty(func(string, int) error { return fmt.Errorf("token revoked: %w", domain.ErrUnauthorized) })(p).(*faulty)
		return wrapped
	}))
	run := h.sync(domain.SyncTriggerScheduled, nil)

	if run.Status != domain.SyncFailed || !strings.Contains(run.ErrorMessage, "token revoked") {
		t.Fatalf("run = %+v", run)
	}
	total := 0
	for _, n := range wrapped.calls {
		total += n
	}
	if total != 1 {
		t.Errorf("provider called %d times after rejecting credentials, want 1", total)
	}
	if c := h.connection(); c.Status != domain.ConnectionError {
		t.Errorf("connection status = %s, want error", c.Status)
	}
	if _, err := h.syncer.SyncConnection(h.ctx, sync.Request{UserID: h.user.ID, Provider: domain.ProviderMock, Trigger: domain.SyncTriggerManual}); !errors.Is(err, sync.ErrConnectionInactive) {
		t.Errorf("sync of disabled connection: err = %v", err)
	}
}

func TestProviderReturningForeignRows_FailsTheUnit(t *testing.T) {
	h := newHarness(t, withProvider(func(p domain.HealthProvider) domain.HealthProvider {
		f := newFaulty(nil)(p).(*faulty)
		f.rewrite = func(rows []domain.DailySummary) {
			for i := range rows {
				rows[i].Provider = domain.ProviderGoogleHealth
			}
		}
		f.rewriteSleep = func(rows []domain.SleepSession) {
			for i := range rows {
				rows[i].Provider = domain.ProviderGoogleHealth
			}
		}
		return f
	}))
	run := h.sync(domain.SyncTriggerManual, nil)
	for _, want := range []string{`"google_health" summary`, `"google_health" sleep session`} {
		if run.Status != domain.SyncPartial || !strings.Contains(run.ErrorMessage, want) {
			t.Errorf("run = %s %q, want partial mentioning %s", run.Status, run.ErrorMessage, want)
		}
	}
	if n := h.count("daily_summaries", h.user.ID) + h.count("sleep_sessions", h.user.ID); n != 0 {
		t.Errorf("%d foreign rows written", n)
	}
}

func TestConcurrentSync_IsRejectedAndStaleRunsAreAbandoned(t *testing.T) {
	h := newHarness(t)
	held, err := testStore.StartSyncRun(h.ctx, h.user.ID, domain.ProviderMock, domain.SyncTriggerManual, dateRange(t, "2026-11-20", "2026-11-21"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.syncer.SyncConnection(h.ctx, sync.Request{UserID: h.user.ID, Provider: domain.ProviderMock, Trigger: domain.SyncTriggerManual})
	if !errors.Is(err, store.ErrSyncInProgress) {
		t.Fatalf("second sync: err = %v, want ErrSyncInProgress", err)
	}

	// The holder "crashed" three hours ago.
	if _, err := testPool.Exec(h.ctx, `UPDATE sync_runs SET started_at = now() - interval '3 hours' WHERE id = $1`, held.ID); err != nil {
		t.Fatal(err)
	}
	if run := h.sync(domain.SyncTriggerManual, nil); run.Status != domain.SyncSucceeded {
		t.Fatalf("sync after stale run = %+v", run)
	}
	runs, err := testStore.ListSyncRuns(h.ctx, h.user.ID, domain.ProviderMock, 10)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(runs, func(r domain.SyncRun) bool { return r.ID == held.ID })
	if i < 0 || runs[i].Status != domain.SyncFailed || !strings.Contains(runs[i].ErrorMessage, "abandoned") {
		t.Errorf("stale run not abandoned: %+v", runs)
	}
}

func TestCanceledContext_StillClosesTheRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHarness(t, withProvider(newFaulty(func(op string, _ int) error {
		if op == "daily" {
			cancel() // the worker received SIGTERM mid-run
		}
		return nil
	})))
	run, err := h.syncer.SyncConnection(ctx, sync.Request{UserID: h.user.ID, Provider: domain.ProviderMock, Trigger: domain.SyncTriggerScheduled})
	if err != nil {
		t.Fatalf("SyncConnection: %v", err)
	}
	if run.Status != domain.SyncFailed || run.FinishedAt == nil || !strings.Contains(run.ErrorMessage, "context canceled") {
		t.Fatalf("run = %+v", run)
	}
	if c := h.connection(); !c.NextSyncAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("interrupted scheduled run not rescheduled: next %s", c.NextSyncAt)
	}
}

func TestInactiveAndUnregisteredConnections_StartNoRun(t *testing.T) {
	h := newHarness(t)
	if err := testStore.SetConnectionStatus(h.ctx, h.user.ID, h.conn.ID, domain.ConnectionRevoked); err != nil {
		t.Fatal(err)
	}
	if _, err := h.syncer.SyncConnection(h.ctx, sync.Request{UserID: h.user.ID, Provider: domain.ProviderMock, Trigger: domain.SyncTriggerManual}); !errors.Is(err, sync.ErrConnectionInactive) {
		t.Errorf("revoked: err = %v", err)
	}

	google := h.connect(h.user, domain.ProviderGoogleHealth) // no adapter registered
	if _, err := h.syncer.SyncConnection(h.ctx, sync.Request{UserID: h.user.ID, Provider: domain.ProviderGoogleHealth, Trigger: domain.SyncTriggerManual}); !errors.Is(err, sync.ErrUnknownProvider) {
		t.Errorf("unregistered provider: err = %v", err)
	}
	h.isolateDue(google)
	if done, err := h.syncer.RunDue(h.ctx); err != nil || done != 0 {
		t.Fatalf("RunDue = %d, %v", done, err)
	}
	c, err := testStore.GetConnection(h.ctx, h.user.ID, domain.ProviderGoogleHealth)
	if err != nil {
		t.Fatal(err)
	}
	if want := t0.Add(time.Hour); !c.NextSyncAt.Equal(want) {
		t.Errorf("unstartable connection next sync = %s, want pushed back to %s", c.NextSyncAt, want)
	}
	if n := h.count("sync_runs", h.user.ID); n != 0 {
		t.Errorf("%d sync runs recorded for connections that cannot sync", n)
	}
}

// Each user's rows are exactly that user's provider data, in their timezone.
func TestRunDue_SyncsUsersInIsolation(t *testing.T) {
	h := newHarness(t)
	other := h.newUser("America/New_York")
	otherConn := h.connect(other, domain.ProviderMock)
	h.isolateDue(h.conn, otherConn)

	if done, err := h.syncer.RunDue(h.ctx); err != nil || done != 2 {
		t.Fatalf("RunDue = %d, %v", done, err)
	}
	for _, u := range []domain.User{h.user, other} {
		runs, err := testStore.ListSyncRuns(h.ctx, u.ID, domain.ProviderMock, 10)
		if err != nil || len(runs) != 1 || runs[0].Status != domain.SyncSucceeded {
			t.Fatalf("%s runs = %+v, %v", u.Timezone, runs, err)
		}
		r := runs[0].Range
		want, err := h.mock.FetchDailySummaries(h.ctx, domain.Connection{UserID: u.ID, Provider: domain.ProviderMock, Location: u.Location()}, r)
		if err != nil {
			t.Fatal(err)
		}
		got, err := testStore.ListDailySummaries(h.ctx, u.ID, domain.ProviderMock, r)
		if err != nil {
			t.Fatal(err)
		}
		slices.Reverse(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: stored summaries differ from that user's provider data", u.Timezone)
		}
	}
	if h.fingerprint(h.user.ID) == h.fingerprint(other.ID) {
		t.Error("two users received identical data")
	}
}

type memCredentials struct {
	creds domain.Credentials
	saved []domain.Credentials
}

func (m *memCredentials) LoadCredentials(context.Context, uuid.UUID, uuid.UUID) (domain.Credentials, error) {
	return m.creds, nil
}

func (m *memCredentials) SaveCredentials(_ context.Context, _, _ uuid.UUID, c domain.Credentials) error {
	m.saved = append(m.saved, c)
	m.creds = c
	return nil
}

func TestCredentials_RefreshedOnlyWhenAboutToExpire(t *testing.T) {
	creds := &memCredentials{creds: domain.Credentials{AccessToken: "fake-access", RefreshToken: "fake-refresh", ExpiresAt: t0.Add(2 * time.Minute)}}
	h := newHarness(t, func(_ *harness, c *sync.Config, _ *[]domain.HealthProvider) { c.Credentials = creds })

	if run := h.sync(domain.SyncTriggerManual, nil); run.Status != domain.SyncSucceeded {
		t.Fatalf("run = %+v", run)
	}
	if len(creds.saved) != 1 || !creds.saved[0].ExpiresAt.Equal(t0.Add(mock.AccessTokenTTL)) || creds.saved[0].AccessToken == "fake-access" {
		t.Fatalf("saved credentials = %v (expiry %v)", len(creds.saved), creds.creds.ExpiresAt)
	}
	h.sync(domain.SyncTriggerManual, nil) // token now valid for an hour
	if len(creds.saved) != 1 {
		t.Errorf("refreshed a token that was not about to expire")
	}
}

// tokenSpy records the access token each fetch sees and advances the clock,
// so a run outlives its access token.
type tokenSpy struct {
	domain.HealthProvider
	h    *harness
	seen []string
}

func (s *tokenSpy) observe(c domain.Connection) {
	s.seen = append(s.seen, c.Credentials.AccessToken)
	s.h.now = s.h.now.Add(time.Minute)
}

func (s *tokenSpy) FetchDailySummaries(ctx context.Context, c domain.Connection, r domain.DateRange) ([]domain.DailySummary, error) {
	s.observe(c)
	return s.HealthProvider.FetchDailySummaries(ctx, c, r)
}

func (s *tokenSpy) FetchSleepSessions(ctx context.Context, c domain.Connection, r domain.DateRange) ([]domain.SleepSession, error) {
	s.observe(c)
	return s.HealthProvider.FetchSleepSessions(ctx, c, r)
}

func (s *tokenSpy) FetchIntradayMetrics(ctx context.Context, c domain.Connection, m domain.MetricType, r domain.TimeRange) ([]domain.IntradayPoint, error) {
	s.observe(c)
	return s.HealthProvider.FetchIntradayMetrics(ctx, c, m, r)
}

func TestCredentials_RefreshedMidRunBeforeTheyExpire(t *testing.T) {
	// Valid for 8 minutes: not refreshed at the start (RefreshBefore is 5m),
	// but each unit takes a minute, so it must be refreshed mid-run.
	creds := &memCredentials{creds: domain.Credentials{AccessToken: "fake-access", RefreshToken: "fake-refresh", ExpiresAt: t0.Add(8 * time.Minute)}}
	var spy *tokenSpy
	h := newHarness(t,
		func(_ *harness, c *sync.Config, _ *[]domain.HealthProvider) { c.Credentials = creds },
		func(h *harness, _ *sync.Config, ps *[]domain.HealthProvider) {
			spy = &tokenSpy{HealthProvider: h.mock, h: h}
			*ps = []domain.HealthProvider{spy}
		})

	if run := h.sync(domain.SyncTriggerManual, nil); run.Status != domain.SyncSucceeded {
		t.Fatalf("run = %+v", run)
	}
	if len(creds.saved) != 1 {
		t.Fatalf("saved %d credentials, want exactly one mid-run refresh", len(creds.saved))
	}
	fresh := creds.saved[0].AccessToken
	if len(spy.seen) < 5 || spy.seen[0] != "fake-access" || spy.seen[len(spy.seen)-1] != fresh {
		t.Fatalf("tokens seen by units = %v; want the original first and the refreshed one last", spy.seen)
	}
	for i, tok := range spy.seen {
		// Unit i runs at t0+i min; from t0+3m the token is inside the window.
		if inWindow := i >= 3; inWindow && tok == "fake-access" {
			t.Errorf("unit %d used the token after it entered the refresh window", i)
		}
	}
}

func TestUnauthorized_RefreshesOnceAndRetriesTheUnit(t *testing.T) {
	// The token looks valid for another hour, but the provider rejects it
	// mid-run: refresh once, retry the unit, keep the connection active.
	creds := &memCredentials{creds: domain.Credentials{AccessToken: "fake-access", RefreshToken: "fake-refresh", ExpiresAt: t0.Add(time.Hour)}}
	h := newHarness(t,
		func(_ *harness, c *sync.Config, _ *[]domain.HealthProvider) { c.Credentials = creds },
		withProvider(newFaulty(func(op string, call int) error {
			if op == string(domain.MetricSteps) && call == 1 {
				return fmt.Errorf("HTTP 401 for fake-access: %w", domain.ErrUnauthorized)
			}
			return nil
		})))
	run := h.sync(domain.SyncTriggerScheduled, nil)
	if run.Status != domain.SyncSucceeded {
		t.Fatalf("run = %s (%s), want succeeded after one forced refresh", run.Status, run.ErrorMessage)
	}
	if len(creds.saved) != 1 || h.connection().Status != domain.ConnectionActive || h.countMetric(domain.MetricSteps) == 0 {
		t.Errorf("saves %d, status %s, steps rows %d", len(creds.saved), h.connection().Status, h.countMetric(domain.MetricSteps))
	}
}

func TestUnauthorized_RejectedRefreshDisablesConnection(t *testing.T) {
	creds := &memCredentials{creds: domain.Credentials{AccessToken: "fake-access", RefreshToken: "fake-refresh", ExpiresAt: t0.Add(time.Hour)}}
	h := newHarness(t,
		func(_ *harness, c *sync.Config, _ *[]domain.HealthProvider) { c.Credentials = creds },
		withProvider(newFaulty(func(op string, _ int) error {
			if op == string(domain.MetricSteps) || op == "refresh" {
				return fmt.Errorf("rejected: %w", domain.ErrUnauthorized)
			}
			return nil
		})))
	run := h.sync(domain.SyncTriggerScheduled, nil)
	if run.Status != domain.SyncPartial || h.connection().Status != domain.ConnectionError || len(creds.saved) != 0 {
		t.Fatalf("run %s, connection %s, saves %d; want partial, error, none", run.Status, h.connection().Status, len(creds.saved))
	}
}

func TestRunDue_SkipsBusyConnectionWithoutRescheduling(t *testing.T) {
	h := newHarness(t)
	held, err := testStore.StartSyncRun(h.ctx, h.user.ID, domain.ProviderMock, domain.SyncTriggerManual, dateRange(t, "2026-11-21", "2026-11-21"))
	if err != nil {
		t.Fatal(err)
	}
	h.isolateDue(h.conn)
	before := h.connection()
	if done, err := h.syncer.RunDue(h.ctx); err != nil || done != 0 {
		t.Fatalf("RunDue = %d, %v", done, err)
	}
	if after := h.connection(); !after.NextSyncAt.Equal(before.NextSyncAt) {
		t.Errorf("busy connection rescheduled %s → %s; it should be retried on the next poll", before.NextSyncAt, after.NextSyncAt)
	}
	runs, err := testStore.ListSyncRuns(h.ctx, h.user.ID, domain.ProviderMock, 10)
	if err != nil || len(runs) != 1 || runs[0].ID != held.ID {
		t.Errorf("runs = %+v, %v; want only the held run", runs, err)
	}
}

func TestUnauthorized_AfterProgressIsPartialAndDisablesConnection(t *testing.T) {
	h := newHarness(t, withProvider(newFaulty(func(op string, _ int) error {
		if op == string(domain.MetricSteps) {
			return fmt.Errorf("token revoked mid-run: %w", domain.ErrUnauthorized)
		}
		return nil
	})))
	run := h.sync(domain.SyncTriggerScheduled, nil)
	if run.Status != domain.SyncPartial || h.count("daily_summaries", h.user.ID) == 0 {
		t.Fatalf("run = %s, want partial with earlier units kept", run.Status)
	}
	if h.count("intraday_metrics", h.user.ID) != h.countMetric(domain.MetricHeartRate) {
		t.Error("units after the rejection still ran")
	}
	if c := h.connection(); c.Status != domain.ConnectionError || strings.Contains(h.cursor(), "synced_through") {
		t.Errorf("connection = %s, cursor %s; want error and no cursor", c.Status, h.cursor())
	}
}

func (h *harness) countMetric(m domain.MetricType) int {
	h.t.Helper()
	var n int
	if err := testPool.QueryRow(h.ctx, `SELECT count(*) FROM intraday_metrics WHERE user_id = $1 AND metric_type = $2`, h.user.ID, m).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func TestRefreshFailures(t *testing.T) {
	expiring := domain.Credentials{AccessToken: "fake-access-secret", RefreshToken: "fake-refresh-secret", ExpiresAt: t0.Add(time.Minute)}
	for name, tc := range map[string]struct {
		err        error
		wantStatus domain.ConnectionStatus
		wantSleeps int
	}{
		"rejected":  {fmt.Errorf("invalid_grant for fake-refresh-secret: %w", domain.ErrUnauthorized), domain.ConnectionError, 0},
		"transient": {fmt.Errorf("token endpoint 503: %w", domain.ErrTransient), domain.ConnectionActive, 3},
	} {
		t.Run(name, func(t *testing.T) {
			creds := &memCredentials{creds: expiring}
			h := newHarness(t,
				func(_ *harness, c *sync.Config, _ *[]domain.HealthProvider) { c.Credentials = creds },
				withProvider(newFaulty(func(op string, _ int) error {
					if op == "refresh" {
						return tc.err
					}
					return nil
				})))
			run := h.sync(domain.SyncTriggerScheduled, nil)
			if run.Status != domain.SyncFailed || !strings.Contains(run.ErrorMessage, "refreshing credentials") {
				t.Fatalf("run = %+v", run)
			}
			if strings.Contains(run.ErrorMessage, "fake-refresh-secret") {
				t.Errorf("token leaked into %q", run.ErrorMessage)
			}
			if name == "rejected" && !strings.Contains(run.ErrorMessage, "[REDACTED]") {
				t.Errorf("token in the upstream error not marked redacted: %q", run.ErrorMessage)
			}
			if c := h.connection(); c.Status != tc.wantStatus || len(h.sleeps) != tc.wantSleeps || len(creds.saved) != 0 {
				t.Errorf("status %s (want %s), %d retries (want %d), %d saves", c.Status, tc.wantStatus, len(h.sleeps), tc.wantSleeps, len(creds.saved))
			}
			if h.count("daily_summaries", h.user.ID) != 0 {
				t.Error("ingested without valid credentials")
			}
		})
	}
}

func TestAbandonedRun_CannotWriteOrChangeConnection(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "provider returns rows"},
		{name: "provider rejects credentials", err: domain.ErrUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h *harness
			h = newHarness(t, withProvider(newFaulty(func(op string, _ int) error {
				if op != "sleep" {
					return nil
				}
				// Abandon the run during the provider request, then claim the slot.
				if _, err := testPool.Exec(h.ctx, `UPDATE sync_runs SET heartbeat_at = now() - interval '3 hours'
					WHERE user_id = $1 AND provider = $2 AND status = 'running'`, h.user.ID, domain.ProviderMock); err != nil {
					t.Fatal(err)
				}
				_, _, err := testStore.ClaimSyncRun(h.ctx, h.user.ID, domain.ProviderMock, domain.SyncTriggerManual, h.now, 2*time.Hour,
					func([]byte) (domain.DateRange, error) { return dateRange(t, "2026-11-20", "2026-11-21"), nil })
				if err != nil {
					t.Fatalf("claiming replacement run: %v", err)
				}
				return tc.err
			})))
			before := h.connection()

			_, err := h.syncer.SyncConnection(h.ctx, sync.Request{UserID: h.user.ID, Provider: domain.ProviderMock, Trigger: domain.SyncTriggerScheduled})
			if !errors.Is(err, store.ErrSyncRunAbandoned) {
				t.Fatalf("err = %v, want abandoned", err)
			}
			if n := h.count("sleep_sessions", h.user.ID); n != 0 {
				t.Errorf("abandoned run wrote %d sleep sessions", n)
			}
			if h.countMetric(domain.MetricHeartRate) != 0 {
				t.Error("kept ingesting after the run was abandoned")
			}
			after := h.connection()
			if after.Status != domain.ConnectionActive || !after.NextSyncAt.Equal(before.NextSyncAt) ||
				after.LastSyncedAt != nil || strings.Contains(h.cursor(), "synced_through") {
				t.Errorf("abandoned run changed the connection: %+v cursor %s", after, h.cursor())
			}
		})
	}
}

func TestHeartbeat_KeepsLongRunsFromLookingStale(t *testing.T) {
	var h *harness
	var stale int64
	h = newHarness(t, withProvider(newFaulty(func(op string, call int) error {
		if op == string(domain.MetricHeartRate) && call == 1 {
			// The run started long ago but its heartbeat is fresh.
			if _, err := testPool.Exec(h.ctx, `UPDATE sync_runs SET started_at = now() - interval '5 hours' WHERE user_id = $1`, h.user.ID); err != nil {
				h.t.Fatal(err)
			}
			n, err := testStore.AbandonStaleSyncRuns(h.ctx, h.user.ID, domain.ProviderMock, 2*time.Hour)
			if err != nil {
				h.t.Fatal(err)
			}
			stale = n
		}
		return nil
	})))
	if run := h.sync(domain.SyncTriggerManual, nil); run.Status != domain.SyncSucceeded {
		t.Fatalf("run = %+v", run)
	}
	if stale != 0 {
		t.Errorf("a run with a fresh heartbeat was abandoned")
	}
}

// failingFinish makes the audit row impossible to close.
type failingFinish struct{ *store.Store }

func (failingFinish) FinishConnectionSyncRun(context.Context, domain.SyncRun, uuid.UUID, store.SyncResult) (domain.SyncRun, error) {
	return domain.SyncRun{}, errors.New("connection reset")
}

func TestFinishFailure_StillIdentifiesTheRun(t *testing.T) {
	h := newHarness(t, func(h *harness, _ *sync.Config, _ *[]domain.HealthProvider) { h.store = failingFinish{testStore} })
	run, err := h.syncer.SyncConnection(h.ctx, sync.Request{UserID: h.user.ID, Provider: domain.ProviderMock, Trigger: domain.SyncTriggerScheduled})
	if err == nil || run.ID == uuid.Nil {
		t.Fatalf("run %s, err %v; want the started run's ID with an error", run.ID, err)
	}
	// RunDue must treat it as a bookkeeping failure, not reschedule a run that never started.
	h.isolateDue(h.conn)
	due := h.connection().NextSyncAt
	if _, err := testPool.Exec(h.ctx, `UPDATE sync_runs SET status = 'failed', finished_at = now() WHERE user_id = $1 AND status = 'running'`, h.user.ID); err != nil {
		t.Fatal(err)
	}
	if done, err := h.syncer.RunDue(h.ctx); err != nil || done != 0 {
		t.Fatalf("RunDue = %d, %v", done, err)
	}
	if after := h.connection(); !after.NextSyncAt.Equal(due) {
		t.Errorf("next sync = %s, want original due time %s after a failed finish", after.NextSyncAt, due)
	}
}
