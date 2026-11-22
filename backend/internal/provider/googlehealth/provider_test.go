package googlehealth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/provider/googlehealth"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func ptr[T any](v T) *T { return &v }

// dailyRollUpReq decodes the civil range of a dailyRollUp request body.
func dailyRollUpReq(t *testing.T, body []byte) (start, end domain.Date, pageSize, window int) {
	t.Helper()
	var req struct {
		Range struct {
			Start struct{ Date struct{ Year, Month, Day int } }
			End   struct{ Date struct{ Year, Month, Day int } }
		}
		WindowSizeDays int
		PageSize       int
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("dailyRollUp body: %v", err)
	}
	s, e := req.Range.Start.Date, req.Range.End.Date
	return domain.Date{Year: s.Year, Month: time.Month(s.Month), Day: s.Day},
		domain.Date{Year: e.Year, Month: time.Month(e.Month), Day: e.Day}, req.PageSize, req.WindowSizeDays
}

// serveDailyRollUp answers each requested civil day with value(day), newest
// first like the real API; a nil value omits the field (unworn day).
func serveDailyRollUp(t *testing.T, f *fakeGoogle, dataType, field string, value func(domain.Date) map[string]any) {
	f.handle("POST /users/me/dataTypes/"+dataType+"/dataPoints:dailyRollUp", func(w http.ResponseWriter, c call) {
		start, end, _, _ := dailyRollUpReq(t, c.Body)
		var pts []any
		// One extra day before the requested range: the adapter must drop it.
		for d := end.AddDays(-1); !d.Before(start.AddDays(-1)); d = d.AddDays(-1) {
			pt := map[string]any{
				"civilStartTime": map[string]any{"date": civil(d), "time": map[string]any{}},
				"civilEndTime":   map[string]any{"date": civil(d.AddDays(1)), "time": map[string]any{}},
			}
			if v := value(d); v != nil {
				pt[field] = v
			}
			pts = append(pts, pt)
		}
		writeJSON(w, http.StatusOK, map[string]any{"rollupDataPoints": pts})
	})
}

func serveDaily(f *fakeGoogle, dataType string, pts ...any) {
	f.handle("GET /users/me/dataTypes/"+dataType+"/dataPoints:reconcile", func(w http.ResponseWriter, _ call) {
		writeJSON(w, http.StatusOK, page(pts, ""))
	})
}

