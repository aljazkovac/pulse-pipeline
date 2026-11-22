package googlehealth_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/credentials"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/sync"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/testdb"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/tokencrypt"
)

var (
	testPool  *pgxpool.Pool
	testStore *store.Store
)

const fakeHealthUserID = "fake-health-user"

// One PostgreSQL container for the package's integration tests; the unit
// tests in this package do not touch it.
func TestMain(m *testing.M) {
	os.Exit(testdb.Main(m, func(_ string, pool *pgxpool.Pool) {
		testPool, testStore = pool, store.New(pool)
	}))
}

// serveDataset answers every endpoint a sync calls with a small fixed
// dataset around 2026-11-20 (Stockholm), the same way on every call.
func serveDataset(t *testing.T, f *fakeGoogle) {
	serveDailyRollUp(t, f, "steps", "steps", func(domain.Date) map[string]any { return map[string]any{"countSum": "8430"} })
	serveDailyRollUp(t, f, "total-calories", "totalCalories", func(domain.Date) map[string]any { return map[string]any{"kcalSum": 2210.4} })
	serveDaily(f, "daily-resting-heart-rate", map[string]any{"dailyRestingHeartRate": map[string]any{"date": civil(date(t, "2026-11-20")), "beatsPerMinute": "54"}})
	serveDaily(f, "daily-heart-rate-variability", map[string]any{"dailyHeartRateVariability": map[string]any{"date": civil(date(t, "2026-11-20")), "averageHeartRateVariabilityMilliseconds": 44.0}})
	serveDaily(f, "daily-oxygen-saturation", map[string]any{"dailyOxygenSaturation": map[string]any{"date": civil(date(t, "2026-11-20")), "averagePercentage": 96.0}})
	hr := func(at, bpm string) any {
		return map[string]any{"heartRate": map[string]any{"sampleTime": map[string]any{"physicalTime": at}, "beatsPerMinute": bpm}}
	}
	serveDaily(f, "heart-rate", hr("2026-11-20T08:00:00Z", "61"), hr("2026-11-20T08:00:05Z", "62"))
	serveDaily(f, "heart-rate-variability", map[string]any{"heartRateVariability": map[string]any{
		"sampleTime": map[string]any{"physicalTime": "2026-11-20T02:00:00Z"}, "rootMeanSquareOfSuccessiveDifferencesMilliseconds": 44.0}})
	serveDaily(f, "oxygen-saturation", map[string]any{"oxygenSaturation": map[string]any{
		"sampleTime": map[string]any{"physicalTime": "2026-11-20T02:00:00Z"}, "percentage": 96.0}})
	serveDaily(f, "steps", map[string]any{"steps": map[string]any{
		"interval": map[string]any{"startTime": "2026-11-20T08:00:00Z", "endTime": "2026-11-20T08:01:00Z"}, "count": "40"}})
	f.handle("POST /users/me/dataTypes/total-calories/dataPoints:rollUp", func(w http.ResponseWriter, _ call) {
		writeJSON(w, http.StatusOK, map[string]any{"rollupDataPoints": []any{
			map[string]any{"startTime": "2026-11-20T08:00:00Z", "totalCalories": map[string]any{"kcalSum": 1.5}},
		}})
	})
	f.handle("GET /users/me/dataTypes/sleep/dataPoints:reconcile", func(w http.ResponseWriter, _ call) {
		writeJSON(w, http.StatusOK, page([]any{sleepPoint("night-1", "2026-11-19T22:00:00Z", "2026-11-20T05:30:00Z", true,
			[3]string{"2026-11-19T22:00:00Z", "2026-11-19T23:00:00Z", "LIGHT"},
			[3]string{"2026-11-19T23:00:00Z", "2026-11-20T00:30:00Z", "DEEP"},
		)}, ""))
	})
}

type integration struct {
	t      *testing.T
	ctx    context.Context
	fake   *fakeGoogle
	user   domain.User
	conn   uuid.UUID
	vault  *credentials.Vault
	syncer *sync.Syncer
}

