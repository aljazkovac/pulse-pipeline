// Package sync is Pulse Pipeline's ingestion orchestrator (ADR-0003). It pulls
// one user's data from a domain.HealthProvider into the store, audits every
// run in sync_runs, and schedules the next nightly run.
//
// Ingestion is idempotent: every write is a natural-key upsert, so a run can
// be retried, overlapped or repeated for the same range and converges to the
// same rows. Each run is split into independent units (one provider request
// plus its upsert); transient provider errors are retried with exponential
// backoff, and a unit that still fails makes the run partial rather than
// discarding what the other units ingested.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
)

var (
	// ErrUnknownProvider means no HealthProvider is registered for a connection.
	ErrUnknownProvider = errors.New("sync: no provider registered")
	// ErrConnectionInactive means the connection is revoked or in error.
	ErrConnectionInactive = errors.New("sync: connection is not active")
)

// Store is the subset of *store.Store the orchestrator needs. Every method
// except ListConnectionsDueForSync is scoped by user ID.
type Store interface {
	GetUser(ctx context.Context, id uuid.UUID) (domain.User, error)
	GetConnection(ctx context.Context, userID uuid.UUID, provider domain.Provider) (domain.ConnectionInfo, error)
	GetConnectionSyncCursor(ctx context.Context, userID, connectionID uuid.UUID) ([]byte, error)
	MarkConnectionSynced(ctx context.Context, userID, connectionID uuid.UUID, syncedAt, nextSyncAt time.Time, cursor []byte) error
	RescheduleConnection(ctx context.Context, userID, connectionID uuid.UUID, nextSyncAt time.Time) error
	SetConnectionStatus(ctx context.Context, userID, connectionID uuid.UUID, status domain.ConnectionStatus) error
	// ListConnectionsDueForSync is system context only (AGENTS.md §1.1).
	ListConnectionsDueForSync(ctx context.Context, now time.Time, limit int32) ([]domain.DueConnection, error)

	AbandonStaleSyncRuns(ctx context.Context, userID uuid.UUID, provider domain.Provider, olderThan time.Duration) (int64, error)
	StartSyncRun(ctx context.Context, userID uuid.UUID, provider domain.Provider, trigger domain.SyncTrigger, r domain.DateRange) (domain.SyncRun, error)
	TouchSyncRun(ctx context.Context, userID, runID uuid.UUID) error
	FinishSyncRun(ctx context.Context, userID, runID uuid.UUID, status domain.SyncStatus, records int32, errMsg string) (domain.SyncRun, error)

	UpsertDailySummaries(ctx context.Context, userID uuid.UUID, summaries []domain.DailySummary) (int, error)
	UpsertSleepSessions(ctx context.Context, userID uuid.UUID, sessions []domain.SleepSession) (int, error)
	UpsertIntradayMetrics(ctx context.Context, userID uuid.UUID, provider domain.Provider, metric domain.MetricType, points []domain.IntradayPoint) (int64, error)
}

var _ Store = (*store.Store)(nil)

// CredentialStore loads and persists a connection's decrypted OAuth tokens.
type CredentialStore interface {
	LoadCredentials(ctx context.Context, userID, connectionID uuid.UUID) (domain.Credentials, error)
	SaveCredentials(ctx context.Context, userID, connectionID uuid.UUID, creds domain.Credentials) error
}

// NoCredentials serves providers that need no OAuth tokens (the mock). The
// encrypted token store replaces it with GoogleHealthProvider (ADR-0002 §3).
type NoCredentials struct{}

// LoadCredentials returns empty credentials, which never need refreshing.
func (NoCredentials) LoadCredentials(context.Context, uuid.UUID, uuid.UUID) (domain.Credentials, error) {
	return domain.Credentials{}, nil
}

// SaveCredentials fails: there is nowhere to persist refreshed tokens.
func (NoCredentials) SaveCredentials(context.Context, uuid.UUID, uuid.UUID, domain.Credentials) error {
	return errors.New("sync: no credential store configured")
}