func TestFetchDailySummaries_MergesRollupsAndDailyTypes(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	r := domain.DateRange{Start: date(t, "2026-11-01"), End: date(t, "2026-11-20")} // 20 days

	serveDailyRollUp(t, f, "steps", "steps", func(d domain.Date) map[string]any {
		if d.Day == 5 {
			return nil // unworn: no steps value
		}
		return map[string]any{"countSum": fmt.Sprint(1000 + d.Day)}
	})
	serveDailyRollUp(t, f, "total-calories", "totalCalories", func(d domain.Date) map[string]any {
		if d.Day == 5 {
			return nil
		}
		return map[string]any{"kcalSum": 2000.6}
	})
	serveDaily(f, "daily-resting-heart-rate",
		map[string]any{"dailyRestingHeartRate": map[string]any{"date": civil(date(t, "2026-11-03")), "beatsPerMinute": "54"}},
		map[string]any{"dailyRestingHeartRate": map[string]any{"date": civil(date(t, "2026-11-04")), "beatsPerMinute": "300"}}, // violates schema CHECK
		map[string]any{"dailyRestingHeartRate": map[string]any{"date": civil(date(t, "2026-10-31")), "beatsPerMinute": "55"}}, // outside r
	)
	serveDaily(f, "daily-heart-rate-variability",
		map[string]any{"dailyHeartRateVariability": map[string]any{"date": civil(date(t, "2026-11-03")), "averageHeartRateVariabilityMilliseconds": 41.5}},
		map[string]any{"dailyHeartRateVariability": map[string]any{"date": civil(date(t, "2026-10-31")), "averageHeartRateVariabilityMilliseconds": 40.0}}, // outside r
		map[string]any{"dailyHeartRateVariability": map[string]any{"date": civil(date(t, "2026-11-06")), "averageHeartRateVariabilityMilliseconds": -1.0}}, // violates CHECK
	)
	serveDaily(f, "daily-oxygen-saturation",
		map[string]any{"dailyOxygenSaturation": map[string]any{"date": civil(date(t, "2026-11-03")), "averagePercentage": 96.4, "lowerBoundPercentage": 94, "upperBoundPercentage": 98}},
		map[string]any{"dailyOxygenSaturation": map[string]any{"date": civil(date(t, "2026-10-31")), "averagePercentage": 95.0}},  // outside r
		map[string]any{"dailyOxygenSaturation": map[string]any{"date": civil(date(t, "2026-11-06")), "averagePercentage": 101.0}}, // violates CHECK
	)

	got, err := p.FetchDailySummaries(ctxT(t), testConn(t), r)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 19 { // 20 days minus the unworn 5th; nothing from 10-31
		t.Fatalf("got %d summaries, want 19", len(got))
	}
	for i := 1; i < len(got); i++ {
		if !got[i-1].Date.Before(got[i].Date) {
			t.Fatalf("summaries not sorted by date: %s then %s", got[i-1].Date, got[i].Date)
		}
	}
	nov3 := got[2]
	want := domain.DailySummary{
		Provider: domain.ProviderGoogleHealth, Date: date(t, "2026-11-03"),
		Steps: ptr[int32](1003), CaloriesKcal: ptr[int32](2001), RestingHeartRate: ptr[int16](54),
		HRVRMSSDMs: ptr(41.5), SpO2AvgPct: ptr(96.4),
	}
	if !reflect.DeepEqual(nov3, want) {
		t.Errorf("2026-11-03 = %+v, want %+v", nov3, want)
	}
	if got[0].Date != r.Start {
		t.Errorf("first summary is %s; days outside the range must be dropped", got[0].Date)
	}
	if got[3].Date != date(t, "2026-11-04") || got[3].RestingHeartRate != nil {
		t.Errorf("out-of-range resting HR stored: %+v", got[3])
	}
	if nov6 := got[4]; nov6.Date != date(t, "2026-11-06") || nov6.HRVRMSSDMs != nil || nov6.SpO2AvgPct != nil {
		t.Errorf("invalid HRV/SpO2 stored: %+v", nov6)
	}
	for _, s := range got {
		if s.SleepScore != nil {
			t.Errorf("%s has a sleep score; the API provides none", s.Date)
		}
	}

	// Range limits: steps in one 20-day call, total-calories in 14 + 6.
	if c := f.callsTo("POST /users/me/dataTypes/steps/dataPoints:dailyRollUp"); len(c) != 1 {
		t.Errorf("steps rollup calls = %d, want 1", len(c))
	}
	cal := f.callsTo("POST /users/me/dataTypes/total-calories/dataPoints:dailyRollUp")
	if len(cal) != 2 {
		t.Fatalf("total-calories rollup calls = %d, want 2 (14-day limit)", len(cal))
	}
	for i, want := range [][2]string{{"2026-11-01", "2026-11-15"}, {"2026-11-15", "2026-11-21"}} {
		start, end, pageSize, window := dailyRollUpReq(t, cal[i].Body)
		if start.String() != want[0] || end.String() != want[1] || window != 1 {
			t.Errorf("calories call %d range %s..%s window %d, want %s..%s (end exclusive) window 1", i, start, end, window, want[0], want[1])
		}
		if days := int(end.Time().Sub(start.Time()).Hours() / 24); pageSize != days {
			t.Errorf("calories call %d pageSize %d, want %d (one page per call)", i, pageSize, days)
		}
	}
	rhr := f.callsTo("GET /users/me/dataTypes/daily-resting-heart-rate/dataPoints:reconcile")
	if len(rhr) != 1 || rhr[0].Query.Get("filter") != `daily_resting_heart_rate.date >= "2026-11-01" AND daily_resting_heart_rate.date < "2026-11-21"` {
		t.Errorf("resting HR filter = %q", rhr[0].Query.Get("filter"))
	}
}

func TestFetchDailySummaries_CivilDaysAcrossDST(t *testing.T) {
	// Daily values are keyed by Google's civil date, so a 23-hour DST day is
	// still exactly one summary.
	f := newFake(t)
	p := f.provider(t)
	serveDailyRollUp(t, f, "steps", "steps", func(domain.Date) map[string]any { return map[string]any{"countSum": "10"} })
	serveDailyRollUp(t, f, "total-calories", "totalCalories", func(domain.Date) map[string]any { return nil })
	for _, dt := range []string{"daily-resting-heart-rate", "daily-heart-rate-variability", "daily-oxygen-saturation"} {
		serveDaily(f, dt)
	}
	r := domain.DateRange{Start: date(t, "2026-03-28"), End: date(t, "2026-03-30")} // DST starts 03-29 in Stockholm
	got, err := p.FetchDailySummaries(ctxT(t), testConn(t), r)
	if err != nil {
		t.Fatal(err)
	}
	var dates []string
	for _, s := range got {
		dates = append(dates, s.Date.String())
	}
	if !reflect.DeepEqual(dates, []string{"2026-03-28", "2026-03-29", "2026-03-30"}) {
		t.Errorf("dates = %v", dates)
	}
}

