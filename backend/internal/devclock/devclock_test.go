package devclock_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/devclock"
)

func TestFromEnv(t *testing.T) {
	want := time.Date(2026, 11, 15, 14, 30, 0, 0, time.UTC)

	t.Run("unset uses real clock", func(t *testing.T) {
		before := time.Now()
		res, err := devclock.FromEnv("development", "  ")
		if err != nil || !res.Pinned.IsZero() || res.Ignored {
			t.Fatalf("FromEnv = %+v, %v", res, err)
		}
		got := res.Now()
		after := time.Now()
		if got.Before(before) || got.After(after) {
			t.Errorf("Now() = %s, want within [%s, %s]", got, before, after)
		}
	})

	t.Run("pinned in development", func(t *testing.T) {
		res, err := devclock.FromEnv("development", "2026-11-15T15:30:00+01:00")
		if err != nil || !res.Pinned.Equal(want) || res.Pinned.Location() != time.UTC || res.Ignored {
			t.Fatalf("FromEnv = %+v, %v; want %s UTC", res, err, want)
		}
		if got := res.Now(); !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("Now() = %s (%s), want %s UTC", got, got.Location(), want)
		}
	})

	t.Run("ignored outside development", func(t *testing.T) {
		for _, env := range []string{"", "production", "staging", "dev"} {
			res, err := devclock.FromEnv(env, "not-even-a-timestamp")
			if err != nil || !res.Pinned.IsZero() || !res.Ignored {
				t.Fatalf("env %q: FromEnv = %+v, %v; want ignored real clock", env, res, err)
			}
			if time.Since(res.Now()) > 5*time.Second {
				t.Errorf("env %q: Now() = %s, want real time", env, res.Now())
			}
		}
	})

	t.Run("malformed in development fails", func(t *testing.T) {
		for _, bad := range []string{"tomorrow", "2026-11-15", "2026-11-15 14:30:00"} {
			_, err := devclock.FromEnv("development", bad)
			if err == nil || !strings.Contains(err.Error(), "PULSE_DEV_NOW") {
				t.Errorf("FromEnv(%q) = %v, want PULSE_DEV_NOW error", bad, err)
			}
		}
	})
}