// RetryPolicy bounds retries of transient provider errors. Attempt n waits a
// random delay in [d/2, d], where d = min(MaxDelay, BaseDelay·2^(n-1)).
type RetryPolicy struct {
	Attempts  int // total tries, including the first
	BaseDelay time.Duration
	MaxDelay  time.Duration
}

func (p RetryPolicy) delay(attempt int) time.Duration {
	d := float64(p.BaseDelay) * math.Pow(2, float64(attempt-1))
	d = math.Min(d, float64(p.MaxDelay))
	half := int64(d / 2)
	return time.Duration(half + rand.Int64N(half+1))
}

// Config tunes the orchestrator. Start from DefaultConfig and override.
type Config struct {
	// InitialBackfillDays is how far back the first sync of a connection reaches.
	InitialBackfillDays int
	// OverlapDays re-fetches days before the cursor, which providers revise
	// after the fact (late uploads, re-scored sleep).
	OverlapDays int
	// ChunkDays is the largest date range per daily-summary or sleep request.
	ChunkDays int
	// IntradayChunkDays is the largest span per intraday request.
	IntradayChunkDays int
	// SyncHour is the local hour (0–23) of the user's nightly sync.
	SyncHour int
	// RetryAfter delays the next scheduled attempt after a partial or failed run.
	RetryAfter time.Duration
	// StaleRunAfter is how long a running run may go without a heartbeat
	// (sent after every unit) before it is considered abandoned by a crashed
	// worker. It must exceed the longest single unit, retries included.
	StaleRunAfter time.Duration
	// RefreshBefore refreshes credentials expiring within this window.
	RefreshBefore time.Duration
	// DueBatchSize caps connections processed per RunDue call.
	DueBatchSize int32
	// Retry governs retries of domain.ErrTransient provider errors.
	Retry RetryPolicy

	// Credentials defaults to NoCredentials.
	Credentials CredentialStore
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Now defaults to time.Now; tests inject a fixed clock.
	Now func() time.Time
	// Sleep waits between retries; it must return early with ctx's error.
	Sleep func(ctx context.Context, d time.Duration) error
}

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{
		InitialBackfillDays: 30,
		OverlapDays:         3,
		ChunkDays:           31,
		IntradayChunkDays:   7,
		SyncHour:            4,
		RetryAfter:          time.Hour,
		StaleRunAfter:       2 * time.Hour,
		RefreshBefore:       5 * time.Minute,
		DueBatchSize:        50,
		Retry:               RetryPolicy{Attempts: 4, BaseDelay: time.Second, MaxDelay: 30 * time.Second},
	}
}

func (c Config) validate() error {
	switch {
	case c.InitialBackfillDays < 1, c.OverlapDays < 0, c.ChunkDays < 1, c.IntradayChunkDays < 1:
		return errors.New("day counts must be positive (overlap may be zero)")
	case c.SyncHour < 0 || c.SyncHour > 23:
		return fmt.Errorf("sync hour %d outside 0–23", c.SyncHour)
	case c.RetryAfter <= 0, c.StaleRunAfter <= 0, c.RefreshBefore < 0:
		return errors.New("durations must be positive")
	case c.DueBatchSize < 1:
		return errors.New("due batch size must be positive")
	case c.Retry.Attempts < 1, c.Retry.BaseDelay <= 0, c.Retry.MaxDelay < c.Retry.BaseDelay:
		return errors.New("retry policy needs at least one attempt and 0 < base delay <= max delay")
	}
	return nil
}

// Syncer runs syncs. It is safe for concurrent use; the store's one-running-
// sync-per-connection guard serializes work on the same connection.
type Syncer struct {
	store     Store
	providers map[domain.Provider]domain.HealthProvider
	cfg       Config
	creds     CredentialStore
	log       *slog.Logger
	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error
}

