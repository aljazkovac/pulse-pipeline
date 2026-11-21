// Package mock implements a deterministic, synthetic domain.HealthProvider for
// local development and automated tests.
//
// Every value is a pure function of (Options.Seed, user ID, local calendar
// day), so the same request always returns the same data, a range fetched in
// one call equals the same range fetched in pieces, and a backfill reproduces
// what a nightly sync saw. The data is plausible rather than physiologically
// exact: resting heart rate and HRV drift around a per-user baseline, nights
// follow ~90-minute sleep cycles, steps come in walking bouts shaped by the
// time of day, and some days have realistic gaps (watch off the wrist all day,
// a charging gap, a missing SpO2 or HRV reading).
//
// Daily summaries are aggregates of the intraday series the provider returns
// for the same local day, so dashboards and tests can cross-check them.
//
// Floating-point rounding may differ in the last bit between CPU
// architectures (Go permits fused multiply-add), so compare datasets produced
// on the same architecture rather than committing golden values.
package mock

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
)

// MaxIntradayRange is the widest TimeRange FetchIntradayMetrics accepts.
// Real provider APIs cap request windows too; callers fetch in chunks.
const MaxIntradayRange = 31 * 24 * time.Hour

// MaxDateRangeDays is the most days FetchDailySummaries and FetchSleepSessions
// accept in one call; longer backfills are fetched in chunks.
const MaxDateRangeDays = 366

// AccessTokenTTL is the lifetime of credentials returned by RefreshAuth.
const AccessTokenTTL = time.Hour

// Options configures a Provider.
type Options struct {
	// Seed selects the synthetic dataset. Different seeds give unrelated data.
	Seed uint64
	// Now returns the current time. No data is returned after it: future
	// days are empty, today is partial, and a night only appears once its
	// sleep session has ended. Defaults to time.Now.
	Now func() time.Time
}

// Provider is the synthetic HealthProvider. It is safe for concurrent use.
type Provider struct {
	seed uint64
	now  func() time.Time
}

var _ domain.HealthProvider = (*Provider)(nil)

// New returns a mock provider.
func New(opts Options) *Provider {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Provider{seed: opts.Seed, now: now}
}

// Name returns domain.ProviderMock.
func (p *Provider) Name() domain.Provider { return domain.ProviderMock }

// FetchDailySummaries returns one summary per local day in r that has data.
// Days the watch was off the wrist, and days after Now, are omitted.
func (p *Provider) FetchDailySummaries(ctx context.Context, conn domain.Connection, r domain.DateRange) ([]domain.DailySummary, error) {
	if err := checkRequest(ctx, conn); err != nil {
		return nil, fmt.Errorf("mock: fetch daily summaries for user %s: %w", conn.UserID, err)
	}
	if err := checkDates(r); err != nil {
		return nil, fmt.Errorf("mock: fetch daily summaries for user %s: %w", conn.UserID, err)
	}
	g := p.newGenerator(conn)
	out := []domain.DailySummary{}
	for d := r.Start; !r.End.Before(d); d = d.AddDays(1) {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("mock: fetch daily summaries for user %s: %w", conn.UserID, err)
		}
		if s, ok := g.summary(d); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// FetchIntradayMetrics returns samples of metric with RecordedAt in r, oldest
// first. Heart rate, steps and calories are per-minute samples (a minute is
// returned once it has fully elapsed before Now); HRV and SpO2 are sampled
// every five minutes during completed main sleep sessions.
func (p *Provider) FetchIntradayMetrics(ctx context.Context, conn domain.Connection, metric domain.MetricType, r domain.TimeRange) ([]domain.IntradayPoint, error) {
	if err := checkRequest(ctx, conn); err != nil {
		return nil, fmt.Errorf("mock: fetch %s metrics for user %s: %w", metric, conn.UserID, err)
	}
	if !metric.Valid() {
		return nil, fmt.Errorf("mock: fetch intraday metrics for user %s: unknown metric type %q", conn.UserID, metric)
	}
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("mock: fetch %s metrics for user %s: %w", metric, conn.UserID, err)
	}
	if r.End.Sub(r.Start) > MaxIntradayRange {
		return nil, fmt.Errorf("mock: fetch %s metrics for user %s: range %s exceeds maximum %s", metric, conn.UserID, r.End.Sub(r.Start), MaxIntradayRange)
	}

	g := p.newGenerator(conn)
	first := domain.DateOf(r.Start.In(conn.Location))
	last := domain.DateOf(r.End.Add(-time.Nanosecond).In(conn.Location))
	inRange := func(t time.Time) bool { return !t.Before(r.Start) && t.Before(r.End) }

	out := []domain.IntradayPoint{}
	switch metric {
	case domain.MetricHeartRate, domain.MetricSteps, domain.MetricCalories:
		for d := first; !last.Before(d); d = d.AddDays(1) {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("mock: fetch %s metrics for user %s: %w", metric, conn.UserID, err)
			}
			for _, m := range g.minutes(d) {
				if inRange(m.at) {
					out = append(out, domain.IntradayPoint{Metric: metric, RecordedAt: m.at, Value: m.value(metric)})
				}
			}
		}
	case domain.MetricHRVRMSSD, domain.MetricSpO2:
		// A main sleep belongs to its wake-up date but may start the evening
		// before, so the session waking the day after `last` can overlap r.
		for d := first; !last.AddDays(1).Before(d); d = d.AddDays(1) {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("mock: fetch %s metrics for user %s: %w", metric, conn.UserID, err)
			}
			for _, pt := range g.sessionSamples(d, metric) {
				if inRange(pt.RecordedAt) {
					out = append(out, pt)
				}
			}
		}
	}
	return out, nil
}

