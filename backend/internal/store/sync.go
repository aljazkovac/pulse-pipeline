package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store/db"
)

var (
	// ErrConnectionInactive indicates the connection is revoked or in error.
	ErrConnectionInactive = errors.New("store: connection is not active")
	// ErrSyncNotDue indicates a scheduled claim found a future due time.
	ErrSyncNotDue = errors.New("store: connection is no longer due for sync")
	// ErrSyncRunAbandoned indicates that a worker no longer owns the run.
	ErrSyncRunAbandoned = errors.New("store: sync run is no longer active (possibly abandoned)")
)

// ClaimSyncRun checks connection eligibility and plans the run from the current
// cursor while holding the same connection lock used by finalization.
func (s *Store) ClaimSyncRun(ctx context.Context, userID uuid.UUID, provider domain.Provider, trigger domain.SyncTrigger, now time.Time, staleAfter time.Duration, plan func([]byte) (domain.DateRange, error)) (domain.SyncRun, uuid.UUID, error) {
	var run domain.SyncRun
	var connectionID uuid.UUID
	err := s.inTx(ctx, func(tx *Store) error {
		conn, err := tx.lockConnectionForSync(ctx, userID, provider)
		if err != nil {
			return err
		}
		if conn.Status != string(domain.ConnectionActive) {
			return fmt.Errorf("%w: %s connection for user %s is %s", ErrConnectionInactive, provider, userID, conn.Status)
		}
		if trigger == domain.SyncTriggerScheduled && conn.NextSyncAt.After(now) {
			return ErrSyncNotDue
		}
		r, err := plan(conn.SyncCursor)
		if err != nil {
			return fmt.Errorf("planning range for connection %s: %w", conn.ID, err)
		}
		if _, err := tx.AbandonStaleSyncRuns(ctx, userID, provider, staleAfter); err != nil {
			return err
		}
		run, err = tx.StartSyncRun(ctx, userID, provider, trigger, r)
		connectionID = conn.ID
		return err
	})
	if err != nil {
		return domain.SyncRun{}, uuid.Nil, fmt.Errorf("claiming %s sync for user %s: %w", provider, userID, err)
	}
	return run, connectionID, nil
}

func (s *Store) lockConnectionForSync(ctx context.Context, userID uuid.UUID, provider domain.Provider) (db.LockConnectionForSyncRow, error) {
	conn, err := s.q.LockConnectionForSync(ctx, db.LockConnectionForSyncParams{UserID: userID, Provider: string(provider)})
	if err != nil {
		return conn, fmt.Errorf("locking %s connection for user %s: %w", provider, userID, notFound(err))
	}
	return conn, nil
}

// RescheduleDueConnection postpones a failed claim if the connection is still
// active and due. A newer schedule is left alone.
func (s *Store) RescheduleDueConnection(ctx context.Context, userID, connectionID uuid.UUID, dueAt, nextSyncAt time.Time) error {
	if err := s.q.RescheduleDueConnection(ctx, db.RescheduleDueConnectionParams{
		ID: connectionID, UserID: userID, DueAt: dueAt.UTC(), NextSyncAt: nextSyncAt.UTC(),
	}); err != nil {
		return fmt.Errorf("rescheduling due connection %s: %w", connectionID, err)
	}
	return nil
}

// SyncUnitWriter writes fetched health data within a run's transaction.
type SyncUnitWriter interface {
	UpsertDailySummaries(context.Context, uuid.UUID, []domain.DailySummary) (int, error)
	UpsertSleepSessions(context.Context, uuid.UUID, []domain.SleepSession) (int, error)
	UpsertIntradayMetrics(context.Context, uuid.UUID, domain.Provider, domain.MetricType, []domain.IntradayPoint) (int64, error)
}

// WriteSyncUnit renews the heartbeat and holds the run row lock until the
// health writes commit. The callback must only write data already fetched.
func (s *Store) WriteSyncUnit(ctx context.Context, userID, runID uuid.UUID, write func(SyncUnitWriter) (int64, error)) (int64, error) {
	var written int64
	err := s.inTx(ctx, func(tx *Store) error {
		err := tx.TouchSyncRun(ctx, userID, runID)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("writing sync run %s: %w", runID, ErrSyncRunAbandoned)
		}
		if err != nil {
			return err
		}
		written, err = write(tx)
		return err
	})
	if err != nil {
		return 0, err
	}
	return written, nil
}

// SyncResult describes the terminal audit record and its connection update.
// A zero SyncedAt or NextSyncAt leaves the corresponding state alone.
type SyncResult struct {
	Status          domain.SyncStatus
	RecordsIngested int32
	ErrorMessage    string
	Unauthorized    bool
	SyncedAt        time.Time
	NextSyncAt      time.Time
	Cursor          []byte
}

// FinishConnectionSyncRun closes a live run and applies its connection update
// atomically. Claim and finish both acquire the connection lock before the run.
func (s *Store) FinishConnectionSyncRun(ctx context.Context, run domain.SyncRun, connectionID uuid.UUID, result SyncResult) (domain.SyncRun, error) {
	var finished domain.SyncRun
	err := s.inTx(ctx, func(tx *Store) error {
		conn, err := tx.lockConnectionForSync(ctx, run.UserID, run.Provider)
		if err != nil {
			return err
		}
		finished, err = tx.FinishSyncRun(ctx, run.UserID, run.ID, result.Status, result.RecordsIngested, result.ErrorMessage)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("finishing sync run %s: %w", run.ID, ErrSyncRunAbandoned)
		}
		if err != nil {
			return err
		}
		if finished.Provider != run.Provider || conn.ID != connectionID {
			return fmt.Errorf("finishing sync run %s: connection changed: %w", run.ID, ErrNotFound)
		}
		switch {
		case result.Unauthorized:
			return tx.SetConnectionStatus(ctx, run.UserID, conn.ID, domain.ConnectionError)
		case !result.SyncedAt.IsZero():
			return tx.MarkConnectionSynced(ctx, run.UserID, conn.ID, result.SyncedAt, result.NextSyncAt, result.Cursor)
		case !result.NextSyncAt.IsZero():
			return tx.RescheduleConnection(ctx, run.UserID, conn.ID, result.NextSyncAt)
		default:
			return nil
		}
	})
	if err != nil {
		return run, err
	}
	return finished, nil
}