func TestFetchDailySummaries_SplitsRollupsAtAPILimits(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	serveDailyRollUp(t, f, "steps", "steps", func(domain.Date) map[string]any { return map[string]any{"countSum": "1"} })
	serveDailyRollUp(t, f, "total-calories", "totalCalories", func(domain.Date) map[string]any { return nil })
	for _, dt := range []string{"daily-resting-heart-rate", "daily-heart-rate-variability", "daily-oxygen-saturation"} {
		serveDaily(f, dt)
	}
	r := domain.DateRange{Start: date(t, "2026-01-01"), End: date(t, "2026-04-10")} // 100 days
	got, err := p.FetchDailySummaries(ctxT(t), testConn(t), r)
	if err != nil || len(got) != 100 {
		t.Fatalf("got %d summaries (%v), want 100", len(got), err)
	}
	steps := f.callsTo("POST /users/me/dataTypes/steps/dataPoints:dailyRollUp")
	if len(steps) != 2 {
		t.Fatalf("steps rollup calls = %d, want 2 (90-day limit)", len(steps))
	}
	if start, end, _, _ := dailyRollUpReq(t, steps[1].Body); start.String() != "2026-04-01" || end.String() != "2026-04-11" {
		t.Errorf("second steps call covers %s..%s, want 2026-04-01..2026-04-11", start, end)
	}
	if n := len(f.callsTo("POST /users/me/dataTypes/total-calories/dataPoints:dailyRollUp")); n != 8 {
		t.Errorf("total-calories rollup calls = %d, want 8 (14-day limit)", n)
	}
}

// physicalFilterRange extracts the two instants of a ">= AND <" filter.
func physicalFilterRange(t *testing.T, filter string) (time.Time, time.Time) {
	t.Helper()
	parts := strings.Split(filter, `"`)
	if len(parts) != 5 {
		t.Fatalf("filter %q is not a two-sided range", filter)
	}
	return ts(t, parts[1]), ts(t, parts[3])
}

func TestFetchIntraday_HeartRatePaginatesAndHonoursDSTDayBounds(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	loc := stockholm(t)
	day := date(t, "2026-10-25") // DST ends: a 25-hour local day
	r := domain.TimeRange{Start: day.StartIn(loc), End: day.AddDays(1).StartIn(loc)}
	if r.End.Sub(r.Start) != 25*time.Hour {
		t.Fatalf("test setup: local day is %s, want 25h", r.End.Sub(r.Start))
	}
	hr := func(at string, bpm string) any {
		return map[string]any{"dataPointName": "", "heartRate": map[string]any{
			"sampleTime": map[string]any{"physicalTime": at, "utcOffset": "7200s"}, "beatsPerMinute": bpm,
		}}
	}
	f.handle("GET /users/me/dataTypes/heart-rate/dataPoints:reconcile", func(w http.ResponseWriter, c call) {
		switch c.Query.Get("pageToken") {
		case "":
			writeJSON(w, http.StatusOK, page([]any{
				hr("2026-10-25T22:59:59Z", "70"), // last second of the local day
				hr("2026-10-25T23:00:00Z", "71"), // next local day: excluded
			}, "fake-page-2"))
		case "fake-page-2":
			writeJSON(w, http.StatusOK, page([]any{
				hr("2026-10-24T22:00:00Z", "60"), // local midnight (CEST)
				hr("2026-10-24T21:59:59Z", "59"), // previous local day: excluded
			}, ""))
		default:
			t.Errorf("unexpected page token %q", c.Query.Get("pageToken"))
		}
	})

	got, err := p.FetchIntradayMetrics(ctxT(t), testConn(t), domain.MetricHeartRate, r)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.IntradayPoint{
		{Metric: domain.MetricHeartRate, RecordedAt: ts(t, "2026-10-25T22:59:59Z"), Value: 70},
		{Metric: domain.MetricHeartRate, RecordedAt: ts(t, "2026-10-24T22:00:00Z"), Value: 60},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("points = %+v, want %+v", got, want)
	}
	calls := f.callsTo("GET /users/me/dataTypes/heart-rate/dataPoints:reconcile")
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2 pages", len(calls))
	}
	q := calls[0].Query
	start, end := physicalFilterRange(t, q.Get("filter"))
	if !start.Equal(r.Start) || !end.Equal(r.End) || !strings.HasPrefix(q.Get("filter"), "heart_rate.sample_time.physical_time >= ") {
		t.Errorf("filter = %q, want heart_rate.sample_time.physical_time over [%s, %s)", q.Get("filter"), r.Start, r.End)
	}
	if q.Get("pageSize") != "10000" {
		t.Errorf("pageSize = %q, want the API maximum", q.Get("pageSize"))
	}
	if calls[1].Query.Get("filter") != q.Get("filter") {
		t.Error("page 2 changed the filter")
	}
}

