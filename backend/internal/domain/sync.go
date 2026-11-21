package domain

import (
	"time"

	"github.com/google/uuid"
)

// ConnectionInfo is the non-secret view of a provider connection, safe to
// return from API and MCP handlers.
type ConnectionInfo struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	Provider     Provider
	Status       ConnectionStatus
	Scopes       []string
	NextSyncAt   time.Time
	LastSyncedAt *time.Time
	CreatedAt    time.Time
}

// DueConnection identifies a connection the nightly scheduler should sync.
type DueConnection struct {
	ID       uuid.UUID
	UserID   uuid.UUID
	Provider Provider
}

// SyncTrigger records why a sync run started.
type SyncTrigger string

const (
	SyncTriggerScheduled SyncTrigger = "scheduled"
	SyncTriggerManual    SyncTrigger = "manual"
	SyncTriggerBackfill  SyncTrigger = "backfill"
)

// SyncStatus is the outcome of a sync run.
type SyncStatus string

const (
	SyncRunning   SyncStatus = "running"
	SyncSucceeded SyncStatus = "succeeded"
	SyncPartial   SyncStatus = "partial"
	SyncFailed    SyncStatus = "failed"
)

// SyncRun is one audited ingestion job for a user and provider.
type SyncRun struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	Provider        Provider
	Trigger         SyncTrigger
	Status          SyncStatus
	Range           DateRange
	RecordsIngested int32
	ErrorMessage    string
	StartedAt       time.Time
	FinishedAt      *time.Time
}
