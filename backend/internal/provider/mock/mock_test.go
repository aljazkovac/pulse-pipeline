package mock_test

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/provider/mock"
)

// Fictional identities only.
var (
	userA = uuid.MustParse("00000000-0000-4000-8000-00000000000a")
	userB = uuid.MustParse("00000000-0000-4000-8000-00000000000b")
	now   = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
)

func newProvider(seed uint64) *mock.Provider {
	return mock.New(mock.Options{Seed: seed, Now: func() time.Time { return now }})
}

func loc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func conn(t *testing.T, user uuid.UUID, tz string) domain.Connection {
	t.Helper()
	return domain.Connection{ID: uuid.New(), UserID: user, Provider: domain.ProviderMock, Location: loc(t, tz)}
}

func date(t *testing.T, s string) domain.Date {
	t.Helper()
	d, err := domain.ParseDate(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func dayRange(c domain.Connection, d domain.Date) domain.TimeRange {
	n := d.AddDays(1)
	return domain.TimeRange{
		Start: time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, c.Location),
		End:   time.Date(n.Year, n.Month, n.Day, 0, 0, 0, 0, c.Location),
	}
}

func summaries(t *testing.T, p *mock.Provider, c domain.Connection, start, end domain.Date) []domain.DailySummary {
	t.Helper()
	out, err := p.FetchDailySummaries(context.Background(), c, domain.DateRange{Start: start, End: end})
	if err != nil {
		t.Fatalf("FetchDailySummaries: %v", err)
	}
	return out
}

func sleeps(t *testing.T, p *mock.Provider, c domain.Connection, start, end domain.Date) []domain.SleepSession {
	t.Helper()
	out, err := p.FetchSleepSessions(context.Background(), c, domain.DateRange{Start: start, End: end})
	if err != nil {
		t.Fatalf("FetchSleepSessions: %v", err)
	}
	return out
}

func intraday(t *testing.T, p *mock.Provider, c domain.Connection, m domain.MetricType, r domain.TimeRange) []domain.IntradayPoint {
	t.Helper()
	out, err := p.FetchIntradayMetrics(context.Background(), c, m, r)
	if err != nil {
		t.Fatalf("FetchIntradayMetrics(%s): %v", m, err)
	}
	return out
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func TestName(t *testing.T) {
	if got := newProvider(1).Name(); got != domain.ProviderMock {
		t.Fatalf("Name() = %q, want %q", got, domain.ProviderMock)
	}
}

func TestDeterministicPerSeedAndUser(t *testing.T) {
	c := conn(t, userA, "Europe/Stockholm")
	start, end := date(t, "2026-09-01"), date(t, "2026-09-30")
	hr := domain.TimeRange{Start: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)}

	a, b := newProvider(7), newProvider(7)
	if !reflect.DeepEqual(summaries(t, a, c, start, end), summaries(t, b, c, start, end)) {
		t.Error("same seed and user: daily summaries differ")
	}
	if !reflect.DeepEqual(sleeps(t, a, c, start, end), sleeps(t, b, c, start, end)) {
		t.Error("same seed and user: sleep sessions differ")
	}
	if !reflect.DeepEqual(intraday(t, a, c, domain.MetricHeartRate, hr), intraday(t, b, c, domain.MetricHeartRate, hr)) {
		t.Error("same seed and user: heart rate differs")
	}

	if reflect.DeepEqual(summaries(t, a, c, start, end), summaries(t, newProvider(8), c, start, end)) {
		t.Error("different seeds produced identical summaries")
	}
	other := conn(t, userB, "Europe/Stockholm")
	if reflect.DeepEqual(summaries(t, a, c, start, end), summaries(t, a, other, start, end)) {
		t.Error("different users produced identical summaries")
	}
}

// A sync that fetches day by day must see exactly what a backfill fetching
// a whole month sees.
func TestResultsIndependentOfRequestChunking(t *testing.T) {
	p := newProvider(42)
	c := conn(t, userA, "Europe/Stockholm")
	start, end := date(t, "2026-08-01"), date(t, "2026-09-29")

	var chunkedSummaries []domain.DailySummary
	var chunkedSleeps []domain.SleepSession
	for d := start; !end.Before(d); d = d.AddDays(7) {
		e := d.AddDays(6)
		if end.Before(e) {
			e = end
		}
		chunkedSummaries = append(chunkedSummaries, summaries(t, p, c, d, e)...)
		chunkedSleeps = append(chunkedSleeps, sleeps(t, p, c, d, e)...)
	}
	if !reflect.DeepEqual(summaries(t, p, c, start, end), chunkedSummaries) {
		t.Error("daily summaries depend on request chunking")
	}
	if !reflect.DeepEqual(sleeps(t, p, c, start, end), chunkedSleeps) {
		t.Error("sleep sessions depend on request chunking")
	}

	whole := domain.TimeRange{Start: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)}
	cut := time.Date(2026, 9, 2, 3, 17, 30, 0, time.UTC) // mid-night, mid-minute
	for _, m := range domain.MetricTypes {
		got := append(
			intraday(t, p, c, m, domain.TimeRange{Start: whole.Start, End: cut}),
			intraday(t, p, c, m, domain.TimeRange{Start: cut, End: whole.End})...)
		if !reflect.DeepEqual(intraday(t, p, c, m, whole), got) {
			t.Errorf("%s samples depend on request chunking", m)
		}
	}
}