func TestFetchIntraday_SampleAndIntervalTypes(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	r := domain.TimeRange{Start: ts(t, "2026-11-20T00:00:00Z"), End: ts(t, "2026-11-21T00:00:00Z")}
	sample := func(field, valueKey string, v any) any {
		return map[string]any{field: map[string]any{"sampleTime": map[string]any{"physicalTime": "2026-11-20T02:00:00Z"}, valueKey: v}}
	}
	serveDaily(f, "heart-rate-variability", sample("heartRateVariability", "rootMeanSquareOfSuccessiveDifferencesMilliseconds", 42.5))
	serveDaily(f, "oxygen-saturation", sample("oxygenSaturation", "percentage", 96.5))
	serveDaily(f, "steps",
		map[string]any{"steps": map[string]any{"interval": map[string]any{"startTime": "2026-11-20T07:05:00Z", "endTime": "2026-11-20T07:06:00Z"}, "count": "40"}},
		map[string]any{"steps": map[string]any{"interval": map[string]any{"startTime": "2026-11-20T07:06:00Z", "endTime": "2026-11-20T07:07:00Z"}}}, // worn, idle
	)

	for _, tc := range []struct {
		metric domain.MetricType
		route  string
		prefix string
		want   []domain.IntradayPoint
	}{
		{domain.MetricHRVRMSSD, "heart-rate-variability", "heart_rate_variability.sample_time.physical_time",
			[]domain.IntradayPoint{{Metric: domain.MetricHRVRMSSD, RecordedAt: ts(t, "2026-11-20T02:00:00Z"), Value: 42.5}}},
		{domain.MetricSpO2, "oxygen-saturation", "oxygen_saturation.sample_time.physical_time",
			[]domain.IntradayPoint{{Metric: domain.MetricSpO2, RecordedAt: ts(t, "2026-11-20T02:00:00Z"), Value: 96.5}}},
		{domain.MetricSteps, "steps", "steps.interval.start_time", []domain.IntradayPoint{
			{Metric: domain.MetricSteps, RecordedAt: ts(t, "2026-11-20T07:05:00Z"), Value: 40},
			{Metric: domain.MetricSteps, RecordedAt: ts(t, "2026-11-20T07:06:00Z"), Value: 0},
		}},
	} {
		got, err := p.FetchIntradayMetrics(ctxT(t), testConn(t), tc.metric, r)
		if err != nil {
			t.Fatalf("%s: %v", tc.metric, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s = %+v, want %+v", tc.metric, got, tc.want)
		}
		calls := f.callsTo("GET /users/me/dataTypes/" + tc.route + "/dataPoints:reconcile")
		if len(calls) != 1 || !strings.HasPrefix(calls[0].Query.Get("filter"), tc.prefix+" >= ") {
			t.Errorf("%s filter = %v", tc.metric, calls)
		}
	}
}

func TestFetchIntraday_CaloriesRollUpSplitsAt14Days(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	r := domain.TimeRange{Start: ts(t, "2026-11-01T00:00:00Z"), End: ts(t, "2026-11-21T00:00:00Z")}
	f.handle("POST /users/me/dataTypes/total-calories/dataPoints:rollUp", func(w http.ResponseWriter, c call) {
		var req struct {
			Range      struct{ StartTime, EndTime string }
			WindowSize string
		}
		if err := json.Unmarshal(c.Body, &req); err != nil || req.WindowSize != "60s" {
			t.Errorf("rollUp body %s (%v)", c.Body, err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"rollupDataPoints": []any{
			map[string]any{"startTime": req.Range.StartTime, "endTime": "x", "totalCalories": map[string]any{"kcalSum": 1.25}},
			map[string]any{"startTime": req.Range.StartTime}, // unworn window: skipped
		}})
	})
	got, err := p.FetchIntradayMetrics(ctxT(t), testConn(t), domain.MetricCalories, r)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.IntradayPoint{
		{Metric: domain.MetricCalories, RecordedAt: ts(t, "2026-11-01T00:00:00Z"), Value: 1.25},
		{Metric: domain.MetricCalories, RecordedAt: ts(t, "2026-11-15T00:00:00Z"), Value: 1.25},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("points = %+v, want %+v", got, want)
	}
	if n := len(f.callsTo("POST /users/me/dataTypes/total-calories/dataPoints:rollUp")); n != 2 {
		t.Errorf("rollUp calls = %d, want 2", n)
	}
}

func sleepPoint(id, start, end string, main bool, stages ...[3]string) map[string]any {
	var st []any
	for _, s := range stages {
		st = append(st, map[string]any{"startTime": s[0], "endTime": s[1], "type": s[2]})
	}
	return map[string]any{
		"dataPointName": "users/me/dataTypes/sleep/dataPoints/" + id,
		"sleep": map[string]any{
			"interval": map[string]any{"startTime": start, "endTime": end},
			"type":     "STAGES", "stages": st,
			"metadata": map[string]any{"stagesStatus": "SUCCEEDED", "processed": true, "mainSleep": main},
			"summary":  map[string]any{"minutesInSleepPeriod": "464", "minutesAsleep": "407"},
		},
	}
}

func TestFetchSleepSessions_LocalWakeDateStagesAndPagination(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	r := domain.DateRange{Start: date(t, "2026-10-24"), End: date(t, "2026-10-25")} // DST ends 10-25
	f.handle("GET /users/me/dataTypes/sleep/dataPoints:reconcile", func(w http.ResponseWriter, c call) {
		if c.Query.Get("pageSize") != "25" {
			t.Errorf("sleep pageSize = %q, want 25", c.Query.Get("pageSize"))
		}
		if c.Query.Get("pageToken") == "" {
			writeJSON(w, http.StatusOK, page([]any{
				// Wakes 06:30 CET on 10-25 (after the DST change).
				// Stages out of order, one duplicated start, one unknown and
				// one empty segment.
				sleepPoint("night-a", "2026-10-24T21:00:00Z", "2026-10-25T05:30:00Z", true,
					[3]string{"2026-10-24T22:30:00Z", "2026-10-24T23:00:00Z", "REM"},
					[3]string{"2026-10-24T21:00:00Z", "2026-10-24T21:30:00Z", "LIGHT"},
					[3]string{"2026-10-24T21:00:00Z", "2026-10-24T21:20:00Z", "DEEP"}, // same start: dropped
					[3]string{"2026-10-24T23:00:00Z", "2026-10-24T23:05:00Z", "AWAKE"},
					[3]string{"2026-10-24T21:30:00Z", "2026-10-24T22:30:00Z", "DEEP"},
					[3]string{"2026-10-24T23:05:00Z", "2026-10-25T05:30:00Z", "SLEEP_STAGE_TYPE_UNSPECIFIED"},
					[3]string{"2026-10-24T23:05:00Z", "2026-10-24T23:05:00Z", "LIGHT"}, // empty: dropped
				),
			}, "fake-sleep-page-2"))
			return
		}
		// Ends 22:30Z on 10-24 = 00:30 CEST on 10-25: local date 10-25.
		// Upstream minutes asleep exceed minutes in bed: efficiency caps at 100.
		nap := sleepPoint("nap-b", "2026-10-24T21:45:00Z", "2026-10-24T22:30:00Z", false)
		nap["sleep"].(map[string]any)["summary"] = map[string]any{"minutesInSleepPeriod": "45", "minutesAsleep": "50"}
		// A CLASSIC night: no Pulse stages rather than only its AWAKE ones.
		classic := sleepPoint("classic-d", "2026-10-24T01:00:00Z", "2026-10-24T05:00:00Z", true,
			[3]string{"2026-10-24T01:00:00Z", "2026-10-24T01:10:00Z", "AWAKE"},
			[3]string{"2026-10-24T01:10:00Z", "2026-10-24T05:00:00Z", "ASLEEP"})
		classic["sleep"].(map[string]any)["type"] = "CLASSIC"
		writeJSON(w, http.StatusOK, page([]any{
			nap, classic,
			// Wakes 23:30 CEST on 10-23: outside r even though the widened query returns it.
			sleepPoint("night-c", "2026-10-23T20:00:00Z", "2026-10-23T21:30:00Z", true),
			// No identifier: unusable.
			map[string]any{"sleep": map[string]any{"interval": map[string]any{"startTime": "2026-10-24T01:00:00Z", "endTime": "2026-10-24T02:00:00Z"}}},
		}, ""))
	})

	got, err := p.FetchSleepSessions(ctxT(t), testConn(t), r)
	if err != nil {
		t.Fatal(err)
	}
	calls := f.callsTo("GET /users/me/dataTypes/sleep/dataPoints:reconcile")
	if f := calls[0].Query.Get("filter"); f != `sleep.interval.civil_end_time >= "2026-10-23" AND sleep.interval.civil_end_time < "2026-10-27"` {
		t.Errorf("filter = %q, want the range widened by a day on each side", f)
	}
	if len(got) != 3 {
		t.Fatalf("sessions = %+v, want classic-d, night-a and nap-b", got)
	}
	classic, night, nap := got[0], got[1], got[2] // sorted by start
	if classic.ProviderSessionID != "classic-d" || classic.SleepDate != date(t, "2026-10-24") || len(classic.Stages) != 0 {
		t.Errorf("classic = %+v, want no stages", classic)
	}
	if night.ProviderSessionID != "night-a" || night.SleepDate != date(t, "2026-10-25") || !night.IsMainSleep ||
		night.Provider != domain.ProviderGoogleHealth || night.EfficiencyPct == nil || *night.EfficiencyPct != 88 {
		t.Errorf("night = %+v (efficiency %v)", night, night.EfficiencyPct)
	}
	wantStages := []domain.SleepStageType{domain.SleepStageLight, domain.SleepStageDeep, domain.SleepStageREM, domain.SleepStageAwake}
	var gotStages []domain.SleepStageType
	for _, s := range night.Stages {
		gotStages = append(gotStages, s.Stage)
	}
	if !reflect.DeepEqual(gotStages, wantStages) {
		t.Errorf("stages = %v, want %v", gotStages, wantStages)
	}
	if night.Stages[0].EndAt != ts(t, "2026-10-24T21:30:00Z") {
		t.Errorf("duplicate-start stage kept instead of the first: %+v", night.Stages[0])
	}
	if nap.ProviderSessionID != "nap-b" || nap.SleepDate != date(t, "2026-10-25") || nap.IsMainSleep ||
		nap.EfficiencyPct == nil || *nap.EfficiencyPct != 100 {
		t.Errorf("nap = %+v, want local wake date 2026-10-25", nap)
	}
}

func TestErrorClassification(t *testing.T) {
	const bodySecret = "fake-body-secret"
	for name, tc := range map[string]struct {
		status       int
		body         string
		transient    bool
		unauthorized bool
		contains     string
	}{
		"429 rate limited":   {429, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Quota exceeded"}}`, true, false, "RESOURCE_EXHAUSTED"},
		"500":                {500, `{"error":{"code":500,"status":"INTERNAL"}}`, true, false, "HTTP 500"},
		"502 html":           {502, "<html>" + bodySecret + "</html>", true, false, "HTTP 502"},
		"503":                {503, `{"error":{"code":503,"status":"UNAVAILABLE"}}`, true, false, "UNAVAILABLE"},
		"504":                {504, `{"error":{"code":504,"status":"DEADLINE_EXCEEDED"}}`, true, false, "DEADLINE_EXCEEDED"},
		"401":                {401, `{"error":{"code":401,"status":"UNAUTHENTICATED"}}`, false, true, "UNAUTHENTICATED"},
		"403 missing scope":  {403, `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"reason":"MISSING_OAUTH_SCOPE"}]}}`, false, false, "MISSING_OAUTH_SCOPE"},
		"400 not linked":     {400, `{"error":{"code":400,"status":"FAILED_PRECONDITION","details":[{"reason":"ACCOUNT_NOT_LINKED"}]}}`, false, false, "ACCOUNT_NOT_LINKED"},
		"long message":       {400, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"` + strings.Repeat("m", 500) + `"}}`, false, false, "…"},
		"404 non-json":       {404, bodySecret, false, false, "HTTP 404"},
		"200 malformed json": {200, `{"dataPoints": [` + bodySecret, false, false, "decoding response"},
		"200 wrong type":     {200, `{"dataPoints": [{"heartRate": {"sampleTime": {"physicalTime": "2026-11-20T08:00:00Z"}, "beatsPerMinute": 72.5}}]}`, false, false, "decoding response"},
		"200 type mismatch":  {200, `{"dataPoints": "fake-body-secret"}`, false, false, "unexpected type at dataPoints"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			p := f.provider(t)
			f.handle("GET /users/me/dataTypes/heart-rate/dataPoints:reconcile", func(w http.ResponseWriter, _ call) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := p.FetchIntradayMetrics(ctxT(t), testConn(t), domain.MetricHeartRate,
				domain.TimeRange{Start: t0.Add(-time.Hour), End: t0})
			if err == nil {
				t.Fatal("want error")
			}
			if got := errors.Is(err, domain.ErrTransient); got != tc.transient {
				t.Errorf("transient = %v, want %v (%v)", got, tc.transient, err)
			}
			if got := errors.Is(err, domain.ErrUnauthorized); got != tc.unauthorized {
				t.Errorf("unauthorized = %v, want %v (%v)", got, tc.unauthorized, err)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.contains) {
				t.Errorf("error %q does not mention %q", msg, tc.contains)
			}
			if strings.Contains(msg, bodySecret) || strings.Contains(msg, fakeAccess) || strings.Contains(msg, "72.5") || len(msg) > 400 {
				t.Errorf("error leaks the body or token, or is unbounded: %q", msg)
			}
		})
	}
}

func TestTransportErrors(t *testing.T) {
	t.Run("timeout is transient", func(t *testing.T) {
		f := newFake(t)
		p := f.provider(t, func(o *googlehealth.Options) { o.HTTPClient = &http.Client{Timeout: 50 * time.Millisecond} })
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		f.handle("GET /users/me/dataTypes/heart-rate/dataPoints:reconcile", func(http.ResponseWriter, call) { <-release })
		_, err := p.FetchIntradayMetrics(ctxT(t), testConn(t), domain.MetricHeartRate, domain.TimeRange{Start: t0.Add(-time.Hour), End: t0})
		if !errors.Is(err, domain.ErrTransient) {
			t.Fatalf("err = %v, want ErrTransient", err)
		}
		if strings.Contains(err.Error(), "filter=") {
			t.Errorf("error repeats the request URL: %v", err)
		}
	})
	t.Run("connection refused is transient", func(t *testing.T) {
		f := newFake(t)
		p := f.provider(t)
		f.srv.Close()
		_, err := p.FetchSleepSessions(ctxT(t), testConn(t), domain.DateRange{Start: date(t, "2026-11-20"), End: date(t, "2026-11-20")})
		if !errors.Is(err, domain.ErrTransient) {
			t.Fatalf("err = %v, want ErrTransient", err)
		}
	})
	t.Run("cancelled context is not retried", func(t *testing.T) {
		f := newFake(t)
		p := f.provider(t)
		ctx, cancel := context.WithCancel(context.Background())
		f.handle("GET /users/me/dataTypes/heart-rate/dataPoints:reconcile", func(http.ResponseWriter, call) { cancel() })
		_, err := p.FetchIntradayMetrics(ctx, testConn(t), domain.MetricHeartRate, domain.TimeRange{Start: t0.Add(-time.Hour), End: t0})
		if !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrTransient) {
			t.Fatalf("err = %v, want context.Canceled and not transient", err)
		}
	})
}

func TestPagination_RepeatedTokenStops(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	f.handle("GET /users/me/dataTypes/steps/dataPoints:reconcile", func(w http.ResponseWriter, _ call) {
		writeJSON(w, http.StatusOK, page(nil, "fake-same-token"))
	})
	_, err := p.FetchIntradayMetrics(ctxT(t), testConn(t), domain.MetricSteps, domain.TimeRange{Start: t0.Add(-time.Hour), End: t0})
	if err == nil || !strings.Contains(err.Error(), "repeated page token") {
		t.Fatalf("err = %v, want a repeated-token error", err)
	}
	if n := len(f.callsTo("GET /users/me/dataTypes/steps/dataPoints:reconcile")); n != 2 {
		t.Errorf("calls = %d, want 2", n)
	}
}

func TestRequestValidation(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	ctx := ctxT(t)
	r := domain.DateRange{Start: date(t, "2026-11-20"), End: date(t, "2026-11-20")}

	noToken := testConn(t)
	noToken.Credentials.AccessToken = ""
	if _, err := p.FetchDailySummaries(ctx, noToken, r); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("missing access token: err = %v, want ErrUnauthorized", err)
	}
	wrong := testConn(t)
	wrong.Provider = domain.ProviderMock
	if _, err := p.FetchSleepSessions(ctx, wrong, r); err == nil {
		t.Error("accepted a mock connection")
	}
	noLoc := testConn(t)
	noLoc.Location = nil
	if _, err := p.FetchSleepSessions(ctx, noLoc, r); err == nil {
		t.Error("accepted a connection without a location")
	}
	if _, err := p.FetchIntradayMetrics(ctx, testConn(t), "made_up", domain.TimeRange{Start: t0.Add(-time.Hour), End: t0}); err == nil {
		t.Error("accepted an unknown metric")
	}
	if len(f.calls) != 0 {
		t.Errorf("invalid requests reached the server: %d calls", len(f.calls))
	}

	if s := fmt.Sprintf("%v %+v %#v", p, p, p); strings.Contains(s, fakeClientSecret) {
		t.Errorf("printing the provider leaks the client secret: %s", s)
	}
	if _, err := googlehealth.New(googlehealth.Options{}); err == nil {
		t.Error("New accepted an empty client ID")
	}
	if _, err := googlehealth.New(googlehealth.Options{ClientID: fakeClientID, HTTPClient: &http.Client{}}); err == nil {
		t.Error("New accepted an HTTP client without a timeout")
	}
}

func TestRefreshAuth(t *testing.T) {
	t.Run("keeps the refresh token unless rotated", func(t *testing.T) {
		for name, rotated := range map[string]string{"not rotated": "", "rotated": "fake-refresh-rotated"} {
			f := newFake(t)
			p := f.provider(t)
			f.token = func(w http.ResponseWriter, form url.Values) {
				want := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {fakeRefresh}, "client_id": {fakeClientID}, "client_secret": {fakeClientSecret}}
				if !reflect.DeepEqual(form, want) {
					t.Errorf("%s: token form = %v", name, form)
				}
				resp := map[string]any{"access_token": "fake-access-new", "expires_in": 3599, "token_type": "Bearer"}
				if rotated != "" {
					resp["refresh_token"] = rotated
				}
				writeJSON(w, http.StatusOK, resp)
			}
			got, err := p.RefreshAuth(ctxT(t), testConn(t))
			if err != nil {
				t.Fatal(err)
			}
			wantRefresh := fakeRefresh
			if rotated != "" {
				wantRefresh = rotated
			}
			want := domain.Credentials{AccessToken: "fake-access-new", RefreshToken: wantRefresh, ExpiresAt: t0.Add(3599 * time.Second)}
			if got != want {
				t.Errorf("%s: credentials differ (expiry %s, want %s)", name, got.ExpiresAt, want.ExpiresAt)
			}
		}
	})

	for name, tc := range map[string]struct {
		status       int
		body         string
		transient    bool
		unauthorized bool
	}{
		"invalid_grant":      {400, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`, false, true},
		"invalid_client":     {401, `{"error":"invalid_client","error_description":"Unauthorized"}`, false, false},
		"503":                {503, `upstream down`, true, false},
		"429":                {429, `{"error":"rate_limit_exceeded"}`, true, false},
		"200 malformed body": {200, `{"access_token":"` + fakeAccess + `"`, false, false},
		"200 no token":       {200, `{"expires_in":3599}`, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			p := f.provider(t)
			f.token = func(w http.ResponseWriter, _ url.Values) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}
			_, err := p.RefreshAuth(ctxT(t), testConn(t))
			if err == nil {
				t.Fatal("want error")
			}
			if errors.Is(err, domain.ErrTransient) != tc.transient || errors.Is(err, domain.ErrUnauthorized) != tc.unauthorized {
				t.Errorf("err = %v: transient %v (want %v), unauthorized %v (want %v)", err,
					errors.Is(err, domain.ErrTransient), tc.transient, errors.Is(err, domain.ErrUnauthorized), tc.unauthorized)
			}
			if msg := err.Error(); strings.Contains(msg, fakeRefresh) || strings.Contains(msg, fakeAccess) || strings.Contains(msg, fakeClientSecret) {
				t.Errorf("error leaks a secret: %q", msg)
			}
		})
	}

	t.Run("missing expires_in defaults to an hour", func(t *testing.T) {
		f := newFake(t)
		f.token = func(w http.ResponseWriter, _ url.Values) {
			writeJSON(w, http.StatusOK, map[string]any{"access_token": "fake-access-new"})
		}
		got, err := f.provider(t).RefreshAuth(ctxT(t), testConn(t))
		if err != nil || !got.ExpiresAt.Equal(t0.Add(time.Hour)) {
			t.Fatalf("expiry = %s (%v), want t0+1h so the token is not refreshed before every unit", got.ExpiresAt, err)
		}
	})

	t.Run("default client does not follow redirects", func(t *testing.T) {
		f := newFake(t)
		f.token = func(w http.ResponseWriter, _ url.Values) {
			w.Header().Set("Location", f.srv.URL+"/evil")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}
		p, err := googlehealth.New(googlehealth.Options{ClientID: fakeClientID, ClientSecret: fakeClientSecret, TokenURL: f.srv.URL + "/token"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.RefreshAuth(ctxT(t), testConn(t)); err == nil {
			t.Fatal("RefreshAuth succeeded on a redirect")
		}
		if len(f.callsTo("POST /evil"))+len(f.callsTo("GET /evil")) > 0 {
			t.Fatal("the token request (client secret, refresh token) was re-sent to the redirect target")
		}
	})

	t.Run("no refresh token", func(t *testing.T) {
		f := newFake(t)
		conn := testConn(t)
		conn.Credentials.RefreshToken = ""
		if _, err := f.provider(t).RefreshAuth(ctxT(t), conn); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("err = %v, want ErrUnauthorized", err)
		}
	})
}
