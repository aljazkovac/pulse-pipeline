# ADR 0002: Canonical Health Data Schema, Store Layer & `HealthProvider` Contract

- **Status:** Proposed
- **Date:** 2026-11-21

## Context

Phase 1 needs a PostgreSQL schema that holds normalized wearable data for many users and many
providers, survives repeated nightly syncs and backfills without duplicates, and serves the
Phase 2 dashboards and Phase 3 MCP tools efficiently. ADR-0001 already fixed the stack
(PostgreSQL 17, `pgx`, `sqlc`, `golang-migrate`) and the `user_id` / `HealthProvider` invariants;
this ADR fixes the concrete tables, keys, token handling and Go package contracts.

Forces:

- **Idempotency:** a sync for a date range must be re-runnable (retries, backfills, provider
  corrections) and converge to the same rows.
- **Isolation:** every row of health data belongs to exactly one user; no API or MCP query may
  read across users (`AGENTS.md` §1.1).
- **Read patterns:** dashboards and MCP tools read one user's data for a date or time range,
  newest first; intraday heart rate is the high-volume table (~1 sample/5 s ≈ 17k rows/user/day).
- **Secrets:** OAuth refresh tokens grant long-lived access to a person's health data.

## Decision

### 1. Tables

All timestamps are `TIMESTAMPTZ` (stored UTC). Local calendar days are `DATE` values computed in
the user's IANA timezone (`users.timezone`) at ingestion time. Every tenant-owned table has
`user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE`, so deleting a user erases all
their data. Every ingested table carries `provider VARCHAR(32)` (`'google_health'`, `'mock'`).

| Table | Primary / unique key | Purpose |
| :--- | :--- | :--- |
| `users` | `id` (UUID); unique `lower(email)` | Account and IANA `timezone`. |
| `provider_connections` | `id`; unique `(user_id, provider)` | Encrypted OAuth tokens, scopes, status, sync cursor, `next_sync_at`. |
| `daily_summaries` | PK `(user_id, provider, summary_date)` | One row per local day: steps, resting HR, HRV (RMSSD), sleep score, calories, SpO2. All metrics nullable (a device may not report them). |
| `intraday_metrics` | PK `(user_id, provider, metric_type, recorded_at)` | One sample per metric per instant (`heart_rate`, `steps`, `hrv_rmssd`, `spo2`, `calories`). |
| `sleep_sessions` | `id`; unique `(user_id, provider, provider_session_id)` | Upstream session ID stays stable when the provider corrects start/end times. `sleep_date` is the local wake-up date. |
| `sleep_stages` | PK `(session_id, start_at)` | Stage intervals (`deep`, `rem`, `light`, `awake`). Composite FK `(session_id, user_id) → sleep_sessions(id, user_id)` guarantees a stage can never point at another user's session. |
| `sync_runs` | `id` | Audit log: trigger, status, requested date range, rows ingested, error. |

Natural keys double as upsert targets: all ingestion writes are
`INSERT … ON CONFLICT (<natural key>) DO UPDATE`. Sleep stages are replaced wholesale per
session (delete + insert inside the session-upsert transaction), because providers re-segment
stages when they re-score a night.

Domain value sets (`metric_type`, `stage`, `status`, `trigger`) are `VARCHAR` + `CHECK`
constraints rather than PostgreSQL `ENUM`s: adding a value is a one-line migration that can be
rolled back, and `sqlc` maps them to plain strings that the Go domain layer types explicitly.

### 2. Indexes

The primary keys serve the main reads by backward scan (e.g.
`WHERE user_id=$1 AND provider=$2 AND metric_type=$3 AND recorded_at >= $4 AND recorded_at < $5 ORDER BY recorded_at`,
forward or backward scan).
Only two secondary indexes are added, each for a read the natural key cannot serve:

- `sleep_sessions (user_id, provider, sleep_date)` — sleep views query by night, not by upstream ID.
- `sync_runs (user_id, provider, started_at DESC)` — "last sync" status in the UI and MCP.

`provider_connections` (one row per user and provider) gets no scheduler index; a sequential scan
for the due-list is cheaper than maintaining one until there are thousands of connections.