func TestDailySummariesAggregateIntradayInUserTimezone(t *testing.T) {
	for _, tz := range []string{"Europe/Stockholm", "UTC", "America/Los_Angeles", "Asia/Kolkata"} {
		t.Run(tz, func(t *testing.T) {
			p := newProvider(3)
			c := conn(t, userA, tz)
			start, end := date(t, "2026-10-15"), date(t, "2026-11-05") // spans EU and US DST ends
			nights := map[domain.Date]domain.SleepSession{}
			for _, s := range sleeps(t, p, c, start, end) {
				if s.IsMainSleep {
					nights[s.SleepDate] = s
				}
			}
			got := summaries(t, p, c, start, end)
			if len(got) == 0 {
				t.Fatal("no summaries")
			}
			for _, s := range got {
				day := dayRange(c, s.Date)
				var steps, kcal float64
				for _, pt := range intraday(t, p, c, domain.MetricSteps, day) {
					steps += pt.Value
				}
				for _, pt := range intraday(t, p, c, domain.MetricCalories, day) {
					kcal += pt.Value
				}
				if s.Steps == nil || float64(*s.Steps) != steps {
					t.Errorf("%s: steps = %v, intraday sum = %v", s.Date, s.Steps, steps)
				}
				if s.CaloriesKcal == nil || float64(*s.CaloriesKcal) != math.Round(kcal) {
					t.Errorf("%s: calories = %v, intraday sum = %v", s.Date, s.CaloriesKcal, kcal)
				}

				night, ok := nights[s.Date]
				if ok != (s.SleepScore != nil) {
					t.Errorf("%s: has main sleep = %v but sleep score = %v", s.Date, ok, s.SleepScore)
				}
				if !ok {
					if s.HRVRMSSDMs != nil || s.SpO2AvgPct != nil {
						t.Errorf("%s: HRV/SpO2 reported without a main sleep", s.Date)
					}
					continue
				}
				sessionRange := domain.TimeRange{Start: night.StartAt, End: night.EndAt}
				for _, tc := range []struct {
					metric domain.MetricType
					got    *float64
				}{{domain.MetricHRVRMSSD, s.HRVRMSSDMs}, {domain.MetricSpO2, s.SpO2AvgPct}} {
					pts := intraday(t, p, c, tc.metric, sessionRange)
					if len(pts) == 0 {
						if tc.got != nil {
							t.Errorf("%s: %s = %v without samples", s.Date, tc.metric, *tc.got)
						}
						continue
					}
					var sum float64
					for _, pt := range pts {
						sum += pt.Value
					}
					if tc.got == nil || *tc.got != round1(sum/float64(len(pts))) {
						t.Errorf("%s: %s = %v, sample mean = %v", s.Date, tc.metric, tc.got, round1(sum/float64(len(pts))))
					}
				}
			}
		})
	}
}

