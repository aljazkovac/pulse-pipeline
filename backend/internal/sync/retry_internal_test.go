package sync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
)

func TestSleepContext(t *testing.T) {
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("elapsed wait: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait: err = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("canceled wait did not return promptly")
	}
}

func TestRetry(t *testing.T) {
	transient := fmt.Errorf("upstream 503: %w", domain.ErrTransient)
	permanent := errors.New("upstream 400")
	newSyncer := func(sleep func(context.Context, time.Duration) error) *Syncer {
		cfg := DefaultConfig()
		cfg.Retry = RetryPolicy{Attempts: 3, BaseDelay: time.Second, MaxDelay: time.Second}
		cfg.Sleep = sleep
		s, err := New(nil, nil, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	noWait := func(context.Context, time.Duration) error { return nil }

	tests := []struct {
		name      string
		results   []error // returned by successive calls; nil succeeds
		sleep     func(context.Context, time.Duration) error
		canceled  bool // the context is already done
		wantCalls int
		wantErr   string // "" for success
	}{
		{"first try succeeds", []error{nil}, noWait, false, 1, ""},
		{"transient then success", []error{transient, transient, nil}, noWait, false, 3, ""},
		{"permanent error is not retried", []error{permanent}, noWait, false, 1, "upstream 400"},
		{"attempts exhausted", []error{transient, transient, transient}, noWait, false, 3, "after 3 attempts: upstream 503"},
		{"canceled while waiting to retry", []error{transient}, sleepContext, true, 1, "waiting to retry (upstream 503"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			calls := 0
			v, err := retry(ctx, newSyncer(tc.sleep), func() (int, error) {
				err := tc.results[calls]
				calls++
				if err != nil {
					return -1, err
				}
				return 42, nil
			})
			if calls != tc.wantCalls {
				t.Errorf("calls = %d, want %d", calls, tc.wantCalls)
			}
			switch {
			case tc.wantErr == "" && (err != nil || v != 42):
				t.Errorf("retry = %d, %v; want 42, nil", v, err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestNoCredentials(t *testing.T) {
	var c NoCredentials
	creds, err := c.LoadCredentials(context.Background(), uuid.New(), uuid.New())
	if err != nil || creds.AccessToken != "" || creds.RefreshToken != "" || !creds.ExpiresAt.IsZero() {
		t.Errorf("LoadCredentials = %v, %v; want empty", creds, err)
	}
	if err := c.SaveCredentials(context.Background(), domain.SyncRun{}, uuid.New(), domain.Credentials{AccessToken: "fake-token"}); err == nil {
		t.Error("SaveCredentials must fail without a credential store")
	}
}

func TestRangeString(t *testing.T) {
	day := d(t, "2026-11-20")
	if got := rangeString(domain.DateRange{Start: day, End: day}); got != "2026-11-20" {
		t.Errorf("single day = %q", got)
	}
	if got := rangeString(domain.DateRange{Start: day, End: day.AddDays(2)}); got != "2026-11-20..2026-11-22" {
		t.Errorf("span = %q", got)
	}
}
