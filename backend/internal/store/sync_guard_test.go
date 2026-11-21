package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/aljazkovac/pulse-pipeline/backend/internal/domain"
	"github.com/aljazkovac/pulse-pipeline/backend/internal/store"
)

func TestSyncRuns_OneRunningPerUserAndProvider(t *testing.T) {
	ctx := ctxT(t)
	a, b := newUser(t), newUser(t)
	r := domain.DateRange{Start: mustDate(t, "2026-11-20"), End: mustDate(t, "2026-11-21")}

	first, err := testStore.StartSyncRun(ctx, a.ID, mock, domain.SyncTriggerScheduled, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testStore.StartSyncRun(ctx, a.ID, mock, domain.SyncTriggerManual, r); !errors.Is(err, store.ErrSyncInProgress) {
		t.Fatalf("second running sync: err = %v, want ErrSyncInProgress", err)
	}
	// Other providers and other users are unaffected.
	if _, err := testStore.StartSyncRun(ctx, a.ID, domain.ProviderGoogleHealth, domain.SyncTriggerManual, r); err != nil {
		t.Errorf("other provider blocked: %v", err)
	}
	if _, err := testStore.StartSyncRun(ctx, b.ID, mock, domain.SyncTriggerManual, r); err != nil {
		t.Errorf("other user blocked: %v", err)
	}
	// Finishing frees the slot.
	if _, err := testStore.FinishSyncRun(ctx, a.ID, first.ID, domain.SyncSucceeded, 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := testStore.StartSyncRun(ctx, a.ID, mock, domain.SyncTriggerManual, r); err != nil {
		t.Errorf("slot not freed after finishing: %v", err)
	}
}

func TestSyncRuns_AbandonStaleIsScopedAndUsesRunAge(t *testing.T) {
	ctx := ctxT(t)
	a, b := newUser(t), newUser(t)
	r := domain.DateRange{Start: mustDate(t, "2026-11-20"), End: mustDate(t, "2026-11-21")}
	runA, err := testStore.StartSyncRun(ctx, a.ID, mock, domain.SyncTriggerScheduled, r)
	if err != nil {
		t.Fatal(err)
	}
	runB, err := testStore.StartSyncRun(ctx, b.ID, mock, domain.SyncTriggerScheduled, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE sync_runs SET started_at = now() - interval '3 hours' WHERE id = ANY($1)`,
		[]any{runA.ID, runB.ID}); err != nil {
		t.Fatal(err)
	}

	if n, err := testStore.AbandonStaleSyncRuns(ctx, a.ID, mock, 4*time.Hour); err != nil || n != 0 {
		t.Fatalf("younger than the threshold: abandoned %d, %v", n, err)
	}
	if n, err := testStore.AbandonStaleSyncRuns(ctx, a.ID, mock, 2*time.Hour); err != nil || n != 1 {
		t.Fatalf("stale run: abandoned %d, %v", n, err)
	}
	runs, err := testStore.ListSyncRuns(ctx, a.ID, mock, 1)
	if err != nil || runs[0].Status != domain.SyncFailed || runs[0].FinishedAt == nil || runs[0].ErrorMessage == "" {
		t.Fatalf("abandoned run = %+v, %v", runs, err)
	}
	if other, _ := testStore.ListSyncRuns(ctx, b.ID, mock, 1); other[0].Status != domain.SyncRunning {
		t.Errorf("abandoning user A's runs touched user B's: %+v", other[0])
	}
	if _, err := testStore.AbandonStaleSyncRuns(ctx, a.ID, mock, 0); err == nil {
		t.Error("non-positive age must be rejected")
	}
}

func TestConnections_RescheduleIsScopedToOwner(t *testing.T) {
	ctx := ctxT(t)
	a, b := newUser(t), newUser(t)
	c, err := testStore.CreateConnection(ctx, a.ID, mock, nil)
	if err != nil {
		t.Fatal(err)
	}
	next := time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)
	if err := testStore.RescheduleConnection(ctx, b.ID, c.ID, next); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another user rescheduling: err = %v, want ErrNotFound", err)
	}
	if err := testStore.RescheduleConnection(ctx, a.ID, c.ID, next); err != nil {
		t.Fatal(err)
	}
	got, err := testStore.GetConnection(ctx, a.ID, mock)
	if err != nil || !got.NextSyncAt.Equal(next) || got.LastSyncedAt != nil {
		t.Fatalf("connection = %+v, %v; want only next_sync_at moved", got, err)
	}
}

func TestSyncRuns_HeartbeatDefersAbandonmentAndIsScoped(t *testing.T) {
	ctx := ctxT(t)
	a, b := newUser(t), newUser(t)
	r := domain.DateRange{Start: mustDate(t, "2026-11-20"), End: mustDate(t, "2026-11-21")}
	run, err := testStore.StartSyncRun(ctx, a.ID, mock, domain.SyncTriggerBackfill, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := testStore.TouchSyncRun(ctx, b.ID, run.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another user touching the run: err = %v, want ErrNotFound", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE sync_runs SET started_at = now() - interval '5 hours' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := testStore.TouchSyncRun(ctx, a.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := testStore.AbandonStaleSyncRuns(ctx, a.ID, mock, 2*time.Hour); err != nil || n != 0 {
		t.Fatalf("old run with a fresh heartbeat abandoned: %d, %v", n, err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE sync_runs SET heartbeat_at = now() - interval '3 hours' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := testStore.AbandonStaleSyncRuns(ctx, a.ID, mock, 2*time.Hour); err != nil || n != 1 {
		t.Fatalf("run with a stale heartbeat: abandoned %d, %v", n, err)
	}
	if err := testStore.TouchSyncRun(ctx, a.ID, run.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("touching an abandoned run: err = %v, want ErrNotFound", err)
	}
}