// Values must satisfy the schema's CHECK constraints and look like a real
// person's year, including the gaps real wearables have.
func TestYearOfDataIsPlausible(t *testing.T) {
	p := newProvider(11)
	c := conn(t, userA, "Europe/Stockholm")
	start, end := date(t, "2025-11-21"), date(t, "2026-11-20")
	days := 0
	for d := start; !end.Before(d); d = d.AddDays(1) {
		days++
	}

	got := summaries(t, p, c, start, end)
	var missingHRV, missingSpO2, missingSleep int
	var totalSteps float64
	for _, s := range got {
		if s.Provider != domain.ProviderMock {
			t.Fatalf("%s: provider = %q", s.Date, s.Provider)
		}
		if s.Steps == nil || *s.Steps < 0 || *s.Steps > 40000 {
			t.Errorf("%s: steps = %v", s.Date, s.Steps)
		} else {
			totalSteps += float64(*s.Steps)
		}
		if s.RestingHeartRate == nil || *s.RestingHeartRate < 40 || *s.RestingHeartRate > 95 {
			t.Errorf("%s: resting HR = %v", s.Date, s.RestingHeartRate)
		}
		if s.CaloriesKcal == nil || *s.CaloriesKcal < 0 || *s.CaloriesKcal > 6000 {
			t.Errorf("%s: calories = %v", s.Date, s.CaloriesKcal)
		}
		if s.SleepScore == nil {
			missingSleep++
		} else if *s.SleepScore < 0 || *s.SleepScore > 100 {
			t.Errorf("%s: sleep score = %d", s.Date, *s.SleepScore)
		}
		if s.HRVRMSSDMs == nil {
			missingHRV++
		} else if *s.HRVRMSSDMs < 5 || *s.HRVRMSSDMs > 250 {
			t.Errorf("%s: HRV = %v", s.Date, *s.HRVRMSSDMs)
		}
		if s.SpO2AvgPct == nil {
			missingSpO2++
		} else if *s.SpO2AvgPct < 88 || *s.SpO2AvgPct > 100 {
			t.Errorf("%s: SpO2 = %v", s.Date, *s.SpO2AvgPct)
		}
	}

	if len(got) >= days || len(got) < days*9/10 {
		t.Errorf("%d of %d days have data; want a few off-wrist days", len(got), days)
	}
	if missingSleep == 0 || missingHRV <= missingSleep || missingSpO2 <= missingSleep {
		t.Errorf("want some days without sleep, HRV and SpO2; got %d, %d, %d", missingSleep, missingHRV, missingSpO2)
	}
	if avg := totalSteps / float64(len(got)); avg < 5000 || avg > 15000 {
		t.Errorf("average steps = %.0f, want 5k–15k", avg)
	}

	week := domain.TimeRange{Start: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)}
	for _, tc := range []struct {
		metric domain.MetricType
		lo, hi float64
	}{
		{domain.MetricHeartRate, 35, 200},
		{domain.MetricSteps, 0, 220},
		{domain.MetricCalories, 0, 20},
		{domain.MetricHRVRMSSD, 5, 250},
		{domain.MetricSpO2, 88, 100},
	} {
		pts := intraday(t, p, c, tc.metric, week)
		if len(pts) == 0 {
			t.Errorf("%s: no samples in a week", tc.metric)
		}
		for i, pt := range pts {
			if pt.Metric != tc.metric || pt.Value < tc.lo || pt.Value > tc.hi {
				t.Fatalf("%s sample %d = %+v, want value in [%v, %v]", tc.metric, i, pt, tc.lo, tc.hi)
			}
			if pt.RecordedAt.Location() != time.UTC {
				t.Fatalf("%s sample %d not in UTC", tc.metric, i)
			}
			if i > 0 && !pt.RecordedAt.After(pts[i-1].RecordedAt) {
				t.Fatalf("%s samples not strictly increasing at %d", tc.metric, i)
			}
		}
	}
}