// New returns a Syncer that dispatches connections to providers by Name().
func New(st Store, providers []domain.HealthProvider, cfg Config) (*Syncer, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("sync: invalid config: %w", err)
	}
	s := &Syncer{
		store: st, providers: map[domain.Provider]domain.HealthProvider{}, cfg: cfg,
		creds: cfg.Credentials, log: cfg.Logger, now: cfg.Now, sleep: cfg.Sleep,
	}
	for _, p := range providers {
		if _, dup := s.providers[p.Name()]; dup {
			return nil, fmt.Errorf("sync: provider %q registered twice", p.Name())
		}
		s.providers[p.Name()] = p
	}
	if s.creds == nil {
		s.creds = NoCredentials{}
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.sleep == nil {
		s.sleep = sleepContext
	}
	return s, nil
}

// Request asks for one sync of a user's connection to a provider.
type Request struct {
	UserID   uuid.UUID
	Provider domain.Provider
	Trigger  domain.SyncTrigger
	// Range is required for backfills and must be nil otherwise: scheduled
	// and manual syncs resume from the connection's cursor.
	Range *domain.DateRange
}

func (r Request) validate() error {
	switch r.Trigger {
	case domain.SyncTriggerScheduled, domain.SyncTriggerManual:
		if r.Range != nil {
			return fmt.Errorf("%s sync takes no explicit range", r.Trigger)
		}
	case domain.SyncTriggerBackfill:
		if r.Range == nil {
			return errors.New("backfill needs a date range")
		}
		if err := r.Range.Validate(); err != nil {
			return fmt.Errorf("backfill: %w", err)
		}
	default:
		return fmt.Errorf("unknown trigger %q", r.Trigger)
	}
	if r.UserID == uuid.Nil {
		return errors.New("user ID is required")
	}
	return nil
}

// SyncConnection runs one sync and returns its finished audit record. A run
// that ingested nothing or only part of its range is still returned with a
// nil error; inspect run.Status. Errors mean no run could be started (unknown
// or inactive connection, another sync in progress: store.ErrSyncInProgress)
// or its bookkeeping could not be written.
func (s *Syncer) SyncConnection(ctx context.Context, req Request) (domain.SyncRun, error) {
	if err := req.validate(); err != nil {
		return domain.SyncRun{}, fmt.Errorf("sync: %w", err)
	}
	hp, ok := s.providers[req.Provider]
	if !ok {
		return domain.SyncRun{}, fmt.Errorf("%w for %q", ErrUnknownProvider, req.Provider)
	}
	user, err := s.store.GetUser(ctx, req.UserID)
	if err != nil {
		return domain.SyncRun{}, fmt.Errorf("sync: %w", err)
	}
	info, err := s.store.GetConnection(ctx, req.UserID, req.Provider)
	if err != nil {
		return domain.SyncRun{}, fmt.Errorf("sync: %w", err)
	}
	if info.Status != domain.ConnectionActive {
		return domain.SyncRun{}, fmt.Errorf("%w: %s connection for user %s is %s", ErrConnectionInactive, req.Provider, req.UserID, info.Status)
	}
	rawCursor, err := s.store.GetConnectionSyncCursor(ctx, req.UserID, info.ID)
	if err != nil {
		return domain.SyncRun{}, fmt.Errorf("sync: %w", err)
	}
	cur, err := parseCursor(rawCursor)
	if err != nil {
		return domain.SyncRun{}, fmt.Errorf("sync: connection %s: %w", info.ID, err)
	}

	loc := user.Location()
	today := domain.DateOf(s.now().In(loc))
	r, err := planRange(req, cur.syncedThrough, today, s.cfg)
	if err != nil {
		return domain.SyncRun{}, fmt.Errorf("sync: user %s: %w", req.UserID, err)
	}

	if n, err := s.store.AbandonStaleSyncRuns(ctx, req.UserID, req.Provider, s.cfg.StaleRunAfter); err != nil {
		return domain.SyncRun{}, fmt.Errorf("sync: %w", err)
	} else if n > 0 {
		s.log.Warn("abandoned stale sync runs", "user_id", req.UserID, "provider", req.Provider, "count", n)
	}
	run, err := s.store.StartSyncRun(ctx, req.UserID, req.Provider, req.Trigger, r)
	if err != nil {
		return domain.SyncRun{}, fmt.Errorf("sync: %w", err)
	}

	conn := domain.Connection{ID: info.ID, UserID: req.UserID, Provider: req.Provider, Location: loc}
	out := s.ingest(ctx, hp, conn, run.ID, r)
	return s.finish(ctx, req, info, loc, run, cur, out)
}