func newIntegration(t *testing.T, creds domain.Credentials) *integration {
	t.Helper()
	testdb.Require(t, testPool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	in := &integration{t: t, ctx: ctx, fake: newFake(t)}
	var err error
	in.user, err = testStore.CreateUser(ctx, fmt.Sprintf("google-%s@example.com", uuid.NewString()), "Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	kr, err := tokencrypt.NewKeyring(1, map[int16][]byte{1: bytes.Repeat([]byte{0x47}, tokencrypt.KeySize)})
	if err != nil {
		t.Fatal(err)
	}
	in.vault = credentials.NewVault(testStore, kr)
	if in.conn, err = in.vault.Link(ctx, in.user.ID, domain.ProviderGoogleHealth, fakeHealthUserID, nil, creds); err != nil {
		t.Fatal(err)
	}

	cfg := sync.DefaultConfig()
	cfg.InitialBackfillDays = 3
	cfg.Credentials = in.vault
	cfg.Now = func() time.Time { return t0 }
	cfg.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if in.syncer, err = sync.New(testStore, []domain.HealthProvider{in.fake.provider(t)}, cfg); err != nil {
		t.Fatal(err)
	}
	return in
}

// sync runs a manual sync, or a backfill of r when r is non-nil.
func (in *integration) sync(r ...domain.DateRange) domain.SyncRun {
	in.t.Helper()
	req := sync.Request{UserID: in.user.ID, Provider: domain.ProviderGoogleHealth, Trigger: domain.SyncTriggerManual}
	if len(r) == 1 {
		req.Trigger, req.Range = domain.SyncTriggerBackfill, &r[0]
	}
	run, err := in.syncer.SyncConnection(in.ctx, req)
	if err != nil {
		in.t.Fatal(err)
	}
	return run
}

func (in *integration) count(sql string) int {
	in.t.Helper()
	var n int
	if err := testPool.QueryRow(in.ctx, sql, in.user.ID).Scan(&n); err != nil {
		in.t.Fatal(err)
	}
	return n
}

// dataset is everything stored for the user in r, for convergence checks.
type dataset struct {
	Daily    []domain.DailySummary
	Sleep    []domain.SleepSession
	Intraday map[domain.MetricType][]domain.IntradayPoint
	Sessions int
}

func (in *integration) snapshot(r domain.DateRange) dataset {
	in.t.Helper()
	gh := domain.ProviderGoogleHealth
	var d dataset
	var err error
	if d.Daily, err = testStore.ListDailySummaries(in.ctx, in.user.ID, gh, r); err != nil {
		in.t.Fatal(err)
	}
	if d.Sleep, err = testStore.ListSleepSessions(in.ctx, in.user.ID, gh, r); err != nil {
		in.t.Fatal(err)
	}
	d.Intraday = map[domain.MetricType][]domain.IntradayPoint{}
	span := domain.TimeRange{Start: r.Start.AddDays(-2).Time(), End: r.End.AddDays(2).Time()}
	for _, m := range domain.MetricTypes {
		if d.Intraday[m], err = testStore.ListIntradayMetrics(in.ctx, in.user.ID, gh, m, span); err != nil {
			in.t.Fatal(err)
		}
	}
	d.Sessions = in.count(`SELECT count(*) FROM sleep_sessions WHERE user_id = $1`)
	return d
}

func TestSync_GoogleHealthEndToEnd(t *testing.T) {
	// The stored access token is about to expire: the run must refresh it,
	// persist the new tokens encrypted, and fetch with the new one.
	in := newIntegration(t, domain.Credentials{AccessToken: "fake-access-old", RefreshToken: fakeRefresh, ExpiresAt: t0.Add(time.Minute)})
	in.fake.setAccess("fake-access-refreshed")
	in.fake.token = func(w http.ResponseWriter, form url.Values) {
		if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != fakeRefresh {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"access_token": "fake-access-refreshed", "expires_in": 3599})
	}
	serveDataset(t, in.fake)

	run := in.sync()
	if run.Status != domain.SyncSucceeded || run.Range.Start != date(t, "2026-11-19") || run.Range.End != date(t, "2026-11-21") {
		t.Fatalf("run = %+v", run)
	}
	creds, err := in.vault.LoadCredentials(in.ctx, in.user.ID, in.conn)
	if err != nil || creds.AccessToken != "fake-access-refreshed" || creds.RefreshToken != fakeRefresh {
		t.Fatalf("stored credentials not the refreshed ones (err %v)", err)
	}
	var accessCT, refreshCT []byte
	if err := testPool.QueryRow(in.ctx, `SELECT access_token_ciphertext, refresh_token_ciphertext FROM provider_connections WHERE id = $1 AND user_id = $2`,
		in.conn, in.user.ID).Scan(&accessCT, &refreshCT); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(accessCT, []byte("fake-access")) || bytes.Contains(refreshCT, []byte(fakeRefresh)) {
		t.Fatal("plaintext token in the database")
	}

	daily, err := testStore.ListDailySummaries(in.ctx, in.user.ID, domain.ProviderGoogleHealth, run.Range)
	if err != nil || len(daily) != 3 {
		t.Fatalf("daily summaries = %d (%v), want one per day of the range", len(daily), err)
	}
	for _, d := range daily {
		if d.Steps == nil || *d.Steps != 8430 || d.CaloriesKcal == nil || *d.CaloriesKcal != 2210 {
			t.Errorf("%s: steps/calories not mapped: %+v", d.Date, d)
		}
		if d.Date == date(t, "2026-11-20") && (d.RestingHeartRate == nil || *d.RestingHeartRate != 54 || d.HRVRMSSDMs == nil || d.SpO2AvgPct == nil) {
			t.Errorf("2026-11-20 daily metrics missing: %+v", d)
		}
	}
	wantIntraday := map[domain.MetricType]int{domain.MetricHeartRate: 2, domain.MetricSteps: 1, domain.MetricHRVRMSSD: 1, domain.MetricSpO2: 1, domain.MetricCalories: 1}
	for m, want := range wantIntraday {
		var n int
		if err := testPool.QueryRow(in.ctx, `SELECT count(*) FROM intraday_metrics WHERE user_id = $1 AND provider = 'google_health' AND metric_type = $2`, in.user.ID, m).Scan(&n); err != nil || n != want {
			t.Errorf("%s rows = %d (%v), want %d", m, n, err, want)
		}
	}
	sleeps, err := testStore.ListSleepSessions(in.ctx, in.user.ID, domain.ProviderGoogleHealth, run.Range)
	if err != nil || len(sleeps) != 1 || sleeps[0].ProviderSessionID != "night-1" || len(sleeps[0].Stages) != 2 || sleeps[0].SleepDate != date(t, "2026-11-20") {
		t.Fatalf("sleep = %+v (%v)", sleeps, err)
	}

	// Re-running the same range converges: identical rows, no duplicates,
	// no further refresh.
	before := in.snapshot(run.Range)
	refreshes := len(in.fake.callsTo("POST /token"))
	if again := in.sync(run.Range); again.Status != domain.SyncSucceeded {
		t.Fatalf("second run = %+v", again)
	}
	if after := in.snapshot(run.Range); !reflect.DeepEqual(before, after) {
		t.Errorf("re-running the same range changed the data:\nbefore %+v\nafter  %+v", before, after)
	}
	if n := len(in.fake.callsTo("POST /token")); n != refreshes {
		t.Errorf("refreshed again although the token is valid for an hour (%d → %d)", refreshes, n)
	}
}

func TestSync_RevokedGrantDisablesConnection(t *testing.T) {
	in := newIntegration(t, domain.Credentials{AccessToken: "fake-access-old", RefreshToken: fakeRefresh, ExpiresAt: t0.Add(time.Minute)})
	in.fake.token = func(w http.ResponseWriter, _ url.Values) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "Token has been expired or revoked."})
	}
	run := in.sync()
	if run.Status != domain.SyncFailed {
		t.Fatalf("run = %+v, want failed", run)
	}
	info, err := testStore.GetConnection(in.ctx, in.user.ID, domain.ProviderGoogleHealth)
	if err != nil || info.Status != domain.ConnectionError {
		t.Fatalf("connection = %+v (%v), want status error until re-linked", info, err)
	}
	if bytes.Contains([]byte(run.ErrorMessage), []byte(fakeRefresh)) || bytes.Contains([]byte(run.ErrorMessage), []byte("fake-access-old")) {
		t.Errorf("run error leaks a token: %q", run.ErrorMessage)
	}

	// Re-linking makes it active again.
	if _, err := in.vault.Link(in.ctx, in.user.ID, domain.ProviderGoogleHealth, fakeHealthUserID, nil,
		domain.Credentials{AccessToken: fakeAccess, RefreshToken: "fake-refresh-new", ExpiresAt: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if info, err := testStore.GetConnection(in.ctx, in.user.ID, domain.ProviderGoogleHealth); err != nil || info.Status != domain.ConnectionActive {
		t.Fatalf("after re-link: %+v (%v)", info, err)
	}
}