// FetchSleepSessions returns completed sessions whose local wake-up date is in
// r: at most one main sleep and one nap per day.
func (p *Provider) FetchSleepSessions(ctx context.Context, conn domain.Connection, r domain.DateRange) ([]domain.SleepSession, error) {
	if err := checkRequest(ctx, conn); err != nil {
		return nil, fmt.Errorf("mock: fetch sleep sessions for user %s: %w", conn.UserID, err)
	}
	if err := checkDates(r); err != nil {
		return nil, fmt.Errorf("mock: fetch sleep sessions for user %s: %w", conn.UserID, err)
	}
	g := p.newGenerator(conn)
	out := []domain.SleepSession{}
	for d := r.Start; !r.End.Before(d); d = d.AddDays(1) {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("mock: fetch sleep sessions for user %s: %w", conn.UserID, err)
		}
		pl := g.plan(d)
		for _, s := range []*sleepPlan{pl.main, pl.nap} {
			if g.completed(s) {
				out = append(out, s.copySession())
			}
		}
	}
	return out, nil
}

// RefreshAuth returns fresh, obviously fake credentials valid for
// AccessTokenTTL, keeping the refresh token. It fails like a real provider
// when the connection has no refresh token.
func (p *Provider) RefreshAuth(ctx context.Context, conn domain.Connection) (domain.Credentials, error) {
	if err := checkRequest(ctx, conn); err != nil {
		return domain.Credentials{}, fmt.Errorf("mock: refresh auth for user %s: %w", conn.UserID, err)
	}
	if conn.Credentials.RefreshToken == "" {
		return domain.Credentials{}, fmt.Errorf("mock: refresh auth for user %s: no refresh token", conn.UserID)
	}
	now := p.now()
	var nonce [8]byte
	binary.BigEndian.PutUint64(nonce[:], uint64(now.UnixNano()))
	sum := p.hash(conn.UserID, "access-token/"+hex.EncodeToString(nonce[:]))
	return domain.Credentials{
		AccessToken:  "mock-access-" + hex.EncodeToString(sum[:12]),
		RefreshToken: conn.Credentials.RefreshToken,
		ExpiresAt:    now.Add(AccessTokenTTL).UTC(),
	}, nil
}

// checkRequest validates what every call needs, failing fast like a real
// adapter would on a mis-wired connection.
// checkDates validates an inclusive date range and its size.
func checkDates(r domain.DateRange) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if days := int(r.End.Time().Sub(r.Start.Time()).Hours()/24) + 1; days > MaxDateRangeDays {
		return fmt.Errorf("range of %d days exceeds maximum %d", days, MaxDateRangeDays)
	}
	return nil
}

func checkRequest(ctx context.Context, conn domain.Connection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch {
	case conn.UserID == uuid.Nil:
		return errors.New("connection user ID is required")
	case conn.Location == nil:
		return errors.New("connection location is required")
	case conn.Provider != domain.ProviderMock:
		return fmt.Errorf("connection provider %q is not %q", conn.Provider, domain.ProviderMock)
	}
	return nil
}

// hash derives 32 deterministic bytes from the seed, the user and a key.
func (p *Provider) hash(user uuid.UUID, key string) [32]byte {
	buf := make([]byte, 0, 8+len(user)+len(key))
	buf = binary.BigEndian.AppendUint64(buf, p.seed)
	buf = append(buf, user[:]...)
	buf = append(buf, key...)
	return sha256.Sum256(buf)
}

// rng returns an independent random stream for (seed, user, key). Each kind of
// value draws from its own stream so changing one never shifts another.
func (p *Provider) rng(user uuid.UUID, key string) *rand.Rand {
	sum := p.hash(user, key)
	return rand.New(rand.NewPCG(binary.BigEndian.Uint64(sum[:8]), binary.BigEndian.Uint64(sum[8:16])))
}
