package sync

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
)

func d(t *testing.T, s string) domain.Date {
	t.Helper()
	v, err := domain.ParseDate(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPlanRange(t *testing.T) {
	cfg := DefaultConfig()
	today := d(t, "2026-11-21")
	cursorAt := func(s string) *domain.Date { v := d(t, s); return &v }
	backfill := func(a, b string) Request {
		return Request{Trigger: domain.SyncTriggerBackfill, Range: &domain.DateRange{Start: d(t, a), End: d(t, b)}}
	}
	tests := []struct {
		name          string
		req           Request
		syncedThrough *domain.Date
		start, end    string
		wantErr       string
	}{
		{"first sync reaches the initial window", Request{Trigger: domain.SyncTriggerScheduled}, nil, "2026-10-23", "2026-11-21", ""},
		{"resumes with overlap", Request{Trigger: domain.SyncTriggerScheduled}, cursorAt("2026-11-20"), "2026-11-17", "2026-11-21", ""},
		{"long outage covers the whole gap", Request{Trigger: domain.SyncTriggerManual}, cursorAt("2026-08-01"), "2026-07-29", "2026-11-21", ""},
		{"cursor ahead of today (timezone change)", Request{Trigger: domain.SyncTriggerScheduled}, cursorAt("2026-11-25"), "2026-11-21", "2026-11-21", ""},
		{"backfill as requested", backfill("2026-01-01", "2026-01-31"), cursorAt("2026-11-20"), "2026-01-01", "2026-01-31", ""},
		{"backfill capped at today", backfill("2026-11-01", "2026-12-31"), nil, "2026-11-01", "2026-11-21", ""},
		{"backfill in the future", backfill("2026-11-22", "2026-11-30"), nil, "", "", "after today"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planRange(tc.req, tc.syncedThrough, today, cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got.Start != d(t, tc.start) || got.End != d(t, tc.end) {
				t.Fatalf("planRange = %s, %v; want %s..%s", rangeString(got), err, tc.start, tc.end)
			}
		})
	}
}

func TestChunkDates_CoverRangeExactly(t *testing.T) {
	r := domain.DateRange{Start: d(t, "2026-10-23"), End: d(t, "2026-11-21")}
	for _, n := range []int{1, 7, 30, 31} {
		chunks := chunkDates(r, n)
		next := r.Start
		for _, c := range chunks {
			if c.Start != next || c.End.Before(c.Start) || int(c.End.Time().Sub(c.Start.Time()).Hours()/24) >= n {
				t.Fatalf("n=%d: bad chunk %s after %s", n, rangeString(c), next)
			}
			next = c.End.AddDays(1)
		}
		if next != r.End.AddDays(1) {
			t.Errorf("n=%d: chunks end at %s, want %s", n, next.AddDays(-1), r.End)
		}
	}
}

func TestLocalSpan_FollowsDST(t *testing.T) {
	stockholm, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	span := localSpan(domain.DateRange{Start: d(t, "2026-10-25"), End: d(t, "2026-10-25")}, stockholm)
	if got := span.End.Sub(span.Start); got != 25*time.Hour {
		t.Errorf("fall-back day spans %s, want 25h", got)
	}
}

func TestNextNightly(t *testing.T) {
	stockholm, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"before the hour: today", time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC), time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)},
		{"exactly at the hour: tomorrow", time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC), time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)},
		{"after the hour: tomorrow", time.Date(2026, 11, 21, 9, 30, 0, 0, time.UTC), time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)},
		{"into summer time", time.Date(2026, 3, 28, 22, 0, 0, 0, time.UTC), time.Date(2026, 3, 29, 2, 0, 0, 0, time.UTC)},
		{"local date ahead of UTC", time.Date(2026, 11, 21, 23, 30, 0, 0, time.UTC), time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)},
	}
	for _, tc := range tests {
		if got := nextNightly(tc.now, stockholm, 4); !got.Equal(tc.want) || got.Location() != time.UTC {
			t.Errorf("%s: nextNightly = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestCursor_PreservesProviderKeysAndNeverRewinds(t *testing.T) {
	c, err := parseCursor([]byte(`{"page_token": "abc", "synced_through": "2026-11-20"}`))
	if err != nil || c.syncedThrough == nil || *c.syncedThrough != d(t, "2026-11-20") {
		t.Fatalf("parseCursor = %+v, %v", c, err)
	}
	raw, err := c.advance(d(t, "2026-11-01"))
	if err != nil || string(raw) != `{"page_token":"abc","synced_through":"2026-11-20"}` {
		t.Errorf("advance backwards = %s, %v", raw, err)
	}
	raw, err = c.advance(d(t, "2026-11-21"))
	if err != nil || string(raw) != `{"page_token":"abc","synced_through":"2026-11-21"}` {
		t.Errorf("advance = %s, %v", raw, err)
	}

	for _, empty := range [][]byte{nil, []byte(`{}`)} {
		if c, err := parseCursor(empty); err != nil || c.syncedThrough != nil {
			t.Errorf("parseCursor(%q) = %+v, %v", empty, c, err)
		}
	}
	for _, bad := range []string{`[]`, `{"synced_through": 5}`, `{"synced_through": "yesterday"}`} {
		if _, err := parseCursor([]byte(bad)); err == nil {
			t.Errorf("parseCursor(%s): want error", bad)
		}
	}
}

func TestRetryPolicy_DelayIsBoundedExponentialWithJitter(t *testing.T) {
	p := RetryPolicy{Attempts: 6, BaseDelay: time.Second, MaxDelay: 5 * time.Second}
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 5 * time.Second, 5: 5 * time.Second} {
		for range 200 {
			if got := p.delay(attempt); got < want/2 || got > want {
				t.Fatalf("attempt %d: delay %s outside [%s, %s]", attempt, got, want/2, want)
			}
		}
	}
}

func TestConfigAndRequestValidation(t *testing.T) {
	if err := DefaultConfig().validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"zero backfill":   func(c *Config) { c.InitialBackfillDays = 0 },
		"hour 24":         func(c *Config) { c.SyncHour = 24 },
		"zero retryAfter": func(c *Config) { c.RetryAfter = 0 },
		"zero batch":      func(c *Config) { c.DueBatchSize = 0 },
		"no attempts":     func(c *Config) { c.Retry.Attempts = 0 },
		"max below base":  func(c *Config) { c.Retry.MaxDelay = c.Retry.BaseDelay / 2 },
	} {
		c := DefaultConfig()
		mutate(&c)
		if _, err := New(nil, nil, c); err == nil {
			t.Errorf("%s: want config error", name)
		}
	}

	user := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	r := &domain.DateRange{Start: d(t, "2026-01-02"), End: d(t, "2026-01-01")}
	for name, req := range map[string]Request{
		"no user":            {Trigger: domain.SyncTriggerManual},
		"manual with range":  {UserID: user, Trigger: domain.SyncTriggerManual, Range: &domain.DateRange{Start: d(t, "2026-01-01"), End: d(t, "2026-01-01")}},
		"backfill w/o range": {UserID: user, Trigger: domain.SyncTriggerBackfill},
		"inverted backfill":  {UserID: user, Trigger: domain.SyncTriggerBackfill, Range: r},
		"unknown trigger":    {UserID: user, Trigger: "cron"},
	} {
		if err := req.validate(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestOutcome_StatusAndMessage(t *testing.T) {
	o := &outcome{}
	if o.status() != domain.SyncSucceeded || o.message() != "" {
		t.Fatal("empty outcome must succeed silently")
	}
	o.errs = []error{errors.New("a"), errors.New("b")}
	if o.status() != domain.SyncFailed {
		t.Error("only failures: want failed")
	}
	o.succeeded = 1
	o.errs = append(o.errs, errors.New("c"), errors.New(strings.Repeat("x", 3000)))
	if o.status() != domain.SyncPartial {
		t.Error("mixed: want partial")
	}
	if msg := o.message(); msg != "a; b; c; (+1 more)" {
		t.Errorf("message = %q", msg)
	}
	o.errs = []error{errors.New(strings.Repeat("y", 3000))}
	if msg := o.message(); len(msg) > 2010 || !strings.HasSuffix(msg, "…") {
		t.Errorf("long message not truncated: %d bytes", len(msg))
	}
	o.errs = []error{errors.New(strings.Repeat("y", 1999) + "åäö")} // 2-byte rune straddles the cut
	if msg := o.message(); !utf8.ValidString(msg) {
		t.Errorf("truncation split a rune: %q", msg[len(msg)-6:])
	}
}

func TestOutcome_ScrubsTokens(t *testing.T) {
	o := &outcome{secrets: []string{"", "fake-access-123", "fake-refresh-456"}}
	o.errs = []error{errors.New("401 for Bearer fake-access-123; refresh fake-refresh-456 rejected")}
	if msg := o.message(); strings.Contains(msg, "fake-") || strings.Count(msg, "[REDACTED]") != 2 {
		t.Errorf("message = %q", msg)
	}
}