// outcome accumulates the result of a run's units.
type outcome struct {
	records      int64
	succeeded    int
	errs         []error
	unauthorized bool
	abandoned    bool     // another worker declared the run stale; touch nothing
	secrets      []string // token values to scrub from the stored message
}

func (o *outcome) status() domain.SyncStatus {
	switch {
	case len(o.errs) == 0:
		return domain.SyncSucceeded
	case o.succeeded > 0:
		return domain.SyncPartial
	default:
		return domain.SyncFailed
	}
}

// message summarizes errors for sync_runs.error_message and logs. Provider
// adapters must keep tokens out of their errors; as a backstop, the token
// values used in this run are scrubbed.
func (o *outcome) message() string {
	const shown, maxLen = 3, 2000
	var parts []string
	for i, err := range o.errs {
		if i == shown {
			parts = append(parts, fmt.Sprintf("(+%d more)", len(o.errs)-shown))
			break
		}
		parts = append(parts, err.Error())
	}
	msg := strings.Join(parts, "; ")
	for _, secret := range o.secrets {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "[REDACTED]")
		}
	}
	if len(msg) > maxLen {
		// Cut on a byte budget, then drop any rune split in half: PostgreSQL
		// rejects invalid UTF-8.
		msg = strings.ToValidUTF8(msg[:maxLen], "") + "…"
	}
	return msg
}

// unit is one provider request plus its upsert, retried and failed alone.
type unit struct {
	name string
	run  func(ctx context.Context) (int64, error)
}

func (s *Syncer) ingest(ctx context.Context, hp domain.HealthProvider, conn domain.Connection, runID uuid.UUID, r domain.DateRange) *outcome {
	out := &outcome{}
	fail := func(err error) {
		out.errs = append(out.errs, err)
		if errors.Is(err, domain.ErrUnauthorized) {
			out.unauthorized = true
		}
	}

	creds, err := s.credentials(ctx, hp, conn, out)
	if err != nil {
		fail(err)
		return out
	}
	conn.Credentials = creds

	for _, u := range s.units(hp, conn, r) {
		if err := ctx.Err(); err != nil {
			fail(fmt.Errorf("stopped before %s: %w", u.name, err))
			break
		}
		n, err := u.run(ctx)
		if err != nil {
			fail(fmt.Errorf("%s: %w", u.name, err))
		} else {
			out.records += n
			out.succeeded++
		}
		if out.unauthorized {
			break // every other request would be rejected too
		}
		if err := s.store.TouchSyncRun(ctx, conn.UserID, runID); errors.Is(err, store.ErrNotFound) {
			// Declared stale by another worker, which may be syncing now.
			out.abandoned = true
			fail(errors.New("run was abandoned as stale by another worker; stopped"))
			break
		} else if err != nil && ctx.Err() == nil {
			s.log.Warn("could not record sync heartbeat", "run_id", runID, "error", err)
		}
	}
	return out
}

