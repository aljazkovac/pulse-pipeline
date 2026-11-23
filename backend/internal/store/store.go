// Package store is Pulse Pipeline's PostgreSQL data access layer (ADR-0002 §5).
//
// It wraps the sqlc-generated queries in store/db, maps rows to domain types,
// and owns transactions. Every method takes a userID and scopes its SQL by it,
// with the exceptions AGENTS.md §1.1 allows, none of which returns health
// data or tokens: ListConnectionsDueForSync and DeleteExpiredSessions are
// system context for background jobs, and GetUserIDByIdentity and
// GetSessionByTokenHash are the authentication boundary that turns a
// validated credential into a user ID (ADR-0005 §2–3).
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store/db"
)

// ErrNotFound is returned when a requested row does not exist for the user.
var ErrNotFound = errors.New("store: not found")

// ErrSyncInProgress is returned by StartSyncRun when the user already has a
// running sync for the provider (ADR-0003).
var ErrSyncInProgress = errors.New("store: sync already in progress")

// intradayBatchSize caps the rows sent in one upsert statement.
const intradayBatchSize = 5000

// Store provides user-scoped access to Pulse Pipeline data.
type Store struct {
	pool *pgxpool.Pool
	q    *db.Queries
	tx   pgx.Tx
}

// New returns a Store backed by pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: db.New(pool)}
}

// inTx reuses an enclosing transaction or opens one for fn.
func (s *Store) inTx(ctx context.Context, fn func(*Store) error) error {
	if s.tx != nil {
		return fn(s)
	}
	if err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(&Store{pool: s.pool, q: s.q.WithTx(tx), tx: tx})
	}); err != nil {
		return fmt.Errorf("transaction: %w", err)
	}
	return nil
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

// CreateUser inserts a user with an IANA timezone.
func (s *Store) CreateUser(ctx context.Context, email, timezone string) (domain.User, error) {
	if _, err := time.LoadLocation(timezone); err != nil {
		return domain.User{}, fmt.Errorf("creating user: invalid timezone %q: %w", timezone, err)
	}
	row, err := s.q.CreateUser(ctx, db.CreateUserParams{Email: email, Timezone: timezone})
	if err != nil {
		return domain.User{}, fmt.Errorf("creating user: %w", err)
	}
	return domain.User{ID: row.ID, Email: row.Email, Timezone: row.Timezone, CreatedAt: row.CreatedAt.UTC()}, nil
}

// GetUser returns the user with id, or ErrNotFound.
func (s *Store) GetUser(ctx context.Context, id uuid.UUID) (domain.User, error) {
	row, err := s.q.GetUser(ctx, id)
	if err != nil {
		return domain.User{}, fmt.Errorf("getting user %s: %w", id, notFound(err))
	}
	return domain.User{ID: row.ID, Email: row.Email, Timezone: row.Timezone, CreatedAt: row.CreatedAt.UTC()}, nil
}

// ---------------------------------------------------------------------------
// Provider connections (non-secret columns only)
// ---------------------------------------------------------------------------

// CreateConnection registers a provider connection for a user.
func (s *Store) CreateConnection(ctx context.Context, userID uuid.UUID, provider domain.Provider, scopes []string) (domain.ConnectionInfo, error) {
	if scopes == nil {
		scopes = []string{}
	}
	row, err := s.q.CreateProviderConnection(ctx, db.CreateProviderConnectionParams{
		UserID: userID, Provider: string(provider), Scopes: scopes,
	})
	if err != nil {
		return domain.ConnectionInfo{}, fmt.Errorf("creating %s connection for user %s: %w", provider, userID, err)
	}
	return connectionInfo(db.GetProviderConnectionRow(row)), nil
}

// GetConnection returns the user's connection to provider, or ErrNotFound.
func (s *Store) GetConnection(ctx context.Context, userID uuid.UUID, provider domain.Provider) (domain.ConnectionInfo, error) {
	row, err := s.q.GetProviderConnection(ctx, db.GetProviderConnectionParams{UserID: userID, Provider: string(provider)})
	if err != nil {
		return domain.ConnectionInfo{}, fmt.Errorf("getting %s connection for user %s: %w", provider, userID, notFound(err))
	}
	return connectionInfo(row), nil
}

