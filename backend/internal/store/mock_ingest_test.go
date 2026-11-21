package store_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	mockprovider "github.com/aljazkovac/pulse-pipeline/backend/internal/provider/mock"
)

// The mock provider's output must satisfy every schema constraint, survive a
// round trip unchanged, and converge when ingested twice (the nightly sync's
// idempotency contract, exercised here without the worker).
func TestMockProviderData_IngestsIdempotentlyAndRoundTrips(t *testing.T) {
	ctx := ctxT(t)
	u := newUser(t)
	now := time.Date(2026, 11, 21, 9, 30, 0, 0, time.UTC)
	p := mockprovider.New(mockprovider.Options{Seed: 2026, Now: func() time.Time { return now }})
	conn := domain.Connection{UserID: u.ID, Provider: p.Name(), Location: u.Location()}
	days := domain.DateRange{Start: mustDate(t, "2026-11-08"), End: mustDate(t, "2026-11-21")}
	span := domain.TimeRange{
		Start: time.Date(2026, 11, 8, 0, 0, 0, 0, conn.Location),
		End:   time.Date(2026, 11, 22, 0, 0, 0, 0, conn.Location),
	}

	summaries, err := p.FetchDailySummaries(ctx, conn, days)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := p.FetchSleepSessions(ctx, conn, days)
	if err != nil {
		t.Fatal(err)
	}
	points := map[domain.MetricType][]domain.IntradayPoint{}
	for _, m := range domain.MetricTypes {
		if points[m], err = p.FetchIntradayMetrics(ctx, conn, m, span); err != nil {
			t.Fatal(err)
		}
	}

	ingest := func() (written int64) {
		t.Helper()
		if _, err := testStore.UpsertDailySummaries(ctx, u.ID, summaries); err != nil {
			t.Fatalf("UpsertDailySummaries: %v", err)
		}
		if _, err := testStore.UpsertSleepSessions(ctx, u.ID, sessions); err != nil {
			t.Fatalf("UpsertSleepSessions: %v", err)
		}
		for _, m := range domain.MetricTypes {
			n, err := testStore.UpsertIntradayMetrics(ctx, u.ID, p.Name(), m, points[m])
			if err != nil {
				t.Fatalf("UpsertIntradayMetrics(%s): %v", m, err)
			}
			written += n
		}
		return written
	}
	counts := func() [4]int {
		return [4]int{
			countRows(t, `SELECT count(*) FROM daily_summaries WHERE user_id = $1`, u.ID),
			countRows(t, `SELECT count(*) FROM sleep_sessions WHERE user_id = $1`, u.ID),
			countRows(t, `SELECT count(*) FROM sleep_stages WHERE user_id = $1`, u.ID),
			countRows(t, `SELECT count(*) FROM intraday_metrics WHERE user_id = $1`, u.ID),
		}
	}

	var total int
	for _, pts := range points {
		total += len(pts)
	}
	if first := ingest(); first != int64(total) {
		t.Fatalf("first ingest wrote %d intraday rows, want %d", first, total)
	}
	before := counts()
	if second := ingest(); second != 0 {
		t.Errorf("re-ingest changed %d intraday rows, want 0", second)
	}
	if after := counts(); after != before {
		t.Errorf("row counts changed on re-ingest: %v → %v", before, after)
	}

	stored, err := testStore.ListDailySummaries(ctx, u.ID, p.Name(), days)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(stored) // newest first → chronological
	if !reflect.DeepEqual(stored, summaries) {
		t.Error("daily summaries changed in the round trip")
	}

	storedSessions, err := testStore.ListSleepSessions(ctx, u.ID, p.Name(), days)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(storedSessions)
	if len(storedSessions) != len(sessions) {
		t.Fatalf("stored %d sleep sessions, fetched %d", len(storedSessions), len(sessions))
	}
	for i := range storedSessions {
		if storedSessions[i].ID == uuid.Nil {
			t.Errorf("stored session %s has no ID", storedSessions[i].ProviderSessionID)
		}
		storedSessions[i].ID = uuid.Nil // assigned by the store; fetched sessions have none
	}
	// Main sleep and nap share a wake-up date; order within a date is not part of the contract.
	key := func(a, b domain.SleepSession) int { return a.StartAt.Compare(b.StartAt) }
	slices.SortFunc(storedSessions, key)
	slices.SortFunc(sessions, key)
	if !reflect.DeepEqual(storedSessions, sessions) {
		t.Error("sleep sessions changed in the round trip")
	}

	for _, m := range domain.MetricTypes {
		stored, err := testStore.ListIntradayMetrics(ctx, u.ID, p.Name(), m, span)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(stored, points[m]) {
			t.Errorf("%s samples changed in the round trip", m)
		}
	}
}