// Some worn days lose a contiguous block of minutes to charging; others are
// fully sampled.
func TestChargingGapsLeaveContiguousHoles(t *testing.T) {
	p := newProvider(11)
	c := conn(t, userA, "Europe/Stockholm")
	var full, gapped int
	for d := date(t, "2026-05-01"); d.Before(date(t, "2026-05-31")); d = d.AddDays(1) {
		day := dayRange(c, d)
		pts := intraday(t, p, c, domain.MetricHeartRate, day)
		if len(pts) == 0 {
			continue // off the wrist all day
		}
		want := int(day.End.Sub(day.Start).Minutes())
		if len(pts) == want {
			full++
			continue
		}
		gapped++
		var holes int
		for i := 1; i < len(pts); i++ {
			if gap := pts[i].RecordedAt.Sub(pts[i-1].RecordedAt); gap != time.Minute {
				holes++
				if local := pts[i-1].RecordedAt.In(c.Location); local.Hour() < 16 || gap > 3*time.Hour {
					t.Errorf("%s: %s hole after %s local, want one evening charging gap", d, gap, local.Format("15:04"))
				}
			}
		}
		if holes != 1 || pts[0].RecordedAt != day.Start.UTC() || !pts[len(pts)-1].RecordedAt.Equal(day.End.Add(-time.Minute)) {
			t.Errorf("%s: %d samples with %d holes, want exactly one interior gap", d, len(pts), holes)
		}
	}
	if full == 0 || gapped == 0 {
		t.Errorf("%d full and %d gapped days in a month; want both", full, gapped)
	}
}

func TestSleepSessionsAreWellFormed(t *testing.T) {
	p := newProvider(5)
	c := conn(t, userA, "Europe/Stockholm") // year includes both DST changes
	got := sleeps(t, p, c, date(t, "2025-11-21"), date(t, "2026-11-21"))

	ids := map[string]bool{}
	mains := map[domain.Date]int{}
	var naps int
	for _, s := range got {
		if ids[s.ProviderSessionID] {
			t.Errorf("duplicate session ID %q", s.ProviderSessionID)
		}
		ids[s.ProviderSessionID] = true
		if s.Provider != domain.ProviderMock || !strings.HasPrefix(s.ProviderSessionID, "mock-") {
			t.Errorf("%s: provider %q, id %q", s.SleepDate, s.Provider, s.ProviderSessionID)
		}
		if s.StartAt.Location() != time.UTC || s.EndAt.Location() != time.UTC {
			t.Errorf("%s: session times not UTC", s.ProviderSessionID)
		}
		if got := domain.DateOf(s.EndAt.In(c.Location)); got != s.SleepDate {
			t.Errorf("%s: sleep date %s, local wake-up date %s", s.ProviderSessionID, s.SleepDate, got)
		}
		if s.EndAt.After(now) {
			t.Errorf("%s: ends after now", s.ProviderSessionID)
		}
		if s.EfficiencyPct == nil || *s.EfficiencyPct < 0 || *s.EfficiencyPct > 100 {
			t.Errorf("%s: efficiency = %v", s.ProviderSessionID, s.EfficiencyPct)
		}
		if s.IsMainSleep {
			mains[s.SleepDate]++
			if d := s.Duration(); d < 5*time.Hour || d > 9*time.Hour+30*time.Minute {
				t.Errorf("%s: main sleep lasts %s", s.ProviderSessionID, d)
			}
		} else {
			naps++
		}

		if len(s.Stages) == 0 {
			t.Fatalf("%s: no stages", s.ProviderSessionID)
		}
		cursor := s.StartAt
		for i, st := range s.Stages {
			if !st.Stage.Valid() {
				t.Errorf("%s stage %d: invalid type %q", s.ProviderSessionID, i, st.Stage)
			}
			if !st.StartAt.Equal(cursor) || !st.EndAt.After(st.StartAt) {
				t.Errorf("%s stage %d: [%s, %s) not contiguous after %s", s.ProviderSessionID, i, st.StartAt, st.EndAt, cursor)
			}
			if i > 0 && st.Stage == s.Stages[i-1].Stage {
				t.Errorf("%s stage %d: repeats %s unmerged", s.ProviderSessionID, i, st.Stage)
			}
			cursor = st.EndAt
		}
		if !cursor.Equal(s.EndAt) {
			t.Errorf("%s: stages end at %s, session at %s", s.ProviderSessionID, cursor, s.EndAt)
		}
	}
	for d, n := range mains {
		if n != 1 {
			t.Errorf("%s: %d main sleeps", d, n)
		}
	}
	if len(mains) < 330 || naps == 0 {
		t.Errorf("%d nights and %d naps in a year; want most nights and some naps", len(mains), naps)
	}
}

