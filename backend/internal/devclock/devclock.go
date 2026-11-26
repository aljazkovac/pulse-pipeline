// Package devclock parses PULSE_DEV_NOW for pulse-api and pulse-worker
// (ADR-0005 §8). Both binaries honor a fixed RFC 3339 clock only when
// ENV=development, so mock data and relative dates in E2E tests are
// deterministic while production deployments always read the real clock.
package devclock

import (
	"fmt"
	"strings"
	"time"
)

// Result holds the resolved clock and metadata for startup logging.
type Result struct {
	// Now returns the current time: fixed at Pinned when Pinned is non-zero,
	// or time.Now otherwise.
	Now func() time.Time
	// Pinned is the parsed UTC instant when ENV=development and PULSE_DEV_NOW
	// is set, or the zero Time otherwise.
	Pinned time.Time
	// Ignored is true when PULSE_DEV_NOW was non-empty outside ENV=development.
	Ignored bool
}

// FromEnv resolves PULSE_DEV_NOW given ENV. Outside ENV=development a non-empty
// value is ignored (Result.Ignored is true) and time.Now is used.
func FromEnv(env, raw string) (Result, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Result{Now: time.Now}, nil
	}
	if env != "development" {
		return Result{Now: time.Now, Ignored: true}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return Result{}, fmt.Errorf("PULSE_DEV_NOW %q: want an RFC 3339 timestamp", raw)
	}
	utc := t.UTC()
	return Result{
		Now:    func() time.Time { return utc },
		Pinned: utc,
	}, nil
}
