package store_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/testdb"
)

const mock = domain.ProviderMock

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func ctxT(t *testing.T) context.Context {
	t.Helper()
	testdb.Require(t, testPool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// newUser creates a user with a unique, obviously fictional example.com address.
func newUser(t *testing.T) domain.User {
	t.Helper()
	u, err := testStore.CreateUser(ctxT(t), fmt.Sprintf("test-%s@example.com", uuid.NewString()), "Europe/Stockholm")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

func mustDate(t *testing.T, s string) domain.Date {
	t.Helper()
	d, err := domain.ParseDate(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func ptr[T any](v T) *T { return &v }

func countRows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(ctxT(t), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

var t0 = time.Date(2026, 11, 20, 22, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// migrations
// ---------------------------------------------------------------------------

func TestMigrations_UpDownUp(t *testing.T) {
	ctx := ctxT(t)
	dbName := "migrate_" + uuid.NewString()[:8]
	if _, err := testPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize()); err != nil {
		t.Fatalf("creating scratch database: %v", err)
	}
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + dbName
	scratch := u.String()

	if err := store.MigrateUp(scratch); err != nil {
		t.Fatalf("first up: %v", err)
	}
	if err := store.MigrateUp(scratch); err != nil {
		t.Fatalf("second up must be a no-op: %v", err)
	}
	if err := store.MigrateDown(scratch); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := store.MigrateUp(scratch); err != nil {
		t.Fatalf("up after down: %v", err)
	}
}

func TestMigrations_RedactURLPasswords(t *testing.T) {
	u := url.URL{Scheme: "postgres", User: url.UserPassword("pulse", "user/test+secret"), Host: "localhost", Path: "/pulse"}
	query := url.Values{
		"password":            {"query/test secret"},
		"sslmode":             {"disable"},
		"x-statement-timeout": {"query/test secret"}, // the driver includes the invalid value in its error
	}
	u.RawQuery = query.Encode()
	err := store.MigrateUp(u.String())
	if err == nil || !strings.Contains(err.Error(), "invalid syntax") {
		t.Fatal("expected the invalid migration option error")
	}
	for _, secret := range []string{"user/test+secret", "user%2Ftest+secret", "query/test secret", "query%2Ftest+secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Error("migration error exposed a password")
		}
	}
}

// ---------------------------------------------------------------------------
// users
// ---------------------------------------------------------------------------

func TestCreateUser_RejectsInvalidTimezoneAndDuplicateEmail(t *testing.T) {
	ctx := ctxT(t)
	if _, err := testStore.CreateUser(ctx, "tz@example.com", "Mars/Olympus_Mons"); err == nil {
		t.Fatal("expected invalid timezone error")
	}
	email := fmt.Sprintf("Dup-%s@Example.com", uuid.NewString())
	if _, err := testStore.CreateUser(ctx, email, "UTC"); err != nil {
		t.Fatal(err)
	}
	// Uniqueness is case-insensitive (users_email_lower_key).
	if _, err := testStore.CreateUser(ctx, strings.ToLower(email), "UTC"); err == nil {
		t.Fatal("expected duplicate email error")
	}
}

func TestGetUser_NotFound(t *testing.T) {
	_, err := testStore.GetUser(ctxT(t), uuid.New())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// daily summaries
// ---------------------------------------------------------------------------

func TestDailySummaries_UpsertIsIdempotentAndUpdates(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	day := mustDate(t, "2026-11-20")
	in := []domain.DailySummary{{
		Provider: mock, Date: day, Steps: ptr[int32](8000), RestingHeartRate: ptr[int16](55),
		HRVRMSSDMs: ptr(42.5), SleepScore: ptr[int16](81),
	}}

	for i := 0; i < 2; i++ {
		if _, err := testStore.UpsertDailySummaries(ctx, u.ID, in); err != nil {
			t.Fatalf("upsert #%d: %v", i, err)
		}
	}
	if n := countRows(t, "SELECT count(*) FROM daily_summaries WHERE user_id=$1", u.ID); n != 1 {
		t.Fatalf("re-running upsert must not duplicate; rows=%d", n)
	}

	// Provider correction: steps change, SpO2 newly reported, sleep score withdrawn.
	in[0].Steps = ptr[int32](8500)
	in[0].SpO2AvgPct = ptr(96.0)
	in[0].SleepScore = nil
	if _, err := testStore.UpsertDailySummaries(ctx, u.ID, in); err != nil {
		t.Fatal(err)
	}

	got, err := testStore.ListDailySummaries(ctx, u.ID, mock, domain.DateRange{Start: day, End: day})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 summary, got %d", len(got))
	}
	g := got[0]
	if g.Date != day || *g.Steps != 8500 || *g.SpO2AvgPct != 96.0 || g.SleepScore != nil || *g.RestingHeartRate != 55 || g.CaloriesKcal != nil {
		t.Fatalf("unexpected summary after correction: %+v", g)
	}
}

func TestDailySummaries_ListNewestFirstWithinRange(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	var in []domain.DailySummary
	start := mustDate(t, "2026-11-01")
	for i := 0; i < 10; i++ {
		in = append(in, domain.DailySummary{Provider: mock, Date: start.AddDays(i), Steps: ptr(int32(1000 * i))})
	}
	if _, err := testStore.UpsertDailySummaries(ctx, u.ID, in); err != nil {
		t.Fatal(err)
	}
	got, err := testStore.ListDailySummaries(ctx, u.ID, mock, domain.DateRange{Start: start.AddDays(2), End: start.AddDays(4)})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-11-05", "2026-11-04", "2026-11-03"}
	if len(got) != len(want) {
		t.Fatalf("want %d rows, got %d", len(want), len(got))
	}
	for i, w := range want {
		if got[i].Date.String() != w {
			t.Errorf("row %d: want %s, got %s", i, w, got[i].Date)
		}
	}
}

func TestDailySummaries_CheckConstraintRejectsImpossibleValues(t *testing.T) {
	u := newUser(t)
	_, err := testStore.UpsertDailySummaries(ctxT(t), u.ID, []domain.DailySummary{
		{Provider: mock, Date: mustDate(t, "2026-11-20"), RestingHeartRate: ptr[int16](900)},
	})
	if err == nil {
		t.Fatal("expected CHECK violation for resting HR 900")
	}
}

// ---------------------------------------------------------------------------
// intraday metrics
// ---------------------------------------------------------------------------

func TestIntraday_UpsertIdempotentDedupedAndCountsChanges(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	pts := []domain.IntradayPoint{
		{RecordedAt: t0, Value: 60},
		{RecordedAt: t0.Add(5 * time.Second), Value: 61},
		{RecordedAt: t0, Value: 62}, // duplicate instant in one batch: last wins
	}

	n, err := testStore.UpsertIntradayMetrics(ctx, u.ID, mock, domain.MetricHeartRate, pts)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("first upsert: want 2 rows written, got %d", n)
	}

	n, err = testStore.UpsertIntradayMetrics(ctx, u.ID, mock, domain.MetricHeartRate, pts)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("identical re-sync must write 0 rows, got %d", n)
	}

	pts[1].Value = 70
	n, err = testStore.UpsertIntradayMetrics(ctx, u.ID, mock, domain.MetricHeartRate, pts)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("one corrected sample: want 1 row written, got %d", n)
	}

	got, err := testStore.ListIntradayMetrics(ctx, u.ID, mock, domain.MetricHeartRate, domain.TimeRange{Start: t0, End: t0.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Value != 62 || got[1].Value != 70 || !got[0].RecordedAt.Before(got[1].RecordedAt) {
		t.Fatalf("unexpected points: %+v", got)
	}
	if got[0].RecordedAt.Location() != time.UTC {
		t.Errorf("timestamps must be returned in UTC, got %s", got[0].RecordedAt.Location())
	}
}

func TestIntraday_LargeBatchSpansMultipleStatements(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	const total = 12_345 // > 2 × internal batch size
	pts := make([]domain.IntradayPoint, total)
	for i := range pts {
		pts[i] = domain.IntradayPoint{RecordedAt: t0.Add(time.Duration(i) * 5 * time.Second), Value: float64(50 + i%40)}
	}
	n, err := testStore.UpsertIntradayMetrics(ctx, u.ID, mock, domain.MetricHeartRate, pts)
	if err != nil {
		t.Fatal(err)
	}
	if n != total {
		t.Fatalf("want %d rows written, got %d", total, n)
	}
	if c := countRows(t, "SELECT count(*) FROM intraday_metrics WHERE user_id=$1", u.ID); c != total {
		t.Fatalf("want %d rows stored, got %d", total, c)
	}
}

func TestIntraday_RangeIsHalfOpenAndMetricScoped(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	hr := []domain.IntradayPoint{{RecordedAt: t0, Value: 60}, {RecordedAt: t0.Add(time.Minute), Value: 61}}
	steps := []domain.IntradayPoint{{RecordedAt: t0, Value: 12}}
	if _, err := testStore.UpsertIntradayMetrics(ctx, u.ID, mock, domain.MetricHeartRate, hr); err != nil {
		t.Fatal(err)
	}
	if _, err := testStore.UpsertIntradayMetrics(ctx, u.ID, mock, domain.MetricSteps, steps); err != nil {
		t.Fatal(err)
	}
	got, err := testStore.ListIntradayMetrics(ctx, u.ID, mock, domain.MetricHeartRate, domain.TimeRange{Start: t0, End: t0.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Value != 60 {
		t.Fatalf("want only the heart-rate sample at t0 (end exclusive), got %+v", got)
	}
}

func TestIntraday_RejectsUnknownMetricAndMismatchedPoint(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	if _, err := testStore.UpsertIntradayMetrics(ctx, u.ID, mock, "blood_alcohol", nil); err == nil {
		t.Fatal("expected unknown metric error")
	}
	_, err := testStore.UpsertIntradayMetrics(ctx, u.ID, mock, domain.MetricHeartRate,
		[]domain.IntradayPoint{{Metric: domain.MetricSteps, RecordedAt: t0, Value: 1}})
	if err == nil {
		t.Fatal("expected mismatched metric error")
	}
}

// ---------------------------------------------------------------------------
// sleep
// ---------------------------------------------------------------------------

func night(t *testing.T, sessionID string, start time.Time, stages ...domain.SleepStageType) domain.SleepSession {
	t.Helper()
	s := domain.SleepSession{
		Provider: mock, ProviderSessionID: sessionID, SleepDate: domain.DateOf(start.Add(8 * time.Hour)),
		StartAt: start, IsMainSleep: true, EfficiencyPct: ptr[int16](90),
	}
	at := start
	for _, st := range stages {
		s.Stages = append(s.Stages, domain.SleepStage{Stage: st, StartAt: at, EndAt: at.Add(30 * time.Minute)})
		at = at.Add(30 * time.Minute)
	}
	s.EndAt = at
	if len(stages) == 0 {
		s.EndAt = start.Add(7 * time.Hour)
	}
	return s
}

func TestSleep_ResyncWithCorrectedTimesUpdatesInPlaceAndReplacesStages(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	s := night(t, "upstream-1", t0, domain.SleepStageLight, domain.SleepStageDeep, domain.SleepStageREM)
	if _, err := testStore.UpsertSleepSessions(ctx, u.ID, []domain.SleepSession{s}); err != nil {
		t.Fatal(err)
	}

	// Provider re-scores the night: start shifts 10 min, stages re-segmented.
	corrected := night(t, "upstream-1", t0.Add(10*time.Minute), domain.SleepStageLight, domain.SleepStageAwake)
	if _, err := testStore.UpsertSleepSessions(ctx, u.ID, []domain.SleepSession{corrected}); err != nil {
		t.Fatal(err)
	}

	if n := countRows(t, "SELECT count(*) FROM sleep_sessions WHERE user_id=$1", u.ID); n != 1 {
		t.Fatalf("corrected session must update in place; sessions=%d", n)
	}
	got, err := testStore.ListSleepSessions(ctx, u.ID, mock, domain.DateRange{Start: corrected.SleepDate, End: corrected.SleepDate})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 session, got %d", len(got))
	}
	g := got[0]
	if !g.StartAt.Equal(corrected.StartAt) || len(g.Stages) != 2 || g.Stages[1].Stage != domain.SleepStageAwake {
		t.Fatalf("stages not replaced: %+v", g)
	}
	if g.ID == uuid.Nil {
		t.Error("session ID must be populated")
	}
}

func TestSleep_ValidationRejectsBadInputWithoutWriting(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	good := night(t, "ok", t0, domain.SleepStageLight)
	bad := night(t, "bad", t0.Add(24*time.Hour), domain.SleepStageLight)
	bad.Stages[0].Stage = "hypnagogic"
	if _, err := testStore.UpsertSleepSessions(ctx, u.ID, []domain.SleepSession{good, bad}); err == nil {
		t.Fatal("expected invalid stage error")
	}
	if n := countRows(t, "SELECT count(*) FROM sleep_sessions WHERE user_id=$1", u.ID); n != 0 {
		t.Fatalf("batch must be all-or-nothing; sessions=%d", n)
	}
}

func TestSleep_DatabaseErrorRollsBackWholeBatch(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	good := night(t, "first", t0, domain.SleepStageLight)
	bad := night(t, "second", t0.Add(24*time.Hour), domain.SleepStageDeep)
	bad.EfficiencyPct = ptr[int16](150) // passes Go validation, violates the DB CHECK
	if _, err := testStore.UpsertSleepSessions(ctx, u.ID, []domain.SleepSession{good, bad}); err == nil {
		t.Fatal("expected CHECK violation")
	}
	if n := countRows(t, "SELECT count(*) FROM sleep_sessions WHERE user_id=$1", u.ID); n != 0 {
		t.Fatalf("first session must be rolled back; sessions=%d", n)
	}
	if n := countRows(t, "SELECT count(*) FROM sleep_stages WHERE user_id=$1", u.ID); n != 0 {
		t.Fatalf("stages must be rolled back; stages=%d", n)
	}
}

func TestSleep_ListGroupsStagesPerSessionNewestFirst(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	n1 := night(t, "n1", t0, domain.SleepStageLight, domain.SleepStageDeep)
	n2 := night(t, "n2", t0.Add(24*time.Hour), domain.SleepStageREM)
	nap := night(t, "nap", t0.Add(24*time.Hour+14*time.Hour)) // no stages reported
	nap.IsMainSleep = false
	if _, err := testStore.UpsertSleepSessions(ctx, u.ID, []domain.SleepSession{n1, n2, nap}); err != nil {
		t.Fatal(err)
	}
	got, err := testStore.ListSleepSessions(ctx, u.ID, mock, domain.DateRange{Start: n1.SleepDate, End: nap.SleepDate})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 sessions, got %d", len(got))
	}
	stagesBy := map[string]int{}
	for _, g := range got {
		stagesBy[g.ProviderSessionID] = len(g.Stages)
		if g.Stages == nil {
			t.Errorf("%s: stages must be an empty slice, not nil", g.ProviderSessionID)
		}
		for i := 1; i < len(g.Stages); i++ {
			if !g.Stages[i-1].StartAt.Before(g.Stages[i].StartAt) {
				t.Errorf("%s: stages must be chronological", g.ProviderSessionID)
			}
		}
	}
	if stagesBy["n1"] != 2 || stagesBy["n2"] != 1 || stagesBy["nap"] != 0 {
		t.Fatalf("stages grouped wrongly: %v", stagesBy)
	}
	if got[len(got)-1].ProviderSessionID != "n1" {
		t.Errorf("oldest night must be last, got order %s,%s,%s", got[0].ProviderSessionID, got[1].ProviderSessionID, got[2].ProviderSessionID)
	}
}

func TestDailySummaries_DatabaseErrorRollsBackWholeBatch(t *testing.T) {
	u := newUser(t)
	_, err := testStore.UpsertDailySummaries(ctxT(t), u.ID, []domain.DailySummary{
		{Provider: mock, Date: mustDate(t, "2026-11-19"), Steps: ptr[int32](10)},
		{Provider: mock, Date: mustDate(t, "2026-11-20"), SleepScore: ptr[int16](101)},
	})
	if err == nil {
		t.Fatal("expected CHECK violation")
	}
	if n := countRows(t, "SELECT count(*) FROM daily_summaries WHERE user_id=$1", u.ID); n != 0 {
		t.Fatalf("batch must be all-or-nothing; rows=%d", n)
	}
}

func TestUpserts_RejectZeroValues(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	if _, err := testStore.UpsertDailySummaries(ctx, u.ID, []domain.DailySummary{{Provider: mock}}); err == nil {
		t.Error("zero date must be rejected")
	}
	if _, err := testStore.UpsertIntradayMetrics(ctx, u.ID, mock, domain.MetricSteps, []domain.IntradayPoint{{Value: 1}}); err == nil {
		t.Error("zero timestamp must be rejected")
	}
	if _, err := testStore.UpsertDailySummaries(ctx, u.ID, []domain.DailySummary{{Date: mustDate(t, "2026-11-20")}}); err == nil {
		t.Error("empty provider must be rejected")
	}
}

func TestUpserts_RejectImpossibleDates(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	day := mustDate(t, "2026-03-02")
	if _, err := testStore.UpsertDailySummaries(ctx, u.ID, []domain.DailySummary{{Provider: mock, Date: day, Steps: ptr[int32](10)}}); err != nil {
		t.Fatal(err)
	}
	invalid := domain.Date{Year: 2026, Month: time.February, Day: 30}
	if _, err := testStore.UpsertDailySummaries(ctx, u.ID, []domain.DailySummary{{Provider: mock, Date: invalid}}); err == nil {
		t.Error("invalid date must be rejected")
	}
	got, err := testStore.ListDailySummaries(ctx, u.ID, mock, domain.DateRange{Start: day, End: day})
	if err != nil || len(got) != 1 || got[0].Steps == nil || *got[0].Steps != 10 {
		t.Fatalf("valid summary changed after invalid write: %v %+v", err, got)
	}
	bad := night(t, "invalid-date", t0, domain.SleepStageLight)
	bad.SleepDate = invalid
	if _, err := testStore.UpsertSleepSessions(ctx, u.ID, []domain.SleepSession{bad}); err == nil {
		t.Error("invalid sleep date must be rejected")
	}
}

func TestIntraday_SubMicrosecondDuplicatesCollapse(t *testing.T) {
	u := newUser(t)
	pts := []domain.IntradayPoint{{RecordedAt: t0.Add(100), Value: 1}, {RecordedAt: t0.Add(900), Value: 2}} // same µs
	n, err := testStore.UpsertIntradayMetrics(ctxT(t), u.ID, mock, domain.MetricHeartRate, pts)
	if err != nil {
		t.Fatalf("sub-microsecond duplicates must not collide in the DB: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 row, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// sync runs & connections
// ---------------------------------------------------------------------------

func TestSyncRuns_Lifecycle(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	r := domain.DateRange{Start: mustDate(t, "2026-11-14"), End: mustDate(t, "2026-11-20")}
	run, err := testStore.StartSyncRun(ctx, u.ID, mock, domain.SyncTriggerManual, r)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.SyncRunning || run.FinishedAt != nil || run.Range != r {
		t.Fatalf("unexpected new run: %+v", run)
	}
	done, err := testStore.FinishSyncRun(ctx, u.ID, run.ID, domain.SyncPartial, 120, "sleep endpoint timed out")
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != domain.SyncPartial || done.RecordsIngested != 120 || done.FinishedAt == nil || done.ErrorMessage == "" {
		t.Fatalf("unexpected finished run: %+v", done)
	}
	if _, err := testStore.FinishSyncRun(ctx, u.ID, run.ID, domain.SyncRunning, 0, ""); err == nil {
		t.Fatal("finishing with non-terminal status must fail")
	}
	if _, err := testStore.FinishSyncRun(ctx, u.ID, run.ID, domain.SyncSucceeded, 0, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("finishing twice: want ErrNotFound, got %v", err)
	}
	runs, err := testStore.ListSyncRuns(ctx, u.ID, mock, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("unexpected runs: %+v", runs)
	}
}

func TestConnections_DueForSyncListsOnlyActiveDue(t *testing.T) {
	ctx := ctxT(t)
	a, b := newUser(t), newUser(t)
	ca, err := testStore.CreateConnection(ctx, a.ID, mock, []string{"activity"})
	if err != nil {
		t.Fatal(err)
	}
	cb, err := testStore.CreateConnection(ctx, b.ID, mock, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := testStore.SetConnectionStatus(ctx, b.ID, cb.ID, domain.ConnectionRevoked); err != nil {
		t.Fatal(err)
	}

	due := dueIDs(t, time.Now().Add(time.Hour)) // generous margin for host/container clock skew
	if !due[ca.ID] || due[cb.ID] {
		t.Fatalf("want active connection due and revoked excluded; due=%v", due)
	}

	// After a sync, the connection is not due until next_sync_at.
	now := time.Now()
	if err := testStore.MarkConnectionSynced(ctx, a.ID, ca.ID, now, now.Add(24*time.Hour), []byte(`{"last_date":"2026-11-20"}`)); err != nil {
		t.Fatal(err)
	}
	if dueIDs(t, now.Add(12*time.Hour))[ca.ID] {
		t.Fatal("connection must not be due before next_sync_at")
	}
	cursor, err := testStore.GetConnectionSyncCursor(ctx, a.ID, ca.ID)
	if err != nil || !strings.Contains(string(cursor), "2026-11-20") {
		t.Fatalf("owner must get the stored cursor: %v %s", err, cursor)
	}
	if _, err := testStore.GetConnectionSyncCursor(ctx, b.ID, ca.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another user must get ErrNotFound, got %v", err)
	}
	info, err := testStore.GetConnection(ctx, a.ID, mock)
	if err != nil {
		t.Fatal(err)
	}
	if info.LastSyncedAt == nil || info.Scopes[0] != "activity" {
		t.Fatalf("unexpected connection: %+v", info)
	}
}

func dueIDs(t *testing.T, now time.Time) map[uuid.UUID]bool {
	t.Helper()
	due, err := testStore.ListConnectionsDueForSync(ctxT(t), now, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	m := make(map[uuid.UUID]bool, len(due))
	for _, d := range due {
		m[d.ID] = true
	}
	return m
}

// ---------------------------------------------------------------------------
// multi-tenant isolation (AGENTS.md §1.1)
// ---------------------------------------------------------------------------

func TestTenantIsolation_UserCannotReadOrModifyAnotherUsersData(t *testing.T) {
	ctx := ctxT(t)
	alice, bob := newUser(t), newUser(t)
	day := mustDate(t, "2026-11-21")
	dr := domain.DateRange{Start: day.AddDays(-2), End: day}

	if _, err := testStore.UpsertDailySummaries(ctx, alice.ID, []domain.DailySummary{{Provider: mock, Date: day, Steps: ptr[int32](1)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := testStore.UpsertIntradayMetrics(ctx, alice.ID, mock, domain.MetricHeartRate, []domain.IntradayPoint{{RecordedAt: t0, Value: 60}}); err != nil {
		t.Fatal(err)
	}
	s := night(t, "alice-night", t0, domain.SleepStageDeep)
	if _, err := testStore.UpsertSleepSessions(ctx, alice.ID, []domain.SleepSession{s}); err != nil {
		t.Fatal(err)
	}
	run, err := testStore.StartSyncRun(ctx, alice.ID, mock, domain.SyncTriggerScheduled, dr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := testStore.CreateConnection(ctx, alice.ID, mock, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Reads as Bob see nothing.
	// (Errors are checked so a failing query cannot masquerade as "no rows".)
	daily, err := testStore.ListDailySummaries(ctx, bob.ID, mock, dr)
	if err != nil || len(daily) != 0 {
		t.Errorf("bob sees alice's daily summaries: %v %+v", err, daily)
	}
	points, err := testStore.ListIntradayMetrics(ctx, bob.ID, mock, domain.MetricHeartRate, domain.TimeRange{Start: t0, End: t0.Add(time.Hour)})
	if err != nil || len(points) != 0 {
		t.Errorf("bob sees alice's intraday metrics: %v %+v", err, points)
	}
	sleeps, err := testStore.ListSleepSessions(ctx, bob.ID, mock, domain.DateRange{Start: s.SleepDate, End: s.SleepDate})
	if err != nil || len(sleeps) != 0 {
		t.Errorf("bob sees alice's sleep: %v %+v", err, sleeps)
	}
	runs, err := testStore.ListSyncRuns(ctx, bob.ID, mock, 10)
	if err != nil || len(runs) != 0 {
		t.Errorf("bob sees alice's sync runs: %v %+v", err, runs)
	}
	if _, err := testStore.GetConnection(ctx, bob.ID, mock); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob sees a connection: %v", err)
	}

	// Writes as Bob against Alice's IDs have no effect.
	if _, err := testStore.FinishSyncRun(ctx, bob.ID, run.ID, domain.SyncFailed, 0, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob finished alice's sync run: %v", err)
	}
	if err := testStore.SetConnectionStatus(ctx, bob.ID, conn.ID, domain.ConnectionRevoked); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob revoking alice's connection: want ErrNotFound, got %v", err)
	}
	if err := testStore.MarkConnectionSynced(ctx, bob.ID, conn.ID, time.Now(), time.Now().Add(time.Hour), nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob marking alice's connection synced: want ErrNotFound, got %v", err)
	}
	info, err := testStore.GetConnection(ctx, alice.ID, mock)
	if err != nil || info.Status != domain.ConnectionActive || info.LastSyncedAt != nil {
		t.Errorf("bob modified alice's connection: %v %+v", err, info)
	}

	// The schema itself refuses a stage that points at another user's session.
	got, err := testStore.ListSleepSessions(ctx, alice.ID, mock, domain.DateRange{Start: s.SleepDate, End: s.SleepDate})
	if err != nil || len(got) != 1 {
		t.Fatalf("listing alice's sleep: %v %+v", err, got)
	}
	_, err = testPool.Exec(ctx,
		`INSERT INTO sleep_stages (session_id, user_id, stage, start_at, end_at) VALUES ($1, $2, 'rem', $3, $4)`,
		got[0].ID, bob.ID, t0.Add(time.Hour), t0.Add(2*time.Hour))
	if err == nil {
		t.Error("composite FK must reject a sleep stage owned by a different user than its session")
	}
}

func TestDeletingUserCascadesToAllData(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	if _, err := testStore.UpsertDailySummaries(ctx, u.ID, []domain.DailySummary{{Provider: mock, Date: mustDate(t, "2026-11-20")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := testStore.UpsertIntradayMetrics(ctx, u.ID, mock, domain.MetricSteps, []domain.IntradayPoint{{RecordedAt: t0, Value: 5}}); err != nil {
		t.Fatal(err)
	}
	if _, err := testStore.UpsertSleepSessions(ctx, u.ID, []domain.SleepSession{night(t, "n", t0, domain.SleepStageLight)}); err != nil {
		t.Fatal(err)
	}
	if _, err := testStore.CreateConnection(ctx, u.ID, mock, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := testStore.StartSyncRun(ctx, u.ID, mock, domain.SyncTriggerManual, domain.DateRange{Start: mustDate(t, "2026-11-20"), End: mustDate(t, "2026-11-20")}); err != nil {
		t.Fatal(err)
	}

	if _, err := testPool.Exec(ctx, "DELETE FROM users WHERE id=$1", u.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"daily_summaries", "intraday_metrics", "sleep_sessions", "sleep_stages", "provider_connections", "sync_runs"} {
		if n := countRows(t, "SELECT count(*) FROM "+table+" WHERE user_id=$1", u.ID); n != 0 {
			t.Errorf("%s: %d rows survived user deletion", table, n)
		}
	}
}