func TestDSTDaysHaveLocalLength(t *testing.T) {
	p := newProvider(1)
	c := conn(t, userA, "Europe/Stockholm")
	for _, tc := range []struct {
		day     string
		minutes int
	}{
		{"2026-03-29", 23 * 60}, // spring forward
		{"2026-10-25", 25 * 60}, // fall back: 02:00–03:00 happens twice
	} {
		t.Run(tc.day, func(t *testing.T) {
			d := date(t, tc.day)
			day := dayRange(c, d)
			if got := int(day.End.Sub(day.Start).Minutes()); got != tc.minutes {
				t.Fatalf("test setup: day lasts %d minutes", got)
			}
			pts := intraday(t, p, c, domain.MetricHeartRate, day)
			if len(pts) == 0 || len(pts) > tc.minutes {
				t.Fatalf("%d heart rate samples, want 1–%d", len(pts), tc.minutes)
			}
			seen := map[time.Time]bool{}
			for _, pt := range pts {
				if seen[pt.RecordedAt] || pt.RecordedAt.Second() != 0 || domain.DateOf(pt.RecordedAt.In(c.Location)) != d {
					t.Fatalf("bad sample time %s", pt.RecordedAt)
				}
				seen[pt.RecordedAt] = true
			}
			// Charging gaps end by 21:00, so the first and last elapsed hours
			// of a worn day are fully sampled; this catches a generator that
			// walks 1440 wall-clock minutes instead of the real day length.
			var first, last int
			for _, pt := range pts {
				if pt.RecordedAt.Before(day.Start.Add(time.Hour)) {
					first++
				}
				if !pt.RecordedAt.Before(day.End.Add(-time.Hour)) {
					last++
				}
			}
			if first != 60 || last != 60 {
				t.Errorf("%d samples in the first hour and %d in the last, want 60 each", first, last)
			}
		})
	}
}