// GetConnectionSyncCursor returns the connection's provider-specific JSON cursor.
func (s *Store) GetConnectionSyncCursor(ctx context.Context, userID, connectionID uuid.UUID) ([]byte, error) {
	cursor, err := s.q.GetConnectionSyncCursor(ctx, db.GetConnectionSyncCursorParams{ID: connectionID, UserID: userID})
	if err != nil {
		return nil, fmt.Errorf("getting sync cursor for connection %s: %w", connectionID, notFound(err))
	}
	return cursor, nil
}

// MarkConnectionSynced records a successful sync and schedules the next one.
// cursor is provider-specific JSON; nil stores an empty object. It returns
// ErrNotFound if the connection does not belong to the user.
func (s *Store) MarkConnectionSynced(ctx context.Context, userID, connectionID uuid.UUID, syncedAt, nextSyncAt time.Time, cursor []byte) error {
	if cursor == nil {
		cursor = []byte("{}")
	}
	syncedAt = syncedAt.UTC()
	n, err := s.q.MarkConnectionSynced(ctx, db.MarkConnectionSyncedParams{
		LastSyncedAt: &syncedAt, NextSyncAt: nextSyncAt.UTC(), SyncCursor: cursor,
		ID: connectionID, UserID: userID,
	})
	if err != nil {
		return fmt.Errorf("marking connection %s synced: %w", connectionID, err)
	}
	if n == 0 {
		return fmt.Errorf("marking connection %s synced: %w", connectionID, ErrNotFound)
	}
	return nil
}

// RescheduleConnection moves the connection's next scheduled sync without
// recording a sync (used after a failed run). It returns ErrNotFound if the
// connection does not belong to the user.
func (s *Store) RescheduleConnection(ctx context.Context, userID, connectionID uuid.UUID, nextSyncAt time.Time) error {
	n, err := s.q.RescheduleConnection(ctx, db.RescheduleConnectionParams{NextSyncAt: nextSyncAt.UTC(), ID: connectionID, UserID: userID})
	if err != nil {
		return fmt.Errorf("rescheduling connection %s: %w", connectionID, err)
	}
	if n == 0 {
		return fmt.Errorf("rescheduling connection %s: %w", connectionID, ErrNotFound)
	}
	return nil
}

// SetConnectionStatus changes a connection's lifecycle status. It returns
// ErrNotFound if the connection does not belong to the user.
func (s *Store) SetConnectionStatus(ctx context.Context, userID, connectionID uuid.UUID, status domain.ConnectionStatus) error {
	n, err := s.q.SetConnectionStatus(ctx, db.SetConnectionStatusParams{Status: string(status), ID: connectionID, UserID: userID})
	if err != nil {
		return fmt.Errorf("setting connection %s status to %s: %w", connectionID, status, err)
	}
	if n == 0 {
		return fmt.Errorf("setting connection %s status to %s: %w", connectionID, status, ErrNotFound)
	}
	return nil
}

// ListConnectionsDueForSync is SYSTEM CONTEXT ONLY (AGENTS.md §1.1): it is the
// single cross-user query, used by the nightly scheduler to decide which users
// to process. It returns identifiers only. API and MCP handlers must not call it.
func (s *Store) ListConnectionsDueForSync(ctx context.Context, now time.Time, limit int32) ([]domain.DueConnection, error) {
	rows, err := s.q.ListConnectionsDueForSync(ctx, db.ListConnectionsDueForSyncParams{Now: now.UTC(), MaxRows: limit})
	if err != nil {
		return nil, fmt.Errorf("listing connections due for sync: %w", err)
	}
	out := make([]domain.DueConnection, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.DueConnection{ID: r.ID, UserID: r.UserID, Provider: domain.Provider(r.Provider)})
	}
	return out, nil
}