// credentials loads the connection's tokens and refreshes them if they are
// about to expire. Zero ExpiresAt means the provider needs no refresh. Every
// token it sees is registered in out for scrubbing.
func (s *Syncer) credentials(ctx context.Context, hp domain.HealthProvider, conn domain.Connection, out *outcome) (domain.Credentials, error) {
	creds, err := s.creds.LoadCredentials(ctx, conn.UserID, conn.ID)
	if err != nil {
		return domain.Credentials{}, fmt.Errorf("loading credentials: %w", err)
	}
	out.secrets = append(out.secrets, creds.AccessToken, creds.RefreshToken)
	if creds.ExpiresAt.IsZero() || creds.ExpiresAt.After(s.now().Add(s.cfg.RefreshBefore)) {
		return creds, nil
	}
	conn.Credentials = creds
	fresh, err := retry(ctx, s, func() (domain.Credentials, error) { return hp.RefreshAuth(ctx, conn) })
	if err != nil {
		return domain.Credentials{}, fmt.Errorf("refreshing credentials: %w", err)
	}
	out.secrets = append(out.secrets, fresh.AccessToken, fresh.RefreshToken)
	if err := s.creds.SaveCredentials(ctx, conn.UserID, conn.ID, fresh); err != nil {
		return domain.Credentials{}, fmt.Errorf("saving refreshed credentials: %w", err)
	}
	return fresh, nil
}

func (s *Syncer) units(hp domain.HealthProvider, conn domain.Connection, r domain.DateRange) []unit {
	var units []unit
	for _, c := range chunkDates(r, s.cfg.ChunkDays) {
		units = append(units,
			unit{"daily summaries " + rangeString(c), func(ctx context.Context) (int64, error) {
				rows, err := retry(ctx, s, func() ([]domain.DailySummary, error) { return hp.FetchDailySummaries(ctx, conn, c) })
				if err != nil {
					return 0, fmt.Errorf("fetching: %w", err)
				}
				for _, row := range rows {
					if row.Provider != hp.Name() {
						return 0, fmt.Errorf("provider returned a %q summary", row.Provider)
					}
				}
				n, err := s.store.UpsertDailySummaries(ctx, conn.UserID, rows)
				return int64(n), err
			}},
			unit{"sleep sessions " + rangeString(c), func(ctx context.Context) (int64, error) {
				rows, err := retry(ctx, s, func() ([]domain.SleepSession, error) { return hp.FetchSleepSessions(ctx, conn, c) })
				if err != nil {
					return 0, fmt.Errorf("fetching: %w", err)
				}
				for _, row := range rows {
					if row.Provider != hp.Name() {
						return 0, fmt.Errorf("provider returned a %q sleep session", row.Provider)
					}
				}
				n, err := s.store.UpsertSleepSessions(ctx, conn.UserID, rows)
				return int64(n), err
			}},
		)
	}
	for _, m := range domain.MetricTypes {
		for _, c := range chunkDates(r, s.cfg.IntradayChunkDays) {
			span := localSpan(c, conn.Location)
			units = append(units, unit{string(m) + " " + rangeString(c), func(ctx context.Context) (int64, error) {
				pts, err := retry(ctx, s, func() ([]domain.IntradayPoint, error) { return hp.FetchIntradayMetrics(ctx, conn, m, span) })
				if err != nil {
					return 0, fmt.Errorf("fetching: %w", err)
				}
				return s.store.UpsertIntradayMetrics(ctx, conn.UserID, hp.Name(), m, pts)
			}})
		}
	}
	return units
}