func TestNoDataAfterNow(t *testing.T) {
	p := newProvider(9)
	c := conn(t, userA, "Europe/Stockholm")
	today := domain.DateOf(now.In(c.Location))

	got := summaries(t, p, c, today.AddDays(-3), today.AddDays(5))
	if last := got[len(got)-1].Date; last != today {
		t.Fatalf("last summary on %s, want today %s", last, today)
	}
	var steps float64
	for _, pt := range intraday(t, p, c, domain.MetricSteps, dayRange(c, today)) {
		steps += pt.Value
	}
	if float64(*got[len(got)-1].Steps) != steps {
		t.Errorf("today's partial steps = %d, intraday so far = %v", *got[len(got)-1].Steps, steps)
	}

	future := domain.TimeRange{Start: now.Add(-6 * time.Hour), End: now.Add(48 * time.Hour)}
	for _, m := range domain.MetricTypes {
		for _, pt := range intraday(t, p, c, m, future) {
			if pt.RecordedAt.After(now) {
				t.Fatalf("%s sample at %s after now", m, pt.RecordedAt)
			}
			if (m == domain.MetricHeartRate || m == domain.MetricSteps) && pt.RecordedAt.Add(time.Minute).After(now) {
				t.Fatalf("%s minute at %s not yet complete", m, pt.RecordedAt)
			}
		}
	}
	for _, s := range sleeps(t, p, c, today, today.AddDays(5)) {
		if s.EndAt.After(now) {
			t.Fatalf("session %s ends after now", s.ProviderSessionID)
		}
	}
	if got := summaries(t, p, c, today.AddDays(1), today.AddDays(5)); len(got) != 0 {
		t.Errorf("%d summaries for future days", len(got))
	}
}

func TestNightInProgressIsWithheld(t *testing.T) {
	c := conn(t, userA, "Europe/Stockholm")
	today := date(t, "2026-11-21")
	night := time.Date(2026, 11, 21, 3, 0, 0, 0, c.Location) // asleep (bedtime ≤ 01:30, wake ≥ 05:00)
	p := mock.New(mock.Options{Seed: 9, Now: func() time.Time { return night }})
	later := newProvider(9) // same dataset, clock after wake-up

	var main *domain.SleepSession
	for _, s := range sleeps(t, later, c, today, today) {
		if s.IsMainSleep {
			main = &s
		}
	}
	if main == nil || !main.StartAt.Before(night) || !main.EndAt.After(night) {
		t.Fatalf("test setup: want a main sleep spanning %s, got %+v", night, main)
	}

	if got := sleeps(t, p, c, today, today); len(got) != 0 {
		t.Errorf("%d sessions returned while the night is in progress", len(got))
	}
	sessionRange := domain.TimeRange{Start: main.StartAt, End: main.EndAt}
	for _, m := range []domain.MetricType{domain.MetricHRVRMSSD, domain.MetricSpO2} {
		if got := intraday(t, p, c, m, sessionRange); len(got) != 0 {
			t.Errorf("%d %s samples from an unfinished night", len(got), m)
		}
	}
	hr := intraday(t, p, c, domain.MetricHeartRate, sessionRange)
	if len(hr) == 0 || hr[len(hr)-1].RecordedAt.Add(time.Minute).After(night) {
		t.Errorf("heart rate during the night should run up to %s", night)
	}
	got := summaries(t, p, c, today, today)
	if len(got) != 1 || got[0].SleepScore != nil || got[0].HRVRMSSDMs != nil || got[0].SpO2AvgPct != nil {
		t.Errorf("today's summary at 03:00 = %+v, want one without sleep metrics", got)
	}
}

