// Package domain holds Pulse Pipeline's provider-agnostic health types and the
// HealthProvider contract (ADR-0002 §5). It must not import pgx, net/http or
// any vendor SDK.
package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Provider identifies an upstream health data source.
type Provider string

const (
	ProviderGoogleHealth Provider = "google_health"
	ProviderMock         Provider = "mock"
)

// MetricType identifies an intraday time series.
type MetricType string

const (
	MetricHeartRate MetricType = "heart_rate"
	MetricSteps     MetricType = "steps"
	MetricHRVRMSSD  MetricType = "hrv_rmssd"
	MetricSpO2      MetricType = "spo2"
	MetricCalories  MetricType = "calories"
)

// MetricTypes lists every supported intraday metric.
var MetricTypes = []MetricType{MetricHeartRate, MetricSteps, MetricHRVRMSSD, MetricSpO2, MetricCalories}

// Valid reports whether m is a supported metric type.
func (m MetricType) Valid() bool {
	for _, t := range MetricTypes {
		if m == t {
			return true
		}
	}
	return false
}

// SleepStageType is a normalized sleep stage.
type SleepStageType string

const (
	SleepStageDeep  SleepStageType = "deep"
	SleepStageREM   SleepStageType = "rem"
	SleepStageLight SleepStageType = "light"
	SleepStageAwake SleepStageType = "awake"
)

// Valid reports whether s is a supported sleep stage.
func (s SleepStageType) Valid() bool {
	switch s {
	case SleepStageDeep, SleepStageREM, SleepStageLight, SleepStageAwake:
		return true
	}
	return false
}

// Date is a local calendar day (no time, no zone). Health "days" are always
// interpreted in the owning user's timezone.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// DateOf returns the calendar day of t in t's location.
func DateOf(t time.Time) Date {
	y, m, d := t.Date()
	return Date{Year: y, Month: m, Day: d}
}

// ParseDate parses a YYYY-MM-DD string.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return Date{}, fmt.Errorf("parsing date %q: %w", s, err)
	}
	return DateOf(t), nil
}

// IsZero reports whether d is the zero Date (unset).
func (d Date) IsZero() bool { return d == Date{} }

// Time returns midnight of d in UTC, which is how dates cross the DB boundary.
func (d Date) Time() time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}

// AddDays returns d shifted by n days.
func (d Date) AddDays(n int) Date { return DateOf(d.Time().AddDate(0, 0, n)) }

// Before reports whether d is strictly before o.
func (d Date) Before(o Date) bool { return d.Time().Before(o.Time()) }

func (d Date) String() string { return d.Time().Format(time.DateOnly) }

// DateRange is an inclusive range of local calendar days.
type DateRange struct {
	Start Date
	End   Date
}

// Validate reports an error if the range is inverted.
func (r DateRange) Validate() error {
	if r.End.Before(r.Start) {
		return fmt.Errorf("invalid date range: end %s before start %s", r.End, r.Start)
	}
	return nil
}

// TimeRange is a half-open instant range [Start, End).
type TimeRange struct {
	Start time.Time
	End   time.Time
}

// Validate reports an error if the range is empty or inverted.
func (r TimeRange) Validate() error {
	if !r.End.After(r.Start) {
		return fmt.Errorf("invalid time range: end %s not after start %s", r.End, r.Start)
	}
	return nil
}

// DailySummary is one user's normalized metrics for one local day. Nil
// pointers mean the provider did not report that metric.
type DailySummary struct {
	Provider         Provider
	Date             Date
	Steps            *int32
	RestingHeartRate *int16
	HRVRMSSDMs       *float64
	SleepScore       *int16
	CaloriesKcal     *int32
	SpO2AvgPct       *float64
}

// IntradayPoint is a single sample of an intraday metric.
type IntradayPoint struct {
	Metric     MetricType
	RecordedAt time.Time
	Value      float64
}

// SleepStage is one contiguous interval in a single stage.
type SleepStage struct {
	Stage   SleepStageType
	StartAt time.Time
	EndAt   time.Time
}

// SleepSession is a normalized sleep period. ProviderSessionID is the stable
// upstream identifier used as the upsert key.
type SleepSession struct {
	ID                uuid.UUID // assigned by the store
	Provider          Provider
	ProviderSessionID string
	SleepDate         Date // local wake-up date
	StartAt           time.Time
	EndAt             time.Time
	IsMainSleep       bool
	EfficiencyPct     *int16
	Stages            []SleepStage
}

// Duration is the time between session start and end.
func (s SleepSession) Duration() time.Duration { return s.EndAt.Sub(s.StartAt) }