func connectionInfo(r db.GetProviderConnectionRow) domain.ConnectionInfo {
	return domain.ConnectionInfo{
		ID: r.ID, UserID: r.UserID, Provider: domain.Provider(r.Provider),
		Status: domain.ConnectionStatus(r.Status), Scopes: r.Scopes,
		NextSyncAt: r.NextSyncAt.UTC(), LastSyncedAt: utcPtr(r.LastSyncedAt), CreatedAt: r.CreatedAt.UTC(),
	}
}

// ---------------------------------------------------------------------------
// Daily summaries
// ---------------------------------------------------------------------------

// UpsertDailySummaries idempotently writes summaries for a user in one
// transaction and returns how many were written.
func (s *Store) UpsertDailySummaries(ctx context.Context, userID uuid.UUID, summaries []domain.DailySummary) (int, error) {
	for _, d := range summaries {
		if d.Provider == "" {
			return 0, fmt.Errorf("upserting daily summary %s: provider is required", d.Date)
		}
		if err := d.Date.Validate(); err != nil {
			return 0, fmt.Errorf("upserting daily summary: %w", err)
		}
	}
	err := s.inTx(ctx, func(tx *Store) error {
		for _, d := range summaries {
			err := tx.q.UpsertDailySummary(ctx, db.UpsertDailySummaryParams{
				UserID: userID, Provider: string(d.Provider), SummaryDate: pgDate(d.Date),
				Steps: pgInt4(d.Steps), RestingHeartRate: pgInt2(d.RestingHeartRate),
				HrvRmssdMs: pgFloat8(d.HRVRMSSDMs), SleepScore: pgInt2(d.SleepScore),
				CaloriesKcal: pgInt4(d.CaloriesKcal), Spo2AvgPct: pgFloat8(d.SpO2AvgPct),
			})
			if err != nil {
				return fmt.Errorf("upserting daily summary %s: %w", d.Date, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("upserting daily summaries for user %s: %w", userID, err)
	}
	return len(summaries), nil
}

// ListDailySummaries returns the user's summaries in r, newest first.
func (s *Store) ListDailySummaries(ctx context.Context, userID uuid.UUID, provider domain.Provider, r domain.DateRange) ([]domain.DailySummary, error) {
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("listing daily summaries: %w", err)
	}
	rows, err := s.q.ListDailySummaries(ctx, db.ListDailySummariesParams{
		UserID: userID, Provider: string(provider), StartDate: pgDate(r.Start), EndDate: pgDate(r.End),
	})
	if err != nil {
		return nil, fmt.Errorf("listing daily summaries for user %s: %w", userID, err)
	}
	out := make([]domain.DailySummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.DailySummary{
			Provider: domain.Provider(row.Provider), Date: fromPgDate(row.SummaryDate),
			Steps: fromInt4(row.Steps), RestingHeartRate: fromInt2(row.RestingHeartRate),
			HRVRMSSDMs: fromFloat8(row.HrvRmssdMs), SleepScore: fromInt2(row.SleepScore),
			CaloriesKcal: fromInt4(row.CaloriesKcal), SpO2AvgPct: fromFloat8(row.Spo2AvgPct),
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Intraday metrics
// ---------------------------------------------------------------------------

// UpsertIntradayMetrics idempotently writes samples of one metric. Duplicate
// timestamps in points are collapsed (last value wins). It returns the number
// of rows inserted or changed; re-writing identical values counts zero.
func (s *Store) UpsertIntradayMetrics(ctx context.Context, userID uuid.UUID, provider domain.Provider, metric domain.MetricType, points []domain.IntradayPoint) (int64, error) {
	if !metric.Valid() {
		return 0, fmt.Errorf("upserting intraday metrics: unknown metric type %q", metric)
	}
	if provider == "" {
		return 0, errors.New("upserting intraday metrics: provider is required")
	}

	// De-duplicate by instant, preserving first-seen order.
	index := make(map[int64]int, len(points))
	times := make([]time.Time, 0, len(points))
	values := make([]float64, 0, len(points))
	for _, p := range points {
		if p.Metric != "" && p.Metric != metric {
			return 0, fmt.Errorf("upserting %s metrics: point has metric %q", metric, p.Metric)
		}
		if p.RecordedAt.IsZero() {
			return 0, fmt.Errorf("upserting %s metrics: point has zero timestamp", metric)
		}
		// PostgreSQL stores microseconds; key on that precision so two
		// sub-microsecond-apart samples cannot collide inside one statement.
		at := p.RecordedAt.UTC().Truncate(time.Microsecond)
		key := at.UnixMicro()
		if i, ok := index[key]; ok {
			values[i] = p.Value
			continue
		}
		index[key] = len(times)
		times = append(times, at)
		values = append(values, p.Value)
	}

	var written int64
	err := s.inTx(ctx, func(tx *Store) error {
		for start := 0; start < len(times); start += intradayBatchSize {
			end := min(start+intradayBatchSize, len(times))
			n, err := tx.q.UpsertIntradayMetrics(ctx, db.UpsertIntradayMetricsParams{
				UserID: userID, Provider: string(provider), MetricType: string(metric),
				RecordedAt: times[start:end], Value: values[start:end],
			})
			if err != nil {
				return fmt.Errorf("upserting %s batch at offset %d: %w", metric, start, err)
			}
			written += n
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("upserting intraday metrics for user %s: %w", userID, err)
	}
	return written, nil
}

// ListIntradayMetrics returns the user's samples of metric in [r.Start, r.End), oldest first.
func (s *Store) ListIntradayMetrics(ctx context.Context, userID uuid.UUID, provider domain.Provider, metric domain.MetricType, r domain.TimeRange) ([]domain.IntradayPoint, error) {
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("listing intraday metrics: %w", err)
	}
	rows, err := s.q.ListIntradayMetrics(ctx, db.ListIntradayMetricsParams{
		UserID: userID, Provider: string(provider), MetricType: string(metric),
		StartAt: r.Start.UTC(), EndAt: r.End.UTC(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s metrics for user %s: %w", metric, userID, err)
	}
	out := make([]domain.IntradayPoint, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.IntradayPoint{Metric: metric, RecordedAt: row.RecordedAt.UTC(), Value: row.Value})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Sleep
// ---------------------------------------------------------------------------

// UpsertSleepSessions idempotently writes sessions keyed by their provider
// session ID, replacing each session's stages wholesale, in one transaction.
func (s *Store) UpsertSleepSessions(ctx context.Context, userID uuid.UUID, sessions []domain.SleepSession) (int, error) {
	for _, ss := range sessions {
		if err := validateSleepSession(ss); err != nil {
			return 0, fmt.Errorf("upserting sleep sessions: %w", err)
		}
	}
	err := s.inTx(ctx, func(tx *Store) error {
		for _, ss := range sessions {
			id, err := tx.q.UpsertSleepSession(ctx, db.UpsertSleepSessionParams{
				UserID: userID, Provider: string(ss.Provider), ProviderSessionID: ss.ProviderSessionID,
				SleepDate: pgDate(ss.SleepDate), StartAt: ss.StartAt.UTC(), EndAt: ss.EndAt.UTC(),
				IsMainSleep: ss.IsMainSleep, EfficiencyPct: pgInt2(ss.EfficiencyPct),
			})
			if err != nil {
				return fmt.Errorf("upserting sleep session %s: %w", ss.ProviderSessionID, err)
			}
			if err := tx.q.DeleteSleepStages(ctx, db.DeleteSleepStagesParams{SessionID: id, UserID: userID}); err != nil {
				return fmt.Errorf("clearing stages of sleep session %s: %w", ss.ProviderSessionID, err)
			}
			if len(ss.Stages) == 0 {
				continue
			}
			params := db.InsertSleepStagesParams{
				SessionID: id, UserID: userID,
				Stage:   make([]string, 0, len(ss.Stages)),
				StartAt: make([]time.Time, 0, len(ss.Stages)),
				EndAt:   make([]time.Time, 0, len(ss.Stages)),
			}
			for _, st := range ss.Stages {
				params.Stage = append(params.Stage, string(st.Stage))
				params.StartAt = append(params.StartAt, st.StartAt.UTC())
				params.EndAt = append(params.EndAt, st.EndAt.UTC())
			}
			if _, err := tx.q.InsertSleepStages(ctx, params); err != nil {
				return fmt.Errorf("inserting stages of sleep session %s: %w", ss.ProviderSessionID, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("upserting sleep sessions for user %s: %w", userID, err)
	}
	return len(sessions), nil
}

func validateSleepSession(ss domain.SleepSession) error {
	switch {
	case ss.Provider == "":
		return fmt.Errorf("sleep session %q: provider is required", ss.ProviderSessionID)
	case ss.ProviderSessionID == "":
		return errors.New("sleep session: provider session ID is required")
	case !ss.EndAt.After(ss.StartAt):
		return fmt.Errorf("sleep session %q: end must be after start", ss.ProviderSessionID)
	}
	if err := ss.SleepDate.Validate(); err != nil {
		return fmt.Errorf("sleep session %q: invalid sleep date: %w", ss.ProviderSessionID, err)
	}
	seen := make(map[int64]bool, len(ss.Stages))
	for _, st := range ss.Stages {
		if !st.Stage.Valid() {
			return fmt.Errorf("sleep session %q: unknown stage %q", ss.ProviderSessionID, st.Stage)
		}
		if !st.EndAt.After(st.StartAt) {
			return fmt.Errorf("sleep session %q: stage at %s must end after it starts", ss.ProviderSessionID, st.StartAt)
		}
		key := st.StartAt.Truncate(time.Microsecond).UnixMicro() // DB precision
		if seen[key] {
			return fmt.Errorf("sleep session %q: duplicate stage start %s", ss.ProviderSessionID, st.StartAt)
		}
		seen[key] = true
	}
	return nil
}

// ListSleepSessions returns the user's sessions whose wake-up date is in r,
// newest first, with their stages in chronological order.
func (s *Store) ListSleepSessions(ctx context.Context, userID uuid.UUID, provider domain.Provider, r domain.DateRange) ([]domain.SleepSession, error) {
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("listing sleep sessions: %w", err)
	}
	rows, err := s.q.ListSleepSessions(ctx, db.ListSleepSessionsParams{
		UserID: userID, Provider: string(provider), StartDate: pgDate(r.Start), EndDate: pgDate(r.End),
	})
	if err != nil {
		return nil, fmt.Errorf("listing sleep sessions for user %s: %w", userID, err)
	}
	// The query groups each session and its stages in the same snapshot.
	out := []domain.SleepSession{}
	for _, row := range rows {
		if len(out) == 0 || out[len(out)-1].ID != row.ID {
			out = append(out, domain.SleepSession{
				ID: row.ID, Provider: domain.Provider(row.Provider), ProviderSessionID: row.ProviderSessionID,
				SleepDate: fromPgDate(row.SleepDate), StartAt: row.StartAt.UTC(), EndAt: row.EndAt.UTC(),
				IsMainSleep: row.IsMainSleep, EfficiencyPct: fromInt2(row.EfficiencyPct),
				Stages: []domain.SleepStage{},
			})
		}
		if row.Stage.Valid {
			session := &out[len(out)-1]
			session.Stages = append(session.Stages, domain.SleepStage{
				Stage: domain.SleepStageType(row.Stage.String), StartAt: row.StageStartAt.UTC(), EndAt: row.StageEndAt.UTC(),
			})
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Sync runs
// ---------------------------------------------------------------------------

// StartSyncRun records the start of an ingestion job. It returns
// ErrSyncInProgress if the user already has a running sync for provider.
func (s *Store) StartSyncRun(ctx context.Context, userID uuid.UUID, provider domain.Provider, trigger domain.SyncTrigger, r domain.DateRange) (domain.SyncRun, error) {
	if err := r.Validate(); err != nil {
		return domain.SyncRun{}, fmt.Errorf("starting sync run: %w", err)
	}
	row, err := s.q.CreateSyncRun(ctx, db.CreateSyncRunParams{
		UserID: userID, Provider: string(provider), Trigger: string(trigger),
		RangeStart: pgDate(r.Start), RangeEnd: pgDate(r.End),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrSyncInProgress
	}
	if err != nil {
		return domain.SyncRun{}, fmt.Errorf("starting %s sync run for user %s: %w", provider, userID, err)
	}
	return syncRun(row), nil
}

// AbandonStaleSyncRuns marks the user's runs for provider whose last
// heartbeat (or start) is older than olderThan, by the database clock, as
// failed, so a crashed worker cannot block syncing forever. It returns how
// many it abandoned.
func (s *Store) AbandonStaleSyncRuns(ctx context.Context, userID uuid.UUID, provider domain.Provider, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		return 0, errors.New("abandoning stale sync runs: olderThan must be positive")
	}
	n, err := s.q.AbandonStaleSyncRuns(ctx, db.AbandonStaleSyncRunsParams{
		UserID: userID, Provider: string(provider), OlderThanSeconds: olderThan.Seconds(),
	})
	if err != nil {
		return 0, fmt.Errorf("abandoning stale %s sync runs for user %s: %w", provider, userID, err)
	}
	return n, nil
}

// TouchSyncRun records that a running job is still making progress. It
// returns ErrNotFound if the run does not belong to the user or is no longer
// running (for example, another worker abandoned it as stale).
func (s *Store) TouchSyncRun(ctx context.Context, userID, runID uuid.UUID) error {
	n, err := s.q.TouchSyncRun(ctx, db.TouchSyncRunParams{ID: runID, UserID: userID})
	if err != nil {
		return fmt.Errorf("touching sync run %s: %w", runID, err)
	}
	if n == 0 {
		return fmt.Errorf("touching sync run %s: %w", runID, ErrNotFound)
	}
	return nil
}

// FinishSyncRun records a running job's outcome. It returns ErrNotFound if the
// run does not belong to the user or has already finished.
func (s *Store) FinishSyncRun(ctx context.Context, userID, runID uuid.UUID, status domain.SyncStatus, records int32, errMsg string) (domain.SyncRun, error) {
	if status == domain.SyncRunning {
		return domain.SyncRun{}, errors.New("finishing sync run: status must be terminal")
	}
	row, err := s.q.FinishSyncRun(ctx, db.FinishSyncRunParams{
		Status: string(status), RecordsIngested: records,
		ErrorMessage: pgtype.Text{String: errMsg, Valid: errMsg != ""},
		ID:           runID, UserID: userID,
	})
	if err != nil {
		return domain.SyncRun{}, fmt.Errorf("finishing sync run %s: %w", runID, notFound(err))
	}
	return syncRun(row), nil
}

// ListSyncRuns returns the user's most recent sync runs for provider.
func (s *Store) ListSyncRuns(ctx context.Context, userID uuid.UUID, provider domain.Provider, limit int32) ([]domain.SyncRun, error) {
	rows, err := s.q.ListSyncRuns(ctx, db.ListSyncRunsParams{UserID: userID, Provider: string(provider), MaxRows: limit})
	if err != nil {
		return nil, fmt.Errorf("listing sync runs for user %s: %w", userID, err)
	}
	out := make([]domain.SyncRun, 0, len(rows))
	for _, row := range rows {
		out = append(out, syncRun(row))
	}
	return out, nil
}

func syncRun(r db.SyncRun) domain.SyncRun {
	return domain.SyncRun{
		ID: r.ID, UserID: r.UserID, Provider: domain.Provider(r.Provider),
		Trigger: domain.SyncTrigger(r.Trigger), Status: domain.SyncStatus(r.Status),
		Range:           domain.DateRange{Start: fromPgDate(r.RangeStart), End: fromPgDate(r.RangeEnd)},
		RecordsIngested: r.RecordsIngested, ErrorMessage: r.ErrorMessage.String,
		StartedAt: r.StartedAt.UTC(), FinishedAt: utcPtr(r.FinishedAt),
	}
}