func TestInvalidRequestsFail(t *testing.T) {
	p := newProvider(1)
	ok := conn(t, userA, "UTC")
	days := domain.DateRange{Start: date(t, "2026-11-01"), End: date(t, "2026-11-02")}
	span := domain.TimeRange{Start: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	noUser := ok
	noUser.UserID = uuid.Nil
	noLoc := ok
	noLoc.Location = nil
	google := ok
	google.Provider = domain.ProviderGoogleHealth

	tests := []struct {
		name   string
		ctx    context.Context
		conn   domain.Connection
		days   domain.DateRange
		span   domain.TimeRange
		metric domain.MetricType
		want   string
	}{
		{"canceled context", canceled, ok, days, span, domain.MetricSteps, "context canceled"},
		{"missing user", context.Background(), noUser, days, span, domain.MetricSteps, "user ID is required"},
		{"missing location", context.Background(), noLoc, days, span, domain.MetricSteps, "location is required"},
		{"other provider", context.Background(), google, days, span, domain.MetricSteps, `provider "google_health"`},
		{"inverted dates", context.Background(), ok, domain.DateRange{Start: days.End, End: days.Start}, span, domain.MetricSteps, "end 2026-11-01 before start"},
		{"inverted span", context.Background(), ok, days, domain.TimeRange{Start: span.End, End: span.Start}, domain.MetricSteps, "invalid time range"},
		{"unknown metric", context.Background(), ok, days, span, "blood_pressure", "unknown metric"},
		{"too many days", context.Background(), ok, domain.DateRange{Start: date(t, "2025-01-01"), End: date(t, "2026-01-02")}, span, domain.MetricSteps, "367 days exceeds maximum 366"},
		{"span too large", context.Background(), ok, days, domain.TimeRange{Start: span.Start, End: span.Start.Add(mock.MaxIntradayRange + time.Minute)}, domain.MetricSteps, "exceeds maximum"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var errs []error
			if _, err := p.FetchIntradayMetrics(tc.ctx, tc.conn, tc.metric, tc.span); err != nil {
				errs = append(errs, err)
			}
			// Date-range methods only see the connection and date checks.
			if !strings.Contains(tc.name, "span") && tc.name != "unknown metric" {
				_, err1 := p.FetchDailySummaries(tc.ctx, tc.conn, tc.days)
				_, err2 := p.FetchSleepSessions(tc.ctx, tc.conn, tc.days)
				if err1 == nil || err2 == nil {
					t.Fatalf("date-range fetch succeeded: %v, %v", err1, err2)
				}
				errs = append(errs, err1, err2)
			}
			if len(errs) == 0 {
				t.Fatal("no error")
			}
			for _, err := range errs {
				if !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "mock: ") {
					t.Errorf("error %q, want it to contain %q", err, tc.want)
				}
			}
		})
	}
}

func TestRefreshAuth(t *testing.T) {
	p := newProvider(1)
	c := conn(t, userA, "UTC")
	c.Credentials = domain.Credentials{AccessToken: "fake-old-access", RefreshToken: "fake-refresh", ExpiresAt: now.Add(-time.Minute)}

	creds, err := p.RefreshAuth(context.Background(), c)
	if err != nil {
		t.Fatalf("RefreshAuth: %v", err)
	}
	if !strings.HasPrefix(creds.AccessToken, "mock-access-") || creds.AccessToken == c.Credentials.AccessToken {
		t.Errorf("access token not refreshed: %q", creds.AccessToken)
	}
	if creds.RefreshToken != "fake-refresh" {
		t.Errorf("refresh token changed")
	}
	if !creds.ExpiresAt.Equal(now.Add(mock.AccessTokenTTL)) {
		t.Errorf("expires at %s, want %s", creds.ExpiresAt, now.Add(mock.AccessTokenTTL))
	}
	other := c
	other.UserID = userB
	if again, _ := p.RefreshAuth(context.Background(), other); again.AccessToken == creds.AccessToken {
		t.Error("two users received the same access token")
	}

	c.Credentials.RefreshToken = ""
	if _, err := p.RefreshAuth(context.Background(), c); err == nil || !strings.Contains(err.Error(), "no refresh token") {
		t.Errorf("RefreshAuth without refresh token: err = %v", err)
	}
}

func TestDefaultClockIsWallTime(t *testing.T) {
	p := mock.New(mock.Options{})
	c := conn(t, userA, "UTC")
	today := domain.DateOf(time.Now().UTC())
	got, err := p.FetchDailySummaries(context.Background(), c, domain.DateRange{Start: today.AddDays(1), End: today.AddDays(3)})
	if err != nil || len(got) != 0 {
		t.Fatalf("future days with default clock: %d summaries, err %v", len(got), err)
	}
}