`intraday_metrics` is **not partitioned** yet. At single-user scale (~6M rows/year) a B-tree PK
is ample; declarative monthly range partitioning on `recorded_at` remains an option that does
not change the key or the queries.

### 3. OAuth token storage

Access and refresh tokens are stored only as **application-level AES-256-GCM ciphertext**
(`BYTEA`) in `provider_connections`, with a `token_key_version SMALLINT` to support key rotation.
The key comes from the `TOKEN_ENCRYPTION_KEY` environment variable (a Kubernetes Secret in
cluster). Plaintext tokens never reach the database, logs, API responses or MCP output, and the
store's read model for connections omits the ciphertext columns except for the one query the
sync worker uses to refresh tokens. The cipher implementation lands with the
`GoogleHealthProvider`; this ADR fixes the column shape.

### 4. System-context query

`ListConnectionsDueForSync` is the only cross-user query. It returns `(connection id, user_id,
provider, sync_cursor)` for active connections with `next_sync_at <= now()` — never health data
or tokens — and lives in a separately named store method that API and MCP handlers do not use
(`AGENTS.md` §1.1 exception).

### 5. Go package contracts (`backend/internal/…`)

- `domain`: pure types (`DailySummary`, `IntradayPoint`, `SleepSession`, `SleepStage`,
  `MetricType`, …) and the `HealthProvider` interface. No `pgx`, HTTP or vendor imports.
- `store`: `sqlc`-generated code in `store/db` (committed, regenerated with `make generate`) plus
  a hand-written `Store` that maps domain types ↔ rows, owns transactions, and requires a
  `userID` on every health-data method.
- `provider/{mock,googlehealth}`: `HealthProvider` implementations (next PRs).
- `sync`: nightly orchestrator writing `sync_runs` (next PRs).

```go
type HealthProvider interface {
    Name() Provider
    FetchDailySummaries(ctx context.Context, conn Connection, r DateRange) ([]DailySummary, error)
    FetchIntradayMetrics(ctx context.Context, conn Connection, metric MetricType, r TimeRange) ([]IntradayPoint, error)
    FetchSleepSessions(ctx context.Context, conn Connection, r DateRange) ([]SleepSession, error)
    RefreshAuth(ctx context.Context, conn Connection) (Credentials, error)
}
```

Providers return normalized domain values only; raw Google Health JSON never leaves
`provider/googlehealth`.

### 6. Migrations

`golang-migrate` SQL files live in `backend/migrations/` and are embedded into the binaries
(`embed.FS`), so the worker, tests and a `make migrate-up` target all apply the same files.
Every `up` migration has a matching `down`.

## Implementation Status

This ADR records decisions; it is delivered across several PRs.

| Section | Status |
| :--- | :--- |
| §1 Tables, §2 Indexes, §4 System-context query, §6 Migrations | Implemented (migration `000001`, `store`, Testcontainers tests). |
| §5 `domain` types + `HealthProvider` interface, `store` package | Implemented. |
| §3 Token storage | Columns only. AES-256-GCM cipher, `TOKEN_ENCRYPTION_KEY`, and the token read/write queries land with `GoogleHealthProvider`. |
| §5 `provider/mock`, `provider/googlehealth`, `sync` | Pending (next Phase 1 PRs). Migrations will be applied by the worker at startup then. |

## Consequences

- **Positive:** Re-running any sync converges (upserts on natural keys); deleting a user cascades
  to all their data; dashboard/MCP reads are single index range scans; type safety from SQL to
  Go via `sqlc`; tokens are useless without the separately held key.
- **Trade-offs:** Wide nullable `daily_summaries` columns need a migration per new daily metric
  (acceptable: few, well-known metrics). Application-level encryption means the key must be
  provisioned in every environment and rotated by re-encrypting rows. `CHECK`-constrained strings
  are validated at runtime, not by the Go compiler — the domain layer exposes typed constants.
- **Go toolchain:** `pgx` v5, `golang-migrate` and Testcontainers require a newer Go than 1.23, so
  both modules move to **Go 1.26** (the oldest release still supported upstream).