// finish records the outcome. It runs on a context detached from ctx's
// cancellation so a worker shutting down mid-run still closes its audit row.
func (s *Syncer) finish(ctx context.Context, req Request, info domain.ConnectionInfo, loc *time.Location, run domain.SyncRun, cur cursor, out *outcome) (domain.SyncRun, error) {
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	status, now := out.status(), s.now()
	if out.abandoned {
		// The run row is already closed and another run may own the
		// connection now: write nothing.
		return run, fmt.Errorf("sync: run %s was abandoned as stale while still running; raise StaleRunAfter", run.ID)
	}
	var bookkeeping error
	switch {
	case out.unauthorized:
		// Stop scheduling until the user reconnects the provider.
		bookkeeping = s.store.SetConnectionStatus(bctx, req.UserID, info.ID, domain.ConnectionError)
	case req.Trigger == domain.SyncTriggerBackfill:
		// Backfills fill history and touch no connection state: the cursor
		// and schedule read before the run may be stale by now.
	case status == domain.SyncSucceeded:
		var raw []byte
		if raw, bookkeeping = cur.advance(run.Range.End); bookkeeping == nil {
			bookkeeping = s.store.MarkConnectionSynced(bctx, req.UserID, info.ID, now, nextNightly(now, loc, s.cfg.SyncHour), raw)
		}
	case req.Trigger == domain.SyncTriggerScheduled:
		// Keep the cursor so the next attempt re-covers the whole range.
		bookkeeping = s.store.RescheduleConnection(bctx, req.UserID, info.ID, now.Add(s.cfg.RetryAfter))
	}

	finished, err := s.store.FinishSyncRun(bctx, req.UserID, run.ID, status, clampInt32(out.records), out.message())
	if err != nil {
		finished = run // keep the run's identity so callers know it started
	}
	if err = errors.Join(bookkeeping, err); err != nil {
		return finished, fmt.Errorf("sync: recording run %s: %w", run.ID, err)
	}
	return finished, nil
}

// RunDue syncs connections whose next_sync_at has passed, up to
// DueBatchSize, and returns how many runs it completed. It is the worker's
// top-level boundary, so per-connection problems are logged, not returned;
// connections that cannot start are pushed back by RetryAfter so they do not
// spin on every poll.
func (s *Syncer) RunDue(ctx context.Context) (int, error) {
	due, err := s.store.ListConnectionsDueForSync(ctx, s.now(), s.cfg.DueBatchSize)
	if err != nil {
		return 0, fmt.Errorf("sync: %w", err)
	}
	done := 0
	for _, c := range due {
		if err := ctx.Err(); err != nil {
			return done, fmt.Errorf("sync: stopped after %d runs: %w", done, err)
		}
		log := s.log.With("user_id", c.UserID, "provider", c.Provider)
		run, err := s.SyncConnection(ctx, Request{UserID: c.UserID, Provider: c.Provider, Trigger: domain.SyncTriggerScheduled})
		switch {
		case err != nil && run.ID == uuid.Nil && ctx.Err() != nil:
			return done, fmt.Errorf("sync: stopped after %d runs: %w", done, ctx.Err())
		case errors.Is(err, store.ErrSyncInProgress):
			log.Info("skipping connection: sync already in progress")
		case err != nil && run.ID == uuid.Nil:
			log.Error("could not start scheduled sync", "error", err)
			if rerr := s.store.RescheduleConnection(ctx, c.UserID, c.ID, s.now().Add(s.cfg.RetryAfter)); rerr != nil {
				log.Error("could not reschedule connection", "error", rerr)
			}
		case err != nil:
			log.Error("sync finished but bookkeeping failed", "run_id", run.ID, "error", err)
		default:
			done++
			level := slog.LevelInfo
			if run.Status != domain.SyncSucceeded {
				level = slog.LevelWarn
			}
			log.Log(ctx, level, "sync finished", "run_id", run.ID, "status", run.Status,
				"range", rangeString(run.Range), "records", run.RecordsIngested, "error", run.ErrorMessage)
		}
	}
	return done, nil
}

// retry calls fn until it succeeds, fails with a non-transient error, or
// exhausts the policy's attempts.
func retry[T any](ctx context.Context, s *Syncer, fn func() (T, error)) (T, error) {
	for attempt := 1; ; attempt++ {
		v, err := fn()
		if err == nil || !errors.Is(err, domain.ErrTransient) || attempt >= s.cfg.Retry.Attempts {
			if err != nil && attempt > 1 {
				err = fmt.Errorf("after %d attempts: %w", attempt, err)
			}
			return v, err
		}
		if serr := s.sleep(ctx, s.cfg.Retry.delay(attempt)); serr != nil {
			var zero T
			return zero, fmt.Errorf("waiting to retry (%v): %w", err, serr)
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func clampInt32(n int64) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n)
}
