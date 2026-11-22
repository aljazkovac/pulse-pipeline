package domain_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
)

func TestDate_ParseStringAndArithmetic(t *testing.T) {
	d, err := domain.ParseDate("2026-02-28")
	if err != nil {
		t.Fatal(err)
	}
	if got := d.AddDays(1).String(); got != "2026-03-01" {
		t.Errorf("AddDays across month end: got %s", got)
	}
	if !d.Before(d.AddDays(1)) || d.Before(d) {
		t.Error("Before must be strict")
	}
	if _, err := domain.ParseDate("2026-13-01"); err == nil {
		t.Error("expected error for invalid month")
	}
}

func TestDateOf_UsesTheTimesLocation(t *testing.T) {
	// 23:30 UTC on Nov 20 is already Nov 21 in Stockholm (UTC+1).
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	instant := time.Date(2026, 11, 20, 23, 30, 0, 0, time.UTC)
	if got := domain.DateOf(instant.In(loc)).String(); got != "2026-11-21" {
		t.Errorf("local date: got %s", got)
	}
	if got := domain.DateOf(instant).String(); got != "2026-11-20" {
		t.Errorf("UTC date: got %s", got)
	}
}

func TestRanges_Validate(t *testing.T) {
	d, _ := domain.ParseDate("2026-11-20")
	if err := (domain.DateRange{Start: d, End: d}).Validate(); err != nil {
		t.Errorf("single-day range must be valid: %v", err)
	}
	if err := (domain.DateRange{Start: d, End: d.AddDays(-1)}).Validate(); err == nil {
		t.Error("inverted date range must be invalid")
	}
	for _, invalid := range []domain.Date{
		{},
		{Year: 2026, Month: time.November},
		{Year: 2026, Month: time.February, Day: 29},
	} {
		if err := (domain.DateRange{Start: invalid, End: d}).Validate(); err == nil {
			t.Errorf("invalid start must be rejected: %#v", invalid)
		}
		if err := (domain.DateRange{Start: d.AddDays(-365), End: invalid}).Validate(); err == nil {
			t.Errorf("invalid end must be rejected: %#v", invalid)
		}
	}
	now := time.Now()
	if err := (domain.TimeRange{Start: now, End: now}).Validate(); err == nil {
		t.Error("empty time range must be invalid")
	}
}

func TestEnums_Valid(t *testing.T) {
	for _, m := range domain.MetricTypes {
		if !m.Valid() {
			t.Errorf("%s must be valid", m)
		}
	}
	if domain.MetricType("weight").Valid() {
		t.Error("unknown metric must be invalid")
	}
	if !domain.SleepStageREM.Valid() || domain.SleepStageType("nap").Valid() {
		t.Error("sleep stage validation wrong")
	}
}

func TestCredentials_AreRedactedWhenFormatted(t *testing.T) {
	c := domain.Credentials{AccessToken: "fake-access-token", RefreshToken: "fake-refresh-token"}
	conn := domain.Connection{Credentials: c}
	asJSON, err := json.Marshal(conn)
	if err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&logBuf, nil)).Info("sync", "conn", conn, "creds", c)
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), fmt.Sprintf("%+v", conn), string(asJSON), logBuf.String()} {
		if containsAny(s, "fake-access-token", "fake-refresh-token") {
			t.Errorf("token leaked in %q", s)
		}
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestUser_LocationFallsBackToUTC(t *testing.T) {
	if got := (domain.User{Timezone: "Not/AZone"}).Location(); got != time.UTC {
		t.Errorf("want UTC fallback, got %s", got)
	}
	if got := (domain.User{Timezone: "Europe/Stockholm"}).Location().String(); got != "Europe/Stockholm" {
		t.Errorf("got %s", got)
	}
}

// StartIn defines where a local calendar day begins, which every provider
// and the sync window rely on. Users may have any IANA timezone, so besides
// ordinary days it handles DST transitions that land on midnight. Those never
// occur in Europe, so each such branch is pinned with one past, settled
// tzdata transition that reaches it.
func TestDate_StartIn(t *testing.T) {
	tests := []struct {
		name, zone, date, want string
	}{
		{"UTC", "UTC", "2026-11-20", "2026-11-20T00:00:00Z"},
		{"ordinary day", "Europe/Stockholm", "2026-11-20", "2026-11-19T23:00:00Z"},
		{"spring forward at 02:00", "Europe/Stockholm", "2026-03-29", "2026-03-28T23:00:00Z"},
		{"fall back at 03:00", "Europe/Stockholm", "2026-10-25", "2026-10-24T22:00:00Z"},
		// 01:00 -> 00:00, so midnight happens twice. time.Date picks the
		// second; StartIn must step back to the first.
		{"repeated midnight", "Asia/Amman", "2021-10-29", "2021-10-28T21:00:00Z"},
		// 00:00 -> 01:00; time.Date resolves into the previous day.
		{"skipped midnight resolved backwards", "America/Sao_Paulo", "2018-11-04", "2018-11-04T03:00:00Z"},
		// 00:00 -> 01:00; time.Date resolves forward into the day.
		{"skipped midnight resolved forwards", "Asia/Amman", "2021-03-26", "2021-03-25T22:00:00Z"},
		// Samoa skipped 2011-12-30 entirely; it shares 2011-12-31's start.
		{"wholly skipped date", "Pacific/Apia", "2011-12-30", "2011-12-30T10:00:00Z"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			loc, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Fatal(err)
			}
			d, err := domain.ParseDate(tc.date)
			if err != nil {
				t.Fatal(err)
			}
			got := d.StartIn(loc)
			if got.Format(time.RFC3339) != tc.want || got.Location() != time.UTC {
				t.Errorf("StartIn(%s) = %s, want %s (UTC)", tc.zone, got.Format(time.RFC3339), tc.want)
			}
			// Days never start out of order (a skipped date shares the next start).
			if next := d.AddDays(1).StartIn(loc); next.Before(got) {
				t.Errorf("next day starts at %s, before %s", next, got)
			}
		})
	}
}
